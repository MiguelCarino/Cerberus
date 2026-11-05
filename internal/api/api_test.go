package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/permits"
	"carino.systems/cerberus/internal/session"
)

const testToken = "s3cret-token"

// fakeSessions implements sessionService.
type fakeSessions struct {
	mu       sync.Mutex
	infos    []session.Info
	preview  string
	backlog  []byte
	out      chan []byte
	detached chan struct{}
	events   chan session.Event
	unsubbed chan struct{}
	calls    []string
	state    string // Get's state for s1 (default running)
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{out: make(chan []byte, 4), detached: make(chan struct{}),
		events: make(chan session.Event, 4), unsubbed: make(chan struct{})}
}

func (f *fakeSessions) record(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakeSessions) lastCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeSessions) known(id string) error {
	if id == "s1" {
		return nil
	}
	return session.ErrNotFound
}

func (f *fakeSessions) List() []session.Info { return f.infos }
func (f *fakeSessions) Get(id string) (session.Info, bool) {
	f.mu.Lock()
	st := f.state
	f.mu.Unlock()
	if st == "" {
		st = session.StateRunning
	}
	return session.Info{ID: id, State: st}, f.known(id) == nil
}

func (f *fakeSessions) setState(st string) {
	f.mu.Lock()
	f.state = st
	f.mu.Unlock()
}
func (f *fakeSessions) Create(ctx context.Context, t session.TaskSpec) ([]session.Info, error) {
	c := "create:" + t.Repo + ":" + t.Task
	if t.Parent != "" {
		c += ":parent=" + t.Parent
	}
	f.record(c)
	return []session.Info{{ID: "s1", Task: t.Task}}, nil
}
func (f *fakeSessions) Input(id string, data []byte) error {
	f.record("input:" + string(data))
	return f.known(id)
}
func (f *fakeSessions) AnswerApproval(id string, seq int, answer string) (session.Info, error) {
	f.record(fmt.Sprintf("answer:%d:%s", seq, answer))
	if err := f.known(id); err != nil {
		return session.Info{}, err
	}
	if seq != 1 {
		return session.Info{}, session.ErrNoApproval
	}
	return session.Info{ID: id, WaitingAsk: "write_file: x.txt"}, nil
}
func (f *fakeSessions) AnswerSave(id string, seq, option int) (session.Info, permits.Saved, error) {
	f.record(fmt.Sprintf("save:%d:%d", seq, option))
	if err := f.known(id); err != nil {
		return session.Info{}, permits.Saved{}, err
	}
	if seq != 1 {
		return session.Info{}, permits.Saved{}, session.ErrNoApproval
	}
	if option != 0 {
		return session.Info{}, permits.Saved{}, session.ErrBadOption
	}
	return session.Info{ID: id, WaitingAsk: "run_command: wc -l *.go"}, permits.Saved{ID: "p1", Tool: "Bash", Content: "wc -l *.go", Text: "Bash(wc -l *.go)"}, nil
}
func (f *fakeSessions) Resize(id string, cols, rows int) error { return f.known(id) }
func (f *fakeSessions) Attach(id string) ([]byte, <-chan []byte, func(), error) {
	if err := f.known(id); err != nil {
		return nil, nil, nil, err
	}
	var once sync.Once
	return f.backlog, f.out, func() { once.Do(func() { close(f.detached) }) }, nil
}
func (f *fakeSessions) FollowUp(ctx context.Context, id, text string) error {
	f.record("followup:" + text)
	return f.known(id)
}
func (f *fakeSessions) RunChecks(ctx context.Context, id string) error { return f.known(id) }
func (f *fakeSessions) Stop(id string) error {
	f.record("stop:" + id)
	return f.known(id)
}
func (f *fakeSessions) Diff(id string) (string, error) { return "diff --git a b\n", f.known(id) }
func (f *fakeSessions) Accept(ctx context.Context, id string, o session.AcceptOpts) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	f.record("accept:" + o.Message)
	if o.Push && id == "s1" {
		return "abc123", errors.New("push rejected")
	}
	return "abc123", f.known(id)
}
func (f *fakeSessions) Discard(id string) error                           { return f.known(id) }
func (f *fakeSessions) Delete(_ context.Context, id string, _ bool) error { return f.known(id) }
func (f *fakeSessions) Subscribe() (<-chan session.Event, func()) {
	var once sync.Once
	return f.events, func() { once.Do(func() { close(f.unsubbed) }) }
}
func (f *fakeSessions) Agents() []session.AgentStatus {
	return []session.AgentStatus{{Name: "cerberus", Available: true}}
}
func (f *fakeSessions) PreviewDir(id string) (string, bool) {
	return f.preview, id == "s1" && f.preview != ""
}

func (f *fakeSessions) SaveTo(id, dir string, overwrite bool) (int, error) {
	f.record(fmt.Sprintf("save:%s:%v", dir, overwrite))
	return 2, f.known(id)
}
func (f *fakeSessions) SaveInto(id, root, dir string, overwrite bool) (int, error) {
	f.record(fmt.Sprintf("saveinto:%s:%s:%v", root, dir, overwrite))
	return 2, f.known(id)
}
func (f *fakeSessions) Archive(id string, w io.Writer) error {
	if err := f.known(id); err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	fw, _ := zw.Create("out.txt")
	_, _ = fw.Write([]byte("hi"))
	return zw.Close()
}
func (f *fakeSessions) Browse(dir string, hidden bool) (session.Listing, error) {
	f.record(fmt.Sprintf("browse:%s:%v", dir, hidden))
	if dir == "/outside" {
		return session.Listing{}, errors.New("outside your home folder")
	}
	return session.Listing{Path: "/home/u", Home: true, Entries: []session.FSEntry{{Name: "proj", Path: "/home/u/proj", Git: true}}}, nil
}

// fakeModels implements modelService.
type fakeModels struct {
	mu   sync.Mutex
	last string
}

func (f *fakeModels) Models(ctx context.Context) []engine.ModelInfo {
	return []engine.ModelInfo{{ID: "openrouter/meta/llama", Backend: "openai"}}
}
func (f *fakeModels) do(action, id string) error {
	f.mu.Lock()
	f.last = action + " " + id
	f.mu.Unlock()
	if id == "missing" {
		return engine.ErrUnknownModel
	}
	return nil
}
func (f *fakeModels) Load(ctx context.Context, id string) error   { return f.do("load", id) }
func (f *fakeModels) Unload(ctx context.Context, id string) error { return f.do("unload", id) }
func (f *fakeModels) Delete(ctx context.Context, id string) error { return f.do("delete", id) }

type fakeGPU struct{}

func (fakeGPU) Snapshot() gpu.Snapshot { return gpu.Snapshot{GPUs: []gpu.GPU{{Name: "Test GPU"}}} }

type testEnv struct {
	srv      *server
	h        http.Handler
	sessions *fakeSessions
	models   *fakeModels
}

func newEnv(t *testing.T, mut func(*config.Config)) *testEnv {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Token = testToken
	cfg.RepoRoots = nil
	cfg.Repos = nil
	// httptest.NewRequest sends "Host: example.com"; the Host check
	// (safety.go) must know it as one of our names.
	cfg.Server.AllowedHosts = []string{"example.com"}
	if mut != nil {
		mut(cfg)
	}
	s := newServer(Deps{Cfg: cfg, Version: "test"})
	fs, fm := newFakeSessions(), &fakeModels{}
	s.sessions, s.models, s.gpu = fs, fm, fakeGPU{}
	s.history, s.fleet = fs, fs
	s.memAvail = func() int { return 12345 }
	s.gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
	})
	return &testEnv{srv: s, h: s.handler(), sessions: fs, models: fm}
}

type reqOpt func(*http.Request)

func from(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }
func withBearer(t string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+t) }
}
func withCookie(t string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: t}) }
}
func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func withHost(h string) reqOpt      { return func(r *http.Request) { r.Host = h } }

// local is a desktop client on this machine: loopback peer and Host.
func local(addr string) reqOpt {
	return func(r *http.Request) { r.RemoteAddr, r.Host = addr, "127.0.0.1:7340" }
}

func (e *testEnv) do(method, target, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	r.RemoteAddr = "192.0.2.10:5555"
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func TestAuthMatrix(t *testing.T) {
	env := newEnv(t, nil)
	noLocal := newEnv(t, func(c *config.Config) { c.Server.LocalNoAuth = false })
	csrf := withHeader(csrfHeader, "1")
	cases := []struct {
		name     string
		env      *testEnv
		method   string
		target   string
		opts     []reqOpt
		want     int
		location string
	}{
		{"no auth denied", env, "GET", "/api/status", nil, 401, ""},
		{"wrong bearer", env, "GET", "/api/status", []reqOpt{withBearer("nope")}, 401, ""},
		{"bearer", env, "GET", "/api/status", []reqOpt{withBearer(testToken)}, 200, ""},
		{"lowercase bearer scheme", env, "GET", "/api/status", []reqOpt{withHeader("Authorization", "bearer "+testToken)}, 200, ""},
		{"cookie GET", env, "GET", "/api/status", []reqOpt{withCookie(testToken)}, 200, ""},
		{"wrong cookie", env, "GET", "/api/status", []reqOpt{withCookie("nope")}, 401, ""},
		{"cookie POST without CSRF header", env, "POST", "/api/sessions/s1/stop", []reqOpt{withCookie(testToken)}, 403, ""},
		{"cookie POST with CSRF header", env, "POST", "/api/sessions/s1/stop", []reqOpt{withCookie(testToken), csrf}, 200, ""},
		{"cookie POST wrong CSRF value", env, "POST", "/api/sessions/s1/stop", []reqOpt{withCookie(testToken), withHeader(csrfHeader, "yes")}, 403, ""},
		{"bearer POST needs no CSRF header", env, "POST", "/api/sessions/s1/stop", []reqOpt{withBearer(testToken)}, 200, ""},
		{"query token sets cookie", env, "GET", "/?token=" + testToken, nil, 302, "/"},
		{"query token keeps other params", env, "GET", "/api/usage?token=" + testToken + "&limit=3", nil, 302, "/api/usage?limit=3"},
		{"query token cannot open-redirect", env, "GET", "//evil.example/?token=" + testToken, nil, 302, "/evil.example/"},
		{"bad query token", env, "GET", "/?token=nope", nil, 302, "/login"},
		{"bad query token on api", env, "GET", "/api/status?token=nope", nil, 401, ""},
		{"query token on POST ignored", env, "POST", "/api/sessions/s1/stop?token=" + testToken, nil, 401, ""},
		{"v1 loopback v4 no auth", env, "GET", "/v1/models", []reqOpt{local("127.0.0.1:4000")}, 200, ""},
		{"v1 loopback v6 no auth", env, "GET", "/v1/models", []reqOpt{local("[::1]:4000")}, 200, ""},
		{"v1 loopback localhost host", env, "GET", "/v1/models", []reqOpt{from("127.0.0.1:4000"), withHost("localhost:7340")}, 200, ""},
		{"v1 loopback rebinding host", env, "GET", "/v1/models", []reqOpt{from("127.0.0.1:4000"), withHost("evil.example:7340")}, 403, ""},
		{"v1 loopback from a web page", env, "GET", "/v1/models", []reqOpt{local("127.0.0.1:4000"), withHeader("Origin", "https://evil.example")}, 401, ""},
		{"v1 loopback POST text/plain", env, "POST", "/v1/chat/completions", []reqOpt{local("127.0.0.1:4000"), withHeader("Content-Type", "text/plain")}, 401, ""},
		{"v1 loopback POST json", env, "POST", "/v1/chat/completions", []reqOpt{local("127.0.0.1:4000"), withHeader("Content-Type", "application/json; charset=utf-8")}, 200, ""},
		{"v1 loopback web page with token", env, "GET", "/v1/models", []reqOpt{local("127.0.0.1:4000"), withHeader("Origin", "https://x"), withBearer(testToken)}, 200, ""},
		{"v1 agent token", noLocal, "GET", "/v1/models", []reqOpt{withBearer(env.srv.cfg.AgentToken())}, 200, ""},
		{"api refuses agent token", env, "POST", "/api/sessions/s1/accept", []reqOpt{withBearer(env.srv.cfg.AgentToken())}, 401, ""},
		{"cookie api cross-site", env, "GET", "/api/status", []reqOpt{withCookie(testToken), withHeader("Sec-Fetch-Site", "cross-site")}, 403, ""},
		{"cookie api from preview", env, "POST", "/api/sessions/s1/stop", []reqOpt{withCookie(testToken), csrf, withHeader("Referer", "http://h/preview/s1/~x/index.html")}, 403, ""},
		{"cookie api same-origin", env, "POST", "/api/sessions/s1/stop", []reqOpt{withCookie(testToken), csrf, withHeader("Sec-Fetch-Site", "same-origin")}, 200, ""},
		{"cookie ui from another site", env, "GET", "/", []reqOpt{withCookie(testToken), withHeader("Sec-Fetch-Site", "cross-site")}, 200, ""},
		{"v1 remote denied", env, "GET", "/v1/models", nil, 401, ""},
		{"v1 remote bearer", env, "GET", "/v1/models", []reqOpt{withBearer(testToken)}, 200, ""},
		{"v1 forwarded-for not trusted", env, "GET", "/v1/models", []reqOpt{withHeader("X-Forwarded-For", "127.0.0.1")}, 401, ""},
		{"v1 loopback without LocalNoAuth", noLocal, "GET", "/v1/models", []reqOpt{local("127.0.0.1:4000")}, 401, ""},
		{"api loopback still needs auth", env, "GET", "/api/status", []reqOpt{local("127.0.0.1:4000")}, 401, ""},
		{"healthz public", env, "GET", "/healthz", nil, 200, ""},
		{"login public", env, "GET", "/login", nil, 200, ""},
		{"assets public", env, "GET", "/assets/app.css", nil, 200, ""},
		{"login when signed in", env, "GET", "/login", []reqOpt{withCookie(testToken)}, 302, "/"},
		{"ui redirects to login", env, "GET", "/", nil, 302, "/login"},
		{"login redirect keeps the theme", env, "GET", "/?theme=color", nil, 302, "/login?theme=color"},
		{"login redirect drops a token", env, "GET", "/?theme=color&token=nope", nil, 302, "/login?theme=color"},
		{"preview needs auth", env, "GET", "/preview/s1/", nil, 401, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.env.do(tc.method, tc.target, "", tc.opts...)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
			}
			if tc.location != "" && w.Header().Get("Location") != tc.location {
				t.Fatalf("Location %q, want %q", w.Header().Get("Location"), tc.location)
			}
		})
	}
}

func TestQueryTokenCookie(t *testing.T) {
	env := newEnv(t, nil)
	w := env.do("GET", "/?token="+testToken, "")
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != cookieName || c[0].Value != testToken || !c[0].HttpOnly ||
		c[0].SameSite != http.SameSiteLaxMode || c[0].Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}
	if w := env.do("GET", "/?token=nope", ""); len(w.Result().Cookies()) != 0 {
		t.Fatal("a bad token must not set a cookie")
	}
}

func TestEmptyServerTokenMatchesNothing(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.Server.Token = "" })
	for _, opt := range []reqOpt{withHeader("Authorization", "Bearer "), withCookie("")} {
		if w := env.do("GET", "/api/status", "", opt); w.Code != 401 {
			t.Fatalf("status %d, want 401", w.Code)
		}
	}
}

func TestModelRoutingWithSlashes(t *testing.T) {
	env := newEnv(t, nil)
	auth := withBearer(testToken)
	cases := []struct {
		target, want string
		code         int
	}{
		{"/api/models/openrouter%2Fmeta%2Fllama/load", "load openrouter/meta/llama", 200},
		{"/api/models/openrouter/meta/llama/unload", "unload openrouter/meta/llama", 200},
		{"/api/models/qwen3-coder/load", "load qwen3-coder", 200},
		{"/api/models/missing/load", "load missing", 404},
		{"/api/models/qwen3-coder/explode", "", 404},
		{"/api/models/load", "", 404},
	}
	for _, tc := range cases {
		env.models.last = ""
		w := env.do("POST", tc.target, "", auth)
		if w.Code != tc.code {
			t.Errorf("%s: status %d, want %d (%s)", tc.target, w.Code, tc.code, w.Body)
		}
		if env.models.last != tc.want {
			t.Errorf("%s: called %q, want %q", tc.target, env.models.last, tc.want)
		}
	}
}

func TestStatusShape(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.Guard.MaxSessions = 3 })
	env.sessions.infos = []session.Info{{State: session.StateRunning}, {State: session.StateWaiting}, {State: session.StatePassed}}
	w := env.do("GET", "/api/status", "", withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "host", "uptime", "gpu", "models", "sessions", "guard", "agents", "repos"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status lacks %q", k)
		}
	}
	var st Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.Guard.Running != 2 || st.Guard.MaxSessions != 3 || st.Guard.MemAvailableMiB != 12345 {
		t.Errorf("status = %+v", st)
	}
	if st.Sessions["running"] != 1 || st.Sessions["passed"] != 1 || len(st.GPU.GPUs) != 1 || len(st.Agents) != 1 || len(st.Models) != 1 {
		t.Errorf("status = %+v", st)
	}
	var guard map[string]any
	_ = json.Unmarshal(m["guard"], &guard)
	for _, k := range []string{"mem_available_mib", "max_sessions", "running"} {
		if _, ok := guard[k]; !ok {
			t.Errorf("guard lacks %q", k)
		}
	}
	if string(m["repos"]) != "[]" {
		t.Errorf("repos = %s, want []", m["repos"])
	}
}

func TestRepos(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("beta/.git")
	mk("Alpha/.git")
	mk("plain")
	mk(".hidden/.git")
	mk("Carino-Systems-Business/.git")
	if err := os.WriteFile(filepath.Join(root, "beta", ".cerberus.json"), []byte(`{"checks":["go test ./..."],"preview":{"dir":"."}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "conf")
	env := newEnv(t, func(c *config.Config) {
		c.RepoRoots = []string{root}
		c.Repos = []config.RepoConfig{{Name: "Configured", Path: conf, Checks: []string{"make"}, Push: true},
			{Path: filepath.Join(root, "beta")}}
	})
	w := env.do("GET", "/api/repos", "", withBearer(testToken))
	var got []Repo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []Repo{
		{Name: "Configured", Path: conf, Checks: true, Push: true},
		{Name: "beta", Path: filepath.Join(root, "beta")},
		{Name: "Alpha", Path: filepath.Join(root, "Alpha")},
	}
	if len(got) != len(want) {
		t.Fatalf("repos = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("repo %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if c, p := cerberusFile(filepath.Join(root, "beta")); !c || !p {
		t.Errorf("cerberusFile = %v %v", c, p)
	}
}

func TestPreview(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	files := map[string]string{
		"index.html": "<h1>site</h1>", ".git/config": "[core]", "sub/.env": "KEY=1",
		"sub/a.txt": "hello", ".hidden": "x",
	}
	for p, body := range files {
		full := filepath.Join(dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(outside, []byte("TOP SECRET"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	env := newEnv(t, nil)
	env.sessions.preview = dir
	auth := withBearer(testToken)
	capURL := "/preview/s1/~" + env.srv.previewCap("s1")

	// The authenticated entry redirects to the capability URL...
	w := env.do("GET", "/preview/s1/sub/a.txt?x=1", "", auth)
	if w.Code != 302 || w.Header().Get("Location") != capURL+"/sub/a.txt?x=1" {
		t.Fatalf("entry: %d %q", w.Code, w.Header().Get("Location"))
	}
	// ...which needs no cookie (a sandboxed page sends none), only the right capability.
	if w := env.do("GET", capURL+"/sub/a.txt", ""); w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("capability URL: %d %q", w.Code, w.Body.String())
	}
	if w := env.do("GET", "/preview/s1/~"+env.srv.previewCap("s2")+"/sub/a.txt", ""); w.Code != 401 {
		t.Fatalf("another session's capability: %d", w.Code)
	}
	if w := env.do("GET", "/preview/s1/~bad/sub/a.txt", "", auth); w.Code != 404 {
		t.Fatalf("bad capability with auth: %d", w.Code)
	}
	if w := env.do("GET", capURL, ""); w.Code != 301 {
		t.Fatalf("capability root without slash: %d", w.Code)
	}
	if w := env.do("GET", "/preview/s1/sub/a.txt", ""); w.Code != 401 {
		t.Fatalf("entry without auth: %d", w.Code)
	}
	cases := []struct {
		target   string
		code     int
		contains string
		absent   string
	}{
		{capURL + "/", 200, "<h1>site</h1>", ""},
		{capURL + "/sub/a.txt", 200, "hello", ""},
		{capURL + "/sub/", 200, "a.txt", ".env"},
		{capURL + "/.git/config", 404, "", "[core]"},
		{capURL + "/.git/", 404, "", ""},
		{capURL + "/sub/.env", 404, "", "KEY"},
		{capURL + "/.hidden", 404, "", ""},
		{capURL + "/link.txt", 0, "", "TOP SECRET"},
		{capURL + "/..%2f..%2fetc%2fpasswd", 404, "", "root:"},
		{"/preview/nope/", 404, "", ""},
		{"/preview/s1", 301, "", ""},
	}
	for _, tc := range cases {
		w := env.do("GET", tc.target, "", auth)
		if tc.code != 0 && w.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.target, w.Code, tc.code)
		}
		if tc.code == 0 && w.Code == 200 {
			t.Errorf("%s: escaping symlink served", tc.target)
		}
		body := w.Body.String()
		if tc.contains != "" && !strings.Contains(body, tc.contains) {
			t.Errorf("%s: body %q lacks %q", tc.target, body, tc.contains)
		}
		if tc.absent != "" && strings.Contains(body, tc.absent) {
			t.Errorf("%s: body %q leaks %q", tc.target, body, tc.absent)
		}
	}
	w = env.do("GET", capURL+"/", "")
	if w.Header().Get("Content-Security-Policy") != previewCSP || w.Header().Get("X-Frame-Options") != "SAMEORIGIN" ||
		strings.Contains(previewCSP, "allow-same-origin") || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("preview headers = %v", w.Header())
	}
}

func TestUIHeaders(t *testing.T) {
	env := newEnv(t, nil)
	w := env.do("GET", "/", "", withCookie(testToken))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<title>Cerberus</title>") {
		t.Fatalf("index: %d", w.Code)
	}
	h := w.Header()
	if h.Get("Cache-Control") != "no-cache" || !strings.HasPrefix(h.Get("Content-Security-Policy"), "default-src 'self'") ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers = %v", h)
	}
	a := env.do("GET", "/assets/app.css", "")
	tag := a.Header().Get("ETag")
	if a.Code != 200 || tag == "" {
		t.Fatalf("asset: %d etag %q", a.Code, tag)
	}
	if c := env.do("GET", "/assets/app.css", "", withHeader("If-None-Match", tag)).Code; c != 304 {
		t.Fatalf("revalidation status %d, want 304", c)
	}
	if c := env.do("GET", "/assets/", "").Code; c != 404 {
		t.Fatalf("asset dir listing status %d, want 404", c)
	}
	// A missing asset is the themed 404 page, as for any unknown path.
	if w := env.do("GET", "/assets/nope.js", ""); w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("missing asset: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestSessionActions(t *testing.T) {
	env := newEnv(t, nil)
	auth := withBearer(testToken)
	w := env.do("POST", "/api/sessions/s1/accept", `{"message":"Ship it","push":false}`, auth)
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res["commit"] != "abc123" || env.sessions.lastCall() != "accept:Ship it" {
		t.Fatalf("accept: %d %v", w.Code, res)
	}
	w = env.do("POST", "/api/sessions/s1/accept", `{"message":"Ship it","push":true}`, auth)
	res = nil
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res["commit"] != "abc123" || res["warning"] != "push rejected" {
		t.Fatalf("accept with failed push: %d %v", w.Code, res)
	}
	if w := env.do("POST", "/api/sessions/s1/input", `{"data":"y\r"}`, auth); w.Code != 200 || env.sessions.lastCall() != "input:y\r" {
		t.Fatalf("input: %d %q", w.Code, env.sessions.lastCall())
	}
	if w := env.do("POST", "/api/sessions/s1/stop", "", auth); w.Code != 200 {
		t.Fatalf("stop without body: %d", w.Code)
	}
	if w := env.do("POST", "/api/sessions/zzz/stop", "", auth); w.Code != 404 {
		t.Fatalf("unknown session: %d", w.Code)
	}
	if w := env.do("POST", "/api/sessions/s1/bogus", "", auth); w.Code != 404 {
		t.Fatalf("unknown action: %d", w.Code)
	}
	if w := env.do("POST", "/api/sessions/s1/input", `{bad`, auth); w.Code != 400 {
		t.Fatalf("bad json: %d", w.Code)
	}
	w = env.do("POST", "/api/sessions", `{"Repo":"/r","Task":"fix it"}`, auth)
	if w.Code != 201 || env.sessions.lastCall() != "create:/r:fix it" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	// Only an agent's spawn may name a parent.
	w = env.do("POST", "/api/sessions", `{"Repo":"/r","Task":"fix it","Parent":"s9"}`, auth)
	if w.Code != 201 || env.sessions.lastCall() != "create:/r:fix it" {
		t.Fatalf("create with Parent: %d %s %q", w.Code, w.Body, env.sessions.lastCall())
	}
	w = env.do("GET", "/api/sessions/s1/diff", "", auth)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("diff: %d %v", w.Code, w.Header())
	}
	if w := env.do("GET", "/api/nothing", "", auth); w.Code != 404 {
		t.Fatalf("unknown endpoint: %d", w.Code)
	}
}

func TestWorkspaceEndpoints(t *testing.T) {
	env := newEnv(t, nil)
	auth := withBearer(testToken)
	w := env.do("POST", "/api/sessions/s1/save", `{"dir":"/home/u/out","overwrite":true}`, auth)
	if w.Code != 200 || env.sessions.lastCall() != "save:/home/u/out:true" || !strings.Contains(w.Body.String(), `"files":2`) {
		t.Fatalf("save: %d %s %q", w.Code, w.Body, env.sessions.lastCall())
	}
	if w := env.do("POST", "/api/sessions/s1/save", `{"dir":"/x"}`, withCookie(testToken)); w.Code != 403 {
		t.Fatalf("cookie save without CSRF header: %d", w.Code)
	}
	w = env.do("GET", "/api/sessions/s1/archive", "", auth)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "cerberus-s1.zip") {
		t.Fatalf("archive: %d %v", w.Code, w.Header())
	}
	if zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err != nil || len(zr.File) != 1 {
		t.Fatalf("archive body: %v", err)
	}
	if w := env.do("GET", "/api/sessions/zzz/archive", "", auth); w.Code != 404 || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("archive unknown: %d %v", w.Code, w.Header())
	}
	w = env.do("GET", "/api/fs?path=/home/u&hidden=1", "", auth)
	if w.Code != 200 || env.sessions.lastCall() != "browse:/home/u:true" || !strings.Contains(w.Body.String(), `"git":true`) {
		t.Fatalf("fs: %d %s", w.Code, w.Body)
	}
	if w := env.do("GET", "/api/fs?path=/outside", "", auth); w.Code != 400 {
		t.Fatalf("fs outside: %d", w.Code)
	}
	if w := env.do("GET", "/api/fs", ""); w.Code == 200 {
		t.Fatal("fs without auth")
	}
}

// sseReader reads SSE frames from a live response.
type sseReader struct{ r *bufio.Reader }

// next returns the next frame's event name and data ("" event for comments).
func (s sseReader) next(t *testing.T) (string, string) {
	t.Helper()
	var event, data string
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			return event, data
		case strings.HasPrefix(line, ":"):
			data = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "event: "):
			event = line[7:]
		case strings.HasPrefix(line, "data: "):
			data = line[6:]
		}
	}
}

func openSSE(t *testing.T, env *testEnv, path string) (sseReader, context.CancelFunc) {
	t.Helper()
	ts := httptest.NewServer(env.h)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	return sseReader{bufio.NewReader(resp.Body)}, cancel
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not called", what)
	}
}

func TestSessionStream(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.ping = 20 * time.Millisecond
	env.sessions.backlog = []byte("hello")
	sse, cancel := openSSE(t, env, "/api/sessions/s1/stream")
	if ev, data := sse.next(t); ev != "backlog" || data != "aGVsbG8=" {
		t.Fatalf("first frame %q %q", ev, data)
	}
	env.sessions.out <- []byte("more")
	for {
		ev, data := sse.next(t)
		if ev == "" && data == "ping" {
			continue
		}
		if ev != "out" || data != "bW9yZQ==" {
			t.Fatalf("frame %q %q", ev, data)
		}
		break
	}
	if ev, data := sse.next(t); ev != "" || data != "ping" {
		t.Fatalf("want ping, got %q %q", ev, data)
	}
	cancel()
	waitClosed(t, env.sessions.detached, "detach")
}

func TestSessionStreamEndsWhenOutputCloses(t *testing.T) {
	env := newEnv(t, nil)
	sse, cancel := openSSE(t, env, "/api/sessions/s1/stream")
	defer cancel()
	sse.next(t)
	close(env.sessions.out)
	waitClosed(t, env.sessions.detached, "detach")
}

func TestEventsStream(t *testing.T) {
	env := newEnv(t, nil)
	sse, cancel := openSSE(t, env, "/api/events")
	env.sessions.events <- session.Event{Type: session.EventState, Session: session.Info{ID: "s1", State: "waiting"}}
	ev, data := sse.next(t)
	var got session.Event
	if ev != "session" || json.Unmarshal([]byte(data), &got) != nil || got.Session.ID != "s1" || got.Type != "state" {
		t.Fatalf("frame %q %q", ev, data)
	}
	cancel()
	waitClosed(t, env.sessions.unsubbed, "unsubscribe")
}

func TestStreamUnknownSession(t *testing.T) {
	env := newEnv(t, nil)
	if w := env.do("GET", "/api/sessions/zzz/stream", "", withBearer(testToken)); w.Code != 404 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestNewWithNilDeps(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.Token = testToken
	cfg.RepoRoots = nil
	cfg.Server.AllowedHosts = []string{"example.com"}
	h := New(Deps{Cfg: cfg})
	for target, want := range map[string]int{"/api/status": 200, "/api/sessions": 503, "/api/models": 503, "/v1/models": 503} {
		r := httptest.NewRequest("GET", target, nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", target, w.Code, want)
		}
	}
}

func TestDeniedNames(t *testing.T) {
	got := deniedNames([]string{"**/Carino-Systems-Business/**", "**/.ssh/**", "**/secrets.json", "**/a/b/**", "**/x*/**"})
	if !got["Carino-Systems-Business"] || !got[".ssh"] || len(got) != 2 {
		t.Fatalf("deniedNames = %v", got)
	}
}

// TestSessionAnswerApproval: POST /api/sessions/{id}/answer answers the
// approval still waiting (seq), is audited with what it was for, and a
// stale one is a 409.
func TestSessionAnswerApproval(t *testing.T) {
	env := newEnv(t, nil)
	auth := withBearer(testToken)
	if w := env.do("POST", "/api/sessions/s1/answer", `{"answer":"a","seq":1}`, auth); w.Code != 200 || env.sessions.lastCall() != "answer:1:a" {
		t.Fatalf("answer: %d %q %s", w.Code, env.sessions.lastCall(), w.Body)
	}
	if w := env.do("POST", "/api/sessions/s1/answer", `{"answer":"y","seq":2}`, auth); w.Code != 409 {
		t.Fatalf("stale approval: %d", w.Code)
	}
	if w := env.do("POST", "/api/sessions/zzz/answer", `{"answer":"y","seq":1}`, auth); w.Code != 404 {
		t.Fatalf("unknown session: %d", w.Code)
	}
	// "Every time": the option index, the same seq guard, a bad option 400.
	if w := env.do("POST", "/api/sessions/s1/answer", `{"answer":"save","seq":1,"option":0}`, auth); w.Code != 200 ||
		env.sessions.lastCall() != "save:1:0" || !strings.Contains(w.Body.String(), `"saved"`) {
		t.Fatalf("save: %d %q %s", w.Code, env.sessions.lastCall(), w.Body)
	}
	if w := env.do("POST", "/api/sessions/s1/answer", `{"answer":"save","seq":1,"option":3}`, auth); w.Code != 400 {
		t.Fatalf("bad option: %d %s", w.Code, w.Body)
	}
	if w := env.do("POST", "/api/sessions/s1/answer", `{"answer":"save","seq":2,"option":0}`, auth); w.Code != 409 {
		t.Fatalf("stale save: %d", w.Code)
	}
}
