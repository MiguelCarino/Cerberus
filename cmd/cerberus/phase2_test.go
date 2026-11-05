package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/subs"
)

func TestParseAnywhere(t *testing.T) {
	cases := []struct {
		args    []string
		wantPos []string
		note    string
		yes     bool
	}{
		{[]string{"set", "NAME", "--note", "hi"}, []string{"set", "NAME"}, "hi", false},
		{[]string{"--yes", "do", "it", "-note=x"}, []string{"do", "it"}, "x", true},
		{[]string{"a", "--", "--yes", "b"}, []string{"a", "--yes", "b"}, "", false},
		{nil, nil, "", false},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		note := fs.String("note", "", "")
		yes := fs.Bool("yes", false, "")
		pos, err := parseAnywhere(fs, c.args)
		if err != nil || !slices.Equal(pos, c.wantPos) || *note != c.note || *yes != c.yes {
			t.Errorf("%v: pos %v note %q yes %v err %v", c.args, pos, *note, *yes, err)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := parseAnywhere(fs, []string{"x", "--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestReadLine(t *testing.T) {
	for in, want := range map[string]string{"abc\n": "abc", "abc\r\nmore": "abc", "abc": "abc", "\n": ""} {
		if got, err := readLine(strings.NewReader(in), 100); err != nil || got != want {
			t.Errorf("readLine(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := readLine(strings.NewReader(strings.Repeat("x", 9000)), 100); err == nil {
		t.Error("over-long line accepted")
	}
	if _, err := readLine(strings.NewReader(""), 100); err != io.EOF {
		t.Errorf("empty input: %v", err)
	}
}

// recorder is a fake daemon that records each request and answers from a
// table keyed by "METHOD /path".
type recorder struct {
	mu    sync.Mutex
	reqs  []string // "METHOD /path?query body"
	reply map[string]string
}

func (rc *recorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"missing or invalid token"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		line := r.Method + " " + r.URL.RequestURI()
		if len(body) > 0 {
			line += " " + string(body)
		}
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, line)
		rc.mu.Unlock()
		reply, ok := rc.reply[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, `{"error":"unknown endpoint"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rc *recorder) last() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.reqs) == 0 {
		return ""
	}
	return rc.reqs[len(rc.reqs)-1]
}

func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.reqs)
}

func runCLI(stdin io.Reader, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, stdin, &out, &errb)
	return code, out.String(), errb.String()
}

func TestSecretCommandsThroughDaemon(t *testing.T) {
	rc := &recorder{reply: map[string]string{
		"GET /api/secrets":           `{"backend":"secret-service","items":[{"Name":"OPENAI","Note":"main key","Updated":"2026-10-30T10:00:00Z","Length":51}]}`,
		"PUT /api/secrets/OPENAI":    `{"ok":true}`,
		"DELETE /api/secrets/OPENAI": `{"ok":true}`,
	}}
	cfgPath := writeConfig(t, strings.TrimPrefix(rc.server(t).URL, "http://"))

	code, out, errOut := runCLI(nil, "secret", "list", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "Vault backend: secret-service") || !strings.Contains(out, "OPENAI") ||
		!strings.Contains(out, "51") || !strings.Contains(out, "main key") {
		t.Fatalf("list: %d %q %q", code, out, errOut)
	}

	code, out, errOut = runCLI(strings.NewReader("sk-abcdef\n"), "secret", "set", "OPENAI", "--note", "main key", "-config", cfgPath)
	if code != 0 || out != "stored OPENAI (9 bytes)\n" {
		t.Fatalf("set: %d %q %q", code, out, errOut)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(strings.SplitN(rc.last(), " ", 3)[2]), &body)
	if !strings.HasPrefix(rc.last(), "PUT /api/secrets/OPENAI ") || body["value"] != "sk-abcdef" || body["note"] != "main key" {
		t.Fatalf("put request %q", rc.last())
	}
	if strings.Contains(out+errOut, "sk-abcdef") {
		t.Fatal("value echoed")
	}
	// A multi-line piped value keeps its inner newlines; one trailing newline goes.
	if code, _, _ := runCLI(strings.NewReader("line1\nline2\n"), "secret", "set", "OPENAI", "-config", cfgPath); code != 0 ||
		!strings.Contains(rc.last(), `"value":"line1\nline2"`) {
		t.Fatalf("multi-line: %d %q", code, rc.last())
	}

	if code, out, _ := runCLI(nil, "secret", "rm", "OPENAI", "-config", cfgPath); code != 0 || out != "removed OPENAI\n" || rc.last() != "DELETE /api/secrets/OPENAI" {
		t.Fatalf("rm: %d %q %q", code, out, rc.last())
	}

	n := rc.count()
	for _, tc := range []struct {
		stdin string
		args  []string
		want  int
	}{
		{"", []string{"secret"}, 2},
		{"", []string{"secret", "nope"}, 2},
		{"", []string{"secret", "set"}, 2},
		{"x", []string{"secret", "set", "bad name"}, 2},
		{"x", []string{"secret", "set", "A", "B"}, 2},
		{"", []string{"secret", "set", "EMPTY"}, 1},
		{"\n", []string{"secret", "set", "EMPTY"}, 1},
		{strings.Repeat("x", 17<<10), []string{"secret", "set", "BIG"}, 1},
		{"", []string{"secret", "rm"}, 2},
		{"", []string{"secret", "rm", "bad/name"}, 2},
		{"", []string{"secret", "list", "extra"}, 2},
	} {
		if code, _, _ := runCLI(strings.NewReader(tc.stdin), append(tc.args, "-config", cfgPath)...); code != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.args, code, tc.want)
		}
	}
	if rc.count() != n {
		t.Errorf("invalid commands reached the daemon: %v", rc.reqs[n:])
	}
}

// TestSecretCommandsLocalFallback: with no daemon listening, the commands use
// the (file-backend) vault directly.
func TestSecretCommandsLocalFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now

	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Server.Listen, cfg.Server.Token = addr, "tok"
	cfg.Vault.Backend = "file"
	cfgPath := filepath.Join(dir, "config.json")
	cfg.SetPath(cfgPath)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	const value = "local-secret-value-42"
	code, out, errOut := runCLI(strings.NewReader(value), "secret", "set", "GH", "--note", "pushes", "-config", cfgPath)
	if code != 0 || !strings.Contains(errOut, "not running") {
		t.Fatalf("set: %d %q %q", code, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "vault.json"))
	if err != nil || bytes.Contains(raw, []byte(value)) {
		t.Fatalf("vault.json: %v (plaintext present: %v)", err, bytes.Contains(raw, []byte(value)))
	}
	code, out, _ = runCLI(nil, "secret", "list", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "Vault backend: file") || !strings.Contains(out, "GH") ||
		!strings.Contains(out, "pushes") || strings.Contains(out, value) {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, _, _ := runCLI(nil, "secret", "rm", "GH", "-config", cfgPath); code != 0 {
		t.Fatalf("rm: %d", code)
	}
	if code, out, _ := runCLI(nil, "secret", "list", "-config", cfgPath); code != 0 || !strings.Contains(out, "No secrets") {
		t.Fatalf("list after rm: %d %q", code, out)
	}
	if code, _, errOut := runCLI(nil, "secret", "rm", "GH", "-config", cfgPath); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("rm missing: %d %q", code, errOut)
	}
}

func TestSearchResumePruneCommands(t *testing.T) {
	rc := &recorder{reply: map[string]string{
		"GET /api/search":              `[{"Session":{"ID":"s9","State":"failed","Title":"fix navbar","Repo":"/r/Topo","Created":"2026-10-30T10:00:00Z"},"Snippets":["the  navbar\nbroke"],"Score":4}]`,
		"POST /api/sessions/s9/resume": `{"ok":true}`,
		"POST /api/prune":              `{"Removed":["a","b"],"Kept":[{"ID":"c","Title":"wip","Reason":"2 commits not on main"}],"Errors":[]}`,
	}}
	cfgPath := writeConfig(t, strings.TrimPrefix(rc.server(t).URL, "http://"))

	code, out, errOut := runCLI(nil, "search", "navbar", "broke", "--state", "failed", "--since", "2026-10-01", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "s9  failed") || !strings.Contains(out, "(/r/Topo)") || !strings.Contains(out, "… the navbar broke …") {
		t.Fatalf("search: %d %q %q", code, out, errOut)
	}
	if want := "GET /api/search?limit=20&q=navbar+broke&since=2026-10-01&state=failed"; rc.last() != want {
		t.Errorf("search request %q, want %q", rc.last(), want)
	}
	if code, _, _ := runCLI(nil, "search", "-config", cfgPath); code != 2 {
		t.Errorf("empty search: %d", code)
	}
	if code, _, _ := runCLI(nil, "search", "--agent", "claude", "--limit", "5", "-config", cfgPath); code != 0 || rc.last() != "GET /api/search?agent=claude&limit=5" {
		t.Errorf("filter-only search: %d %q", code, rc.last())
	}

	code, out, _ = runCLI(nil, "resume", "s9", "also", "update", "docs", "-config", cfgPath)
	if code != 0 || rc.last() != `POST /api/sessions/s9/resume {"text":"also update docs"}` || !strings.Contains(out, "/#/s/s9") {
		t.Fatalf("resume: %d %q %q", code, out, rc.last())
	}
	if code, _, _ := runCLI(nil, "resume", "s9", "-config", cfgPath); code != 0 || rc.last() != `POST /api/sessions/s9/resume {"text":""}` {
		t.Fatalf("resume without text: %d %q", code, rc.last())
	}
	if code, _, _ := runCLI(nil, "resume", "-config", cfgPath); code != 2 {
		t.Errorf("resume without id: %d", code)
	}
	if code, _, errOut := runCLI(nil, "resume", "zzz", "-config", cfgPath); code != 1 || !strings.Contains(errOut, "unknown endpoint") {
		t.Errorf("resume error: %d %q", code, errOut)
	}

	code, out, _ = runCLI(nil, "prune", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "Removed 2 finished") || !strings.Contains(out, "Kept c  wip: 2 commits not on main") || rc.last() != "POST /api/prune" {
		t.Fatalf("prune: %d %q %q", code, out, rc.last())
	}
	if code, _, _ := runCLI(nil, "prune", "extra", "-config", cfgPath); code != 2 {
		t.Errorf("prune with args: %d", code)
	}
}

func TestFleetCommand(t *testing.T) {
	rc := &recorder{reply: map[string]string{
		"GET /api/repos/find": `[{"Name":"Topo","Path":"/r/Topo","Dirty":false},{"Name":"Law","Path":"/r/Law","Dirty":true}]`,
		"POST /api/batches":   `{"batch":{"ID":"b1"},"sessions":[{"ID":"s1","State":"queued","Repo":"/r/Topo"},{"ID":"s2","State":"queued","Repo":"/r/Law"}]}`,
	}}
	cfgPath := writeConfig(t, strings.TrimPrefix(rc.server(t).URL, "http://"))

	// Declined: nothing is created.
	code, out, _ := runCLI(strings.NewReader("n\n"), "fleet", "--has", "carino-navbar.js", "update the navbar in {name}", "-config", cfgPath)
	if code != 1 || !strings.Contains(out, "2 repo(s) match") || !strings.Contains(out, "(dirty)") || !strings.Contains(out, "Start 2 session(s)?") {
		t.Fatalf("declined: %d %q", code, out)
	}
	if rc.last() != "GET /api/repos/find?has=carino-navbar.js" {
		t.Fatalf("declined made %q", rc.last())
	}
	// No answer at all (stdin closed) also declines.
	if code, _, _ := runCLI(strings.NewReader(""), "fleet", "--has", "x", "t", "-config", cfgPath); code != 1 || strings.HasPrefix(rc.last(), "POST") {
		t.Fatalf("eof: %d %q", code, rc.last())
	}

	code, out, errOut := runCLI(strings.NewReader("y\n"), "fleet", "--has", "carino-navbar.js", "--name", "T*", "--agent", "claude-headless",
		"--escalate", "claude", "--title", "Navbar v3", "update", "{name}", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "Fleet task b1: 2 session(s) queued") || !strings.Contains(out, "/#/s/s2") {
		t.Fatalf("confirmed: %d %q %q", code, out, errOut)
	}
	var spec session.BatchSpec
	_ = json.Unmarshal([]byte(strings.SplitN(rc.last(), " ", 3)[2]), &spec)
	if !slices.Equal(spec.Repos, []string{"/r/Topo", "/r/Law"}) || spec.Task != "update {name}" || spec.Agent != "claude-headless" ||
		spec.EscalateTo != "claude" || spec.Title != "Navbar v3" {
		t.Fatalf("spec %+v", spec)
	}

	before := rc.count()
	if code, _, _ := runCLI(nil, "fleet", "--yes", "--name", "Topo", "t", "-config", cfgPath); code != 0 || rc.count() != before+2 {
		t.Fatalf("--yes: %d, %d requests", code, rc.count()-before)
	}
	for _, args := range [][]string{{"fleet", "task"}, {"fleet", "--has", "x"}, {"fleet"}} {
		if code, _, _ := runCLI(nil, append(args, "-config", cfgPath)...); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
	rc.reply["GET /api/repos/find"] = `[]`
	if code, _, errOut := runCLI(nil, "fleet", "--has", "x", "--yes", "t", "-config", cfgPath); code != 1 || !strings.Contains(errOut, "no repos match") {
		t.Errorf("no match: %d %q", code, errOut)
	}
}

// ---------- plan activity ----------

type fakeTracker struct {
	mu    sync.Mutex
	calls []bool
}

func (f *fakeTracker) Snapshot() []subs.PlanUsage { return []subs.PlanUsage{{Name: "claude"}} }
func (f *fakeTracker) SetActive(on bool) {
	f.mu.Lock()
	f.calls = append(f.calls, on)
	f.mu.Unlock()
}
func (f *fakeTracker) last() (bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return false, 0
	}
	return f.calls[len(f.calls)-1], len(f.calls)
}

type fakeFeed struct {
	mu    sync.Mutex
	infos []session.Info
	ch    chan session.Event
	subs  int
}

func (f *fakeFeed) Subscribe() (<-chan session.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs++
	f.ch = make(chan session.Event, 4)
	return f.ch, func() {}
}
func (f *fakeFeed) List() []session.Info {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.infos)
}
func (f *fakeFeed) set(infos ...session.Info) {
	f.mu.Lock()
	f.infos = infos
	ch := f.ch
	f.mu.Unlock()
	ch <- session.Event{Type: session.EventState}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPlanActivity(t *testing.T) {
	tr := &fakeTracker{}
	a := newPlanActivity(tr)
	if len(a.Snapshot()) != 1 {
		t.Fatal("snapshot not passed through")
	}
	feed := &fakeFeed{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.followSessions(ctx, feed, map[string]bool{"claude": true})
		close(done)
	}()
	waitFor(t, "subscribe", func() bool { feed.mu.Lock(); defer feed.mu.Unlock(); return feed.ch != nil })

	feed.set(session.Info{Agent: "shell", State: session.StateRunning})
	feed.set(session.Info{Agent: "claude", State: session.StateRunning})
	waitFor(t, "live", func() bool { on, _ := tr.last(); return on })

	// The watch expiring must not turn tracking idle while Claude runs.
	a.SetActive(true)
	a.SetActive(false)
	if on, _ := tr.last(); !on {
		t.Fatal("watch expiry turned tracking idle during a live session")
	}
	feed.set(session.Info{Agent: "claude", State: session.StatePassed})
	waitFor(t, "idle", func() bool { on, _ := tr.last(); return !on })

	// A dropped subscription is renewed.
	feed.mu.Lock()
	close(feed.ch)
	feed.mu.Unlock()
	waitFor(t, "resubscribe", func() bool { feed.mu.Lock(); defer feed.mu.Unlock(); return feed.subs == 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("followSessions did not stop")
	}
	// No subscription agents: returns at once.
	a.followSessions(context.Background(), feed, nil)

	cfg := config.Defaults()
	cfg.Subscriptions = []config.SubscriptionConfig{{Agents: []string{"claude", "claude-headless"}}, {Agents: []string{"codex"}}}
	if got := subscriptionAgents(cfg); len(got) != 3 || !got["codex"] {
		t.Errorf("subscriptionAgents = %v", got)
	}
}
