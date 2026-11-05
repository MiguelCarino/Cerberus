package agent

import (
	"bytes"
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

	"carino.systems/cerberus/internal/config"
)

// fakeServer scripts one reply per /chat/completions request and records
// each request's headers and body.
type fakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	replies []func(w http.ResponseWriter, req map[string]any)
	bodies  []map[string]any
	headers []http.Header
}

func newFake(t *testing.T, replies ...func(http.ResponseWriter, map[string]any)) (*fakeServer, string) {
	f := &fakeServer{t: t, replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/v1"
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("bad request body: %v", err)
	}
	f.mu.Lock()
	n := len(f.bodies)
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if n >= len(f.replies) {
		f.t.Errorf("unexpected request %d", n+1)
		http.Error(w, `{"error":{"message":"no more replies"}}`, 500)
		return
	}
	f.replies[n](w, body)
}

func (f *fakeServer) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...)
}

// sse writes each chunk as an SSE event, then [DONE].
func sse(chunks ...string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, ": keep-alive\n\ndata: [DONE]\n\n")
	}
}

func content(s string) string {
	b, _ := json.Marshal(s)
	return `{"choices":[{"index":0,"delta":{"content":` + string(b) + `}}]}`
}

// toolChunks streams one tool call per index, with its arguments split in two.
func toolChunks(calls ...[2]string) []string {
	var out []string
	for i, c := range calls {
		half := len(c[1]) / 2
		out = append(out,
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"call_%d","type":"function","function":{"name":%q,"arguments":""}}]}}]}`, i, i, c[0]),
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}}]}`, i, jsonStr(c[1][:half])),
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}}]}`, i, jsonStr(c[1][half:])),
		)
	}
	return append(out, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func testOptions(t *testing.T, base string) options {
	t.Helper()
	return options{
		Base: base, Model: "test-model", Key: "k123", Session: "s-1", Cwd: t.TempDir(), Task: "do the thing",
		MaxSteps: 10, Approve: "auto", DenyPaths: config.Defaults().Agent.DenyPaths,
		CommandTimeout: 30 * time.Second, ContextMax: 16384, Color: false,
	}
}

// toolMessages returns the tool-role contents of a request, in order.
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

func TestToolCallThenAnswer(t *testing.T) {
	step1 := append([]string{`{"choices":[{"index":0,"delta":{"reasoning_content":"let me think"}}]}`},
		toolChunks([2]string{"write_file", `{"path":"hello.txt","content":"hi\nthere\n"}`})...)
	f, base := newFake(t,
		sse(step1...),
		sse(content("<thi"), content("nk>secret plan</th"), content("ink>All "), content("done.")),
	)
	o := testOptions(t, base)
	if err := os.WriteFile(filepath.Join(o.Cwd, "AGENTS.md"), []byte("Always use tabs."), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := run(context.Background(), o, strings.NewReader(""), &out)
	if code != exitOK {
		t.Fatalf("exit %d, output:\n%s", code, out.String())
	}
	got, err := os.ReadFile(filepath.Join(o.Cwd, "hello.txt"))
	if err != nil || string(got) != "hi\nthere\n" {
		t.Fatalf("file = %q, %v", got, err)
	}
	s := out.String()
	for _, want := range []string{"▸ write_file: hello.txt (2 lines)", "created ", "All done.", "· thinking…",
		"\x1b]777;cerberus;done\x07", "done · 2 steps · 1 tool call", "10 prompt + 5 completion tokens"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	for _, leak := range []string{"secret plan", "let me think", "<think>"} {
		if strings.Contains(s, leak) {
			t.Errorf("reasoning %q printed in full:\n%s", leak, s)
		}
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	if reqs[0]["tools"] == nil || reqs[0]["stream"] != true || reqs[0]["model"] != "test-model" {
		t.Errorf("first request: %v", reqs[0])
	}
	sys := reqs[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, o.Cwd) || !strings.Contains(sys, "Always use tabs.") {
		t.Errorf("system prompt lacks cwd or AGENTS.md: %s", sys)
	}
	if tm := toolMessages(reqs[1]); len(tm) != 1 || !strings.Contains(tm[0], "created") {
		t.Errorf("tool messages: %q", tm)
	}
	h := f.headers[0]
	if h.Get("X-Cerberus-Client") != "cerberus-agent" || h.Get("X-Cerberus-Session") != "s-1" ||
		h.Get("Authorization") != "Bearer k123" {
		t.Errorf("headers: %v", h)
	}
}

func TestApprovalFlow(t *testing.T) {
	cmd := func(word string) [2]string {
		return [2]string{"run_command", `{"command":"echo ` + word + `"}`}
	}
	f, base := newFake(t,
		sse(toolChunks(cmd("A"), cmd("B"), cmd("C"))...),
		sse(toolChunks(cmd("D"))...),
		sse(toolChunks(cmd("E"))...),
		sse(content("finished")),
	)
	o := testOptions(t, base)
	o.Approve = "ask"
	// y (with \r\n), n (\r), a reason, a stray empty line, then a; E needs no answer.
	stdin := strings.NewReader("y\r\nn\rplease use printf\n\na\n")
	var out bytes.Buffer
	if code := run(context.Background(), o, stdin, &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	s := out.String()
	if n := strings.Count(s, "\x1b]777;cerberus;waiting;approval\x07"); n != 4 {
		t.Errorf("%d waiting markers, want 4:\n%s", n, s)
	}
	if n := strings.Count(s, "\x1b]777;cerberus;resumed\x07"); n != 4 {
		t.Errorf("%d resumed markers, want 4", n)
	}
	reqs := f.requests()
	tm := toolMessages(reqs[1])
	want := []string{"A", "The user refused this.", "The user refused this: please use printf"}
	if len(tm) != 3 {
		t.Fatalf("tool messages %q", tm)
	}
	for i := range want {
		if tm[i] != want[i] {
			t.Errorf("tool message %d = %q, want %q", i, tm[i], want[i])
		}
	}
	if tm := toolMessages(reqs[3]); len(tm) != 5 || tm[3] != "D" || tm[4] != "E" {
		t.Errorf("after always: %q", tm)
	}
}

func TestApprovalInputClosed(t *testing.T) {
	f, base := newFake(t,
		sse(toolChunks([2]string{"write_file", `{"path":"x.txt","content":"x"}`})...),
		sse(content("ok")),
	)
	o := testOptions(t, base)
	o.Approve = "ask"
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(o.Cwd, "x.txt")); err == nil {
		t.Error("file written without approval")
	}
	if tm := toolMessages(f.requests()[1]); len(tm) != 1 || !strings.Contains(tm[0], "refused") {
		t.Errorf("tool messages %q", tm)
	}
}

func TestDeniedWriteNeverAsks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	f, base := newFake(t,
		sse(toolChunks(
			[2]string{"write_file", `{"path":"secrets.json","content":"{}"}`},
			[2]string{"write_file", fmt.Sprintf(`{"path":%q,"content":"x"}`, outside)},
		)...),
		sse(content("ok")),
	)
	o := testOptions(t, base)
	o.Approve = "ask"
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out.String(), "waiting;approval") {
		t.Error("asked about a refused write")
	}
	tm := toolMessages(f.requests()[1])
	if len(tm) != 2 || !strings.Contains(tm[0], "deny_paths") || !strings.Contains(tm[1], "outside the working directory") {
		t.Errorf("tool messages %q", tm)
	}
}

func TestToolRejectionFallback(t *testing.T) {
	f, base := newFake(t,
		func(w http.ResponseWriter, req map[string]any) {
			if req["tools"] == nil {
				t.Error("first request had no tools")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"this model does not support tools","type":"invalid_request_error"}}`)
		},
		sse(content("plain answer")),
	)
	o := testOptions(t, base)
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[1]["tools"] != nil {
		t.Fatalf("retry still had tools: %v", reqs)
	}
	sys := reqs[1]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "No tools are available") {
		t.Errorf("plain-chat system prompt: %s", sys)
	}
	if s := out.String(); !strings.Contains(s, "plain chat") || !strings.Contains(s, "plain answer") {
		t.Errorf("output:\n%s", s)
	}
}

func TestServerErrorExit2(t *testing.T) {
	_, base := newFake(t, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	})
	var out bytes.Buffer
	if code := run(context.Background(), testOptions(t, base), strings.NewReader(""), &out); code != exitServer {
		t.Fatalf("exit %d", code)
	}
	if s := out.String(); !strings.Contains(s, "401") || !strings.Contains(s, "bad key") || strings.Contains(s, ";done") {
		t.Errorf("output:\n%s", s)
	}
}

func TestErrorChunkExit2(t *testing.T) {
	_, base := newFake(t, sse(content("partial"), `{"error":{"message":"model crashed"}}`))
	var out bytes.Buffer
	if code := run(context.Background(), testOptions(t, base), strings.NewReader(""), &out); code != exitServer {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "model crashed") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestLastStepHasNoTools(t *testing.T) {
	f, base := newFake(t,
		sse(toolChunks([2]string{"list_dir", `{}`})...),
		sse(content("summary")),
	)
	o := testOptions(t, base)
	o.MaxSteps = 1
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	reqs := f.requests()
	if reqs[1]["tools"] != nil {
		t.Error("last step offered tools")
	}
	msgs := reqs[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" || last["content"] != lastStepNudge {
		t.Errorf("last message %v", last)
	}
}

func TestWholeJSONReply(t *testing.T) {
	_, base := newFake(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"whole reply"},"finish_reason":"stop"}]}`)
	})
	var out bytes.Buffer
	if code := run(context.Background(), testOptions(t, base), strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "whole reply") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestBadToolArguments(t *testing.T) {
	f, base := newFake(t,
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{oops"}}]}}]}`),
		sse(content("ok")),
	)
	var out bytes.Buffer
	if code := run(context.Background(), testOptions(t, base), strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if tm := toolMessages(f.requests()[1]); len(tm) != 1 || !strings.Contains(tm[0], "not valid JSON") {
		t.Errorf("tool messages %q", tm)
	}
}

func TestParseFlags(t *testing.T) {
	cfg := config.Defaults()
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "task.md")
	os.WriteFile(taskFile, []byte("  fix it \n"), 0o644)
	var out bytes.Buffer
	o, _, ok := parseFlags([]string{"--base", "http://x/v1/", "--model", "m", "--task-file", taskFile,
		"--cwd", dir, "--key", "K", "--approve", "auto", "--max-steps", "3"}, cfg, &out)
	if !ok {
		t.Fatalf("parse failed: %s", out.String())
	}
	if o.Base != "http://x/v1" || o.Task != "fix it" || o.Key != "K" || o.MaxSteps != 3 || o.Approve != "auto" ||
		o.Cwd != dir || o.CommandTimeout != 120*time.Second {
		t.Errorf("options %+v", o)
	}
	for _, bad := range [][]string{
		{"--approve", "maybe", "task"},
		{"--cwd", filepath.Join(dir, "nope"), "task"},
		{"--cwd", dir},
	} {
		if _, code, ok := parseFlags(bad, cfg, &out); ok || code == exitOK {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestMain_EndToEnd(t *testing.T) {
	t.Setenv("CERBERUS_SESSION_DIR", "") // never write into a real session's folder
	old := loadConfig
	t.Cleanup(func() { loadConfig = old })
	loadConfig = config.Defaults
	_, base := newFake(t, sse(content("hello from main")))
	var out bytes.Buffer
	code := Main([]string{"--base", base, "--model", "m", "--cwd", t.TempDir(), "--approve", "auto", "say", "hi"},
		strings.NewReader(""), &out)
	if code != exitOK || !strings.Contains(out.String(), "hello from main") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

// TestAgentHelp checks that -h prints the same "Usage: cerberus agent"
// heading as the other subcommands and exits 0.
func TestAgentHelp(t *testing.T) {
	var out bytes.Buffer
	_, code, ok := parseFlags([]string{"-h"}, &config.Config{}, &out)
	if ok || code != exitOK {
		t.Fatalf("-h: ok=%v code=%d", ok, code)
	}
	if s := out.String(); !strings.HasPrefix(s, "Usage: cerberus agent") || !strings.Contains(s, "-model") {
		t.Errorf("help text:\n%s", s)
	}
}

// A reply that spends its budget thinking says so instead of ending blank
// (gateway phase G4).
func TestThinkingBudgetNote(t *testing.T) {
	_, base := newFake(t, sse(
		`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking hard"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
	))
	var out bytes.Buffer
	run(context.Background(), testOptions(t, base), strings.NewReader(""), &out)
	if !strings.Contains(out.String(), "ran out while the model was thinking") {
		t.Errorf("no budget note:\n%s", out.String())
	}
}

func TestRanOutThinking(t *testing.T) {
	cases := []struct {
		finish        string
		thought       bool
		answer, calls int
		want          bool
	}{
		{"length", true, 0, 0, true},
		{"length", true, 3, 0, false},
		{"length", true, 0, 1, false},
		{"length", false, 0, 0, false},
		{"stop", true, 0, 0, false},
	}
	for _, c := range cases {
		if got := ranOutThinking(c.finish, c.thought, c.answer, c.calls); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}
