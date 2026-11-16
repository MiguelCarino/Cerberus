package chat

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/records"
)

// testClock is a settable clock for Options.Now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newEnvClock is newEnv with the manager on clock.
func newEnvClock(t *testing.T, base string, clock *testClock) *env {
	t.Helper()
	home := isolate(t)
	cfg := config.Defaults()
	e := &env{home: home, cfg: cfg, note: &fakeNotifier{ch: make(chan notify.Message, 16)}}
	m, err := New(Options{
		DataDir: filepath.Join(home, "cerberus-data"), Config: func() *config.Config { return cfg },
		Gateway: func() (string, string) { return base, "tok" }, Notifier: e.note,
		ContextLength: func(string) int { return 32768 }, DefaultModel: func() string { return "local-model" },
		Claude: func() string { return "" }, Home: home, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	e.m = m
	return e
}

func TestSystemPromptHoldsAllDay(t *testing.T) {
	at := func(h, min int) time.Time { return time.Date(2026, 11, 15, h, min, 0, 0, time.Local) }
	a := systemPrompt("/w", agent.ModeAsk, true, at(0, 1))
	b := systemPrompt("/w", agent.ModeAsk, true, at(23, 59))
	if a != b {
		t.Fatalf("the system prompt changed within a day:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "Today is Sunday 2026-11-15.") || strings.Contains(a, "23:59") || strings.Contains(a, "00:01") {
		t.Fatalf("prompt %q", a)
	}
	if systemPrompt("/w", agent.ModeAsk, true, at(0, 1).AddDate(0, 0, 1)) == a {
		t.Fatal("the next day kept the same prompt")
	}
}

// Two turns a few minutes apart: the same system prompt byte for byte, each
// turn's sent time on that turn only in the requests (and the first turn
// keeping its own on the second request, so the prefix is unchanged), the
// stored conversation as typed, and records.Stitch joining the two
// requests' lines.
func TestTurnsMinutesApartKeepThePrefix(t *testing.T) {
	f, base := newFakeAI(t, sse(text("one")), sse(text("two")))
	clock := &testClock{t: time.Date(2026, 11, 15, 9, 1, 30, 0, time.Local)}
	e := newEnvClock(t, base, clock)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "first")
	w.until("done", nil)
	w.until("state", idle)
	clock.Add(4 * time.Minute)
	e.send(t, id, "second")
	w.until("done", func(m map[string]any) bool { return w.count("done") == 2 })
	w.until("state", idle)

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	msgs := func(r map[string]any) []map[string]any {
		var out []map[string]any
		for _, m := range r["messages"].([]any) {
			out = append(out, m.(map[string]any))
		}
		return out
	}
	r1, r2 := msgs(reqs[0]), msgs(reqs[1])
	if r1[0]["role"] != "system" || r1[0]["content"] != r2[0]["content"] {
		t.Fatalf("system prompts differ:\n%v\n%v", r1[0]["content"], r2[0]["content"])
	}
	first := "first\n\n[sent Sunday 2026-11-15 09:01 local time]"
	second := "second\n\n[sent Sunday 2026-11-15 09:05 local time]"
	if len(r1) != 2 || r1[1]["content"] != first {
		t.Fatalf("request 1: %v", r1)
	}
	if len(r2) != 4 || r2[1]["content"] != first || r2[2]["content"] != "one" || r2[3]["content"] != second {
		t.Fatalf("request 2: %v", r2)
	}
	v, err := e.m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Chat.Messages; len(got) != 4 || got[0].Content != "first" || got[2].Content != "second" {
		t.Fatalf("stored %+v", got)
	}

	// The gateway's records of the two requests (level all keeps the
	// system prompt, and Stitch only joins lines that share it).
	s, err := records.Open(records.Config{Enabled: true, Text: records.TextAll, Dir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	start := time.Date(2026, 11, 15, 9, 1, 30, 0, time.Local)
	s.Add(lineOf(t, reqs[0], id, start, "one"))
	s.Add(lineOf(t, reqs[1], id, start.Add(4*time.Minute), "two"))
	ls, err := s.List(records.Filter{Day: start.UTC().Format(time.DateOnly), Limit: 10})
	if err != nil || len(ls) != 2 {
		t.Fatalf("lines %d %v", len(ls), err)
	}
	ref := ls[0].Ref
	if ls[0].Messages != 4 {
		ref = ls[1].Ref
	}
	// A listing leaves the text out: read the line itself, as the API does.
	p := strings.Split(ref, "/")
	off, _ := strconv.ParseInt(p[2], 10, 64)
	newest, err := s.Get(p[0], p[1], off)
	if err != nil || newest.Text == nil || newest.Text.System != r1[0]["content"] {
		t.Fatalf("line %+v %v", newest, err)
	}
	st := s.Stitch(newest)
	want := "user: " + first + "\nassistant: one\nuser: " + second
	if st.Earlier != 1 || st.Missing || st.Text == nil || st.Text.Messages != want {
		t.Fatalf("stitched %d %v:\n%s", st.Earlier, st.Missing, st.Text.Messages)
	}
}

// lineOf is the line the gateway writes for a chat request (its shapeOf:
// the system prompt whole, the text the turn after the last assistant
// message, Prefix the messages before it).
func lineOf(t *testing.T, body map[string]any, session string, at time.Time, reply string) records.Line {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var msgs []struct{ Role, Content string }
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	from := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			from = i + 1
			break
		}
	}
	var sys, conv strings.Builder
	for i, m := range msgs {
		switch {
		case m.Role == "system":
			sys.WriteString(m.Content + "\n")
		case i >= from:
			conv.WriteString(m.Role + ": " + m.Content + "\n")
		}
	}
	return records.Line{Time: at, API: "chat", Model: "api/model", Session: session, Client: "cerberus-chat",
		Messages: len(msgs), Prefix: from, Finish: "stop", Status: 200,
		Text: &records.Text{System: strings.TrimSpace(sys.String()), Messages: strings.TrimSpace(conv.String()), Reply: reply}}
}

// A chat that goes on past midnight: the next day's run names the new day
// (the one place the system prompt changes), and the first turn still
// carries the time it was sent the day before, so the model can tell
// yesterday's turn from today's. The stored turns stay as typed.
func TestTurnAcrossMidnight(t *testing.T) {
	f, base := newFakeAI(t, sse(text("one")), sse(text("two")))
	clock := &testClock{t: time.Date(2026, 11, 15, 23, 58, 0, 0, time.Local)}
	e := newEnvClock(t, base, clock)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "first")
	w.until("done", nil)
	w.until("state", idle)
	clock.Add(5 * time.Minute)
	e.send(t, id, "second")
	w.until("done", func(m map[string]any) bool { return w.count("done") == 2 })
	w.until("state", idle)

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	content := func(r map[string]any, i int) string {
		return r["messages"].([]any)[i].(map[string]any)["content"].(string)
	}
	if s := content(reqs[0], 0); !strings.Contains(s, "Today is Sunday 2026-11-15.") {
		t.Fatalf("day 1 prompt %q", s)
	}
	if s := content(reqs[1], 0); !strings.Contains(s, "Today is Monday 2026-11-16.") {
		t.Fatalf("day 2 prompt %q", s)
	}
	if got := content(reqs[1], 1); got != "first\n\n[sent Sunday 2026-11-15 23:58 local time]" {
		t.Fatalf("first turn on day 2: %q", got)
	}
	if got := content(reqs[1], 3); got != "second\n\n[sent Monday 2026-11-16 00:03 local time]" {
		t.Fatalf("second turn: %q", got)
	}
	v, err := e.m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range v.Chat.Messages {
		if strings.Contains(m.Content, "[sent ") {
			t.Fatalf("a sent note was stored: %+v", v.Chat.Messages)
		}
	}
}
