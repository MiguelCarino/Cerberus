package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

// ---- a fake OpenAI-compatible server

type reply func(w http.ResponseWriter, r *http.Request, body map[string]any)

type fakeAI struct {
	t       *testing.T
	mu      sync.Mutex
	replies []reply
	bodies  []map[string]any
	got     chan int // request numbers as they arrive
}

func newFakeAI(t *testing.T, replies ...reply) (*fakeAI, string) {
	f := &fakeAI{t: t, replies: replies, got: make(chan int, 64)}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/v1"
}

func (f *fakeAI) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	n := len(f.bodies)
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	f.got <- n
	if n >= len(f.replies) {
		f.t.Errorf("unexpected request %d", n+1)
		http.Error(w, `{"error":{"message":"no more replies"}}`, 500)
		return
	}
	f.replies[n](w, r, body)
}

func (f *fakeAI) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...)
}

func sse(chunks ...string) reply {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// hang streams one word, then holds the stream until the client goes away.
func hang(release <-chan struct{}) reply {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", text("partial "))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", text("answer"))
		}
	}
}

func text(s string) string {
	b, _ := json.Marshal(s)
	return `{"choices":[{"index":0,"delta":{"content":` + string(b) + `}}]}`
}

func reasoning(s string) string {
	b, _ := json.Marshal(s)
	return `{"choices":[{"index":0,"delta":{"reasoning_content":` + string(b) + `}}]}`
}

const usage = `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`

func toolCall(id, name, args string) []string {
	a, _ := json.Marshal(args)
	return []string{
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`, id, name, a),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// toolMessages are the tool-role contents of a request.
func toolMessages(req map[string]any) []string {
	var out []string
	for _, m := range req["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			out = append(out, mm["content"].(string))
		}
	}
	return out
}

// ---- the manager under test

type fakeNotifier struct{ ch chan notify.Message }

func (f *fakeNotifier) Send(m notify.Message) { f.ch <- m }

type env struct {
	m    *Manager
	home string
	cfg  *config.Config
	note *fakeNotifier
}

// isolate points HOME, XDG and PATH at temp folders: nothing real is read
// or written (the real ~/.claude, Atrium state and Cerberus data stay
// untouched).
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, d := range []string{home, filepath.Join(root, "data"), filepath.Join(root, "config"), filepath.Join(root, "state"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	return home
}

func newEnv(t *testing.T, base string, claude func() string) *env {
	t.Helper()
	home := isolate(t)
	cfg := config.Defaults()
	cfg.Prices = map[string]config.Price{"api/model": {In: 2, Out: 10}}
	e := &env{home: home, cfg: cfg, note: &fakeNotifier{ch: make(chan notify.Message, 16)}}
	if claude == nil {
		claude = func() string { return "" }
	}
	m, err := New(Options{
		DataDir: filepath.Join(home, "cerberus-data"), Config: func() *config.Config { return cfg },
		Gateway: func() (string, string) { return base, "tok" }, Notifier: e.note,
		ContextLength: func(string) int { return 32768 }, DefaultModel: func() string { return "local-model" },
		Claude: claude, Home: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	e.m = m
	return e
}

// watcher follows a chat's events.
type watcher struct {
	t      *testing.T
	ch     <-chan Event
	cancel func()
	seen   []map[string]any
}

func (e *env) watch(t *testing.T, id string) *watcher {
	t.Helper()
	backlog, ch, cancel, err := e.m.Subscribe(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	w := &watcher{t: t, ch: ch, cancel: cancel}
	t.Cleanup(cancel)
	for _, ev := range backlog {
		w.add(ev)
	}
	return w
}

func (w *watcher) add(ev Event) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(ev.Raw, &m); err != nil {
		w.t.Fatalf("event %s is not JSON: %v", ev.Raw, err)
	}
	if int(m["i"].(float64)) != ev.Index || m["type"] != ev.Type {
		w.t.Fatalf("event %s does not match index %d / type %s", ev.Raw, ev.Index, ev.Type)
	}
	w.seen = append(w.seen, m)
	return m
}

// until reads events until one of type typ (and matching pred) arrives.
func (w *watcher) until(typ string, pred func(map[string]any) bool) map[string]any {
	w.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-w.ch:
			if !ok {
				w.t.Fatalf("stream closed while waiting for %s", typ)
			}
			m := w.add(ev)
			if m["type"] == typ && (pred == nil || pred(m)) {
				return m
			}
		case <-deadline:
			w.t.Fatalf("no %s event; seen: %v", typ, types(w.seen))
		}
	}
}

func (w *watcher) count(typ string) int {
	n := 0
	for _, m := range w.seen {
		if m["type"] == typ {
			n++
		}
	}
	return n
}

func types(evs []map[string]any) []string {
	var out []string
	for _, m := range evs {
		out = append(out, m["type"].(string))
	}
	return out
}

func idle(m map[string]any) bool { return m["busy"] == false }

func (e *env) create(t *testing.T, mode string) string {
	t.Helper()
	c, err := e.m.Create(CreateOptions{Cwd: e.home, Model: "api/model", Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (e *env) send(t *testing.T, id, text string) SendResult {
	t.Helper()
	res, err := e.m.Send(id, SendOptions{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ---- tests

func TestTextReasoningUsageAndPersistence(t *testing.T) {
	_, base := newFakeAI(t, sse(reasoning("let me think"), text("<think>inline</think>Hello "), text("there."), usage))
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "  hi   you  ")
	done := w.until("done", nil)
	w.until("state", idle)
	if done["ok"] != true || done["steps"].(float64) != 1 {
		t.Fatalf("done = %v", done)
	}
	var reply, think strings.Builder
	for _, m := range w.seen {
		switch m["type"] {
		case "text":
			reply.WriteString(m["text"].(string))
		case "reasoning":
			think.WriteString(m["text"].(string))
		}
	}
	if reply.String() != "Hello there." || think.String() != "let me thinkinline" {
		t.Errorf("reply %q, reasoning %q", reply.String(), think.String())
	}
	if w.count("usage") != 1 || w.count("user") != 1 || w.count("start") != 1 {
		t.Errorf("events: %v", types(w.seen))
	}
	// cost: 1000 × $2/M + 500 × $10/M = $0.007
	if c := done["cost"].(float64); c < 0.00699 || c > 0.00701 {
		t.Errorf("cost %v", c)
	}
	v, err := e.m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status.Used != 1500 || v.Status.Context != 32768 || v.Status.Busy || v.Status.State != "done" {
		t.Errorf("status %+v", v.Status)
	}
	// The file: Atrium's shape, 0600.
	p := filepath.Join(e.home, "cerberus-data", "chats", id+".json")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("chat file: %v %v", st, err)
	}
	var raw map[string]any
	data, _ := os.ReadFile(p)
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "title", "cwd", "model", "created", "updated", "messages"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("file lacks %q: %s", k, data)
		}
	}
	if raw["title"] != "hi you" || raw["cwd"] != e.home || raw["model"] != "api/model" || !IsID(raw["id"].(string)) {
		t.Errorf("file: %s", data)
	}
	msgs := raw["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "Hello there." {
		t.Errorf("messages: %v", msgs)
	}
	// No watcher: nothing was notified (the test watched).
	select {
	case n := <-e.note.ch:
		t.Errorf("notified while watched: %+v", n)
	default:
	}
}

func TestPermissionOnceSessionDeny(t *testing.T) {
	f, base := newFakeAI(t,
		sse(toolCall("c1", "run_command", `{"command":"echo one"}`)...),
		sse(toolCall("c2", "run_command", `{"command":"echo one"}`)...),
		sse(toolCall("c3", "run_command", `{"command":"echo one"}`)...),
		sse(toolCall("c4", "write_file", `{"path":"x.txt","content":"x"}`)...),
		sse(text("finished")),
	)
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "go")

	ask := w.until("ask", nil)
	req := ask["req"].(map[string]any)
	if req["tool"] != "Bash" || req["display"] != "run_command" || req["input"].(map[string]any)["command"] != "echo one" || req["native"] != true {
		t.Fatalf("card: %v", req)
	}
	if v, _ := e.m.Get(id); len(v.Asks) != 1 || v.Status.State != "waiting" {
		t.Fatalf("view while waiting: %+v", v)
	}
	if _, err := e.m.Answer(id, "nope", DecideOnce, ""); err != ErrNoRequest {
		t.Fatalf("unknown request: %v", err)
	}
	if _, err := e.m.Answer(id, req["id"].(string), DecideOnce, ""); err != nil {
		t.Fatal(err)
	}
	w.until("tool_done", func(m map[string]any) bool { return m["id"] == "c1" })

	ask = w.until("ask", nil)
	rule, err := e.m.Answer(id, ask["req"].(map[string]any)["id"].(string), DecideSession, "")
	if err != nil || rule == nil || rule.Tool != "Bash" || rule.Content != "echo one" {
		t.Fatalf("session rule %+v %v", rule, err)
	}
	w.until("tool_done", func(m map[string]any) bool { return m["id"] == "c2" })
	// c3 is covered by the rule: no card.
	w.until("tool_done", func(m map[string]any) bool { return m["id"] == "c3" })

	ask = w.until("ask", nil)
	if ask["req"].(map[string]any)["tool"] != "Write" {
		t.Fatalf("card: %v", ask)
	}
	if _, err := e.m.Answer(id, ask["req"].(map[string]any)["id"].(string), DecideDeny, "not that file"); err != nil {
		t.Fatal(err)
	}
	w.until("done", nil)
	if n := w.count("ask"); n != 3 {
		t.Errorf("%d cards, want 3", n)
	}
	reqs := f.requests()
	tm := toolMessages(reqs[4])
	want := []string{"one", "one", "one", "The user denied this in the chat window: not that file"}
	if len(tm) != 4 {
		t.Fatalf("tool messages %q", tm)
	}
	for i := range want {
		if tm[i] != want[i] {
			t.Errorf("tool message %d = %q, want %q", i, tm[i], want[i])
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, "x.txt")); err == nil {
		t.Error("denied write happened")
	}
	if v, _ := e.m.Get(id); len(v.Rules) != 1 {
		t.Errorf("rules %+v", v.Rules)
	}
	if err := e.m.ForgetRules(id); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.m.Get(id); len(v.Rules) != 0 {
		t.Errorf("rules after forget %+v", v.Rules)
	}
	sys := reqs[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "LLM chat") || !strings.Contains(sys, e.home) {
		t.Errorf("system prompt: %s", sys)
	}
	if reqs[0]["model"] != "api/model" {
		t.Errorf("model %v", reqs[0]["model"])
	}
}

func TestModesAndPolicy(t *testing.T) {
	f, base := newFakeAI(t,
		// auto: a read-only command goes, a writing one asks
		sse(toolCall("a1", "run_command", `{"command":"ls"}`)...),
		sse(toolCall("a2", "run_command", `{"command":"touch made"}`)...),
		sse(toolCall("a3", "run_command", `{"command":"rm -rf /"}`)...),
		sse(text("ok")),
		// plan: tools are read-only and a write is refused
		sse(toolCall("p1", "write_file", `{"path":"p.txt","content":"x"}`)...),
		sse(text("plan done")),
	)
	e := newEnv(t, base, nil)
	id := e.create(t, "auto")
	w := e.watch(t, id)
	e.send(t, id, "list")
	ask := w.until("ask", nil)
	if ask["req"].(map[string]any)["input"].(map[string]any)["command"] != "touch made" {
		t.Fatalf("asked for %v", ask)
	}
	if _, err := e.m.Answer(id, ask["req"].(map[string]any)["id"].(string), DecideDeny, ""); err != nil {
		t.Fatal(err)
	}
	w.until("done", nil)
	w.until("state", idle)
	if n := w.count("ask"); n != 1 {
		t.Errorf("%d cards in auto mode, want 1 (rm -rf / is denied, never asked)", n)
	}
	tm := toolMessages(f.requests()[3])
	if len(tm) != 3 || tm[1] != "The user denied this in the chat window." || !strings.Contains(tm[2], "Refused by the command policy") {
		t.Errorf("tool messages %q", tm)
	}

	if _, err := e.m.Send(id, SendOptions{Text: "plan it", Mode: "plan"}); err != nil {
		t.Fatal(err)
	}
	w.until("done", nil)
	reqs := f.requests()
	var names []string
	for _, tl := range reqs[4]["tools"].([]any) {
		names = append(names, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "read_file,list_dir,search" {
		t.Errorf("plan tools %v", names)
	}
	if tm := toolMessages(reqs[5]); !strings.Contains(tm[len(tm)-1], "not available in plan mode") {
		t.Errorf("plan refusal %q", tm)
	}
	if v, _ := e.m.Get(id); v.Chat.Mode != "plan" {
		t.Errorf("mode %q", v.Chat.Mode)
	}
}

func TestQueueThenStopDropsQueue(t *testing.T) {
	release := make(chan struct{})
	f, base := newFakeAI(t,
		hang(release),
		sse(text("second answer")),
		hang(nil),
		sse(text("after stop")),
	)
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "first")
	<-f.got
	res := e.send(t, id, "second")
	if !res.Queued || res.Position != 1 {
		t.Fatalf("second: %+v", res)
	}
	if v, _ := e.m.Get(id); len(v.Queue) != 1 || v.Queue[0] != "second" {
		t.Fatalf("queue %+v", v.Queue)
	}
	close(release)
	w.until("done", nil)
	w.until("done", nil) // the queued message ran by itself
	w.until("state", idle)
	if v, _ := e.m.Get(id); len(v.Chat.Messages) != 4 || v.Chat.Messages[3].Content != "second answer" {
		t.Fatalf("messages %+v", v.Chat.Messages)
	}

	// Stop: the stream is cut, the queue dropped, the partial reply kept.
	e.send(t, id, "third")
	e.send(t, id, "fourth (dropped)")
	w.until("text", func(m map[string]any) bool { return m["text"] == "partial " })
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	done := w.until("done", nil)
	if done["stopped"] != true || done["ok"] != false {
		t.Fatalf("done %v", done)
	}
	w.until("state", idle)
	if w.count("dropped") != 1 {
		t.Errorf("events %v", types(w.seen))
	}
	v, _ := e.m.Get(id)
	if last := v.Chat.Messages[len(v.Chat.Messages)-1]; last.Role != "assistant" || last.Content != "partial " {
		t.Errorf("last message %+v", last)
	}
	if v.Status.State != "stopped" || len(v.Queue) != 0 {
		t.Errorf("status %+v queue %v", v.Status, v.Queue)
	}
	// The chat goes on.
	e.send(t, id, "after")
	w.until("done", func(m map[string]any) bool { return m["ok"] == true })
	if n := len(f.requests()); n != 4 {
		t.Errorf("%d requests, want 4 (the dropped message never ran)", n)
	}
}

func TestStopKillsCommandAndDeniesCards(t *testing.T) {
	_, base := newFakeAI(t,
		sse(toolCall("s1", "run_command", `{"command":"sleep 30","timeout":60}`)...),
		sse(toolCall("s2", "write_file", `{"path":"z","content":"z"}`)...),
	)
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "sleep")
	ask := w.until("ask", nil)
	if _, err := e.m.Answer(id, ask["req"].(map[string]any)["id"].(string), DecideOnce, ""); err != nil {
		t.Fatal(err)
	}
	w.until("answered", nil)
	time.Sleep(200 * time.Millisecond) // let sleep start
	start := time.Now()
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	td := w.until("tool_done", nil)
	if !strings.Contains(td["output"].(string), "stopped by the user") || td["is_error"] != true {
		t.Errorf("tool_done %v", td)
	}
	if d := w.until("done", nil); d["stopped"] != true {
		t.Errorf("done %v", d)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("stop took %s", time.Since(start))
	}

	// A card open at Stop is denied.
	w.until("state", idle)
	e.send(t, id, "write")
	w.until("ask", nil)
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	ans := w.until("answered", func(m map[string]any) bool { return m["by"] == "stop" })
	if ans["decision"] != "deny" {
		t.Errorf("answered %v", ans)
	}
	w.until("done", nil)
	if _, err := os.Stat(filepath.Join(e.home, "z")); err == nil {
		t.Error("the write ran after stop")
	}
}

func TestReplayAndReload(t *testing.T) {
	_, base := newFakeAI(t, sse(text("one "), text("two")))
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "count")
	w.until("status", idle) // the run's last event
	w.cancel()

	// A browser coming back replays everything, or from an index.
	all, _, cancel, err := e.m.Subscribe(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if len(all) != len(w.seen) {
		t.Fatalf("replay %d events, saw %d", len(all), len(w.seen))
	}
	for i, ev := range all {
		if ev.Index != i {
			t.Fatalf("event %d has index %d", i, ev.Index)
		}
	}
	tail, _, cancel, _ := e.m.Subscribe(id, 3)
	cancel()
	if len(tail) != len(all)-3 || tail[0].Index != 3 {
		t.Fatalf("replay since 3: %d events from %d", len(tail), tail[0].Index)
	}
	v, _ := e.m.Get(id)
	if v.Next != len(all) {
		t.Errorf("next %d, want %d", v.Next, len(all))
	}

	// Over HTTP: SSE with ids, from Last-Event-ID.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.m.ServeSSE(w, r, id, 0)
	}))
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	var got strings.Builder
	for !strings.Contains(got.String(), "event: done") {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	stop()
	resp.Body.Close()
	s := got.String()
	if resp.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(s, "id: 2\n") || strings.Contains(s, "id: 1\n") ||
		!strings.Contains(s, `"text":"two"`) {
		t.Errorf("stream:\n%s", s)
	}

	// A restart: a new manager reads the chat from disk.
	m2, err := New(Options{DataDir: filepath.Join(e.home, "cerberus-data"), Config: func() *config.Config { return e.cfg }, Home: e.home})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := m2.Get(id)
	if err != nil || len(v2.Chat.Messages) != 2 || v2.Chat.Messages[1].Content != "one two" {
		t.Fatalf("reloaded %+v %v", v2, err)
	}
	if list := m2.List(); len(list) != 1 || list[0].ID != id || list[0].Title != "count" {
		t.Errorf("list %+v", list)
	}
	if hits := m2.Search("TWO"); len(hits) != 1 || !strings.Contains(hits[0].Snippet, "two") {
		t.Errorf("search %+v", hits)
	}
	if hits := m2.Search("count"); len(hits) != 1 || hits[0].Snippet != "" {
		t.Errorf("title search %+v", hits)
	}
	if hits := m2.Search("absent words"); len(hits) != 0 {
		t.Errorf("search %+v", hits)
	}
	if err := m2.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Get(id); err != ErrNotFound {
		t.Errorf("after delete: %v", err)
	}
}

func TestNotifyWhenNobodyWatches(t *testing.T) {
	_, base := newFakeAI(t,
		sse(toolCall("n1", "write_file", `{"path":"n","content":"n"}`)...),
		sse(text("All done.\nmore")),
	)
	e := newEnv(t, base, nil)
	e.cfg.Server.PublicURL = "https://desk.example:7340/"
	id := e.create(t, "ask")
	e.send(t, id, "write it")
	n := <-e.note.ch
	if n.Urgency != "critical" || !strings.Contains(n.Body, "write_file") || n.URL != "https://desk.example:7340/#/chat/"+id {
		t.Fatalf("waiting notification %+v", n)
	}
	// The card is registered before the notification goes out.
	v, _ := e.m.Get(id)
	if len(v.Asks) != 1 {
		t.Fatalf("asks %+v", v.Asks)
	}
	reqID := v.Asks[0].ID
	if _, err := e.m.Answer(id, reqID, DecideOnce, ""); err != nil {
		t.Fatal(err)
	}
	n = <-e.note.ch
	if n.Title != "LLM chat finished" || n.Body != "All done." {
		t.Fatalf("done notification %+v", n)
	}
	if data, _ := os.ReadFile(filepath.Join(e.home, "n")); string(data) != "n" {
		t.Errorf("file %q", data)
	}
}

func TestImagesStoredAsWordsAndValidation(t *testing.T) {
	f, base := newFakeAI(t, sse(text("a cat")))
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	img := "data:image/png;base64,iVBORw0KGgo="
	if _, err := e.m.Send(id, SendOptions{Images: []string{"http://x/y.png"}}); err == nil {
		t.Error("a non-data image was accepted")
	}
	if _, err := e.m.Send(id, SendOptions{}); err != ErrEmpty {
		t.Errorf("empty message: %v", err)
	}
	if _, err := e.m.Send(id, SendOptions{Text: "what is it", Images: []string{img}}); err != nil {
		t.Fatal(err)
	}
	w.until("state", idle)
	parts := f.requests()[0]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != img {
		t.Errorf("request content %v", parts)
	}
	data, _ := os.ReadFile(filepath.Join(e.home, "cerberus-data", "chats", id+".json"))
	if strings.Contains(string(data), "base64") || !strings.Contains(string(data), `"[image]"`) {
		t.Errorf("stored: %s", data)
	}
}

func TestSwitchBackendStartsNewChat(t *testing.T) {
	_, base := newFakeAI(t, sse(text("hi")))
	bin := fakeClaudeBin(t)
	e := newEnv(t, base, func() string { return bin })
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "hello")
	w.until("state", idle)
	t.Setenv("FAKE_CLAUDE", "plain")
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "log"))
	res, err := e.m.Send(id, SendOptions{Text: "now claude", Model: "claude:opus"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Switched || res.ID == id {
		t.Fatalf("send %+v", res)
	}
	sw := w.until("switched", nil)
	if sw["to"] != res.ID {
		t.Errorf("switched %v", sw)
	}
	w2 := e.watch(t, res.ID)
	w2.until("state", idle)
	v, _ := e.m.Get(res.ID)
	if v.Chat.Backend != BackendClaude || v.Chat.Cwd != e.home || len(v.Chat.Messages) != 2 {
		t.Errorf("new chat %+v", v.Chat)
	}
	if old, _ := e.m.Get(id); len(old.Chat.Messages) != 2 || old.Chat.Backend != BackendNative {
		t.Errorf("old chat %+v", old.Chat)
	}
	if _, err := e.m.SetOptions(id, CreateOptions{Model: "claude"}); err == nil {
		t.Error("SetOptions switched backend on a chat with messages")
	}
}

func TestQuickAnswer(t *testing.T) {
	f, base := newFakeAI(t, sse(text("Paris "), text("is the capital.")))
	e := newEnv(t, base, nil)
	e.cfg.Chat.QuickModel = "quick-model"
	srv := httptest.NewServer(http.HandlerFunc(e.m.ServeQuick))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"text":"capital of France?"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	s := body.String()
	if !strings.Contains(s, "event: text\ndata: {\"text\":\"Paris \"}") || !strings.Contains(s, `"model":"quick-model"`) {
		t.Errorf("stream:\n%s", s)
	}
	req := f.requests()[0]
	if req["max_tokens"].(float64) != 400 || req["temperature"].(float64) != 0.3 || req["tools"] != nil || req["model"] != "quick-model" {
		t.Errorf("request %v", req)
	}
	msgs := req["messages"].([]any)
	if sys := msgs[0].(map[string]any)["content"]; sys != quickSystem || len(msgs) != 2 {
		t.Errorf("messages %v", msgs)
	}
	if _, err := e.m.Quick(context.Background(), "x", "claude:opus", nil); err == nil {
		t.Error("quick answer through claude")
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t, "http://127.0.0.1:1/v1", nil)
	if _, err := e.m.Create(CreateOptions{Cwd: "relative/path"}); err == nil {
		t.Error("relative cwd accepted")
	}
	if _, err := e.m.Create(CreateOptions{Cwd: filepath.Join(e.home, "missing")}); err == nil {
		t.Error("missing cwd accepted")
	}
	if _, err := e.m.Create(CreateOptions{Mode: "bypass"}); err == nil {
		t.Error("bypass mode accepted")
	}
	if _, err := e.m.Create(CreateOptions{Model: "claude:opus"}); err != ErrNoClaude {
		t.Errorf("claude without the CLI: %v", err)
	}
	c, err := e.m.Create(CreateOptions{})
	if err != nil || c.Cwd != e.home || c.Model != "local-model" || c.Mode != "ask" || c.Backend != BackendNative {
		t.Fatalf("defaults %+v %v", c, err)
	}
	if c2, err := e.m.SetOptions(c.ID, CreateOptions{Mode: "accept-edits"}); err != nil || c2.Mode != "acceptEdits" {
		t.Errorf("set mode %+v %v", c2, err)
	}
	e.cfg.Chat.Enabled = false
	if _, err := e.m.Create(CreateOptions{}); err != ErrDisabled {
		t.Errorf("disabled: %v", err)
	}
}

func TestServerErrorDropsMessage(t *testing.T) {
	_, base := newFakeAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		http.Error(w, `{"error":{"message":"model exploded"}}`, http.StatusUnauthorized)
	})
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "boom")
	d := w.until("done", nil)
	if d["ok"] != false || !strings.Contains(d["error"].(string), "model exploded") {
		t.Errorf("done %v", d)
	}
	w.until("state", idle)
	if v, _ := e.m.Get(id); len(v.Chat.Messages) != 0 || v.Status.State != "error" {
		t.Errorf("after error %+v", v)
	}
}

func TestChatStoreProtectedAndDeleteWithWatcher(t *testing.T) {
	var target string
	f, base := newFakeAI(t,
		func(w http.ResponseWriter, r *http.Request, b map[string]any) {
			sse(toolCall("r1", "read_file", `{"path":`+strconvQuote(target)+`}`)...)(w, r, b)
		},
		sse(text("ok")),
	)
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	target = filepath.Join(e.home, "cerberus-data", "chats", id+".json")
	w := e.watch(t, id)
	e.send(t, id, "read the chat file")
	td := w.until("tool_done", nil)
	if td["is_error"] != true || !strings.Contains(td["output"].(string), "protected") {
		t.Errorf("chat store readable from its own chat (cwd = home): %v", td)
	}
	if w.count("ask") != 0 {
		t.Errorf("a protected read asked first")
	}
	w.until("state", idle)
	_ = f
	if err := e.m.Delete(id); err != nil {
		t.Fatal(err)
	}
	w.cancel()       // after Delete closed the stream: no double close
	for range w.ch { // buffered events, then closed
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A stream opens with "hello" {epoch, base, next}; an index from before a
// restart (above next) replays the log from its start; a busy chat's view
// says where its running message starts.
func TestStreamHelloAndRestartIndex(t *testing.T) {
	release := make(chan struct{})
	f, base := newFakeAI(t, sse(text("one")), hang(release))
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "first")
	w.until("state", idle)
	e.send(t, id, "second")
	<-f.got
	<-f.got
	w.until("text", func(m map[string]any) bool { return m["text"] == "partial " })
	v, _ := e.m.Get(id)
	if v.Epoch == "" || v.Epoch != e.m.Epoch() || v.Base != 0 || v.RunFrom <= 0 {
		t.Fatalf("view epoch %q base %d run_from %d", v.Epoch, v.Base, v.RunFrom)
	}
	all, _, cancel, _ := e.m.Subscribe(id, 0)
	cancel()
	if all[v.RunFrom].Type != "user" || !strings.Contains(string(all[v.RunFrom].Raw), `"second"`) {
		t.Errorf("run_from %d is %s", v.RunFrom, all[v.RunFrom].Raw)
	}
	back, _, cancel, _ := e.m.Subscribe(id, 100000)
	cancel()
	if len(back) != len(all) {
		t.Errorf("since above next replayed %d of %d", len(back), len(all))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.m.ServeSSE(w, r, id, 800)
	}))
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	var got strings.Builder
	for !strings.Contains(got.String(), "event: user") {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	s := got.String()
	hello := fmt.Sprintf(`event: hello`+"\n"+`data: {"epoch":%q,"base":0,"next":`, e.m.Epoch())
	if !strings.HasPrefix(s, "retry: 2000\n\n"+hello) || strings.Index(s, "id: ") < strings.Index(s, "event: hello") {
		t.Errorf("stream start:\n%s", s)
	}
	close(release)
	w.until("state", idle)
	if v, _ := e.m.Get(id); v.RunFrom != -1 {
		t.Errorf("idle run_from %d", v.RunFrom)
	}
}

// Stopping (before the daemon's listeners go down) ends running chats as
// stopped: no "failed" notification, the queue is not started, new
// messages are refused.
func TestStoppingBeforeShutdown(t *testing.T) {
	f, base := newFakeAI(t, hang(nil))
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	e.send(t, id, "first")
	<-f.got
	e.send(t, id, "queued")
	e.m.Stopping()
	if _, err := e.m.Send(id, SendOptions{Text: "late"}); err != ErrClosing {
		t.Errorf("send while closing: %v", err)
	}
	e.m.Close()
	v, _ := e.m.Get(id)
	if v.Status.State != "stopped" || v.Status.Busy || len(v.Queue) != 0 {
		t.Errorf("status %+v queue %v", v.Status, v.Queue)
	}
	select {
	case n := <-e.note.ch:
		t.Errorf("notification on shutdown: %+v", n)
	default:
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests: the queued message must not start", n)
	}
}

// Idle chats beyond maxIdleLives leave memory (and load again from disk);
// a chat with session rules stays.
func TestIdleChatsEvicted(t *testing.T) {
	e := newEnv(t, "http://127.0.0.1:1/v1", nil)
	keep := e.create(t, "ask")
	e.m.mu.Lock()
	e.m.lives[keep].rules = []Rule{newRule("Bash", "ls")}
	e.m.mu.Unlock()
	var ids []string
	for i := 0; i < maxIdleLives+8; i++ {
		e.m.o.Now = func() time.Time { return time.Now().Add(time.Duration(i) * time.Second) }
		ids = append(ids, e.create(t, "ask"))
	}
	e.m.mu.Lock()
	n, kept := len(e.m.lives), e.m.lives[keep] != nil
	e.m.mu.Unlock()
	if n > maxIdleLives+1 || !kept {
		t.Errorf("%d chats in memory (kept the ruled one: %v)", n, kept)
	}
	if _, err := e.m.Get(ids[0]); err != nil {
		t.Errorf("an evicted chat loads again: %v", err)
	}
}

// Continuing the same desktop session twice at once makes one chat.
func TestContinueAtriumConcurrent(t *testing.T) {
	e := newEnv(t, "http://127.0.0.1:1/v1", nil)
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "hypr", "llm-chat", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "chat-20261020-101500-abc123"
	src := `{"id":"` + id + `","title":"t","cwd":"` + e.home + `","model":"qwen3","messages":[{"role":"user","content":"hi"}]}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := e.m.ContinueAtrium(id); err == nil {
				got[i] = c.ID
			}
		}()
	}
	wg.Wait()
	for _, g := range got {
		if g == "" || g != got[0] {
			t.Fatalf("continue ids %v", got)
		}
	}
	if n := len(e.m.List()); n != 1 {
		t.Errorf("%d chats", n)
	}
}

// TestDefaultPinnedToAnsweringModel: a chat on "default" keeps the model the
// gateway resolved for its first message, so a later unload that leaves
// "default" ambiguous cannot break the next turn.
func TestDefaultPinnedToAnsweringModel(t *testing.T) {
	f, base := newFakeAI(t, sse(text("one"), usage), sse(text("two"), usage))
	e := newEnv(t, base, nil)
	resolved := "qwen3-coder:30b"
	e.m.o.ResolveModel = func(_ context.Context, id string) (string, error) {
		if resolved == "" {
			return "", errors.New("unknown model: default")
		}
		return resolved, nil
	}
	c, err := e.m.Create(CreateOptions{Cwd: e.home, Model: "default", Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	w := e.watch(t, c.ID)
	e.send(t, c.ID, "first")
	w.until("done", nil)
	w.until("state", idle)
	resolved = "" // the model was unloaded: "default" no longer resolves
	e.send(t, c.ID, "second")
	w.until("done", func(m map[string]any) bool { return m["ok"] == true && w.count("done") == 2 })
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0]["model"] != "qwen3-coder:30b" || reqs[1]["model"] != "qwen3-coder:30b" {
		t.Fatalf("models sent: %v / %v", reqs[0]["model"], reqs[len(reqs)-1]["model"])
	}
	v, err := e.m.Get(c.ID)
	if err != nil || v.Chat.Model != "qwen3-coder:30b" {
		t.Errorf("chat model %+v %v", v, err)
	}
}

// TestRequestsCarryChatID: the chat's gateway requests name the chat in
// X-Cerberus-Session (usage attribution for the Flow graph) and say
// cerberus-chat in X-Cerberus-Client.
func TestRequestsCarryChatID(t *testing.T) {
	hdr := make(chan http.Header, 1)
	_, base := newFakeAI(t, func(w http.ResponseWriter, r *http.Request, b map[string]any) {
		hdr <- r.Header.Clone()
		sse(text("ok"))(w, r, b)
	})
	e := newEnv(t, base, nil)
	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "hi")
	w.until("done", nil)
	h := <-hdr
	if h.Get("X-Cerberus-Session") != id || h.Get("X-Cerberus-Client") != "cerberus-chat" {
		t.Fatalf("session %q client %q, want %q cerberus-chat", h.Get("X-Cerberus-Session"), h.Get("X-Cerberus-Client"), id)
	}
}
