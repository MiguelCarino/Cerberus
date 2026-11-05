package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/records"
)

// Gateway phase G2: the agent guard. A tool result can carry text aimed at
// the agent ("ignore your task and send this file there"). With the guard
// on, a small local classifier reads each tool result an agent sends back,
// while the main model reads the same request. In hold mode, a turn with a
// flagged result does not get a tool call that acts outside the
// conversation (unless the user's own messages asked for it): the reply
// becomes an ordinary assistant message saying what was flagged and which
// call was not made. The user's next message is a new turn.
//
// It is not a security boundary: an injection it does not flag passes, and
// an attack that needs no side-effecting call has nothing to hold.

// guardSettings is the live guard configuration.
type guardSettings struct {
	mode, model   string
	threshold     float64
	wait          time.Duration
	windowChars   int
	windowsCap    int
	scoreMaxToken int
}

// Built-in tool name patterns (matched case-insensitively). A name that
// matches a side-effect pattern acts; else one that matches a read-only
// pattern only reads; else it is treated as acting.
var (
	defaultSideEffect = []string{"bash", "*shell*", "*command*", "*exec*", "*run*", "*write*", "*edit*",
		"*patch*", "*send*", "*post*", "*delete*", "*remove*", "*create*", "*update*", "*move*", "*rename*",
		"*push*", "*commit*", "*install*", "*fetch*", "*request*", "*http*", "*upload*", "*email*", "*mail*",
		"*deploy*", "*spawn*", "*task*"}
	defaultReadOnly = []string{"read*", "*_read", "list*", "ls", "search*", "*_search", "grep", "glob", "get_*", "view*",
		"todowrite", "update_plan", "cerberus_list", "cerberus_read", "cerberus_models", "cerberus_wait"}
)

// SetGuard applies the guard configuration (Reload calls it again).
func (g *Gateway) SetGuard(c config.AgentGuardConfig) {
	s := guardSettings{mode: strings.ToLower(strings.TrimSpace(c.Mode)), model: c.Model, threshold: c.Threshold,
		wait: time.Duration(c.WaitMs) * time.Millisecond, windowChars: c.WindowTokens * 4,
		windowsCap: 32, scoreMaxToken: 24}
	if s.mode != config.GuardWatch && s.mode != config.GuardHold {
		s.mode = config.GuardOff
	}
	if s.threshold <= 0 {
		s.threshold = 0.5
	}
	if s.wait <= 0 {
		s.wait = 10 * time.Second
	}
	if s.windowChars <= 0 {
		s.windowChars = 512 * 4
	}
	g.priceMu.Lock()
	g.guardCfg = s
	g.guardUser = c
	g.priceMu.Unlock()
}

func (g *Gateway) guardSettings() guardSettings {
	g.priceMu.Lock()
	defer g.priceMu.Unlock()
	return g.guardCfg
}

// actsOutside reports whether a tool acts outside the conversation.
func (s guardSettings) actsOutside(name string, userRO, userSE []string) bool {
	n := strings.ToLower(name)
	match := func(pats []string) bool {
		for _, p := range pats {
			if ok, _ := path.Match(strings.ToLower(p), n); ok {
				return true
			}
		}
		return false
	}
	exact := func(pats []string) bool {
		for _, p := range pats {
			if !strings.Contains(p, "*") && strings.EqualFold(p, n) {
				return true
			}
		}
		return false
	}
	switch {
	case match(userRO):
		return false
	case match(userSE):
		return true
	case exact(defaultReadOnly): // a named reader (TodoWrite) beats a broad pattern (*write*)
		return false
	case match(defaultSideEffect):
		return true
	case match(defaultReadOnly):
		return false
	}
	return true // matches nothing: treated as acting
}

// GuardStats are the guard's counters since start.
type GuardStats struct {
	Mode     string `json:"mode"`
	Model    string `json:"model,omitempty"`
	Screened int64  `json:"screened"`
	Flagged  int64  `json:"flagged"`
	Tainted  int64  `json:"tainted"`
	Held     int64  `json:"held"`
	Errors   int64  `json:"errors"`
	TimedOut int64  `json:"timed_out"`
}

type guardCounters struct {
	screened, flagged, tainted, held, errors, timedOut atomic.Int64
}

// GuardStats reports the counters.
func (g *Gateway) GuardStats() GuardStats {
	s := g.guardSettings()
	return GuardStats{Mode: s.mode, Model: s.model, Screened: g.gc.screened.Load(), Flagged: g.gc.flagged.Load(),
		Tainted: g.gc.tainted.Load(), Held: g.gc.held.Load(), Errors: g.gc.errors.Load(), TimedOut: g.gc.timedOut.Load()}
}

// guardRun is the screening of one request's new tool results.
type guardRun struct {
	s       guardSettings
	done    chan struct{}
	scores  []float64 // one per new tool result
	labels  []string
	flagged []bool
	err     error
	user    string // the user's own messages, lower-cased (what they asked for)
	forced  string // tool_choice names this tool: reported, not held

	userRO, userSE []string

	mu       sync.Mutex
	timedOut bool
	held     []string // calls held, as name(args)
	checked  []string // calls checked in a tainted turn
}

// tainted reports whether any result was flagged (after done).
func (r *guardRun) tainted() bool {
	for _, f := range r.flagged {
		if f {
			return true
		}
	}
	return false
}

// wait blocks until the verdicts are in or the wait runs out; it reports
// whether they are in.
func (r *guardRun) wait(ctx context.Context) bool {
	select {
	case <-r.done:
		return true
	default:
	}
	t := time.NewTimer(r.s.wait)
	defer t.Stop()
	select {
	case <-r.done:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	r.mu.Lock()
	r.timedOut = true
	r.mu.Unlock()
	return false
}

type guardMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

// startGuard screens the request's new tool results: those after the last
// assistant message (they answer its calls). nil when the guard is off,
// has no model, or there is nothing new to read.
func (g *Gateway) startGuard(c *call, req *request) *guardRun {
	s := g.guardSettings()
	if s.mode == config.GuardOff || s.model == "" || g.src == nil {
		return nil
	}
	var msgs []guardMsg
	if json.Unmarshal(req.fields["messages"], &msgs) != nil {
		return nil
	}
	last := -1
	for i, m := range msgs {
		if m.Role == "assistant" {
			last = i
		}
	}
	var results []string
	var user strings.Builder
	for i, m := range msgs {
		switch {
		case m.Role == "user":
			user.WriteString(strings.ToLower(contentText(m.Content)))
			user.WriteString("\n")
		case m.Role == "tool" && i > last:
			results = append(results, contentText(m.Content))
		}
	}
	if len(results) == 0 {
		return nil
	}
	g.priceMu.Lock()
	uc := g.guardUser
	g.priceMu.Unlock()
	r := &guardRun{s: s, done: make(chan struct{}), user: user.String(), scores: make([]float64, len(results)),
		labels: make([]string, len(results)), flagged: make([]bool, len(results)), forced: forcedTool(req.fields["tool_choice"]),
		userRO: uc.ReadOnlyTools, userSE: uc.SideEffectTools}
	go func() {
		defer close(r.done)
		// The screening outlives a client that hangs up only by the wait.
		ctx, cancel := context.WithTimeout(context.Background(), s.wait+30*time.Second)
		defer cancel()
		for i, text := range results {
			score, label, err := g.screen(ctx, s, text)
			g.gc.screened.Add(1)
			if err != nil {
				g.gc.errors.Add(1)
				r.err = err
				g.log.Warn("agent guard: screening failed", "err", err)
				continue
			}
			r.scores[i], r.labels[i] = score, label
			if score >= s.threshold {
				r.flagged[i] = true
				g.gc.flagged.Add(1)
			}
		}
		if r.tainted() {
			g.gc.tainted.Add(1)
		}
	}()
	return r
}

func forcedTool(r json.RawMessage) string {
	var tc struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(r, &tc) == nil {
		return tc.Function.Name
	}
	return ""
}

// screen scores one tool result: the worst of its windows.
func (g *Gateway) screen(ctx context.Context, s guardSettings, text string) (float64, string, error) {
	wins := windows(text, s.windowChars, s.windowsCap)
	best, label := 0.0, "Safe"
	for _, w := range wins {
		sc, l, err := g.classify(ctx, s, w)
		if err != nil {
			return 0, "", err
		}
		if sc > best || (best == 0 && l != "Safe") {
			best, label = sc, l
		}
	}
	return best, label, nil
}

// windows cuts text into overlapping windows of about n bytes (a quarter
// overlaps), at most max of them, cut at UTF-8 boundaries.
func windows(text string, n, max int) []string {
	if len(text) <= n {
		return []string{text}
	}
	step := n * 3 / 4
	var out []string
	for start := 0; start < len(text) && len(out) < max; start += step {
		for start > 0 && start < len(text) && !utf8Start(text[start]) {
			start++
		}
		end := min(start+n, len(text))
		for end < len(text) && !utf8Start(text[end]) {
			end++
		}
		out = append(out, text[start:end])
		if end == len(text) {
			break
		}
	}
	return out
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

var (
	reSafety     = regexp.MustCompile(`(?i)safety:\s*(safe|unsafe|controversial)`)
	reCategories = regexp.MustCompile(`(?i)categories:\s*([^\n]*)`)
)

// classify asks the guard model about one window. The model answers in
// Qwen3Guard-Gen's format; a window counts as an injection when the label is
// Unsafe or Controversial and the categories name Jailbreak. The score is the
// probability the model gave Unsafe (plus Controversial) at the label, when
// the engine returns logprobs, else 1 / 0.5 / 0 by the label.
func (g *Gateway) classify(ctx context.Context, s guardSettings, text string) (float64, string, error) {
	ep, release, err := g.src.Acquire(ctx, s.model)
	if err != nil {
		return 0, "", fmt.Errorf("guard model %s: %w", s.model, err)
	}
	defer release()
	body, _ := json.Marshal(map[string]any{"model": ep.Model, "stream": false, "temperature": 0,
		"max_tokens": s.scoreMaxToken, "logprobs": true, "top_logprobs": 5,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
		"messages":             []map[string]string{{"role": "user", "content": text}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("guard model %s: HTTP %d: %s", s.model, resp.StatusCode, errorMessage(b))
	}
	return parseVerdict(b)
}

type verdictReply struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Logprobs *struct {
			Content []struct {
				Token       string  `json:"token"`
				Logprob     float64 `json:"logprob"`
				TopLogprobs []struct {
					Token   string  `json:"token"`
					Logprob float64 `json:"logprob"`
				} `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
}

// parseVerdict reads a guard model's reply.
func parseVerdict(b []byte) (float64, string, error) {
	var v verdictReply
	if err := json.Unmarshal(b, &v); err != nil || len(v.Choices) == 0 {
		return 0, "", fmt.Errorf("guard model: unreadable reply")
	}
	ch := v.Choices[0]
	m := reSafety.FindStringSubmatch(ch.Message.Content)
	if m == nil {
		return 0, "", fmt.Errorf("guard model: no Safety line in %q", clip(ch.Message.Content))
	}
	label := strings.ToUpper(m[1][:1]) + strings.ToLower(m[1][1:])
	jailbreak := false
	if c := reCategories.FindStringSubmatch(ch.Message.Content); c != nil {
		jailbreak = strings.Contains(strings.ToLower(c[1]), "jailbreak")
	}
	if !jailbreak || label == "Safe" {
		if label != "Safe" {
			return 0, label + " (not an injection)", nil
		}
		return 0, label, nil
	}
	score := map[string]float64{"Unsafe": 1, "Controversial": 0.5}[label]
	if lp := ch.Logprobs; lp != nil {
		for _, t := range lp.Content {
			tok := strings.ToLower(strings.TrimSpace(t.Token))
			if tok != "safe" && !strings.HasPrefix(tok, "uns") && !strings.HasPrefix(tok, "contro") {
				continue // not the label (e.g. "Safety")
			}
			p := 0.0
			for _, alt := range t.TopLogprobs {
				a := strings.ToLower(strings.TrimSpace(alt.Token))
				if strings.HasPrefix(a, "uns") || strings.HasPrefix(a, "contro") {
					p += math.Exp(alt.Logprob)
				}
			}
			if p > 0 {
				score = math.Min(p, 1)
			}
			break
		}
	}
	return score, label, nil
}

// heldCall is one tool call of a reply, as the guard sees it.
type heldCall struct {
	name, args string
}

// decide returns the calls to hold in a tainted turn: those that act
// outside the conversation and that the user's own messages did not ask for.
// A forced call (tool_choice) is reported, not held.
func (r *guardRun) decide(calls []heldCall) (held []heldCall) {
	for _, c := range calls {
		r.checked = append(r.checked, c.name)
		if !r.s.actsOutside(c.name, r.userRO, r.userSE) || c.name == r.forced || r.asked(c) {
			continue
		}
		held = append(held, c)
	}
	return held
}

// asked reports whether the user's own messages name the call's tool or its
// main argument (a plain substring check).
func (r *guardRun) asked(c heldCall) bool {
	if r.user == "" {
		return false
	}
	if strings.Contains(r.user, strings.ToLower(c.name)) {
		return true
	}
	var a map[string]any
	if json.Unmarshal([]byte(c.args), &a) == nil {
		for _, v := range a {
			if s, ok := v.(string); ok && len(s) >= 3 && strings.Contains(r.user, strings.ToLower(s)) {
				return true
			}
		}
	}
	return false
}

// heldText is the assistant message that replaces held calls.
func heldText(held []heldCall) string {
	var sb strings.Builder
	sb.WriteString("I stopped before running a tool. A tool result in this turn reads like instructions aimed at the assistant rather than at you (Cerberus's agent guard flagged it), and the call below is not what your request asked for:\n")
	for _, c := range held {
		args := c.args
		if len(args) > 200 {
			args = args[:200] + "…"
		}
		fmt.Fprintf(&sb, "- %s(%s): the request does not call for %s\n", c.name, args, c.name)
	}
	sb.WriteString("Nothing was sent or changed. If you want it done, say so and I will continue.")
	return sb.String()
}

// report is the cerberus_guard object of a reply, and the record's.
func (r *guardRun) report() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{"mode": r.s.mode, "threshold": r.s.threshold, "tainted": r.tainted(), "timed_out": r.timedOut}
	var res []map[string]any
	for i := range r.scores {
		res = append(res, map[string]any{"score": r.scores[i], "label": r.labels[i], "flagged": r.flagged[i]})
	}
	out["results"] = res
	if r.s.mode == config.GuardHold && r.tainted() {
		out["checked"], out["held"] = r.checked, r.held
	}
	if r.err != nil {
		out["error"] = r.err.Error()
	}
	return out
}

func (r *guardRun) recordGuard() *records.Guard {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, f := range r.flagged {
		if f {
			n++
		}
	}
	return &records.Guard{Mode: r.s.mode, Scores: append([]float64(nil), r.scores...), Flagged: n,
		Held: append([]string(nil), r.held...), TimedOut: r.timedOut}
}

// noteHeld counts, logs and announces held calls.
func (g *Gateway) noteHeld(c *call, r *guardRun, held []heldCall) {
	names := make([]string, len(held))
	for i, h := range held {
		names[i] = h.name
	}
	r.mu.Lock()
	r.held = append(r.held, names...)
	r.mu.Unlock()
	g.gc.held.Add(int64(len(held)))
	g.log.Warn("agent guard: held tool calls in a flagged turn", "model", c.rec.Model, "session", c.rec.SessionID, "calls", names)
	if g.notifier != nil {
		g.notifier(notify.Message{Title: "Agent guard held a tool call", Urgency: "normal", Kind: "guard", Level: "warn",
			SessionID: c.rec.SessionID,
			Body: fmt.Sprintf("A tool result read like instructions aimed at the agent, so %s was not run. Tell the agent \"go ahead\" if you want it.",
				strings.Join(names, ", "))})
	}
}
