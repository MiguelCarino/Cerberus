// Package bench is Cerberus's stable benchmark (gateway phase G6): a fixed,
// append-only prompt set of six kinds and three prefill sizes, run through
// the gateway at temperature 0, recording time to first token, decode and
// prefill speed and energy per request. Runs are JSON files in
// <data>/bench, carrying the model's configuration, so two runs compare
// like for like (`cerberus bench compare`). It is the ground for the Phase 3
// tokens-per-joule tuner and for the Leaderboard.
package bench

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
)

//go:embed prompts.json
var promptsJSON []byte

// Prompt is one bench prompt.
type Prompt struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	MaxTokens int    `json:"max_tokens"`
	Prompt    string `json:"prompt"`
}

// Set is the embedded prompt set.
type Set struct {
	Prompts []Prompt `json:"prompts"`
	Prefill struct {
		Question  string   `json:"question"`
		MaxTokens int      `json:"max_tokens"`
		Filler    []string `json:"filler"`
	} `json:"prefill"`
}

// Load returns the embedded prompt set.
func Load() (Set, error) {
	var s Set
	err := json.Unmarshal(promptsJSON, &s)
	return s, err
}

// Kinds are the prompt kinds, in order.
var Kinds = []string{"code", "edit", "proof", "procedure", "chat", "prose"}

// PrefillSizes are the prefill prompt sizes in tokens; 0 is ctx/2.
var PrefillSizes = []int{8192, 32768, 0}

// Options are what a run does.
type Options struct {
	Model    string   `json:"model"`
	Repeat   int      `json:"repeat"`
	Kinds    []string `json:"kinds,omitempty"` // empty = all
	Prefill  bool     `json:"prefill"`
	Label    string   `json:"label,omitempty"`
	Thinking bool     `json:"thinking"` // default off
	Ctx      int      `json:"ctx,omitempty"`
}

// Sample is one request's figures.
type Sample struct {
	Prompt           string  `json:"prompt"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TTFTMs           float64 `json:"ttft_ms"`
	DurationMs       float64 `json:"duration_ms"`
	DecodeTPS        float64 `json:"decode_tps"`
	PrefillTPS       float64 `json:"prefill_tps"`
	EnergyJ          float64 `json:"energy_j,omitempty"`
	TokPerJ          float64 `json:"tok_per_j,omitempty"`
	Error            string  `json:"error,omitempty"`
}

// Stats are a kind's medians and spread.
type Stats struct {
	TTFTMs     float64 `json:"ttft_ms"`
	DecodeTPS  float64 `json:"decode_tps"`
	PrefillTPS float64 `json:"prefill_tps"`
	TokPerJ    float64 `json:"tok_per_j,omitempty"`
	EnergyJ    float64 `json:"energy_j,omitempty"`
}

// KindResult is one kind's samples and summary.
type KindResult struct {
	Kind    string   `json:"kind"`
	Samples []Sample `json:"samples"`
	Median  Stats    `json:"median"`
	Min     Stats    `json:"min"`
	Max     Stats    `json:"max"`
	Errors  int      `json:"errors,omitempty"`
}

// ModelConfig is what a run records about the model's setup.
type ModelConfig struct {
	Backend     string   `json:"backend,omitempty"`
	Path        string   `json:"path,omitempty"`
	Ctx         int      `json:"ctx,omitempty"`
	Parallel    int      `json:"parallel,omitempty"`
	Args        []string `json:"args,omitempty"`
	Draft       string   `json:"draft,omitempty"`
	GPU         string   `json:"gpu,omitempty"`
	PowerLimitW float64  `json:"power_limit_w,omitempty"`
	Version     string   `json:"cerberus_version,omitempty"`
}

// Run is one bench run.
type Run struct {
	ID       string       `json:"id"`
	Time     time.Time    `json:"time"`
	Done     time.Time    `json:"done,omitzero"`
	State    string       `json:"state"` // running | done | failed
	Options  Options      `json:"options"`
	Config   ModelConfig  `json:"config"`
	Kinds    []KindResult `json:"kinds"`
	Error    string       `json:"error,omitempty"`
	Progress string       `json:"progress,omitempty"`
}

// Client is how a run reaches the gateway and the usage records.
type Client struct {
	Base  string // http://host:port (the Cerberus root)
	Token string
	HTTP  *http.Client
}

// ErrBusy is returned when agent sessions are running and the run was not forced.
var ErrBusy = errors.New("agent sessions are running: their requests would share the GPU and skew the numbers (use --force to run anyway)")

func (c Client) do(ctx context.Context, method, path string, in, out any, hdr map[string]string) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cerberus-Client", "cerberus-bench")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if out != nil {
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return resp, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
		}
		return resp, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp, nil
}

// Busy reports whether agent sessions are running (GET /api/status).
func (c Client) Busy(ctx context.Context) (bool, error) {
	var st struct {
		Sessions map[string]int `json:"sessions"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/api/status", nil, &st, nil); err != nil {
		return false, err
	}
	for _, s := range []string{"starting", "running", "waiting", "checking", "retrying"} {
		if st.Sessions[s] > 0 {
			return true, nil
		}
	}
	return false, nil
}

// filler builds a prefill text of about tokens tokens (4 bytes a token)
// from the fixed filler lines, numbered so no two lines are equal.
func (s Set) filler(tokens int) string {
	var sb strings.Builder
	want := tokens * 4
	for i := 0; sb.Len() < want; i++ {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, s.Prefill.Filler[i%len(s.Prefill.Filler)])
	}
	return sb.String()
}

type job struct {
	kind, name string
	prompt     string
	maxTokens  int
}

// jobs lists a run's requests.
func (s Set) jobs(o Options) []job {
	var out []job
	for _, p := range s.Prompts {
		if len(o.Kinds) > 0 && !slices.Contains(o.Kinds, p.Kind) {
			continue
		}
		out = append(out, job{kind: p.Kind, name: p.Name, prompt: p.Prompt, maxTokens: p.MaxTokens})
	}
	if o.Prefill {
		for _, n := range PrefillSizes {
			if n == 0 {
				if o.Ctx <= 0 {
					continue
				}
				n = o.Ctx / 2
			}
			if o.Ctx > 0 && n+256 > o.Ctx {
				continue // does not fit the context
			}
			out = append(out, job{kind: fmt.Sprintf("prefill_%dk", n/1024), name: fmt.Sprintf("prefill_%d", n),
				prompt: s.filler(n) + "\n" + s.Prefill.Question, maxTokens: s.Prefill.MaxTokens})
		}
	}
	return out
}

// Execute runs the bench: every job o.Repeat times (after one unmeasured
// warm-up request that loads the model), filling r.Kinds. progress, when
// set, hears each step.
func Execute(ctx context.Context, c Client, set Set, o Options, r *Run, progress func(string)) error {
	if o.Repeat <= 0 {
		o.Repeat = 3
	}
	jobs := set.jobs(o)
	if len(jobs) == 0 {
		return errors.New("no prompts match the kinds asked for")
	}
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	say("warming up " + o.Model)
	if _, err := c.one(ctx, o, job{kind: "warmup", name: "warmup", prompt: "Say OK.", maxTokens: 4}, r.ID+"-warmup"); err != nil {
		return fmt.Errorf("warm-up: %w", err)
	}
	byKind := map[string]*KindResult{}
	var order []string
	n := 0
	for rep := 0; rep < o.Repeat; rep++ {
		for _, j := range jobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			n++
			say(fmt.Sprintf("%d/%d %s", n, o.Repeat*len(jobs), j.name))
			s, err := c.one(ctx, o, j, fmt.Sprintf("%s-%d", r.ID, n))
			if err != nil {
				s.Error = err.Error()
			}
			kr := byKind[j.kind]
			if kr == nil {
				kr = &KindResult{Kind: j.kind}
				byKind[j.kind] = kr
				order = append(order, j.kind)
			}
			kr.Samples = append(kr.Samples, s)
		}
	}
	for _, k := range order {
		kr := byKind[k]
		summarize(kr)
		r.Kinds = append(r.Kinds, *kr)
	}
	return nil
}

// one sends one streamed request and measures it.
func (c Client) one(ctx context.Context, o Options, j job, tag string) (Sample, error) {
	s := Sample{Prompt: j.name}
	body := map[string]any{"model": o.Model, "stream": true, "temperature": 0, "max_tokens": j.maxTokens,
		"stream_options": map[string]bool{"include_usage": true},
		"messages":       []map[string]string{{"role": "user", "content": j.prompt}}}
	if !o.Thinking {
		body["reasoning_effort"] = "none"
		body["chat_template_kwargs"] = map[string]bool{"enable_thinking": false}
	}
	start := time.Now()
	resp, err := c.do(ctx, http.MethodPost, "/v1/chat/completions", body, nil, map[string]string{"X-Cerberus-Session": tag})
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return s, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var first time.Time
	deltas := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		p, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		p = strings.TrimSpace(p)
		if p == "[DONE]" {
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(p), &ch) != nil {
			continue
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			return s, fmt.Errorf("stream error: %s", ch.Error)
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" || c.Delta.ReasoningContent != "" {
				if first.IsZero() {
					first = time.Now()
				}
				deltas++
			}
		}
		if ch.Usage != nil {
			s.PromptTokens, s.CompletionTokens = ch.Usage.PromptTokens, ch.Usage.CompletionTokens
		}
	}
	if err := sc.Err(); err != nil {
		return s, err
	}
	end := time.Now()
	if s.CompletionTokens == 0 {
		s.CompletionTokens = deltas
	}
	s.DurationMs = ms(end.Sub(start))
	if !first.IsZero() {
		s.TTFTMs = ms(first.Sub(start))
		if dec := end.Sub(first).Seconds(); dec > 0.05 && s.CompletionTokens > 1 {
			s.DecodeTPS = float64(s.CompletionTokens-1) / dec
		}
		if t := first.Sub(start).Seconds(); t > 0 && s.PromptTokens > 0 {
			s.PrefillTPS = float64(s.PromptTokens) / t
		}
	}
	s.EnergyJ = c.energyOf(ctx, tag)
	if s.EnergyJ > 0 && s.CompletionTokens > 0 {
		s.TokPerJ = float64(s.CompletionTokens) / s.EnergyJ
	}
	return s, nil
}

// energyOf reads a request's energy from the usage records (the gateway
// tags it with the session header the run sent).
func (c Client) energyOf(ctx context.Context, tag string) float64 {
	var u struct {
		Recent []struct {
			SessionID string
			EnergyJ   float64
		} `json:"recent"`
	}
	// The record is written as the gateway finishes the request, which can
	// be a moment after the client read the end of the stream.
	for try := 0; try < 4; try++ {
		if try > 0 {
			time.Sleep(time.Duration(try) * 50 * time.Millisecond)
		}
		if _, err := c.do(ctx, http.MethodGet, "/api/usage?limit=20", nil, &u, nil); err != nil {
			return 0
		}
		for _, r := range u.Recent {
			if r.SessionID == tag {
				return r.EnergyJ
			}
		}
	}
	return 0
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// summarize fills a kind's medians and spread from its good samples.
func summarize(kr *KindResult) {
	pick := func(f func(Sample) float64) (med, lo, hi float64) {
		var v []float64
		for _, s := range kr.Samples {
			if s.Error == "" {
				if x := f(s); x > 0 {
					v = append(v, x)
				}
			}
		}
		if len(v) == 0 {
			return 0, 0, 0
		}
		sort.Float64s(v)
		m := v[len(v)/2]
		if len(v)%2 == 0 {
			m = (v[len(v)/2-1] + v[len(v)/2]) / 2
		}
		return m, v[0], v[len(v)-1]
	}
	for _, s := range kr.Samples {
		if s.Error != "" {
			kr.Errors++
		}
	}
	kr.Median.TTFTMs, kr.Min.TTFTMs, kr.Max.TTFTMs = pick(func(s Sample) float64 { return s.TTFTMs })
	kr.Median.DecodeTPS, kr.Min.DecodeTPS, kr.Max.DecodeTPS = pick(func(s Sample) float64 { return s.DecodeTPS })
	kr.Median.PrefillTPS, kr.Min.PrefillTPS, kr.Max.PrefillTPS = pick(func(s Sample) float64 { return s.PrefillTPS })
	kr.Median.TokPerJ, kr.Min.TokPerJ, kr.Max.TokPerJ = pick(func(s Sample) float64 { return s.TokPerJ })
	kr.Median.EnergyJ, kr.Min.EnergyJ, kr.Max.EnergyJ = pick(func(s Sample) float64 { return s.EnergyJ })
}

// NewRun starts a run record with the model's configuration.
func NewRun(cfg *config.Config, o Options, ver string) *Run {
	now := time.Now()
	r := &Run{ID: NewID(now, o.Model), Time: now, State: "running", Options: o}
	r.Config.Version = ver
	for _, m := range cfg.Models {
		if m.ID == o.Model {
			r.Config = ModelConfig{Backend: m.Backend, Path: m.Path, Ctx: m.Ctx, Parallel: m.Parallel,
				Args: m.Args, Draft: m.Draft, Version: ver}
		}
	}
	if r.Config.Ctx == 0 && (r.Config.Backend == "" || r.Config.Backend == "llamacpp") {
		r.Config.Ctx = cfg.Engines.LlamaCpp.Ctx
	}
	if r.Config.Parallel == 0 && (r.Config.Backend == "" || r.Config.Backend == "llamacpp") {
		r.Config.Parallel = cfg.Engines.LlamaCpp.Parallel
		r.Config.Args = append(r.Config.Args, cfg.Engines.LlamaCpp.Args...)
	}
	r.Options.Ctx = r.Config.Ctx
	return r
}

// ---------- storage ----------

var reID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[A-Za-z0-9._-]{1,80}$`)

// NewID names a run from its start time and model.
func NewID(t time.Time, model string) string {
	m := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(model, "_")
	if len(m) > 60 {
		m = m[:60]
	}
	if m == "" {
		m = "model"
	}
	return t.UTC().Format("20060102-150405") + "-" + m
}

// Save writes r to dir/<id>.json (0600).
func Save(dir string, r *Run) error {
	if !reID.MatchString(r.ID) {
		return fmt.Errorf("bad run id %q", r.ID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, r.ID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, r.ID+".json"))
}

// Get reads one run by id, or from a file path.
func Get(dir, idOrPath string) (*Run, error) {
	p := idOrPath
	if reID.MatchString(idOrPath) {
		p = filepath.Join(dir, idOrPath+".json")
	} else if !strings.HasSuffix(p, ".json") {
		return nil, fmt.Errorf("no bench run %q", idOrPath)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// List returns the runs in dir, newest first.
func List(dir string) ([]Run, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Run
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !reID.MatchString(id) {
			continue
		}
		if r, err := Get(dir, id); err == nil {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// ---------- compare ----------

// Row is one kind in a comparison.
type Row struct {
	Kind                string
	A, B                Stats
	DecodePct, TTFTPct  float64 // B against A; 0 when either is missing
	PrefillPct, TokJPct float64
}

func pct(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	return (b - a) / a * 100
}

// Compare lines up two runs by kind.
func Compare(a, b *Run) []Row {
	bk := map[string]KindResult{}
	for _, k := range b.Kinds {
		bk[k.Kind] = k
	}
	var out []Row
	for _, ka := range a.Kinds {
		kb, ok := bk[ka.Kind]
		if !ok {
			continue
		}
		out = append(out, Row{Kind: ka.Kind, A: ka.Median, B: kb.Median,
			DecodePct: pct(ka.Median.DecodeTPS, kb.Median.DecodeTPS), TTFTPct: pct(ka.Median.TTFTMs, kb.Median.TTFTMs),
			PrefillPct: pct(ka.Median.PrefillTPS, kb.Median.PrefillTPS), TokJPct: pct(ka.Median.TokPerJ, kb.Median.TokPerJ)})
	}
	return out
}

// Summary says what changed from a to b in plain words.
func Summary(a, b *Run, rows []Row) string {
	if len(rows) == 0 {
		return "the two runs have no kind in common"
	}
	word := func(p float64) string {
		switch {
		case p >= 2:
			return fmt.Sprintf("+%.0f%%", p)
		case p <= -2:
			return fmt.Sprintf("%.0f%%", p)
		}
		return "flat"
	}
	var dec, pre []string
	for _, r := range rows {
		if strings.HasPrefix(r.Kind, "prefill") {
			if r.PrefillPct != 0 {
				pre = append(pre, r.Kind+" "+word(r.PrefillPct))
			}
		} else if r.DecodePct != 0 {
			dec = append(dec, r.Kind+" "+word(r.DecodePct))
		}
	}
	var parts []string
	if len(dec) > 0 {
		parts = append(parts, "decode: "+strings.Join(dec, ", "))
	}
	if len(pre) > 0 {
		parts = append(parts, "prefill: "+strings.Join(pre, ", "))
	}
	s := strings.Join(parts, "; ")
	if s == "" {
		s = "no comparable figures"
	}
	switch {
	case a.Config.Draft == "" && b.Config.Draft != "":
		s += " (B uses the draft model " + filepath.Base(b.Config.Draft) + ": this is what speculative decoding gains)"
	case a.Config.Draft != "" && b.Config.Draft == "":
		s += " (A used the draft model " + filepath.Base(a.Config.Draft) + ", B does not)"
	}
	if a.Options.Model != b.Options.Model {
		s += fmt.Sprintf(" (different models: %s, %s)", a.Options.Model, b.Options.Model)
	}
	return s
}
