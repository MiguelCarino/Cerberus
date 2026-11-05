//go:build !windows

package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/session"
)

// ccEnv is the real API handler over a real session manager on temp
// folders, with fake agents: "echo" prints a greeting and then echoes each
// input line until stdin closes; "quick" prints its task and exits 0.
type ccEnv struct {
	t   *testing.T
	cfg *config.Config
	m   *session.Manager
	srv *server
	ts  *httptest.Server
}

func newCCEnv(t *testing.T, mut func(*config.Config)) *ccEnv {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	bin := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	echo := script("echo.sh", "echo \"hello-$CERBERUS_SESSION\"\nwhile IFS= read -r l; do echo \"got:$l\"; done\n")
	quick := script("quick.sh", "echo \"child did: $1\"\nexit 0\n")

	cfg := config.Defaults()
	cfg.Server.Token = testToken
	cfg.RepoRoots, cfg.Repos = nil, nil
	cfg.Guard = config.GuardConfig{MaxSessions: 12}
	cfg.Notify = config.NotifyConfig{Disabled: true}
	cfg.Agents = []config.AgentProfile{
		{Name: "echo", Command: []string{"bash", echo}},
		{Name: "quick", Command: []string{"bash", quick, "{task}"}},
	}
	cfg.DefaultAgent = "echo"
	cfg.Agent.Spawn = config.SpawnConfig{Enabled: true, MaxDepth: 2, MaxChildren: 4, MaxTotal: 8}
	if mut != nil {
		mut(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := session.NewManager(cfg, nil, nil, notify.New(cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	s := newServer(Deps{Cfg: cfg, Version: "test", Log: log})
	s.sessions, s.history, s.fleet, s.masker, s.control = m, m, m, m, m
	s.ping = time.Hour
	s.gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session":"`+r.Header.Get(sessionHeader)+`"}`)
	})
	ts := httptest.NewServer(s.handler())
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer scancel()
		m.Shutdown(sctx)
	})
	return &ccEnv{t: t, cfg: cfg, m: m, srv: s, ts: ts}
}

// person starts a session as a person would (no parent).
func (e *ccEnv) person(agent, task string) string {
	e.t.Helper()
	out, err := e.m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Agent: agent, Task: task})
	if err != nil {
		e.t.Fatal(err)
	}
	return out[0].ID
}

// call sends a request; auth is "admin", "" (none), or a session id whose
// agent token and X-Cerberus-Session are sent.
func (e *ccEnv) call(method, path, body, auth string, hdr ...string) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	switch auth {
	case "":
	case "admin":
		req.Header.Set("Authorization", "Bearer "+testToken)
	default:
		req.Header.Set("Authorization", "Bearer "+e.cfg.SessionAgentToken(auth))
		req.Header.Set(sessionHeader, auth)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (e *ccEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	events, unsub := e.m.Subscribe()
	defer unsub()
	tick := time.NewTicker(50 * time.Millisecond) // test-only safety net
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		select {
		case <-events:
		case <-tick.C:
		}
	}
}

func (e *ccEnv) state(id string) string {
	info, _ := e.m.Get(id)
	return info.State
}

func (e *ccEnv) spawn(caller, body string) (int, session.Info) {
	e.t.Helper()
	code, raw := e.call("POST", "/api/agent/spawn", body, caller)
	var info session.Info
	if code == http.StatusCreated {
		if err := json.Unmarshal(raw, &info); err != nil {
			e.t.Fatal(err)
		}
	}
	return code, info
}

func TestAgentScopeEnforcement(t *testing.T) {
	e := newCCEnv(t, func(c *config.Config) { c.Agent.Spawn.MaxChildren = 2; c.Agent.Spawn.MaxDepth = 1 })
	a := e.person("echo", "parent A")
	b := e.person("echo", "person B")
	e.waitFor("A running", func() bool { return e.state(a) == session.StateRunning })

	code, child := e.spawn(a, `{"task":"child work","agent":"quick","name":"kid"}`)
	if code != http.StatusCreated || child.Parent != a || child.Depth != 1 || child.Name != "kid" ||
		child.Workspace != session.WorkspaceScratch {
		t.Fatalf("spawn: %d %+v", code, child)
	}

	// The agent token reaches /v1 (pinned to its own session) and its own
	// scope, never the control API.
	sessTok := e.cfg.SessionAgentToken(a)
	if code, raw := e.call("GET", "/v1/models", "", "", "Authorization", "Bearer "+sessTok,
		sessionHeader, b); code != 200 || !strings.Contains(string(raw), `"session":"`+a+`"`) {
		t.Fatalf("/v1 with a session token: %d %s", code, raw)
	}
	if code, _ := e.call("GET", "/v1/models", "", "", "Authorization", "Bearer "+e.cfg.AgentToken()); code != 200 {
		t.Fatalf("/v1 with the shared agent token: %d", code)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/sessions", ""},
		{"POST", "/api/sessions", `{"task":"x","workspace":"scratch"}`},
		{"GET", "/api/sessions/" + child.ID, ""},
		{"PATCH", "/api/sessions/" + child.ID, `{"name":"x"}`},
		{"POST", "/api/sessions/bulk", `{"ids":["` + child.ID + `"],"op":{"kind":"stop"}}`},
		{"POST", "/api/sessions/" + child.ID + "/stop", ""},
		{"POST", "/api/sessions/" + child.ID + "/kill", ""},
		{"POST", "/api/sessions/" + child.ID + "/approve", `{"approve":true}`},
		{"POST", "/api/sessions/" + child.ID + "/input", `{"data":"x"}`},
		{"GET", "/api/feed", ""},
		{"GET", "/api/feed/stream", ""},
		{"GET", "/api/wall/stream?ids=" + child.ID, ""},
		{"GET", "/api/events", ""},
		{"GET", "/api/status", ""},
		{"GET", "/api/secrets", ""},
		{"GET", "/api/status?token=" + sessTok, ""},
	} {
		if code, raw := e.call(c.method, c.path, c.body, a, "X-Cerberus", "1"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with an agent token: %d %s", c.method, c.path, code, raw)
		}
	}

	// The agent scope takes only a session token whose header matches.
	scopeCases := []struct {
		name string
		hdr  []string
		want int
	}{
		{"no token", nil, 401},
		{"forged", []string{"Authorization", "Bearer cas-" + a + ".00", sessionHeader, a}, 401},
		{"admin token", []string{"Authorization", "Bearer " + testToken, sessionHeader, a}, 403},
		{"shared agent token", []string{"Authorization", "Bearer " + e.cfg.AgentToken(), sessionHeader, a}, 403},
		{"admin cookie", []string{"Cookie", cookieName + "=" + testToken, sessionHeader, a}, 403},
		{"no header", []string{"Authorization", "Bearer " + sessTok}, 403},
		{"other session's header", []string{"Authorization", "Bearer " + sessTok, sessionHeader, b}, 403},
		{"unknown session", []string{"Authorization", "Bearer " + e.cfg.SessionAgentToken("nope"), sessionHeader, "nope"}, 403},
		{"ok", []string{"Authorization", "Bearer " + sessTok, sessionHeader, a}, 200},
	}
	for _, c := range scopeCases {
		if code, raw := e.call("GET", "/api/agent/children", "", "", c.hdr...); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, raw, c.want)
		}
	}

	// Descendants only.
	code, raw := e.call("GET", "/api/agent/children", "", a)
	var kids []session.Info
	if err := json.Unmarshal(raw, &kids); code != 200 || err != nil || len(kids) != 1 || kids[0].ID != child.ID {
		t.Fatalf("children of A: %d %s", code, raw)
	}
	if code, raw := e.call("GET", "/api/agent/children", "", b); code != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("children of B: %d %s", code, raw)
	}
	for _, p := range []string{"/api/agent/sessions/" + b + "/tail", "/api/agent/sessions/" + a + "/tail",
		"/api/agent/sessions/" + b + "/wait?timeout=0"} {
		if code, _ := e.call("GET", p, "", a); code != 404 {
			t.Errorf("A reads %s: %d", p, code)
		}
	}
	for _, p := range []string{"/api/agent/sessions/" + b + "/stop", "/api/agent/sessions/" + b + "/send"} {
		if code, _ := e.call("POST", p, `{"text":"hi"}`, a); code != 404 {
			t.Errorf("A steers %s: %d", p, code)
		}
	}
	if e.state(b) != session.StateRunning {
		t.Fatalf("B was touched: %s", e.state(b))
	}
	// The child cannot see its parent either.
	if code, _ := e.call("GET", "/api/agent/sessions/"+a+"/tail", "", child.ID); code != 404 {
		t.Fatalf("child reads its parent: %d", code)
	}

	// Limits: depth 1 means a child cannot spawn; two children at most.
	if code, _ := e.spawn(child.ID, `{"task":"grandchild","agent":"quick"}`); code != http.StatusForbidden {
		t.Fatalf("grandchild over MaxDepth: %d", code)
	}
	if code, _ := e.spawn(a, `{"task":"second","agent":"echo"}`); code != http.StatusCreated {
		t.Fatalf("second child: %d", code)
	}
	if code, _ := e.spawn(a, `{"task":"third","agent":"quick"}`); code != http.StatusForbidden {
		t.Fatalf("third child over MaxChildren: %d", code)
	}
	// An agent cannot pick folders.
	if code, _ := e.spawn(b, `{"task":"x","workspace":"folder"}`); code != http.StatusBadRequest {
		t.Fatalf("folder workspace: %d", code)
	}

	// Wait, tail, send, stop on a descendant.
	code, raw = e.call("GET", "/api/agent/sessions/"+child.ID+"/wait?timeout=20", "", a)
	var w waitReply
	if err := json.Unmarshal(raw, &w); code != 200 || err != nil || !w.Done || w.Session.State != session.StatePassed {
		t.Fatalf("wait: %d %s", code, raw)
	}
	code, raw = e.call("GET", "/api/agent/sessions/"+child.ID+"/tail?kib=4", "", a)
	var tr tailReply
	if err := json.Unmarshal(raw, &tr); code != 200 || err != nil || !strings.Contains(tr.Text, "child did: child work") {
		t.Fatalf("tail: %d %s", code, raw)
	}
	second := ""
	for _, k := range e.m.Descendants(a) {
		if k.Agent == "echo" {
			second = k.ID
		}
	}
	e.waitFor("second running", func() bool { return e.state(second) == session.StateRunning })
	// send never types into a running child (see TestAgentSendRunningChildAndOptionTask).
	if code, raw := e.call("POST", "/api/agent/sessions/"+second+"/send", `{"text":"ping"}`, a); code != http.StatusConflict {
		t.Fatalf("send to a running child: %d %s", code, raw)
	}
	if code, raw := e.call("GET", "/api/agent/sessions/"+second+"/wait?timeout=0", "", a); code != 200 ||
		!strings.Contains(string(raw), `"done":false`) {
		t.Fatalf("wait timeout 0: %d %s", code, raw)
	}
	if code, raw := e.call("POST", "/api/agent/sessions/"+second+"/stop", "", a); code != 200 {
		t.Fatalf("stop: %d %s", code, raw)
	}
	e.waitFor("second stopped", func() bool { return e.state(second) == session.StateStopped })
}

func TestSpawnApprovalFlow(t *testing.T) {
	e := newCCEnv(t, func(c *config.Config) { c.Agent.Spawn.RequireApproval = true })
	a := e.person("echo", "parent")
	code, kid := e.spawn(a, `{"task":"needs ok","agent":"quick"}`)
	if code != http.StatusCreated || kid.State != session.StateQueued || kid.WaitingReason != session.ReasonNeedsApproval {
		t.Fatalf("spawn with approval: %d %+v", code, kid)
	}
	if code, _ := e.call("POST", "/api/sessions/"+kid.ID+"/approve", `{}`, "admin"); code != 400 {
		t.Fatalf("approve without a decision: %d", code)
	}
	if code, raw := e.call("POST", "/api/sessions/"+kid.ID+"/approve", `{"approve":true}`, "admin"); code != 200 {
		t.Fatalf("approve: %d %s", code, raw)
	}
	e.waitFor("approved child passes", func() bool { return e.state(kid.ID) == session.StatePassed })
	if code, _ := e.call("POST", "/api/sessions/"+kid.ID+"/approve", `{"approve":true}`, "admin"); code != 409 {
		t.Fatalf("approve twice: %d", code)
	}
	_, rej := e.spawn(a, `{"task":"no","agent":"quick"}`)
	if code, _ := e.call("POST", "/api/sessions/"+rej.ID+"/approve", `{"approve":false}`, "admin"); code != 200 {
		t.Fatalf("reject: %d", code)
	}
	if st := e.state(rej.ID); st != session.StateDiscarded {
		t.Fatalf("rejected child is %s", st)
	}
}

func TestPatchBulkKill(t *testing.T) {
	e := newCCEnv(t, nil)
	a, b := e.person("echo", "one"), e.person("echo", "two")

	code, raw := e.call("PATCH", "/api/sessions/"+a, `{"Name":"First","Tags":["Web","api"],"Pinned":true,"Priority":3}`, "admin")
	var info session.Info
	if err := json.Unmarshal(raw, &info); code != 200 || err != nil || info.Name != "First" ||
		!slices.Equal(info.Tags, []string{"web", "api"}) || !info.Pinned || info.Priority != 3 {
		t.Fatalf("patch: %d %s", code, raw)
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/sessions/" + a, `{"Tags":["bad tag!"]}`, 400},
		{"/api/sessions/" + a, `{"Priority":11}`, 400},
		{"/api/sessions/nope", `{"Name":"x"}`, 404},
		{"/api/sessions/" + a, `not json`, 400},
	} {
		if code, raw := e.call("PATCH", c.path, c.body, "admin"); code != c.want {
			t.Errorf("patch %s %s: %d %s", c.path, c.body, code, raw)
		}
	}

	code, raw = e.call("POST", "/api/sessions/bulk", `{"ids":["`+a+`","`+b+`","nope"],"op":{"kind":"tag","tag":"Batch"}}`, "admin")
	var br bulkReply
	if err := json.Unmarshal(raw, &br); code != 200 || err != nil || br.OK != 2 || br.Error != 1 || len(br.Results) != 3 {
		t.Fatalf("bulk tag: %d %s", code, raw)
	}
	if i, _ := e.m.Get(b); !slices.Contains(i.Tags, "batch") {
		t.Fatalf("tags after bulk: %v", i.Tags)
	}
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = `"x` + string(rune('a'+i%26)) + strings.Repeat("y", i) + `"`
	}
	if code, _ := e.call("POST", "/api/sessions/bulk", `{"ids":[`+strings.Join(ids, ",")+`],"op":{"kind":"stop"}}`, "admin"); code != 400 {
		t.Fatalf("101 ids: %d", code)
	}
	if code, _ := e.call("POST", "/api/sessions/bulk", `{"ids":[],"op":{"kind":"stop"}}`, "admin"); code != 400 {
		t.Fatalf("no ids: %d", code)
	}
	code, raw = e.call("POST", "/api/sessions/bulk", `{"ids":["`+a+`"],"op":{"kind":"explode"}}`, "admin")
	if err := json.Unmarshal(raw, &br); code != 200 || err != nil || br.Error != 1 {
		t.Fatalf("unknown kind: %d %s", code, raw)
	}

	e.waitFor("both running", func() bool {
		return e.state(a) == session.StateRunning && e.state(b) == session.StateRunning
	})
	code, raw = e.call("POST", "/api/sessions/bulk", `{"ids":["`+a+`","`+b+`"],"op":{"kind":"input","data":"hey\r"}}`, "admin")
	if err := json.Unmarshal(raw, &br); code != 200 || err != nil || br.OK != 2 {
		t.Fatalf("bulk input: %d %s", code, raw)
	}
	e.waitFor("broadcast echoed", func() bool {
		ra, _ := e.m.Transcript(a)
		rb, _ := e.m.Transcript(b)
		return strings.Contains(string(ra), "got:hey") && strings.Contains(string(rb), "got:hey")
	})

	if code, raw := e.call("POST", "/api/sessions/"+a+"/kill", "", "admin", "X-Cerberus", "1"); code != 200 {
		t.Fatalf("kill: %d %s", code, raw)
	}
	e.waitFor("killed", func() bool { return e.state(a) == session.StateStopped })
	if code, _ := e.call("POST", "/api/sessions/"+a+"/kill", "", "admin"); code != 409 {
		t.Fatalf("kill a stopped session: %d", code)
	}
	if code, _ := e.call("POST", "/api/sessions/nope/kill", "", "admin"); code != 404 {
		t.Fatalf("kill unknown: %d", code)
	}
}

// ccSSE reads "event:"/"data:" pairs from a stream.
type ccSSE struct {
	sc   *bufio.Scanner
	body io.Closer
}

func (e *ccEnv) stream(path string) *ccSSE {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		e.t.Fatalf("stream %s: %d %s", path, resp.StatusCode, raw)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	e.t.Cleanup(func() { resp.Body.Close() })
	return &ccSSE{sc: sc, body: resp.Body}
}

// next returns the next event, or fails after the deadline.
func (r *ccSSE) next(t *testing.T) (string, []byte) {
	t.Helper()
	type ev struct {
		name string
		data []byte
		ok   bool
	}
	done := make(chan ev, 1)
	go func() {
		var name string
		for r.sc.Scan() {
			line := r.sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				done <- ev{name, []byte(strings.TrimPrefix(line, "data: ")), true}
				return
			}
		}
		done <- ev{}
	}()
	select {
	case e := <-done:
		if !e.ok {
			t.Fatal("stream ended")
		}
		return e.name, e.data
	case <-time.After(20 * time.Second):
		r.body.Close()
		t.Fatal("no event within 20 s")
	}
	return "", nil
}

func TestWallStreamMultiplexes(t *testing.T) {
	e := newCCEnv(t, nil)
	a, b := e.person("echo", "wall a"), e.person("echo", "wall b")
	e.waitFor("greetings", func() bool {
		ra, _ := e.m.Transcript(a)
		rb, _ := e.m.Transcript(b)
		return strings.Contains(string(ra), "hello-"+a) && strings.Contains(string(rb), "hello-"+b)
	})
	if code, _ := e.call("GET", "/api/wall/stream?ids=", "", "admin"); code != 400 {
		t.Fatalf("no ids: %d", code)
	}
	if code, _ := e.call("GET", "/api/wall/stream?ids="+a+",nope", "", "admin"); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = a + strings.Repeat("x", i)
	}
	if code, _ := e.call("GET", "/api/wall/stream?ids="+strings.Join(many, ","), "", "admin"); code != 400 {
		t.Fatalf("17 ids: %d", code)
	}

	st := e.stream("/api/wall/stream?ids=" + a + "," + b + "," + a)
	decode := func(raw []byte) (wallChunk, string) {
		var c wallChunk
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(c.B64)
		if err != nil {
			t.Fatal(err)
		}
		return c, string(data)
	}
	for _, id := range []string{a, b} {
		name, raw := st.next(t)
		c, text := decode(raw)
		if name != "out" || !c.Backlog || c.ID != id || !strings.Contains(text, "hello-"+id) {
			t.Fatalf("backlog event %s %+v %q", name, c, text)
		}
	}
	for _, id := range []string{b, a} {
		if code, _ := e.call("POST", "/api/sessions/"+id+"/input", `{"data":"to-`+id+`\r"}`, "admin"); code != 200 {
			t.Fatal("input failed")
		}
	}
	seen := map[string]string{}
	for !strings.Contains(seen[a], "got:to-"+a) || !strings.Contains(seen[b], "got:to-"+b) {
		name, raw := st.next(t)
		c, text := decode(raw)
		if name != "out" || c.Backlog {
			t.Fatalf("live event %s %+v", name, c)
		}
		seen[c.ID] += text
	}
	if strings.Contains(seen[a], "to-"+b) || strings.Contains(seen[b], "to-"+a) {
		t.Fatalf("output crossed sessions: %q", seen)
	}
}

func TestFeedAndFeedStream(t *testing.T) {
	e := newCCEnv(t, nil)
	start := time.Now().Add(-time.Second)
	a := e.person("echo", "feed me")
	e.waitFor("running", func() bool { return e.state(a) == session.StateRunning })

	st := e.stream("/api/feed/stream")
	if code, _ := e.call("PATCH", "/api/sessions/"+a, `{"Name":"Fed"}`, "admin"); code != 200 {
		t.Fatal("patch failed")
	}
	if code, _ := e.call("POST", "/api/sessions/"+a+"/input", `{"data":"ab"}`, "admin"); code != 200 {
		t.Fatal("input failed")
	}
	if code, _ := e.call("POST", "/api/sessions/"+a+"/input", `{"data":"c\r"}`, "admin"); code != 200 {
		t.Fatal("input failed")
	}
	var got []session.FeedItem
	for len(got) < 2 {
		name, raw := st.next(t)
		var it session.FeedItem
		if err := json.Unmarshal(raw, &it); err != nil || name != "feed" {
			t.Fatalf("feed event %s %s", name, raw)
		}
		if it.Kind == session.FeedUpdate || it.Kind == session.FeedInput {
			got = append(got, it)
		}
	}
	if got[0].Kind != session.FeedUpdate || got[0].Name != "Fed" || got[1].Kind != session.FeedInput || got[1].Text != "abc ⏎" {
		t.Fatalf("feed items %+v", got)
	}

	code, raw := e.call("GET", "/api/feed?since="+start.UTC().Format(time.RFC3339Nano)+"&limit=50", "", "admin")
	var items []session.FeedItem
	if err := json.Unmarshal(raw, &items); code != 200 || err != nil || len(items) < 3 || items[0].Kind != session.FeedCreated {
		t.Fatalf("feed: %d %s", code, raw)
	}
	if code, _ := e.call("GET", "/api/feed?since=yesterday", "", "admin"); code != 400 {
		t.Fatalf("bad since: %d", code)
	}
	// Replay with ?since= on the stream.
	st2 := e.stream("/api/feed/stream?since=" + start.UTC().Format(time.RFC3339Nano))
	if name, raw := st2.next(t); name != "feed" || !strings.Contains(string(raw), `"Kind":"created"`) {
		t.Fatalf("replay: %s %s", name, raw)
	}
}

func TestTailClip(t *testing.T) {
	raw := []byte(strings.Repeat("a", 10) + "\nlast line ñ\n")
	if s, cut := tail(raw, 100); cut || s != string(raw) {
		t.Fatalf("short tail %q %v", s, cut)
	}
	if s, cut := tail(raw, 14); !cut || s != "last line ñ\n" {
		t.Fatalf("cut tail %q %v", s, cut)
	}
}
