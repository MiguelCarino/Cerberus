package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// fakeAgentAPI stands in for the daemon's /api/agent scope.
type fakeAgentAPI struct {
	mu    sync.Mutex
	calls []string // "METHOD path body"
	auth  []string
}

func (f *fakeAgentAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI()+" "+strings.TrimSpace(string(body)))
		f.auth = append(f.auth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Cerberus-Session"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/agent/spawn":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"ID":"c1","Title":"t","Name":"kid","State":"queued","WaitingReason":"needs approval"}`)
		case r.URL.Path == "/api/agent/children":
			io.WriteString(w, `[{"ID":"c1","Title":"t","State":"running","Agent":"cerberus","Parent":"s-1","Activity":"building"},`+
				`{"ID":"g1","Title":"g","State":"passed","Agent":"quick","Parent":"c1"}]`)
		case strings.HasSuffix(r.URL.Path, "/tail"):
			io.WriteString(w, `{"id":"c1","state":"passed","text":"all good\n","truncated":true}`)
		case strings.HasSuffix(r.URL.Path, "/wait"):
			io.WriteString(w, `{"done":true,"session":{"ID":"c1","State":"failed","ExitCode":3,"Error":"checks failed"}}`)
		case strings.HasSuffix(r.URL.Path, "/send"), strings.HasSuffix(r.URL.Path, "/stop") && strings.Contains(r.URL.Path, "/c1/"):
			io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"no such child session \"zz\""}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAgentAPI) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestSpawnClientCalls(t *testing.T) {
	f := &fakeAgentAPI{}
	c := NewSpawnClient(f.server(t).URL+"/", "cas-tok", "s-1")
	ctx := context.Background()
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{ToolSpawn, map[string]any{"task": "do x", "workspace": "same"}, "Started child session c1 (kid): queued — needs approval."},
		{ToolList, nil, "c1 · t · running · cerberus · building\ng1 · g · passed · quick · child of c1"},
		{ToolRead, map[string]any{"id": "c1", "tail_kib": 999}, "Session c1 is passed. Last 64 KiB of its transcript:\nall good\n"},
		{ToolSend, map[string]any{"id": "c1", "text": "more"}, "Sent to c1."},
		{ToolWait, map[string]any{"id": "c1", "timeout_s": 5000}, "Session c1 is failed (exit 3): checks failed."},
		{ToolStop, map[string]any{"id": "c1"}, "Stopping c1."},
	}
	for _, tc := range cases {
		got, err := c.Call(ctx, tc.name, tc.args)
		if err != nil || !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: %q, %v; want prefix %q", tc.name, got, err, tc.want)
		}
	}
	log := f.log()
	wants := []string{
		`POST /api/agent/spawn {"agent":"","model":"","name":"","task":"do x","workspace":"same"}`,
		"GET /api/agent/children ",
		"GET /api/agent/sessions/c1/tail?kib=64 ",
		`POST /api/agent/sessions/c1/send {"text":"more"}`,
		"GET /api/agent/sessions/c1/wait?timeout=600 ",
		"POST /api/agent/sessions/c1/stop ",
	}
	for i, w := range wants {
		if i >= len(log) || log[i] != w {
			t.Errorf("request %d = %q, want %q", i, log[min(i, len(log)-1)], w)
		}
	}
	for _, a := range f.auth {
		if a != "Bearer cas-tok|s-1" {
			t.Fatalf("auth headers %q", a)
		}
	}
	if _, err := c.Call(ctx, ToolStop, map[string]any{"id": "zz"}); err == nil || !strings.Contains(err.Error(), `refused (404): no such child session "zz"`) {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := c.Call(ctx, ToolSend, map[string]any{"id": "c1"}); err == nil || !strings.Contains(err.Error(), "text is required") {
		t.Fatalf("missing text: %v", err)
	}
	if _, err := NewSpawnClient("", "k", "s").Call(ctx, ToolList, nil); err == nil || !strings.Contains(err.Error(), "CERBERUS_URL") {
		t.Fatalf("no URL: %v", err)
	}
}

func TestSpawnClientGating(t *testing.T) {
	cfg := config.Defaults()
	t.Setenv("CERBERUS_URL", "")
	if spawnClient(cfg, "k", "s") != nil {
		t.Fatal("tools without CERBERUS_URL")
	}
	t.Setenv("CERBERUS_URL", "http://127.0.0.1:1")
	if spawnClient(cfg, "k", "") != nil {
		t.Fatal("tools without a session")
	}
	if spawnClient(cfg, "k", "s") == nil {
		t.Fatal("no tools with CERBERUS_URL, a session and spawn enabled")
	}
	cfg.Agent.Spawn.Enabled = false
	if spawnClient(cfg, "k", "s") != nil {
		t.Fatal("tools with spawn disabled")
	}
}

func toolNamesIn(req map[string]any) []string {
	var out []string
	tools, _ := req["tools"].([]any)
	for _, raw := range tools {
		out = append(out, raw.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return out
}

// In ask mode, spawn and stop need approval like run_command; list, read,
// send and wait do not.
func TestSpawnToolsInLoop(t *testing.T) {
	api := &fakeAgentAPI{}
	apiURL := api.server(t).URL
	f, base := newFake(t,
		sse(toolChunks(
			[2]string{ToolSpawn, `{"task":"split work"}`},
			[2]string{ToolList, `{}`},
			[2]string{ToolStop, `{"id":"c1"}`},
		)...),
		sse(content("ok")),
	)
	o := testOptions(t, base)
	o.Approve = "ask"
	o.Spawn = NewSpawnClient(apiURL, o.Key, o.Session)
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader("n\ny\n"), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	s := out.String()
	if n := strings.Count(s, "\x1b]777;cerberus;waiting;approval\x07"); n != 2 {
		t.Errorf("%d approvals asked, want 2 (spawn, stop):\n%s", n, s)
	}
	if !strings.Contains(s, "Approve cerberus_spawn?") || !strings.Contains(s, "▸ cerberus_spawn: split work") {
		t.Errorf("spawn card or approval missing:\n%s", s)
	}
	reqs := f.requests()
	names := toolNamesIn(reqs[0])
	for _, want := range []string{"run_command", ToolSpawn, ToolList, ToolRead, ToolSend, ToolWait, ToolStop} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("tools offered %v lack %s", names, want)
		}
	}
	sys := reqs[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "cerberus_spawn") {
		t.Errorf("system prompt does not mention child sessions")
	}
	tm := toolMessages(reqs[1])
	if len(tm) != 3 || tm[0] != "The user refused this." || !strings.Contains(tm[1], "c1 · t · running") || tm[2] != "Stopping c1." {
		t.Fatalf("tool messages %q", tm)
	}
	calls := api.log()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "GET /api/agent/children") || !strings.HasPrefix(calls[1], "POST /api/agent/sessions/c1/stop") {
		t.Fatalf("api calls %q (a refused spawn must not reach the daemon)", calls)
	}
}

func TestNoSpawnToolsWithoutClient(t *testing.T) {
	f, base := newFake(t,
		sse(toolChunks([2]string{ToolList, `{}`})...),
		sse(content("ok")),
	)
	o := testOptions(t, base)
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	reqs := f.requests()
	for _, n := range toolNamesIn(reqs[0]) {
		if strings.HasPrefix(n, "cerberus_") {
			t.Fatalf("spawn tool %s offered without a client", n)
		}
	}
	if tm := toolMessages(reqs[1]); len(tm) != 1 || !strings.Contains(tm[0], `there is no tool "cerberus_list"`) {
		t.Fatalf("tool messages %q", tm)
	}
	sys := reqs[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "cerberus_spawn") {
		t.Fatal("system prompt mentions child sessions without the tools")
	}
}

func TestSpawnToolSchemas(t *testing.T) {
	for _, tool := range SpawnTools() {
		raw, err := json.Marshal(tool.Schema)
		if err != nil || tool.Schema["type"] != "object" || !isSpawnTool(tool.Name) {
			t.Fatalf("%s: %s %v", tool.Name, raw, err)
		}
	}
}
