package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// capServer is a fake gateway: /v1/models reports ctx (when set), and a
// chat request whose body is over cap bytes gets llama-server's context
// overflow 400 without using a scripted reply. status, when set, answers
// the next requests with that status and text first.
type capServer struct {
	t       *testing.T
	mu      sync.Mutex
	ctx     int
	cap     int
	fails   []func(http.ResponseWriter) // answered first, one per request
	replies []func(http.ResponseWriter, map[string]any)
	bodies  []map[string]any
	sizes   []int
	refused int
	used    int
}

func newCapServer(t *testing.T, s *capServer) string {
	s.t = t
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func (s *capServer) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		if s.ctx == 0 {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"object":"list","data":[{"id":"other","cerberus":{"ctx":1}},{"id":"m","context_length":%d,"cerberus":{"ctx":%d}}]}`, s.ctx*2, s.ctx)
		return
	case "/v1/chat/completions":
	default:
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		s.t.Errorf("bad body: %v", err)
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.sizes = append(s.sizes, len(raw))
	if len(s.fails) > 0 {
		f := s.fails[0]
		s.fails = s.fails[1:]
		s.mu.Unlock()
		f(w)
		return
	}
	if s.cap > 0 && len(raw) > s.cap {
		s.refused++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error"}}`)
		return
	}
	n := s.used
	s.used++
	s.mu.Unlock()
	if n >= len(s.replies) {
		s.t.Errorf("unexpected request %d", n+1)
		http.Error(w, `{"error":{"message":"no more replies"}}`, 500)
		return
	}
	s.replies[n](w, body)
}

func failWith(status int, msg string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, msg)
	}
}

// readTurn is one reply that reads f.txt with a call id unique to step i.
// The limit differs per step (f.txt is one line, so the result is the
// same): the same call again and again would be stopped by the stuck-loop
// guard (repeat.go), which these tests are not about.
func readTurn(i int) func(http.ResponseWriter, map[string]any) {
	return sse(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_s%d","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"f.txt\",\"limit\":%d}"}}]}}]}`, i, i+1),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
}

const bigResult = 4000

// fitSetup writes a 4000-byte f.txt and returns the options and the size
// of the first request's body.
func fitSetup(t *testing.T, base string) (RunOptions, int) {
	t.Helper()
	o := engineOptions(t, base, ModeBypass, nil)
	if err := os.WriteFile(filepath.Join(o.Cwd, "f.txt"), []byte(strings.Repeat("abcdefghij", bigResult/10)), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &engine{o: o, messages: o.Messages, toolsOn: true}
	raw, err := json.Marshal(e.request(false))
	if err != nil {
		t.Fatal(err)
	}
	return o, len(raw)
}

// toolContents are the tool messages of a request body, in order.
func toolContents(body map[string]any) []string {
	var out []string
	for _, m := range body["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			out = append(out, mm["content"].(string))
		}
	}
	return out
}

// pairsValid: every assistant tool call is followed by its tool message.
func pairsValid(t *testing.T, body map[string]any) {
	t.Helper()
	msgs := body["messages"].([]any)
	for i := 0; i < len(msgs); i++ {
		m := msgs[i].(map[string]any)
		calls, _ := m["tool_calls"].([]any)
		for j, c := range calls {
			id := c.(map[string]any)["id"]
			if i+1+j >= len(msgs) || msgs[i+1+j].(map[string]any)["tool_call_id"] != id {
				t.Fatalf("tool call %v at %d has no result after it", id, i)
			}
		}
	}
}

func TestFitMessagesElidesOldestAndKeepsPairs(t *testing.T) {
	big := strings.Repeat("x", 3000)
	msgs := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "task " + big}}
	for i := 0; i < 7; i++ {
		id := "call_0" // reused ids, as some servers do
		msgs = append(msgs, Message{Role: "assistant", ToolCalls: []toolCall{{ID: id, Type: "function",
			Function: functionCall{Name: "read_file", Arguments: `{}`}}}}, Message{Role: "tool", ToolCallID: id, Content: big})
	}
	msgs = append(msgs, Message{Role: "user", Content: "last " + big})
	orig := append([]Message(nil), msgs...)

	// No context known: nothing changes.
	out, n, _ := fitMessages(msgs, 0, 0, false)
	if n != 0 || len(out) != len(msgs) {
		t.Fatalf("elided %d without a context", n)
	}
	// Plenty of room: nothing changes.
	if _, n, _ = fitMessages(msgs, 0, 1_000_000, false); n != 0 {
		t.Fatalf("elided %d with room", n)
	}
	// Tight: oldest first, the newest 4 turns stay whole.
	out, n, tokens := fitMessages(msgs, 0, 7000, false)
	if n != 3 {
		t.Fatalf("elided %d, want 3 (the 4 newest turns are kept)", n)
	}
	var tools []Message
	for _, m := range out {
		if m.Role == "tool" {
			tools = append(tools, m)
		}
	}
	if len(tools) != 7 {
		t.Fatalf("tool messages %d, want 7 (pairs stay)", len(tools))
	}
	for i, m := range tools {
		stub := strings.HasPrefix(m.Content, "(output of read_file elided")
		if stub != (i < 3) || m.ToolCallID != "call_0" {
			t.Errorf("tool %d: stub=%v %q", i, stub, truncRunes(m.Content, 60))
		}
	}
	if !strings.Contains(tools[0].Content, "3000 chars") || tokens <= 0 {
		t.Errorf("stub %q tokens %d", tools[0].Content, tokens)
	}
	if out[0].Content != "sys" || out[1].Content != orig[1].Content || out[len(out)-1].Content != orig[len(orig)-1].Content {
		t.Error("system, task or last user turn changed")
	}
	for i := range msgs {
		if msgs[i].Content != orig[i].Content {
			t.Fatalf("the caller's history changed at %d", i)
		}
	}
	// Hard: every result but the newest turn's, whatever the estimate.
	out, n, _ = fitMessages(msgs, 0, 0, true)
	if n != 6 || out[len(out)-2].Content != big {
		t.Fatalf("hard elided %d, newest %q", n, truncRunes(out[len(out)-2].Content, 40))
	}
	// A short result is left alone.
	short := []Message{{Role: "assistant", ToolCalls: []toolCall{{ID: "a"}}}, {Role: "tool", ToolCallID: "a", Content: "ok"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "b"}}}, {Role: "tool", ToolCallID: "b", Content: "ok"}}
	if _, n, _ = fitMessages(short, 0, 0, true); n != 0 {
		t.Errorf("short results elided: %d", n)
	}
}

func TestRunElidesOldResultsToFitTheContext(t *testing.T) {
	s := &capServer{}
	base := newCapServer(t, s)
	o, first := fitSetup(t, base)
	// The model's window: the first request plus about 4.5 results, at
	// 70%; the server refuses anything over 6 results.
	s.ctx = int(float64(first+bigResult*9/2) / bytesPerToken / fitShare)
	s.cap = first + 6*bigResult
	const steps = 8
	for i := 0; i < steps; i++ {
		s.replies = append(s.replies, readTurn(i))
	}
	s.replies = append(s.replies, sse(content("Done.")))
	o.MaxSteps = 20
	var saved []Message
	o.OnSave = func(_ string, m []Message) { saved = m }
	var evs []Event
	res := Run(context.Background(), o, collect(&evs))
	if res.Err != nil || !res.Done.OK {
		t.Fatalf("run failed: %v %+v", res.Err, res.Done)
	}
	if s.refused != 0 {
		t.Fatalf("the server refused %d requests (sizes %v, cap %d)", s.refused, s.sizes, s.cap)
	}
	last := s.bodies[len(s.bodies)-1]
	pairsValid(t, last)
	tools := toolContents(last)
	if len(tools) != steps {
		t.Fatalf("tool messages %d", len(tools))
	}
	for i, c := range tools {
		stub := strings.HasPrefix(c, "(output of read_file elided")
		if i >= steps-fitKeepTurns && stub {
			t.Errorf("newest result %d was elided", i)
		}
		if i == 0 && !stub {
			t.Errorf("oldest result kept whole (%d bytes)", len(c))
		}
	}
	notes := 0
	for _, e := range evs {
		if e.Type == EvNote && strings.Contains(e.Text, "shortened to fit the model's context") {
			notes++
		}
	}
	if notes == 0 {
		t.Error("no note about the elision")
	}
	// conversation.json keeps the full history.
	for _, m := range saved {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "(output of") {
			t.Fatal("the saved conversation holds a stub")
		}
	}
}

func TestRunOverflowElidesHarderAndRetriesOnce(t *testing.T) {
	s := &capServer{}
	base := newCapServer(t, s)
	o, first := fitSetup(t, base)
	s.cap = first + bigResult*7/2 // no /models: the overflow is the only signal
	for i := 0; i < 5; i++ {
		s.replies = append(s.replies, readTurn(i))
	}
	s.replies = append(s.replies, sse(content("Done.")))
	var evs []Event
	res := Run(context.Background(), o, collect(&evs))
	if res.Err != nil || !res.Done.OK {
		t.Fatalf("run failed: %v %+v", res.Err, res.Done)
	}
	if s.refused != 1 {
		t.Fatalf("refused %d, want 1 (sizes %v)", s.refused, s.sizes)
	}
	notes := 0
	for _, e := range evs {
		if e.Type == EvNote && strings.Contains(e.Text, "context is full") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("overflow notes %d", notes)
	}
	last := s.bodies[len(s.bodies)-1]
	pairsValid(t, last)
	tools := toolContents(last)
	for i, c := range tools {
		if stub := strings.HasPrefix(c, "(output of"); stub != (i < len(tools)-1) {
			t.Errorf("result %d stub=%v", i, stub)
		}
	}
}

func TestRunOverflowTwiceFails(t *testing.T) {
	s := &capServer{cap: 10} // nothing fits
	base := newCapServer(t, s)
	o, _ := fitSetup(t, base)
	res := Run(context.Background(), o, nil)
	if res.Err == nil || res.Done.OK || s.refused != 2 {
		t.Fatalf("err %v refused %d", res.Err, s.refused)
	}
	if !strings.Contains(res.Done.Error, "context size") {
		t.Errorf("error %q", res.Done.Error)
	}
}

func shortWaits(t *testing.T) {
	old := retryWaits
	retryWaits = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { retryWaits = old })
}

func TestRunRetriesTransientEngineFailure(t *testing.T) {
	shortWaits(t)
	s := &capServer{fails: []func(http.ResponseWriter){
		failWith(503, "the local engine failed and is restarting; try again"),
		failWith(502, "upstream: connection reset by peer"),
	}, replies: []func(http.ResponseWriter, map[string]any){sse(content("Hello."))}}
	base := newCapServer(t, s)
	o := engineOptions(t, base, ModeAsk, nil)
	var evs []Event
	res := Run(context.Background(), o, collect(&evs))
	if res.Err != nil || !res.Done.OK || len(s.bodies) != 3 {
		t.Fatalf("err %v requests %d", res.Err, len(s.bodies))
	}
	var notes []string
	for _, e := range evs {
		if e.Type == EvNote {
			notes = append(notes, e.Text)
		}
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "retrying") || !strings.Contains(notes[1], "2 of 2") {
		t.Errorf("notes %q", notes)
	}
	if body := s.bodies[2]; body["tools"] == nil {
		t.Error("a retry dropped the tools")
	}
}

func TestRunRetryGivesUpAfterTwo(t *testing.T) {
	shortWaits(t)
	s := &capServer{fails: []func(http.ResponseWriter){
		failWith(503, "engine terminated"), failWith(503, "engine terminated"), failWith(503, "engine terminated"),
	}}
	base := newCapServer(t, s)
	res := Run(context.Background(), engineOptions(t, base, ModeAsk, nil), nil)
	if res.Err == nil || len(s.bodies) != 3 {
		t.Fatalf("err %v requests %d", res.Err, len(s.bodies))
	}
}

func TestRunNeverRetriesLimitsOrNeverFits(t *testing.T) {
	shortWaits(t)
	for _, f := range []func(http.ResponseWriter){
		failWith(429, "token budget exceeded"),
		failWith(503, "not enough free VRAM for this model"),
		failWith(401, "bad key"),
	} {
		s := &capServer{fails: []func(http.ResponseWriter){f}}
		base := newCapServer(t, s)
		res := Run(context.Background(), engineOptions(t, base, ModeAsk, nil), nil)
		if res.Err == nil || len(s.bodies) != 1 {
			t.Errorf("err %v requests %d", res.Err, len(s.bodies))
		}
	}
}

func TestRunRetryWaitEndsWithCancel(t *testing.T) {
	old := retryWaits
	retryWaits = []time.Duration{time.Hour}
	t.Cleanup(func() { retryWaits = old })
	s := &capServer{fails: []func(http.ResponseWriter){failWith(503, "engine terminated")}}
	base := newCapServer(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	o := engineOptions(t, base, ModeAsk, nil)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	done := make(chan Result, 1)
	go func() { done <- Run(ctx, o, nil) }()
	select {
	case res := <-done:
		if !res.Done.Stopped {
			t.Errorf("done %+v", res.Done)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retry wait ignored the cancel")
	}
}

func TestModelContextFromModels(t *testing.T) {
	s := &capServer{ctx: 32768}
	base := newCapServer(t, s)
	c := newClient(base, "", "")
	if got := c.modelContext(context.Background(), "m"); got != 32768 {
		t.Errorf("ctx %d", got)
	}
	if got := c.modelContext(context.Background(), "missing"); got != 0 {
		t.Errorf("missing model ctx %d", got)
	}
}
