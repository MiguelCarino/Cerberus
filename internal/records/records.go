// Package records keeps request records (gateway phase G3): one JSON line per
// gateway request saying what the client sent and what came back. Counts,
// switches, tools and finish reasons by default, and the text only when the
// owner turns that on. Off by default.
//
// Layout: <dir>/turns/<YYYY-MM-DD>/<pid>-<ms>.jsonl, a new file each day
// (UTC) and every 64 MiB. Files are 0600, folders 0700. Old days go first
// when the retention or the size cap is passed.
package records

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Text levels.
const (
	TextNone = "none" // counts only (the default)
	TextUser = "user" // plus user and assistant text
	TextAll  = "all"  // plus system prompts and tool results
)

// MaxField bounds one text field.
const MaxField = 256 << 10

// fileCap starts a new file when one passes it.
var fileCap int64 = 64 << 20

// Call is a tool call in the reply.
type Call struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

// Text holds a line's text, at the configured level.
type Text struct {
	System    string `json:"system,omitempty"`    // level all
	Messages  string `json:"messages,omitempty"`  // the conversation as role: text lines
	Reply     string `json:"reply,omitempty"`     // the answer
	Reasoning string `json:"reasoning,omitempty"` // level all
}

// Guard is the agent guard's verdict on a request (G2).
type Guard struct {
	Mode     string    `json:"mode"`
	Scores   []float64 `json:"scores,omitempty"`
	Flagged  int       `json:"flagged"`
	Held     []string  `json:"held,omitempty"`
	TimedOut bool      `json:"timed_out,omitempty"`
}

// Line is one request record.
type Line struct {
	Time     time.Time `json:"time"`
	API      string    `json:"api"`
	Stream   bool      `json:"stream"`
	Model    string    `json:"model"`
	Backend  string    `json:"backend,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Session  string    `json:"session,omitempty"`
	User     string    `json:"user,omitempty"`
	Client   string    `json:"client"` // family: claude-code, codex, opencode, aider, cerberus-agent, ...

	Messages int    `json:"messages"`
	Tools    int    `json:"tools"`
	Images   int    `json:"images"`
	Thinking *bool  `json:"thinking,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Sampled  bool   `json:"sampled"`
	Schema   bool   `json:"schema,omitempty"`
	Budget   int    `json:"budget,omitempty"`

	PromptTokens     int `json:"prompt_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`

	Finish     string `json:"finish"` // stop, length, length_in_thinking, tool_calls, disconnect, error, held
	Status     int    `json:"status"`
	Error      string `json:"error,omitempty"`
	TTFTMs     int64  `json:"ttft_ms"`
	DurationMs int64  `json:"duration_ms"`

	Calls        []Call `json:"calls,omitempty"`
	Results      int    `json:"results,omitempty"`       // tool results sent back
	ResultErrors int    `json:"result_errors,omitempty"` // of them, marked errors by the client (Messages API)
	Matched      int    `json:"matched,omitempty"`       // of them, answering a call this gateway returned

	Guard *Guard `json:"guard,omitempty"`
	Text  *Text  `json:"text,omitempty"`

	// Ref locates the line for GET /api/records/{day}/{file}/{offset}; set
	// when listing, never written.
	Ref string `json:"ref,omitempty"`
}

// Config is the part of the gateway's config the store uses.
type Config struct {
	Enabled    bool
	Text       string
	Dir        string
	RetainDays int
	MaxMiB     int
}

// Store appends lines. A nil *Store records nothing.
type Store struct {
	mu      sync.Mutex
	cfg     Config
	secrets func() []string
	f       *os.File
	day     string
	size    int64
	now     func() time.Time
	written int64
	dropped int64
	swept   string // the day retention last ran
}

// Open returns a store for cfg; nil (recording off) when cfg.Enabled is false.
func Open(cfg Config, secrets func() []string) (*Store, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Dir == "" {
		return nil, errors.New("records: no directory")
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "turns"), 0o700); err != nil {
		return nil, err
	}
	return &Store{cfg: norm(cfg), secrets: secrets, now: time.Now}, nil
}

func norm(c Config) Config {
	switch c.Text {
	case TextUser, TextAll:
	default:
		c.Text = TextNone
	}
	if c.RetainDays <= 0 {
		c.RetainDays = 14
	}
	if c.MaxMiB <= 0 {
		c.MaxMiB = 1024
	}
	return c
}

// TextLevel is the configured text level ("" for a nil store).
func (s *Store) TextLevel() string {
	if s == nil {
		return ""
	}
	return s.cfg.Text
}

// Dir is the records folder.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.cfg.Dir
}

// Stats is what /api/status says about the store.
type Stats struct {
	On      bool   `json:"on"`
	Text    string `json:"text,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Written int64  `json:"written"`
	Dropped int64  `json:"dropped"`
}

// Stats reports counters since start.
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{On: true, Text: s.cfg.Text, Dir: s.cfg.Dir, Written: s.written, Dropped: s.dropped}
}

// Add writes l, with its text cut to the configured level, clipped and with
// known secrets masked.
func (s *Store) Add(l Line) {
	if s == nil {
		return
	}
	l.Ref = ""
	l.Text = s.shape(l.Text)
	b, err := json.Marshal(l)
	if err != nil {
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(l.Time.UTC(), b); err != nil {
		s.dropped++
		return
	}
	s.written++
}

func (s *Store) shape(t *Text) *Text {
	if t == nil || s.cfg.Text == TextNone {
		return nil
	}
	out := Text{Messages: t.Messages, Reply: t.Reply}
	if s.cfg.Text == TextAll {
		out.System, out.Reasoning = t.System, t.Reasoning
	}
	var secrets []string
	if s.secrets != nil {
		secrets = s.secrets()
	}
	for _, f := range []*string{&out.System, &out.Messages, &out.Reply, &out.Reasoning} {
		*f = clip(redact(*f, secrets))
	}
	if out == (Text{}) {
		return nil
	}
	return &out
}

// tokenShapes are API key and token formats masked even when the vault
// does not hold them.
var tokenShapes = regexp.MustCompile(`\b(sk-[A-Za-z0-9_\-]{16,}|sk-ant-[A-Za-z0-9_\-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{16}|cas-[0-9a-z\-]{10,}\.[0-9a-f]{32,}|c(?:uk|ls|ag)-[0-9a-f]{32,})\b`)

const mask = "[redacted]"

func redact(s string, secrets []string) string {
	if s == "" {
		return s
	}
	vals := slices.Clone(secrets)
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		if len(v) >= 6 {
			s = strings.ReplaceAll(s, v, mask)
		}
	}
	return tokenShapes.ReplaceAllString(s, mask)
}

func clip(s string) string {
	if len(s) <= MaxField {
		return s
	}
	cut := MaxField
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…[clipped]"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func (s *Store) write(t time.Time, b []byte) error {
	day := t.Format(time.DateOnly)
	if s.f == nil || day != s.day || s.size+int64(len(b)) > fileCap {
		if s.f != nil {
			_ = s.f.Close()
			s.f = nil
		}
		dir := filepath.Join(s.cfg.Dir, "turns", day)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		name := filepath.Join(dir, fmt.Sprintf("%d-%d.jsonl", os.Getpid(), s.now().UnixMilli()))
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		s.f, s.day, s.size = f, day, 0
		if s.swept != day {
			s.swept = day
			s.sweepLocked(t)
		}
	}
	n, err := s.f.Write(b)
	s.size += int64(n)
	return err
}

// Sweep applies retention and the size cap now.
func (s *Store) Sweep() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now().UTC())
}

func (s *Store) sweepLocked(now time.Time) {
	days := listDays(s.cfg.Dir)
	cutoff := now.AddDate(0, 0, -s.cfg.RetainDays).Format(time.DateOnly)
	var kept []string
	for _, d := range days {
		if d < cutoff && d != s.day {
			_ = os.RemoveAll(filepath.Join(s.cfg.Dir, "turns", d))
			continue
		}
		kept = append(kept, d)
	}
	limit := int64(s.cfg.MaxMiB) << 20
	total := int64(0)
	sizes := map[string]int64{}
	for _, d := range kept {
		sizes[d] = dirSize(filepath.Join(s.cfg.Dir, "turns", d))
		total += sizes[d]
	}
	for _, d := range kept { // oldest first
		if total <= limit || d == s.day {
			break
		}
		_ = os.RemoveAll(filepath.Join(s.cfg.Dir, "turns", d))
		total -= sizes[d]
	}
}

var reDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var reFile = regexp.MustCompile(`^\d+-\d+\.jsonl$`)

func listDays(dir string) []string {
	ents, _ := os.ReadDir(filepath.Join(dir, "turns"))
	var out []string
	for _, e := range ents {
		if e.IsDir() && reDay.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func dirSize(dir string) int64 {
	ents, _ := os.ReadDir(dir)
	var n int64
	for _, e := range ents {
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// Filter selects lines when listing.
type Filter struct {
	Day     string // YYYY-MM-DD; "" = the newest day
	Session string
	Client  string
	Limit   int // default 200
}

// Days lists the days that have records, newest first.
func (s *Store) Days() []string {
	if s == nil {
		return nil
	}
	d := listDays(s.cfg.Dir)
	slices.Reverse(d)
	return d
}

// List returns a day's lines, newest first, without their text.
func (s *Store) List(f Filter) ([]Line, error) {
	if s == nil {
		return nil, nil
	}
	if f.Day == "" {
		if days := s.Days(); len(days) > 0 {
			f.Day = days[0]
		} else {
			return nil, nil
		}
	}
	if !reDay.MatchString(f.Day) {
		return nil, errors.New("day must be YYYY-MM-DD")
	}
	if f.Limit <= 0 || f.Limit > 5000 {
		f.Limit = 200
	}
	dir := filepath.Join(s.cfg.Dir, "turns", f.Day)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Line
	for _, e := range ents {
		if !reFile.MatchString(e.Name()) {
			continue
		}
		fh, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		br := bufio.NewReaderSize(fh, 64<<10)
		var off int64
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				var l Line
				if json.Unmarshal(line, &l) == nil && match(l, f) {
					l.Text = nil
					l.Ref = f.Day + "/" + strings.TrimSuffix(e.Name(), ".jsonl") + "/" + strconv.FormatInt(off, 10)
					out = append(out, l)
				}
			}
			off += int64(len(line))
			if err != nil {
				break
			}
		}
		fh.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func match(l Line, f Filter) bool {
	return (f.Session == "" || l.Session == f.Session) && (f.Client == "" || l.Client == f.Client)
}

// ErrNotFound is a ref that names no line.
var ErrNotFound = errors.New("no such record")

// Get returns one line, text included, by its ref parts.
func (s *Store) Get(day, file string, offset int64) (Line, error) {
	if s == nil {
		return Line{}, ErrNotFound
	}
	if !reDay.MatchString(day) || !reFile.MatchString(file+".jsonl") || offset < 0 {
		return Line{}, ErrNotFound
	}
	fh, err := os.Open(filepath.Join(s.cfg.Dir, "turns", day, file+".jsonl"))
	if err != nil {
		return Line{}, ErrNotFound
	}
	defer fh.Close()
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		return Line{}, ErrNotFound
	}
	line, err := bufio.NewReaderSize(fh, 64<<10).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Line{}, ErrNotFound
	}
	var l Line
	if json.Unmarshal(line, &l) != nil {
		return Line{}, ErrNotFound
	}
	l.Ref = day + "/" + file + "/" + strconv.FormatInt(offset, 10)
	return l, nil
}

// DeleteBefore removes every day before day (YYYY-MM-DD) and reports how many.
func (s *Store) DeleteBefore(day string) (int, error) {
	if s == nil {
		return 0, nil
	}
	if !reDay.MatchString(day) {
		return 0, errors.New("before must be YYYY-MM-DD")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range listDays(s.cfg.Dir) {
		if d < day {
			if d == s.day && s.f != nil {
				_ = s.f.Close()
				s.f = nil
			}
			if os.RemoveAll(filepath.Join(s.cfg.Dir, "turns", d)) == nil {
				n++
			}
		}
	}
	return n, nil
}

// Close closes the open file.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		err := s.f.Close()
		s.f = nil
		return err
	}
	return nil
}

// ClientFamily names a client from its User-Agent (and Cerberus's own
// X-Cerberus-Client, which wins).
func ClientFamily(cerberusClient, ua string) string {
	if cerberusClient != "" {
		return cerberusClient
	}
	u := strings.ToLower(ua)
	switch {
	case u == "":
		return "other"
	case strings.Contains(u, "claude-cli") || strings.Contains(u, "claude-code"):
		return "claude-code"
	case strings.Contains(u, "codex"):
		return "codex"
	case strings.Contains(u, "opencode"):
		return "opencode"
	case strings.Contains(u, "aider") || strings.Contains(u, "litellm"):
		return "aider"
	case strings.HasPrefix(u, "anthropic") || strings.Contains(u, "anthropic-typescript"):
		return "anthropic-sdk"
	case strings.HasPrefix(u, "openai"):
		return "openai-sdk"
	case strings.HasPrefix(u, "curl/"):
		return "curl"
	case strings.Contains(u, "mozilla"):
		return "browser"
	}
	return "other"
}
