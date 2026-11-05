package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/gateway"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/subs"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/vault"
	"carino.systems/cerberus/internal/voice"
)

// ---------- fakes for the Phase 2 services ----------

func (f *fakeSessions) Search(q session.SearchQuery) []session.SearchHit {
	f.record(fmt.Sprintf("search:%s|%s|%s|%s|%d|%s|%s", q.Text, q.Repo, q.Agent, q.State, q.Limit,
		q.Since.Format(time.DateOnly), q.Until.Format(time.DateOnly)))
	return []session.SearchHit{{Session: session.Info{ID: "s1", Title: "fix navbar"}, Snippets: []string{"…the navbar…"}, Score: 3}}
}
func (f *fakeSessions) Resume(ctx context.Context, id, text string) error {
	f.record("resume:" + id + ":" + text)
	if id == "s1" && text == "accepted" {
		return errors.New("session is accepted and cannot be resumed")
	}
	return f.known(id)
}
func (f *fakeSessions) Prune(ctx context.Context) (session.PruneReport, error) {
	f.record("prune")
	return session.PruneReport{Removed: []string{"old1"}, Kept: []session.PruneKept{{ID: "old2", Reason: "unaccepted commits"}}}, nil
}
func (f *fakeSessions) Transcript(id string) ([]byte, error) {
	if err := f.known(id); err != nil {
		return nil, err
	}
	return []byte("$ make test\nok\n"), nil
}
func (f *fakeSessions) FindRepos(sel session.RepoSelector) []session.RepoMatch {
	f.record("find:" + sel.HasFile + ":" + sel.NameGlob)
	return []session.RepoMatch{{Name: "Topo", Path: "/r/Topo"}, {Name: "Law", Path: "/r/Law", Dirty: true}}
}
func (f *fakeSessions) CreateBatch(ctx context.Context, b session.BatchSpec) (session.BatchInfo, []session.Info, error) {
	f.record(fmt.Sprintf("batch:%d:%s:%s", len(b.Repos), b.Task, b.EscalateTo))
	if len(b.Repos) == 0 {
		return session.BatchInfo{}, nil, errors.New("pick at least one repo")
	}
	return session.BatchInfo{ID: "b1", Task: b.Task, Sessions: []string{"s1"}}, []session.Info{{ID: "s1", State: "queued"}}, nil
}
func (f *fakeSessions) Batches() []session.BatchInfo { return nil }
func (f *fakeSessions) Batch(id string) (session.BatchInfo, []session.Info, bool) {
	if id != "b1" {
		return session.BatchInfo{}, nil, false
	}
	return session.BatchInfo{ID: "b1", Counts: map[string]int{"passed": 1}}, []session.Info{{ID: "s1", State: "passed"}}, true
}
func (f *fakeSessions) AcceptBatch(ctx context.Context, id string, o session.AcceptOpts) []session.BatchResult {
	if ctx.Err() != nil {
		return nil
	}
	f.record(fmt.Sprintf("acceptbatch:%s:%s:%v", id, o.Message, o.Push))
	return []session.BatchResult{{SessionID: "s1", Result: session.ResultAccepted, Commit: "abc"}}
}
func (f *fakeSessions) DiscardBatch(id string) []session.BatchResult {
	f.record("discardbatch:" + id)
	return []session.BatchResult{{SessionID: "s1", Result: session.ResultDiscarded}}
}

type fakePlans struct {
	mu     sync.Mutex
	active []bool
}

func (p *fakePlans) Snapshot() []subs.PlanUsage {
	return []subs.PlanUsage{{Name: "claude", Kind: "claude-code", Window5hTokens: 1200, WeeklyLimit: 0}}
}
func (p *fakePlans) SetActive(on bool) {
	p.mu.Lock()
	p.active = append(p.active, on)
	p.mu.Unlock()
}
func (p *fakePlans) calls() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.active...)
}

type fakeSpend struct{}

func (fakeSpend) SpendByProvider(since time.Time) map[string]float64 {
	if since.Day() == 1 && since.Hour() == 0 && time.Now().Day() != 1 {
		return map[string]float64{"openai": 12.5}
	}
	return map[string]float64{"openai": 0.5}
}

type fakeBudgets struct{}

func (fakeBudgets) Budgets() []gateway.BudgetStatus {
	return []gateway.BudgetStatus{{Budget: config.Budget{Provider: "openai", Period: "day", SoftUSD: 1, HardUSD: 2}, Spent: 0.5, State: "ok"}}
}

type fakeUsageStore struct{}

func (fakeUsageStore) Recent(n int) []usage.Record           { return nil }
func (fakeUsageStore) Aggregate(since time.Time) []usage.Agg { return nil }

type fakeVoice struct {
	ok       bool
	gotMime  string
	gotBytes int
	err      error
}

func (v *fakeVoice) Available() (bool, string) {
	if v.ok {
		return true, "command"
	}
	return false, "no whisper backend found"
}
func (v *fakeVoice) MaxSeconds() int { return 300 }
func (v *fakeVoice) Transcribe(ctx context.Context, audio io.Reader, mime string) (string, error) {
	n, err := io.Copy(io.Discard, audio)
	v.gotMime, v.gotBytes = mime, int(n)
	if err != nil {
		return "", err
	}
	if v.err != nil {
		return "", v.err
	}
	return "fix the navbar", nil
}

// phase2Env is newEnv plus a real file-backend vault in a temp config dir,
// fake plans, spend, budgets and voice, and logs captured in a buffer.
type phase2Env struct {
	*testEnv
	vault *vault.Vault
	plans *fakePlans
	voice *fakeVoice
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newPhase2Env(t *testing.T, mut func(*config.Config)) *phase2Env {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var cfgDir string
	env := newEnv(t, func(c *config.Config) {
		cfgDir = t.TempDir()
		c.SetPath(filepath.Join(cfgDir, "config.json"))
		c.Vault.Backend = vault.BackendFile // never the desktop keyring in tests
		if mut != nil {
			mut(c)
		}
	})
	v, err := vault.Open(env.srv.cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	pe := &phase2Env{testEnv: env, vault: v, plans: &fakePlans{}, voice: &fakeVoice{ok: true}, logs: logs}
	s := env.srv
	s.log = log
	s.secrets, s.plans, s.voice = v, pe.plans, pe.voice
	s.spend, s.budgets = fakeSpend{}, fakeBudgets{}
	env.h = s.handler()
	return pe
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

// ---------- tests ----------

// TestPhase2Auth: every new endpoint needs the token, and cookie writes need
// the CSRF header.
func TestPhase2Auth(t *testing.T) {
	env := newPhase2Env(t, nil)
	csrf := withHeader(csrfHeader, "1")
	cases := []struct {
		method, target, body string
		want                 int
	}{
		{"GET", "/api/secrets", "", 200},
		{"PUT", "/api/secrets/OPENAI", `{"value":"abcdefgh"}`, 200},
		{"DELETE", "/api/secrets/OPENAI", "", 200},
		{"GET", "/api/search?q=x", "", 200},
		{"POST", "/api/sessions/s1/resume", `{"text":"go on"}`, 200},
		{"POST", "/api/prune", "", 200},
		{"GET", "/api/sessions/s1/transcript", "", 200},
		{"GET", "/api/repos/find?has=x", "", 200},
		{"GET", "/api/batches", "", 200},
		{"POST", "/api/batches", `{"repos":["/r/Topo"],"task":"t"}`, 201},
		{"GET", "/api/batches/b1", "", 200},
		{"POST", "/api/batches/b1/accept", `{}`, 200},
		{"POST", "/api/batches/b1/discard", "", 200},
		{"GET", "/api/voice", "", 200},
		{"POST", "/api/voice/transcribe", "RIFF", 200},
		{"POST", "/api/usage/watch", "", 200},
	}
	for _, tc := range cases {
		name := tc.method + " " + tc.target
		if w := env.do(tc.method, tc.target, tc.body); w.Code != 401 {
			t.Errorf("%s without auth: %d", name, w.Code)
		}
		if w := env.do(tc.method, tc.target, tc.body, withBearer("nope")); w.Code != 401 {
			t.Errorf("%s with a wrong token: %d", name, w.Code)
		}
		if w := env.do(tc.method, tc.target, tc.body, withBearer(env.srv.cfg.AgentToken())); w.Code != 401 {
			t.Errorf("%s with the agent token: %d", name, w.Code)
		}
		if !safeMethod(tc.method) {
			if w := env.do(tc.method, tc.target, tc.body, withCookie(testToken)); w.Code != 403 {
				t.Errorf("%s cookie without CSRF header: %d", name, w.Code)
			}
		}
		if tc.method == "DELETE" { // the PUT above created it
			if w := env.do("PUT", "/api/secrets/OPENAI", `{"value":"abcdefgh"}`, withBearer(testToken)); w.Code != 200 {
				t.Fatalf("re-put: %d", w.Code)
			}
		}
		if w := env.do(tc.method, tc.target, tc.body, withCookie(testToken), csrf); w.Code != tc.want {
			t.Errorf("%s with cookie and CSRF: %d, want %d (%s)", name, w.Code, tc.want, w.Body)
		}
	}
}

func TestSecretsEndpoints(t *testing.T) {
	env := newPhase2Env(t, nil)
	auth := withBearer(testToken)
	if w := env.do("PUT", "/api/secrets/GITHUB_TOKEN", `{"value":"ghp_abcdef123456","note":"fleet pushes"}`, auth); w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	w := env.do("GET", "/api/secrets", "", auth)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil || w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if string(raw["backend"]) != `"file"` {
		t.Errorf("backend = %s", raw["backend"])
	}
	var items []map[string]any
	_ = json.Unmarshal(raw["items"], &items)
	if len(items) != 1 || items[0]["Name"] != "GITHUB_TOKEN" || items[0]["Note"] != "fleet pushes" ||
		items[0]["Length"] != float64(16) || items[0]["Updated"] == nil {
		t.Fatalf("items = %v", items)
	}
	if len(items[0]) != 4 {
		t.Errorf("item has unexpected fields: %v", items[0])
	}
	big := `{"value":"` + strings.Repeat("a", 40<<10) + `"}`
	for _, tc := range []struct {
		method, target, body string
		want                 int
	}{
		{"PUT", "/api/secrets/bad%20name", `{"value":"abcdefgh"}`, 400},
		{"PUT", "/api/secrets/" + strings.Repeat("x", 65), `{"value":"abcdefgh"}`, 400},
		{"PUT", "/api/secrets/EMPTY", `{"value":""}`, 400},
		{"PUT", "/api/secrets/EMPTY", `{"value":`, 400},
		{"PUT", "/api/secrets/BIG", big, 413},
		{"PUT", "/api/secrets/BIGVAL", `{"value":"` + strings.Repeat("a", 17<<10) + `"}`, 400},
		{"DELETE", "/api/secrets/NOPE", "", 404},
		{"DELETE", "/api/secrets/GITHUB_TOKEN", "", 200},
		{"DELETE", "/api/secrets/GITHUB_TOKEN", "", 404},
	} {
		if w := env.do(tc.method, tc.target, tc.body, auth); w.Code != tc.want {
			t.Errorf("%s %s: %d, want %d (%s)", tc.method, tc.target, w.Code, tc.want, w.Body)
		}
	}
	if w := env.do("GET", "/api/secrets", "", auth); !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("empty list = %s", w.Body)
	}
	// Without a vault the endpoints answer 503.
	plain := newEnv(t, nil)
	if w := plain.do("GET", "/api/secrets", "", auth); w.Code != 503 {
		t.Errorf("no vault: %d", w.Code)
	}
}

// TestSecretNeverLeaks stores a known secret, then exercises every endpoint
// and checks that neither a response nor a log line contains it.
func TestSecretNeverLeaks(t *testing.T) {
	const secret = "sk-live-NEVER-SHOW-ME-0123456789"
	env := newPhase2Env(t, func(c *config.Config) {
		c.Server.TLSCert, c.Server.TLSKey = "/x/cert.pem", "/x/key.pem"
	})
	auth := withBearer(testToken)
	var bodies []string
	call := func(method, target, body string, opts ...reqOpt) {
		w := env.do(method, target, body, append([]reqOpt{auth}, opts...)...)
		bodies = append(bodies, w.Body.String())
		for k, vs := range w.Header() {
			bodies = append(bodies, k+": "+strings.Join(vs, ","))
		}
	}
	call("PUT", "/api/secrets/OPENAI", `{"value":"`+secret+`","note":"key"}`)
	call("PUT", "/api/secrets/OPENAI", `{"value":"`+secret+`","note":"`+strings.Repeat("n", 2000)+`"}`) // note too long
	call("PUT", "/api/secrets/bad%20name", `{"value":"`+secret+`"}`)
	call("PUT", "/api/secrets/OPENAI", `{"value":"`+secret+`",`) // malformed
	call("PUT", "/api/secrets/OPENAI", `{"value":"`+secret+strings.Repeat("x", 40<<10)+`"}`)
	call("PUT", "/api/secrets/OPENAI", `{"value":`+secret+`}`) // invalid JSON quoting the value
	for _, target := range []string{"/api/secrets", "/api/status", "/api/usage", "/api/search?q=NEVER",
		"/api/voice", "/api/batches", "/api/repos/find", "/api/sessions/s1/transcript", "/api/repos"} {
		call("GET", target, "")
	}
	call("POST", "/api/usage/watch", "")
	call("POST", "/api/prune", "")
	call("DELETE", "/api/secrets/OPENAI", "")
	v, err := env.vault.Get("OPENAI")
	if err == nil {
		t.Fatalf("secret still stored (%d bytes)", len(v))
	}
	for _, b := range bodies {
		if strings.Contains(b, secret) || strings.Contains(b, "NEVER-SHOW-ME") {
			t.Errorf("secret in response: %.120s", b)
		}
	}
	if logs := env.logs.String(); strings.Contains(logs, "NEVER-SHOW-ME") {
		t.Errorf("secret in logs")
	} else if !strings.Contains(logs, "secret stored") {
		t.Errorf("expected a log line for the store; logs:\n%s", logs)
	}
}

func TestStatusPhase2Fields(t *testing.T) {
	env := newPhase2Env(t, nil)
	st := decode[map[string]json.RawMessage](t, env.do("GET", "/api/status", "", withBearer(testToken)))
	if string(st["vault_backend"]) != `"file"` || string(st["voice"]) != `{"available":true,"backend":"command"}` || string(st["tls"]) != "false" {
		t.Fatalf("vault_backend %s voice %s tls %s", st["vault_backend"], st["voice"], st["tls"])
	}
	if want, _ := json.Marshal(env.srv.cfg.DefaultAgent); string(st["default_agent"]) != string(want) {
		t.Errorf("default_agent %s, want %s", st["default_agent"], want)
	}
	env.voice.ok = false
	env.srv.cfg.Server.TLSCert, env.srv.cfg.Server.TLSKey = "c", "k"
	env.srv.cfg.Server.ExtraListen = []string{"100.64.0.1:7340"}
	st = decode[map[string]json.RawMessage](t, env.do("GET", "/api/status", "", withBearer(testToken)))
	if string(st["voice"]) != `{"available":false,"backend":""}` || string(st["tls"]) != "true" {
		t.Fatalf("voice %s tls %s", st["voice"], st["tls"])
	}
	// Without the services the fields are present and empty.
	plain := decode[map[string]json.RawMessage](t, newEnv(t, nil).do("GET", "/api/status", "", withBearer(testToken)))
	if string(plain["vault_backend"]) != `""` || string(plain["voice"]) != `{"available":false,"backend":""}` {
		t.Fatalf("plain status %s %s", plain["vault_backend"], plain["voice"])
	}
}

func TestUsagePhase2(t *testing.T) {
	env := newPhase2Env(t, nil)
	env.srv.usage = fakeUsageStore{}
	u := decode[map[string]json.RawMessage](t, env.do("GET", "/api/usage", "", withBearer(testToken)))
	for _, k := range []string{"recent", "agg_24h", "spend", "budgets", "plans"} {
		if _, ok := u[k]; !ok {
			t.Errorf("usage lacks %q", k)
		}
	}
	var spend Spend
	_ = json.Unmarshal(u["spend"], &spend)
	if spend.Today["openai"] != 0.5 || spend.Month["openai"] == 0 {
		t.Errorf("spend = %+v", spend)
	}
	var budgets []map[string]any
	_ = json.Unmarshal(u["budgets"], &budgets)
	if len(budgets) != 1 || budgets[0]["provider"] != "openai" || budgets[0]["period"] != "day" ||
		budgets[0]["hard_usd"] != float64(2) || budgets[0]["spent"] != 0.5 || budgets[0]["state"] != "ok" {
		t.Errorf("budgets = %v", budgets)
	}
	var plans []map[string]any
	_ = json.Unmarshal(u["plans"], &plans)
	if len(plans) != 1 || plans[0]["Name"] != "claude" || plans[0]["Window5hTokens"] != float64(1200) {
		t.Errorf("plans = %v", plans)
	}

	// No spend, budget or plan sources: empty but present.
	plain := newEnv(t, nil)
	plain.srv.usage = fakeUsageStore{}
	w := plain.do("GET", "/api/usage", "", withBearer(testToken))
	for _, want := range []string{`"spend":{"today":{},"month":{}}`, `"budgets":[]`, `"plans":[]`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("plain usage lacks %s: %s", want, w.Body)
		}
	}
	if w := plain.do("POST", "/api/usage/watch", "", withBearer(testToken)); w.Code != 503 {
		t.Errorf("watch without tracker: %d", w.Code)
	}
}

func TestUsageWatchExpires(t *testing.T) {
	env := newPhase2Env(t, nil)
	w := env.do("POST", "/api/usage/watch", "", withBearer(testToken))
	got := decode[map[string]any](t, w)
	if got["ok"] != true || got["until"] == nil {
		t.Fatalf("watch reply %v", got)
	}
	if c := env.plans.calls(); len(c) != 1 || !c[0] {
		t.Fatalf("SetActive calls %v", c)
	}
	// A second watch replaces the timer; only the last one turns tracking off.
	env.srv.watchUsage(time.Hour)
	env.srv.watchUsage(20 * time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := env.plans.calls()
		if len(c) == 4 && !c[3] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SetActive calls %v", c)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if c := env.plans.calls(); len(c) != 4 {
		t.Fatalf("extra SetActive calls %v", c)
	}
	env.srv.watch.mu.Lock()
	if env.srv.watch.timer != nil {
		t.Error("timer kept after expiry")
	}
	env.srv.watch.mu.Unlock()
}

func TestSearchEndpoint(t *testing.T) {
	env := newEnv(t, nil)
	auth := withBearer(testToken)
	w := env.do("GET", "/api/search?q=navbar+fix&repo=Topo&agent=claude&state=failed&since=2026-10-01&until=2026-10-31&limit=9999", "", auth)
	if w.Code != 200 {
		t.Fatalf("search %d %s", w.Code, w.Body)
	}
	if got, want := env.sessions.lastCall(), "search:navbar fix|Topo|claude|failed|500|2026-10-01|2026-11-01"; got != want {
		t.Errorf("call %q, want %q", got, want)
	}
	hits := decode[[]map[string]any](t, w)
	if len(hits) != 1 || hits[0]["Score"] != float64(3) || hits[0]["Session"].(map[string]any)["ID"] != "s1" ||
		len(hits[0]["Snippets"].([]any)) != 1 {
		t.Errorf("hits = %v", hits)
	}
	for _, q := range []string{"since=yesterday", "until=2026-13-01", "limit=0", "limit=x"} {
		if w := env.do("GET", "/api/search?"+q, "", auth); w.Code != 400 {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
	q, err := parseSearch(func(k string) string {
		return map[string]string{"since": "2026-10-01T10:00:00Z"}[k]
	}, time.UTC)
	if err != nil || !q.Since.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) || !q.Until.IsZero() || q.Limit != 0 {
		t.Errorf("parseSearch = %+v, %v", q, err)
	}
}

func TestHistoryActions(t *testing.T) {
	env := newEnv(t, nil)
	auth := withBearer(testToken)
	if w := env.do("POST", "/api/sessions/s1/resume", `{"text":"also fix the docs"}`, auth); w.Code != 200 {
		t.Fatalf("resume %d %s", w.Code, w.Body)
	}
	if got := env.sessions.lastCall(); got != "resume:s1:also fix the docs" {
		t.Errorf("call %q", got)
	}
	if w := env.do("POST", "/api/sessions/s1/resume", "", auth); w.Code != 200 || env.sessions.lastCall() != "resume:s1:" {
		t.Errorf("resume without body: %d %q", w.Code, env.sessions.lastCall())
	}
	if w := env.do("POST", "/api/sessions/s1/resume", `{"text":"accepted"}`, auth); w.Code != 409 {
		t.Errorf("refused resume: %d", w.Code)
	}
	if w := env.do("POST", "/api/sessions/zzz/resume", "", auth); w.Code != 404 {
		t.Errorf("unknown resume: %d", w.Code)
	}

	w := env.do("POST", "/api/prune", "", auth)
	rep := decode[map[string]json.RawMessage](t, w)
	if w.Code != 200 || string(rep["Removed"]) != `["old1"]` || string(rep["Errors"]) != "[]" || !strings.Contains(string(rep["Kept"]), "unaccepted commits") {
		t.Errorf("prune %d %s", w.Code, w.Body)
	}

	w = env.do("GET", "/api/sessions/s1/transcript", "", auth)
	if w.Code != 200 || w.Body.String() != "$ make test\nok\n" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("transcript %d %q %v", w.Code, w.Body, w.Header())
	}
	if w := env.do("GET", "/api/sessions/zzz/transcript", "", auth); w.Code != 404 {
		t.Errorf("unknown transcript: %d", w.Code)
	}
}

func TestFleetEndpoints(t *testing.T) {
	env := newEnv(t, nil)
	auth := withBearer(testToken)
	w := env.do("GET", "/api/repos/find?has=carino-navbar.js&name=T*", "", auth)
	if env.sessions.lastCall() != "find:carino-navbar.js:T*" {
		t.Errorf("call %q", env.sessions.lastCall())
	}
	repos := decode[[]map[string]any](t, w)
	if len(repos) != 2 || repos[1]["Name"] != "Law" || repos[1]["Dirty"] != true || repos[0]["Path"] != "/r/Topo" {
		t.Errorf("repos = %v", repos)
	}
	if w := env.do("GET", "/api/batches", "", auth); w.Body.String() != "[]\n" {
		t.Errorf("batches = %q", w.Body)
	}
	w = env.do("POST", "/api/batches", `{"Repos":["/r/Topo","/r/Law"],"Task":"update {name}","EscalateTo":"claude"}`, auth)
	if w.Code != 201 || env.sessions.lastCall() != "batch:2:update {name}:claude" {
		t.Fatalf("create %d %s %q", w.Code, w.Body, env.sessions.lastCall())
	}
	view := decode[map[string]json.RawMessage](t, w)
	if !strings.Contains(string(view["batch"]), `"ID":"b1"`) || !strings.Contains(string(view["sessions"]), `"s1"`) {
		t.Errorf("create reply %s", w.Body)
	}
	if w := env.do("POST", "/api/batches", `{"Task":"x"}`, auth); w.Code != 400 {
		t.Errorf("empty repos: %d", w.Code)
	}
	if w := env.do("POST", "/api/batches", `{"Repos":`, auth); w.Code != 400 {
		t.Errorf("bad json: %d", w.Code)
	}
	w = env.do("GET", "/api/batches/b1", "", auth)
	view = decode[map[string]json.RawMessage](t, w)
	if w.Code != 200 || !strings.Contains(string(view["batch"]), `"passed":1`) || !strings.Contains(string(view["sessions"]), `"passed"`) {
		t.Errorf("get %d %s", w.Code, w.Body)
	}
	if w := env.do("GET", "/api/batches/nope", "", auth); w.Code != 404 {
		t.Errorf("unknown batch: %d", w.Code)
	}
	w = env.do("POST", "/api/batches/b1/accept", `{"message":"Navbar v3","push":true}`, auth)
	res := decode[[]map[string]any](t, w)
	if w.Code != 200 || env.sessions.lastCall() != "acceptbatch:b1:Navbar v3:true" || len(res) != 1 || res[0]["Result"] != "accepted" {
		t.Errorf("accept %d %s %q", w.Code, w.Body, env.sessions.lastCall())
	}
	w = env.do("POST", "/api/batches/b1/discard", "", auth)
	if w.Code != 200 || env.sessions.lastCall() != "discardbatch:b1" || !strings.Contains(w.Body.String(), `"discarded"`) {
		t.Errorf("discard %d %s", w.Code, w.Body)
	}
	for target, want := range map[string]int{"/api/batches/nope/accept": 404, "/api/batches/b1/explode": 404} {
		if w := env.do("POST", target, "", auth); w.Code != want {
			t.Errorf("%s: %d, want %d", target, w.Code, want)
		}
	}
}

func TestVoiceEndpoints(t *testing.T) {
	env := newPhase2Env(t, nil)
	auth := withBearer(testToken)
	vi := decode[map[string]any](t, env.do("GET", "/api/voice", "", auth))
	if vi["available"] != true || vi["backend"] != "command" || vi["reason"] != "" || vi["max_seconds"] != float64(300) ||
		!strings.Contains(vi["secure_hint"].(string), "tailscale cert") {
		t.Errorf("voice = %v", vi)
	}
	// Over TLS no hint is needed; with an https PublicURL the hint names it.
	vi = decode[map[string]any](t, env.do("GET", "/api/voice", "", auth, func(r *http.Request) { r.TLS = &tls.ConnectionState{} }))
	if vi["secure_hint"] != "" {
		t.Errorf("hint over TLS: %v", vi["secure_hint"])
	}
	env.srv.cfg.Server.PublicURL = "https://desk.tail.ts.net:7341"
	vi = decode[map[string]any](t, env.do("GET", "/api/voice", "", auth))
	if !strings.Contains(vi["secure_hint"].(string), "https://desk.tail.ts.net:7341") {
		t.Errorf("hint = %v", vi["secure_hint"])
	}
	env.voice.ok = false
	vi = decode[map[string]any](t, env.do("GET", "/api/voice", "", auth))
	if vi["available"] != false || vi["reason"] != "no whisper backend found" {
		t.Errorf("unavailable voice = %v", vi)
	}
	env.voice.ok = true

	w := env.do("POST", "/api/voice/transcribe", "OggS-audio", auth, withHeader("Content-Type", "audio/ogg; codecs=opus"))
	if w.Code != 200 || w.Body.String() != `{"text":"fix the navbar"}`+"\n" || env.voice.gotMime != "audio/ogg; codecs=opus" || env.voice.gotBytes != 10 {
		t.Fatalf("transcribe %d %s %q %d", w.Code, w.Body, env.voice.gotMime, env.voice.gotBytes)
	}
	for err, want := range map[error]int{
		voice.ErrTooLong: 413, voice.ErrTooLarge: 413, voice.ErrUnsupported: 415, voice.ErrNoAudio: 400,
		fmt.Errorf("%w: none", voice.ErrUnavailable): 503, context.DeadlineExceeded: 504, errors.New("ffmpeg died"): 502,
	} {
		env.voice.err = err
		if w := env.do("POST", "/api/voice/transcribe", "x", auth); w.Code != want {
			t.Errorf("%v: %d, want %d", err, w.Code, want)
		}
	}
	env.voice.err = nil

	// Content-Length over 25 MiB is refused before reading; a body that lies
	// about its size is cut off by the reader.
	r := httptest.NewRequest("POST", "/api/voice/transcribe", strings.NewReader("x"))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.ContentLength = voice.MaxBytes + 1
	rec := httptest.NewRecorder()
	env.h.ServeHTTP(rec, r)
	if rec.Code != 413 {
		t.Errorf("declared too large: %d", rec.Code)
	}
	r = httptest.NewRequest("POST", "/api/voice/transcribe", io.LimitReader(zeroReader{}, voice.MaxBytes+100))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.ContentLength = -1
	rec = httptest.NewRecorder()
	env.h.ServeHTTP(rec, r)
	if rec.Code != 413 || env.voice.gotBytes != voice.MaxBytes+1 {
		t.Errorf("streamed too large: %d after %d bytes", rec.Code, env.voice.gotBytes)
	}

	if w := newEnv(t, nil).do("POST", "/api/voice/transcribe", "x", auth); w.Code != 503 {
		t.Errorf("no voice: %d", w.Code)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestCookieSecureOverTLS(t *testing.T) {
	env := newEnv(t, nil)
	plain := env.do("GET", "/?token="+testToken, "").Result().Cookies()
	secure := env.do("GET", "/?token="+testToken, "", func(r *http.Request) { r.TLS = &tls.ConnectionState{} }).Result().Cookies()
	if len(plain) != 1 || plain[0].Secure {
		t.Errorf("plain HTTP cookie = %+v", plain)
	}
	if len(secure) != 1 || !secure[0].Secure || !secure[0].HttpOnly {
		t.Errorf("TLS cookie = %+v", secure)
	}
}
