package api

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

type recReloader struct {
	mu   sync.Mutex
	cfgs []*config.Config
}

func (r *recReloader) Reload(c *config.Config) {
	r.mu.Lock()
	r.cfgs = append(r.cfgs, c)
	r.mu.Unlock()
}

func (r *recReloader) last() *config.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cfgs) == 0 {
		return nil
	}
	return r.cfgs[len(r.cfgs)-1]
}

func newConfigEnv(t *testing.T) (*safetyEnv, *recReloader) {
	t.Helper()
	e := newSafetyEnv(t, nil)
	rl := &recReloader{}
	e.srv.reloaders = []Reloader{rl}
	if err := e.srv.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	e.srv.noteDisk() // as serve does: the daemon starts from the file
	return e, rl
}

func diskConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConfigGetAndSchema(t *testing.T) {
	e, _ := newConfigEnv(t)
	w := e.do("GET", "/api/config", "", withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("GET %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("GET /api/config leaks the token")
	}
	var v ConfigView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.Config == nil || v.RestartPending || v.Problems == nil {
		t.Fatalf("view %+v err %v", v, err)
	}
	w = e.do("GET", "/api/config/schema", "", withBearer(testToken))
	var sc ConfigSchema
	if err := json.Unmarshal(w.Body.Bytes(), &sc); err != nil || len(sc.Fields) < 50 || sc.Groups[0] != config.GroupServer {
		t.Fatalf("schema %d fields, err %v", len(sc.Fields), err)
	}
	if w := e.do("GET", "/api/config", ""); w.Code != 401 {
		t.Fatalf("unauthenticated GET %d", w.Code)
	}
}

func TestConfigPatchAtomic(t *testing.T) {
	e, rl := newConfigEnv(t)
	before, _ := os.ReadFile(e.cfgPath)

	// One bad value refuses the whole change.
	w := e.do("PATCH", "/api/config", `{"set":{"engines.idle_ttl_sec":42,"guard.max_sessions":0}}`, withBearer(testToken))
	if w.Code != 422 {
		t.Fatalf("invalid PATCH %d %s", w.Code, w.Body)
	}
	var bad struct{ Problems []config.Problem }
	_ = json.Unmarshal(w.Body.Bytes(), &bad)
	if len(bad.Problems) == 0 || bad.Problems[0].Path != "guard.max_sessions" {
		t.Fatalf("problems %+v", bad.Problems)
	}
	if after, _ := os.ReadFile(e.cfgPath); string(after) != string(before) {
		t.Fatal("refused PATCH changed the file")
	}
	if e.srv.conf().Engines.IdleTTLSec != 600 || rl.last() != nil {
		t.Fatal("refused PATCH changed the live config")
	}
	// Unknown path and wrong type: 400, nothing changed.
	for _, body := range []string{`{"set":{"nope.x":1}}`, `{"set":{"engines.idle_ttl_sec":"soon"}}`, `{"set":{"server.token":"x"}}`, `{}`} {
		if w := e.do("PATCH", "/api/config", body, withBearer(testToken)); w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}

	// A valid change: saved, applied live, reported per path.
	w = e.do("PATCH", "/api/config", `{"set":{"engines.idle_ttl_sec":"2m","guard.max_sessions":5,"server.listen":"127.0.0.1:7381"}}`, withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("PATCH %d %s", w.Code, w.Body)
	}
	var v ConfigView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	live := map[string]bool{}
	for _, c := range v.Changed {
		live[c.Path] = c.Live
	}
	if len(v.Changed) != 3 || !live["engines.idle_ttl_sec"] || !live["guard.max_sessions"] || live["server.listen"] {
		t.Fatalf("changed %+v", v.Changed)
	}
	if !v.RestartPending || len(v.Pending) != 1 || v.Pending[0] != "server.listen" {
		t.Fatalf("pending %v %v", v.RestartPending, v.Pending)
	}
	got := rl.last()
	if got == nil || got.Engines.IdleTTLSec != 120 || got.Guard.MaxSessions != 5 {
		t.Fatalf("reloader got %+v", got)
	}
	disk := diskConfig(t, e.cfgPath)
	if disk["engines"].(map[string]any)["idle_ttl_sec"].(float64) != 120 {
		t.Fatal("not saved")
	}
	if disk["server"].(map[string]any)["token"] != testToken {
		t.Fatal("save lost the token")
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("PATCH reply leaks the token")
	}
	// GET agrees, and changing the restart setting back clears pending.
	w = e.do("PATCH", "/api/config", `{"set":{"server.listen":"127.0.0.1:7340"}}`, withBearer(testToken))
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.RestartPending {
		t.Fatalf("pending after revert: %v", v.Pending)
	}
	// The audit record names the paths, never values.
	recs := e.records(t)
	found := false
	for _, r := range recs {
		if r.Route == "PATCH /api/config" && strings.Contains(r.Detail, "engines.idle_ttl_sec (live)") &&
			strings.Contains(r.Detail, "server.listen (restart)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit record for the change: %+v", recs)
	}
}

func TestConfigPatchKeepsRotatedToken(t *testing.T) {
	e, _ := newConfigEnv(t)
	if w := e.do("PATCH", "/api/config", `{"set":{"guard.max_sessions":4}}`, withBearer(testToken)); w.Code != 200 {
		t.Fatalf("PATCH %d", w.Code)
	}
	tok, err := e.srv.rotateToken()
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("PATCH", "/api/config", `{"set":{"guard.max_sessions":6}}`, withBearer(tok)); w.Code != 200 {
		t.Fatalf("PATCH after rotation %d %s", w.Code, w.Body)
	}
	disk := diskConfig(t, e.cfgPath)
	if disk["server"].(map[string]any)["token"] != tok {
		t.Fatal("a settings save wrote the old token back")
	}
	if e.srv.view().TokenSnapshot() != tok || e.srv.conf().TokenSnapshot() != tok {
		t.Fatal("live snapshot kept the old token")
	}
	var v ConfigView
	w := e.do("GET", "/api/config", "", withBearer(tok))
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.RestartPending {
		t.Fatalf("rotation must not mark a restart: %v", v.Pending)
	}
}

func TestConfigNoFile(t *testing.T) {
	env := newEnv(t, nil) // no path
	if w := env.do("PATCH", "/api/config", `{"set":{"guard.max_sessions":4}}`, withBearer(testToken)); w.Code != 409 {
		t.Fatalf("PATCH without a file %d", w.Code)
	}
}

func TestConfigInitDone(t *testing.T) {
	e, rl := newConfigEnv(t)
	e.srv.cfg.Server.InitDone = false
	e.srv.boot.Server.InitDone = false
	if w := e.do("POST", "/api/config/init-done", "", withBearer(testToken)); w.Code != 200 {
		t.Fatalf("init-done %d %s", w.Code, w.Body)
	}
	if !e.srv.conf().Server.InitDone || rl.last() == nil {
		t.Fatal("init_done not applied")
	}
	if disk := diskConfig(t, e.cfgPath); disk["server"].(map[string]any)["init_done"] != true {
		t.Fatal("init_done not saved")
	}
	var v ConfigView
	_ = json.Unmarshal(e.do("GET", "/api/config", "", withBearer(testToken)).Body.Bytes(), &v)
	if v.RestartPending {
		t.Fatalf("init_done must apply live: %v", v.Pending)
	}
	if w := e.do("POST", "/api/config/init-done", "", withBearer(testToken)); w.Code != 200 {
		t.Fatalf("second init-done %d", w.Code)
	}
}

func TestConfigRestart(t *testing.T) {
	e, _ := newConfigEnv(t)
	if w := e.do("POST", "/api/config/restart", "", withBearer(testToken)); w.Code != 503 {
		t.Fatalf("restart without a hook %d", w.Code)
	}
	old := restartDelay
	restartDelay = time.Millisecond
	t.Cleanup(func() { restartDelay = old })
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	e.srv.restart = func() { calls.Add(1); done <- struct{}{} }
	w := e.do("POST", "/api/config/restart", "", withBearer(testToken))
	if w.Code != 202 {
		t.Fatalf("restart %d %s", w.Code, w.Body)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("restart hook not called")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	var v ConfigView
	_ = json.Unmarshal(e.do("GET", "/api/config", "", withBearer(testToken)).Body.Bytes(), &v)
	if !v.Restartable {
		t.Fatal("restartable false with a hook")
	}
}

// A file changed outside the daemon (a hand edit, or the CLI writing it
// while it could not reach the daemon) is kept by the next PATCH, and
// applied with it; a file edited into invalid JSON refuses the PATCH.
func TestConfigPatchKeepsDiskEdit(t *testing.T) {
	e, rl := newConfigEnv(t)
	c, err := config.Load(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c.Budgets = []config.Budget{{Provider: "*", Period: "month", SoftUSD: 7}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if w := e.do("PATCH", "/api/config", `{"set":{"guard.max_sessions":4}}`, withBearer(testToken)); w.Code != 200 {
		t.Fatalf("PATCH %d %s", w.Code, w.Body)
	}
	disk, err := config.Load(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Guard.MaxSessions != 4 || len(disk.Budgets) != 1 || disk.Budgets[0].SoftUSD != 7 {
		t.Fatalf("disk after PATCH: guard %+v budgets %+v", disk.Guard, disk.Budgets)
	}
	if live := e.srv.conf(); len(live.Budgets) != 1 || rl.last() == nil || len(rl.last().Budgets) != 1 {
		t.Fatal("the outside edit was not applied live")
	}
	if disk.TokenSnapshot() != testToken {
		t.Fatal("token lost")
	}
	// A second PATCH with no outside edit takes the live config as is.
	if w := e.do("PATCH", "/api/config", `{"set":{"guard.max_sessions":5}}`, withBearer(testToken)); w.Code != 200 {
		t.Fatalf("PATCH %d %s", w.Code, w.Body)
	}
	if err := os.WriteFile(e.cfgPath, []byte(`{"server":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := e.do("PATCH", "/api/config", `{"set":{"guard.max_sessions":6}}`, withBearer(testToken)); w.Code != 409 {
		t.Fatalf("PATCH over invalid JSON %d %s", w.Code, w.Body)
	}
	if raw, _ := os.ReadFile(e.cfgPath); string(raw) != `{"server":` {
		t.Fatal("the hand edit was overwritten")
	}
}

// A public listen address without allow_public is refused (the daemon
// would not start on it after a restart).
func TestConfigPatchRefusesPublicListen(t *testing.T) {
	e, _ := newConfigEnv(t)
	for _, body := range []string{`{"set":{"server.listen":"8.8.8.8:7340"}}`, `{"set":{"server.extra_listen":["203.0.113.5:7340"]}}`} {
		if w := e.do("PATCH", "/api/config", body, withBearer(testToken)); w.Code != 422 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
}

// A restart the saved config would not survive is refused: the daemon
// keeps running.
func TestConfigRestartRefusedByCheck(t *testing.T) {
	e, _ := newConfigEnv(t)
	called := false
	e.srv.restart = func() { called = true }
	e.srv.restartOK = func() error { return errors.New("cannot listen on 10.255.255.1:7340") }
	w := e.do("POST", "/api/config/restart", "", withBearer(testToken))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "10.255.255.1") {
		t.Fatalf("restart %d %s", w.Code, w.Body)
	}
	time.Sleep(2 * restartDelay)
	if called {
		t.Fatal("restart ran after a failed check")
	}
}

// TestLivePathsClassified keeps the live list honest: every live prefix
// names a real setting that the schema does not flag as needing a restart.
func TestLivePathsClassified(t *testing.T) {
	restart := map[string]bool{}
	paths := map[string]bool{}
	for _, f := range config.Schema() {
		paths[f.Path] = true
		restart[f.Path] = f.Restart
	}
	for _, pre := range livePrefixes {
		matched := false
		for p := range paths {
			if p == pre || (strings.HasSuffix(pre, ".") && strings.HasPrefix(p, pre)) {
				matched = true
				if restart[p] {
					t.Errorf("%s is live here but the schema says it needs a restart", p)
				}
			}
		}
		if !matched {
			t.Errorf("live prefix %q matches no setting", pre)
		}
	}
}
