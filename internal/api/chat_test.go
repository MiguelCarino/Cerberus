package api

import (
	"bufio"
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

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/config"
)

// ---- a fake OpenAI-compatible server for the chat's native runs

type aiReply func(w http.ResponseWriter, r *http.Request)

type fakeChatAI struct {
	t       *testing.T
	mu      sync.Mutex
	replies []aiReply
	n       int
}

func newFakeChatAI(t *testing.T, replies ...aiReply) (*fakeChatAI, string) {
	f := &fakeChatAI{t: t, replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		n := f.n
		f.n++
		f.mu.Unlock()
		if n >= len(f.replies) {
			http.Error(w, `{"error":{"message":"no more replies"}}`, 500)
			return
		}
		f.replies[n](w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/v1"
}

func aiSSE(chunks ...string) aiReply {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// aiHang streams one word, then holds until the client goes away.
func aiHang() aiReply {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", aiText("partial "))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

func aiFail(status int, msg string) aiReply {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, msg)
	}
}

func aiText(s string) string {
	b, _ := json.Marshal(s)
	return `{"choices":[{"index":0,"delta":{"content":` + string(b) + `}}]}`
}

func aiToolCall(id, name, args string) []string {
	a, _ := json.Marshal(args)
	return []string{
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`, id, name, a),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// ---- the environment: the safety env (audit, devices) plus a real chat manager

type chatEnv struct {
	*safetyEnv
	home   string
	state  string
	m      *chat.Manager
	ts     *httptest.Server
	claude string // what the manager's Claude finder answers
	mu     sync.Mutex
}

// newChatEnv isolates HOME and XDG folders, then serves the API with a
// chat manager whose native runs go to base.
func newChatEnv(t *testing.T, base string) *chatEnv {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	for _, d := range []string{home, state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	env := newSafetyEnv(t, func(c *config.Config) {
		c.Notify = config.NotifyConfig{Disabled: true}
	})
	env.srv.ping = time.Hour
	e := &chatEnv{safetyEnv: env, home: home, state: state}
	m, err := chat.New(chat.Options{
		DataDir: filepath.Join(root, "cerberus-data"), Config: env.srv.conf,
		Gateway:       func() (string, string) { return base, "tok" },
		ContextLength: func(string) int { return 8192 }, DefaultModel: func() string { return "local-model" },
		Claude: func() string {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.claude
		},
		Home: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	e.m = m
	env.srv.chats = m
	env.h = env.srv.handler()
	e.ts = httptest.NewServer(env.h)
	t.Cleanup(e.ts.Close)
	return e
}

var csrfOK = withHeader(csrfHeader, "1")

// call does a request as the admin (bearer) and decodes the JSON reply.
func (e *chatEnv) call(t *testing.T, method, target, body string, out any) int {
	t.Helper()
	w := e.do(method, target, body, withBearer(testToken))
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, target, err, w.Body)
		}
	}
	return w.Code
}

func (e *chatEnv) create(t *testing.T, mode string) chat.Chat {
	t.Helper()
	var c chat.Chat
	if code := e.call(t, "POST", "/api/chats", `{"cwd":"`+e.home+`","model":"api/model","mode":"`+mode+`"}`, &c); code != 201 {
		t.Fatalf("create: %d", code)
	}
	return c
}

// sseEvent is one event read from a stream.
type sseEvent struct {
	ID   string
	Type string
	Data map[string]any
}

// sseClient reads a chat stream.
type sseClient struct {
	t      *testing.T
	resp   *http.Response
	cancel func()
	evs    chan sseEvent
	seen   []sseEvent
}

func (e *chatEnv) stream(t *testing.T, path, tok string) *sseClient {
	t.Helper()
	resp, cancel := openStream(t, e.ts.URL, path, tok)
	t.Cleanup(cancel)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream %s: %d %s", path, resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content type %q", ct)
	}
	c := &sseClient{t: t, resp: resp, cancel: cancel, evs: make(chan sseEvent, 256)}
	go readSSE(resp.Body, c.evs)
	return c
}

func readSSE(r io.Reader, out chan<- sseEvent) {
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Type != "" {
				out <- ev
			}
			ev = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			ev.ID = line[4:]
		case strings.HasPrefix(line, "event: "):
			ev.Type = line[7:]
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(line[6:]), &ev.Data)
		}
	}
}

// until reads events until one of type typ ("" = any) matching pred (if any).
func (c *sseClient) until(typ string, pred func(sseEvent) bool) sseEvent {
	c.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-c.evs:
			if !ok {
				c.t.Fatalf("stream ended while waiting for %s; seen %v", typ, evTypes(c.seen))
			}
			c.seen = append(c.seen, ev)
			if (typ == "" || ev.Type == typ) && (pred == nil || pred(ev)) {
				return ev
			}
		case <-deadline:
			c.t.Fatalf("no %s event; seen %v", typ, evTypes(c.seen))
		}
	}
}

// ended reports whether the stream ends within d.
func (c *sseClient) ended(d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-c.evs:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func evTypes(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func isIdle(ev sseEvent) bool { return ev.Data["busy"] == false }

// ---- tests

func TestChatAuthAndScope(t *testing.T) {
	e := newChatEnv(t, "http://127.0.0.1:1/v1")
	c := e.create(t, "ask")
	agentTok := e.cfg().SessionAgentToken("s1")
	reads := []string{"/api/chats", "/api/chats/" + c.ID, "/api/chats/models", "/api/chats/search?q=x",
		"/api/chats/atrium", "/api/chats/claude", "/api/chats/rules?chat=" + c.ID, "/api/chats/" + c.ID + "/rules"}
	for _, p := range reads {
		if w := e.do("GET", p, ""); w.Code != 401 {
			t.Errorf("GET %s without a token: %d", p, w.Code)
		}
		if w := e.do("GET", p, "", withBearer(agentTok), withHeader(sessionHeader, "s1")); w.Code != 401 {
			t.Errorf("GET %s with a session agent token: %d", p, w.Code)
		}
		if w := e.do("GET", p, "", withBearer(e.cfg().AgentToken())); w.Code != 401 {
			t.Errorf("GET %s with the shared agent token: %d", p, w.Code)
		}
		if w := e.do("GET", p, "", withBearer(testToken)); w.Code != 200 {
			t.Errorf("GET %s as admin: %d %s", p, w.Code, w.Body)
		}
		if w := e.do("GET", p, "", withCookie(testToken)); w.Code != 200 {
			t.Errorf("GET %s with the cookie: %d", p, w.Code)
		}
	}
	writes := []struct{ method, path, body string }{
		{"POST", "/api/chats", `{}`},
		{"PATCH", "/api/chats/" + c.ID, `{"mode":"plan"}`},
		{"POST", "/api/chats/" + c.ID + "/messages", `{"text":"hi"}`},
		{"POST", "/api/chats/" + c.ID + "/answer", `{"req":"x","decision":"once"}`},
		{"POST", "/api/chats/" + c.ID + "/stop", ``},
		{"DELETE", "/api/chats/" + c.ID + "/rules", ``},
		{"DELETE", "/api/chats/rules?chat=" + c.ID, ``},
		{"DELETE", "/api/chats/" + c.ID, ``},
		{"POST", "/api/chats/atrium/chat-20261020-101500-abc123/continue", ``},
		{"POST", "/api/chats/claude/abc/continue", ``},
		{"POST", "/api/quick", `{"text":"hi"}`},
	}
	for _, wr := range writes {
		if w := e.do(wr.method, wr.path, wr.body, withCookie(testToken)); w.Code != 403 || !strings.Contains(w.Body.String(), csrfHeader) {
			t.Errorf("%s %s with the cookie and no CSRF header: %d %s", wr.method, wr.path, w.Code, w.Body)
		}
		if w := e.do(wr.method, wr.path, wr.body, withBearer(agentTok), withHeader(sessionHeader, "s1")); w.Code != 401 {
			t.Errorf("%s %s with a session agent token: %d", wr.method, wr.path, w.Code)
		}
		if w := e.do(wr.method, wr.path, wr.body); w.Code != 401 {
			t.Errorf("%s %s without a token: %d", wr.method, wr.path, w.Code)
		}
	}
	// The agent scope has no chat routes either.
	if w := e.do("GET", "/api/agent/chats", "", withBearer(agentTok), withHeader(sessionHeader, "s1")); w.Code != 404 {
		t.Errorf("/api/agent/chats: %d", w.Code)
	}
	// With the cookie and the CSRF header a write goes through.
	if w := e.do("PATCH", "/api/chats/"+c.ID, `{"mode":"plan"}`, withCookie(testToken), csrfOK); w.Code != 200 {
		t.Errorf("cookie write with CSRF: %d %s", w.Code, w.Body)
	}
	// Bad ids are 404, never a path.
	for _, p := range []string{"/api/chats/..%2F..%2Fetc", "/api/chats/nope", "/api/chats/rules?chat=../x", "/api/chats/nope/stream"} {
		if w := e.do("GET", p, "", withBearer(testToken)); w.Code != 404 {
			t.Errorf("GET %s: %d", p, w.Code)
		}
	}
	if w := e.do("GET", "/api/chats/chat-20200101-000000-aaaaaa", "", withBearer(testToken)); w.Code != 404 {
		t.Errorf("unknown chat: %d %s", w.Code, w.Body)
	}
}

func (e *chatEnv) cfg() *config.Config { return e.srv.conf() }

func TestChatDisabledAndUnavailable(t *testing.T) {
	e := newChatEnv(t, "http://127.0.0.1:1/v1")
	e.srv.cfg.Chat.Enabled = false
	for _, rq := range [][2]string{{"GET", "/api/chats"}, {"POST", "/api/chats"}, {"GET", "/api/chats/models"}, {"POST", "/api/quick"}} {
		if w := e.do(rq[0], rq[1], `{"text":"x"}`, withBearer(testToken)); w.Code != 403 || !strings.Contains(w.Body.String(), "chat.enabled") {
			t.Errorf("%s %s while off: %d %s", rq[0], rq[1], w.Code, w.Body)
		}
	}
	plain := newEnv(t, nil)
	if w := plain.do("GET", "/api/chats", "", withBearer(testToken)); w.Code != 503 {
		t.Errorf("no chat service: %d", w.Code)
	}
}

func TestChatLifecycleStreamReplayAndAudit(t *testing.T) {
	_, base := newFakeChatAI(t,
		aiSSE(aiText("Hello "), aiText("there.")),
		aiSSE(aiText("Second.")),
	)
	e := newChatEnv(t, base)
	c := e.create(t, "ask")
	if !chat.IsID(c.ID) || c.Cwd != e.home || c.Model != "api/model" || c.Mode != "ask" {
		t.Fatalf("created %+v", c)
	}
	var list []chat.Summary
	if code := e.call(t, "GET", "/api/chats", "", &list); code != 200 || len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("list %d %+v", code, list)
	}

	live := e.stream(t, "/api/chats/"+c.ID+"/stream", testToken)
	var res chat.SendResult
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", `{"text":"hi there friend secret-words"}`, &res); code != 200 || res.ID != c.ID || res.Queued {
		t.Fatalf("send %d %+v", code, res)
	}
	done := live.until("done", nil)
	if done.Data["ok"] != true {
		t.Fatalf("done %v", done.Data)
	}
	live.until("state", isIdle)
	var reply strings.Builder
	for _, ev := range live.seen {
		if ev.Type == "text" {
			reply.WriteString(ev.Data["text"].(string))
		}
	}
	if reply.String() != "Hello there." {
		t.Errorf("reply %q", reply.String())
	}

	var v chat.View
	if code := e.call(t, "GET", "/api/chats/"+c.ID, "", &v); code != 200 {
		t.Fatalf("get %d", code)
	}
	if len(v.Chat.Messages) != 2 || v.Chat.Messages[1].Content != "Hello there." || v.Status.Busy || v.Status.Context != 8192 || v.Next == 0 {
		t.Fatalf("view %+v", v)
	}

	// Replay: from 0 is the whole log again; from=N starts at N; the
	// Last-Event-ID header wins when it is later.
	all := e.stream(t, "/api/chats/"+c.ID+"/stream?from=0", testToken)
	first := all.until("user", nil)
	all.until("done", nil)
	if first.Data["text"] != "hi there friend secret-words" {
		t.Errorf("replayed user event %v", first.Data)
	}
	from := v.Next - 2
	tail := e.stream(t, fmt.Sprintf("/api/chats/%s/stream?from=%d", c.ID, from), testToken)
	last := fmt.Sprint(v.Next - 1)
	tail.until("", func(ev sseEvent) bool { return ev.ID == last })
	// The stream opens with hello (no id): the log's epoch and range.
	if len(tail.seen) != 3 || tail.seen[0].Type != "hello" || tail.seen[0].ID != "" || tail.seen[0].Data["epoch"] != v.Epoch ||
		tail.seen[1].ID != fmt.Sprint(from) {
		t.Errorf("tail replay from %d: %+v", from, tail.seen)
	}
	// Last-Event-ID (a browser reconnecting) wins over an earlier from.
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/chats/"+c.ID+"/stream?from=0", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Last-Event-ID", fmt.Sprint(v.Next-2))
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan sseEvent, 64)
	go readSSE(resp.Body, ch)
	if ev := <-ch; ev.Type != "hello" {
		t.Errorf("first event %+v, want hello", ev)
	}
	if ev := <-ch; ev.ID != last {
		t.Errorf("after Last-Event-ID %d the first event is %s", v.Next-2, ev.ID)
	}
	cancel()
	resp.Body.Close()
	if w := e.do("GET", "/api/chats/"+c.ID+"/stream?from=-1", "", withBearer(testToken)); w.Code != 400 {
		t.Errorf("negative from: %d", w.Code)
	}

	// A second message keeps streaming to the open watchers.
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", `{"text":"again"}`, nil); code != 200 {
		t.Fatalf("second send %d", code)
	}
	live.until("text", func(ev sseEvent) bool { return ev.Data["text"] == "Second." })
	live.until("state", isIdle)

	// PATCH, search.
	var patched chat.Chat
	if code := e.call(t, "PATCH", "/api/chats/"+c.ID, `{"mode":"plan","model":"other/model"}`, &patched); code != 200 || patched.Mode != "plan" || patched.Model != "other/model" {
		t.Fatalf("patch %d %+v", code, patched)
	}
	if code := e.call(t, "PATCH", "/api/chats/"+c.ID, `{"mode":"bypassPermissions"}`, nil); code != 400 {
		t.Errorf("bypass mode: %d", code)
	}
	var hits []chat.Hit
	if code := e.call(t, "GET", "/api/chats/search?q=FRIEND", "", &hits); code != 200 || len(hits) != 1 || hits[0].ID != c.ID {
		t.Errorf("search %d %+v", code, hits)
	}
	if code := e.call(t, "GET", "/api/chats/search?q=", "", &hits); code != 200 || len(hits) != 0 {
		t.Errorf("empty search %d %+v", code, hits)
	}

	// Delete ends the open streams.
	if code := e.call(t, "DELETE", "/api/chats/"+c.ID, "", nil); code != 200 {
		t.Fatalf("delete %d", code)
	}
	if !live.ended(3 * time.Second) {
		t.Error("the stream stayed open after delete")
	}
	if code := e.call(t, "GET", "/api/chats/"+c.ID, "", nil); code != 404 {
		t.Errorf("get after delete %d", code)
	}

	// Audit: every write recorded with its route and targets, never the words.
	recs := e.records(t)
	routes := map[string]audit.Record{}
	for _, r := range recs {
		routes[r.Route] = r
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), "secret-words") {
			t.Errorf("audit holds the message text: %s", raw)
		}
	}
	for _, want := range []string{"POST /api/chats", "POST /api/chats/{id}/messages", "PATCH /api/chats/{id}", "DELETE /api/chats/{id}"} {
		r, ok := routes[want]
		if !ok {
			t.Errorf("no audit record for %s (have %v)", want, routes)
			continue
		}
		if r.Actor != audit.Owner("admin") || len(r.Targets) == 0 || r.Targets[0] != c.ID {
			t.Errorf("%s record %+v", want, r)
		}
	}
	if r := routes["PATCH /api/chats/{id}"]; !strings.Contains(r.Detail, "mode=plan") {
		t.Errorf("patch detail %q", r.Detail)
	}
}

func TestChatPermissionAnswersAndRules(t *testing.T) {
	_, base := newFakeChatAI(t,
		aiSSE(aiToolCall("c1", "run_command", `{"command":"echo one"}`)...),
		aiSSE(aiToolCall("c2", "run_command", `{"command":"echo one"}`)...),
		aiSSE(aiToolCall("c3", "write_file", `{"path":"x.txt","content":"x"}`)...),
		aiSSE(aiText("finished")),
	)
	e := newChatEnv(t, base)
	c := e.create(t, "ask")
	s := e.stream(t, "/api/chats/"+c.ID+"/stream", testToken)
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", `{"text":"go"}`, nil); code != 200 {
		t.Fatal(code)
	}
	reqID := func(ev sseEvent) string { return ev.Data["req"].(map[string]any)["id"].(string) }

	ask := s.until("ask", nil)
	var v chat.View
	e.call(t, "GET", "/api/chats/"+c.ID, "", &v)
	if len(v.Asks) != 1 || v.Status.State != "waiting" {
		t.Fatalf("view while waiting %+v", v.Status)
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/answer", `{"req":"nope","decision":"once"}`, nil); code != 409 {
		t.Errorf("unknown request: %d", code)
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/answer", `{"decision":"once"}`, nil); code != 400 {
		t.Errorf("no req: %d", code)
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/answer", `{"req":"`+reqID(ask)+`","decision":"maybe"}`, nil); code != 400 {
		t.Errorf("bad decision: %d", code)
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/answer", `{"req":"`+reqID(ask)+`","decision":"once"}`, nil); code != 200 {
		t.Fatalf("once: %d", code)
	}
	s.until("tool_done", func(ev sseEvent) bool { return ev.Data["id"] == "c1" })

	ask = s.until("ask", nil)
	var ans struct {
		OK   bool       `json:"ok"`
		Rule *chat.Rule `json:"rule"`
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/answer", `{"req":"`+reqID(ask)+`","decision":"session"}`, &ans); code != 200 || ans.Rule == nil || ans.Rule.Tool != "Bash" {
		t.Fatalf("session: %d %+v", code, ans)
	}
	ask = s.until("ask", nil)
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/answer", `{"req":"`+reqID(ask)+`","decision":"deny","note":"not that file"}`, nil); code != 200 {
		t.Fatalf("deny: %d", code)
	}
	s.until("done", nil)
	if _, err := os.Stat(filepath.Join(e.home, "x.txt")); err == nil {
		t.Error("the denied write happened")
	}

	var rules struct {
		Chat  string      `json:"chat"`
		Rules []chat.Rule `json:"rules"`
	}
	if code := e.call(t, "GET", "/api/chats/"+c.ID+"/rules", "", &rules); code != 200 || len(rules.Rules) != 1 || rules.Rules[0].Content != "echo one" {
		t.Fatalf("rules %d %+v", code, rules)
	}
	if code := e.call(t, "GET", "/api/chats/rules?chat="+c.ID, "", &rules); code != 200 || len(rules.Rules) != 1 || rules.Chat != c.ID {
		t.Fatalf("rules (query form) %d %+v", code, rules)
	}
	if code := e.call(t, "DELETE", "/api/chats/rules?chat="+c.ID, "", nil); code != 200 {
		t.Fatalf("forget %d", code)
	}
	if code := e.call(t, "GET", "/api/chats/"+c.ID+"/rules", "", &rules); code != 200 || len(rules.Rules) != 0 {
		t.Fatalf("rules after forget %+v", rules)
	}
	recs := e.records(t)
	var decisions []string
	for _, r := range recs {
		if r.Route == "POST /api/chats/{id}/answer" && r.Status == 200 {
			decisions = append(decisions, r.Detail)
		}
	}
	// Newest first.
	if len(decisions) != 3 || decisions[2] != "once" || !strings.HasPrefix(decisions[1], "session rule ") || decisions[0] != "deny" {
		t.Errorf("answer audit details %q", decisions)
	}
}

func TestChatQueueStopAndValidation(t *testing.T) {
	_, base := newFakeChatAI(t, aiHang())
	e := newChatEnv(t, base)
	c := e.create(t, "ask")
	s := e.stream(t, "/api/chats/"+c.ID+"/stream", testToken)
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", `{"text":"first"}`, nil); code != 200 {
		t.Fatal(code)
	}
	s.until("text", nil)
	var res chat.SendResult
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", `{"text":"second"}`, &res); code != 200 || !res.Queued || res.Position != 1 {
		t.Fatalf("queue %d %+v", code, res)
	}
	s.until("queued", nil)
	if code := e.call(t, "DELETE", "/api/chats/"+c.ID, "", nil); code != 409 {
		t.Errorf("delete while running: %d", code)
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/stop", "", nil); code != 200 {
		t.Fatalf("stop %d", code)
	}
	s.until("dropped", nil)
	done := s.until("done", nil)
	if done.Data["stopped"] != true {
		t.Errorf("done after stop %v", done.Data)
	}
	s.until("state", isIdle)

	bad := []struct {
		body string
		want int
	}{
		{`{"text":"  "}`, 400},
		{`{"text":"x","images":["http://example.com/a.png"]}`, 400},
		{`{"text":"x","mode":"yolo"}`, 400},
		{`{"text":"x","model":"claude"}`, 400}, // no claude CLI here
		{`{"text":`, 400},
	}
	for _, b := range bad {
		if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", b.body, nil); code != b.want {
			t.Errorf("send %s: %d, want %d", b.body, code, b.want)
		}
	}
	defer func(n int64) { maxChatBody = n }(maxChatBody)
	maxChatBody = 1 << 20
	big := `{"text":"x","images":["data:image/png;base64,` + strings.Repeat("A", int(maxChatBody)) + `"]}`
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", big, nil); code != 413 {
		t.Errorf("oversized message: %d", code)
	}
	if code := e.call(t, "POST", "/api/chats/chat-20200101-000000-aaaaaa/messages", `{"text":"x"}`, nil); code != 404 {
		t.Errorf("send to an unknown chat: %d", code)
	}
	if code := e.call(t, "POST", "/api/chats", `{"cwd":"relative/dir"}`, nil); code != 400 {
		t.Errorf("relative cwd: %d", code)
	}
	if code := e.call(t, "POST", "/api/chats", `{"model":"claude"}`, nil); code != 400 {
		t.Errorf("claude without the CLI: %d", code)
	}
}

func TestChatStreamRevokeAndCap(t *testing.T) {
	_, base := newFakeChatAI(t)
	e := newChatEnv(t, base)
	c := e.create(t, "ask")
	tok, d := e.pair(t, "phone")
	s := e.stream(t, "/api/chats/"+c.ID+"/stream", tok)
	if w := e.do("DELETE", "/api/devices/"+d.ID, "", withBearer(testToken)); w.Code != 200 {
		t.Fatalf("revoke %d", w.Code)
	}
	if !s.ended(3 * time.Second) {
		t.Fatal("the revoked device's chat stream stayed open")
	}
	// The chat stream and the quick answer count toward the stream cap.
	for i := 0; i < maxStreamsPerActor; i++ {
		if !e.srv.conns.acquire("owner:admin") {
			t.Fatalf("slot %d refused", i)
		}
	}
	if w := e.do("GET", "/api/chats/"+c.ID+"/stream", "", withBearer(testToken)); w.Code != 429 {
		t.Errorf("chat stream over the cap: %d", w.Code)
	}
	if w := e.do("POST", "/api/quick", `{"text":"hi"}`, withBearer(testToken)); w.Code != 429 {
		t.Errorf("quick over the cap: %d", w.Code)
	}
	for i := 0; i < maxStreamsPerActor; i++ {
		e.srv.conns.release("owner:admin")
	}
}

func TestQuickAnswer(t *testing.T) {
	_, base := newFakeChatAI(t, aiSSE(aiText("Paris "), aiText("is the capital.")), aiFail(500, "model fell over"))
	e := newChatEnv(t, base)
	ts := e.ts
	post := func(body string) (*http.Response, []sseEvent) {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/api/quick", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Content-Type") != "text/event-stream" {
			io.Copy(io.Discard, resp.Body)
			return resp, nil
		}
		ch := make(chan sseEvent, 64)
		readSSE(resp.Body, ch)
		var evs []sseEvent
		for ev := range ch {
			evs = append(evs, ev)
		}
		return resp, evs
	}
	resp, evs := post(`{"text":"capital of France?","model":"api/model"}`)
	if resp.StatusCode != 200 || len(evs) != 3 || evs[0].Type != "text" || evs[2].Type != "done" {
		t.Fatalf("quick: %d %+v", resp.StatusCode, evs)
	}
	if got := evs[0].Data["text"].(string) + evs[1].Data["text"].(string); got != "Paris is the capital." || evs[2].Data["model"] != "api/model" {
		t.Errorf("quick text %q, done %v", got, evs[2].Data)
	}
	if resp, _ := post(`{"text":"x"}`); resp.StatusCode != 502 {
		t.Errorf("upstream failure: %d", resp.StatusCode)
	}
	for body, want := range map[string]int{`{"text":" "}`: 400, `{"text":"x","model":"claude:opus"}`: 400, `{`: 400} {
		if resp, _ := post(body); resp.StatusCode != want {
			t.Errorf("quick %s: %d, want %d", body, resp.StatusCode, want)
		}
	}
	found := false
	for _, r := range e.records(t) {
		if r.Route == "POST /api/quick" {
			found = true
			if strings.Contains(r.Detail, "France") {
				t.Errorf("audit holds the question: %+v", r)
			}
		}
	}
	if !found {
		t.Error("quick answers are not audited")
	}
}

func TestChatModels(t *testing.T) {
	e := newChatEnv(t, "http://127.0.0.1:1/v1")
	var m ChatModels
	if code := e.call(t, "GET", "/api/chats/models", "", &m); code != 200 {
		t.Fatal(code)
	}
	if len(m.Models) != 1 || m.Models[0].ID != "openrouter/meta/llama" || m.Default == "" || m.DefaultMode != "ask" ||
		len(m.Modes) != 4 || m.Claude.Available || len(m.Claude.Models) != 0 {
		t.Fatalf("models %+v", m)
	}
	if m.Home != e.home {
		t.Errorf("home %q, want %q (the UI shows paths under it as ~)", m.Home, e.home)
	}
	raw := e.do("GET", "/api/chats/models", "", withBearer(testToken)).Body.String()
	if !strings.Contains(raw, `"Loaded":false`) {
		t.Errorf("no loaded marks: %s", raw)
	}
	e.mu.Lock()
	e.claude = "/opt/fake/claude"
	e.mu.Unlock()
	e.call(t, "GET", "/api/chats/models", "", &m)
	if !m.Claude.Available || m.Claude.Models[0] != "claude" {
		t.Errorf("claude block %+v", m.Claude)
	}
}

func TestChatAtriumImport(t *testing.T) {
	e := newChatEnv(t, "http://127.0.0.1:1/v1")
	dir := filepath.Join(e.state, "hypr", "llm-chat", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "chat-20261020-101500-abc123"
	src := `{"id":"` + id + `","title":"notes question","cwd":"` + e.home + `","model":"qwen3","created":1760000000.5,` +
		`"updated":1760000100.25,"messages":[{"role":"user","content":"what is this"},{"role":"assistant","content":"a cat"}]}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var list []chat.AtriumSession
	if code := e.call(t, "GET", "/api/chats/atrium", "", &list); code != 200 || len(list) != 1 || list[0].ID != id || list[0].Messages != 2 {
		t.Fatalf("atrium list %d %+v", code, list)
	}
	var open chat.Chat
	if code := e.call(t, "GET", "/api/chats/atrium?id="+id, "", &open); code != 200 || open.Title != "notes question" || len(open.Messages) != 2 {
		t.Fatalf("atrium open %d %+v", code, open)
	}
	if code := e.call(t, "GET", "/api/chats/atrium?id=../../etc/passwd", "", nil); code != 404 {
		t.Errorf("bad atrium id: %d", code)
	}
	var cont chat.Chat
	if code := e.call(t, "POST", "/api/chats/atrium/"+id+"/continue", "", &cont); code != 200 || cont.ID == id || cont.Source != "atrium:"+id || len(cont.Messages) != 2 {
		t.Fatalf("continue %d %+v", code, cont)
	}
	var again chat.Chat
	if e.call(t, "POST", "/api/chats/atrium/"+id+"/continue", "", &again); again.ID != cont.ID {
		t.Errorf("second continue made %s, want %s", again.ID, cont.ID)
	}
	if code := e.call(t, "POST", "/api/chats/atrium/chat-20200101-000000-aaaaaa/continue", "", nil); code != 404 {
		t.Errorf("unknown atrium session: %d", code)
	}
	if code := e.call(t, "GET", "/api/chats/"+cont.ID, "", nil); code != 200 {
		t.Errorf("the copy is a chat: %d", code)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, id+".json"))
	if string(raw) != src {
		t.Error("the Atrium file was written")
	}
}

func TestChatClaudeSessions(t *testing.T) {
	e := newChatEnv(t, "http://127.0.0.1:1/v1")
	var out struct {
		Available bool                 `json:"available"`
		Sessions  []chat.ClaudeSession `json:"sessions"`
	}
	if code := e.call(t, "GET", "/api/chats/claude", "", &out); code != 200 || out.Available || len(out.Sessions) != 0 {
		t.Fatalf("without the CLI: %d %+v", code, out)
	}
	if code := e.call(t, "POST", "/api/chats/claude/aaa/continue", "", nil); code != 400 {
		t.Errorf("continue without the CLI: %d", code)
	}
	e.mu.Lock()
	e.claude = "/opt/fake/claude" // listed only, never run here
	e.mu.Unlock()
	proj := filepath.Join(e.home, ".claude", "projects", "-home-x-repo")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","cwd":"` + e.home + `","message":{"content":"fix the bug"}}` + "\n" +
		`{"type":"ai-title","aiTitle":"Fixing the login bug"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "aaa.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.call(t, "GET", "/api/chats/claude", "", &out); code != 200 || !out.Available || len(out.Sessions) != 1 ||
		out.Sessions[0].ID != "aaa" || out.Sessions[0].Title != "Fixing the login bug" {
		t.Fatalf("sessions %d %+v", code, out)
	}
	var c chat.Chat
	if code := e.call(t, "POST", "/api/chats/claude/aaa/continue", "", &c); code != 200 || c.ClaudeSession != "aaa" || c.Backend != chat.BackendClaude {
		t.Fatalf("continue %d %+v", code, c)
	}
	if code := e.call(t, "POST", "/api/chats/claude/zzz/continue", "", nil); code != 404 {
		t.Errorf("unknown claude session: %d", code)
	}
}

// TestChatAsksAnsweredFromOutside: an open card shows in GET
// /api/chats/asks and on /api/events ("asks"), and answering it from there
// (not the chat's own stream) lets the run go on; the list then empties.
func TestChatAsksAnsweredFromOutside(t *testing.T) {
	_, base := newFakeChatAI(t,
		aiSSE(aiToolCall("c1", "run_command", `{"command":"echo out"}`)...),
		aiSSE(aiText("finished")),
	)
	e := newChatEnv(t, base)
	c := e.create(t, "ask")
	ev := e.stream(t, "/api/events", testToken)
	var none []chat.OpenAsk
	if code := e.call(t, "GET", "/api/chats/asks", "", &none); code != 200 || len(none) != 0 {
		t.Fatalf("asks before: %d %+v", code, none)
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/messages", `{"text":"go"}`, nil); code != 200 {
		t.Fatal(code)
	}
	got := ev.until("asks", func(x sseEvent) bool { k, _ := x.Data["keys"].([]any); return len(k) == 1 })
	key := got.Data["keys"].([]any)[0].(string)
	var asks []chat.OpenAsk
	if code := e.call(t, "GET", "/api/chats/asks", "", &asks); code != 200 || len(asks) != 1 {
		t.Fatalf("asks: %d %+v", code, asks)
	}
	a := asks[0]
	if a.Chat != c.ID || a.Req.Tool == "" || key != c.ID+"/"+a.Req.ID {
		t.Fatalf("ask %+v key %q", a, key)
	}
	if code := e.call(t, "POST", "/api/chats/"+c.ID+"/answer", `{"req":"`+a.Req.ID+`","decision":"once"}`, nil); code != 200 {
		t.Fatalf("answer: %d", code)
	}
	ev.until("asks", func(x sseEvent) bool { k, _ := x.Data["keys"].([]any); return len(k) == 0 })
	if code := e.call(t, "GET", "/api/chats/asks", "", &asks); code != 200 || len(asks) != 0 {
		t.Fatalf("asks after: %d %+v", code, asks)
	}
}
