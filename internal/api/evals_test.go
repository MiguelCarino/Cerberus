package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/evals"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// fakeEvalSessions is a session manager whose sessions end (passed) as
// soon as they are created, each with a scratch folder holding main.go.
type fakeEvalSessions struct {
	mu    sync.Mutex
	root  string
	infos map[string]session.Info
	specs []session.TaskSpec
	n     int
}

func (f *fakeEvalSessions) Create(ctx context.Context, t session.TaskSpec) ([]session.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := "es" + string(rune('0'+f.n))
	dir := filepath.Join(f.root, id)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	now := time.Now()
	in := session.Info{ID: id, State: session.StatePassed, Workspace: t.Workspace, Worktree: dir, Agent: t.Agent, Model: t.Model,
		Tags: t.Tags, Owner: t.Owner, SetupHash: "abcdef123456", RunStarted: now.Add(-3 * time.Second), RunEnded: now,
		ChecksOutput: "PASS", EnergyJ: 3600}
	f.infos[id] = in
	f.specs = append(f.specs, t)
	return []session.Info{in}, nil
}

func (f *fakeEvalSessions) Get(id string) (session.Info, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.infos[id]
	return in, ok
}

func (f *fakeEvalSessions) List() []session.Info { return nil }
func (f *fakeEvalSessions) Stop(id string) error { return nil }
func (f *fakeEvalSessions) Subscribe() (<-chan session.Event, func()) {
	return make(chan session.Event), func() {}
}
func (f *fakeEvalSessions) Transcript(id string) ([]byte, error) {
	return []byte("done KEY=sk-secret-1\n"), nil
}
func (f *fakeEvalSessions) Diff(id string) (string, error) { return "", nil }
func (f *fakeEvalSessions) MaskSecrets(s []string) []string {
	for i := range s {
		s[i] = strings.ReplaceAll(s[i], "sk-secret-1", "••••")
	}
	return s
}
func (f *fakeEvalSessions) Agents() []session.AgentStatus {
	return []session.AgentStatus{{Name: "cerberus", Available: true}}
}

// fakeJudgeGateway answers judge calls with a verdict (fail for the
// criterion "nope", pass otherwise) and records usage like the gateway.
type fakeJudgeGateway struct {
	use  *usage.Store
	mu   sync.Mutex
	seen []map[string]any
	hdr  []http.Header
}

var judgeCritRe = regexp.MustCompile(`Criterion ([a-z0-9_]+):`)

func (g *fakeJudgeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	g.seen = append(g.seen, body)
	g.hdr = append(g.hdr, r.Header.Clone())
	g.mu.Unlock()
	verdict := "pass"
	if m := judgeCritRe.FindStringSubmatch(string(raw)); m != nil && m[1] == "nope" {
		verdict = "fail"
	}
	g.use.Add(usage.Record{Time: time.Now(), Model: "m-local", Client: r.Header.Get("X-Cerberus-Client"),
		SessionID: r.Header.Get("X-Cerberus-Session"), PromptTokens: 30, CompletionTokens: 10, EnergyJ: 360, Status: 200, Local: true})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"model": "m-local",
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
			"content": `{"verdict":"` + verdict + `","rationale":"looked","evidence":["main.go:2"]}`}}},
		"usage": map[string]int{"prompt_tokens": 30, "completion_tokens": 10}})
}

type evalEnv struct {
	*safetyEnv
	eng  *evals.Engine
	gw   *fakeJudgeGateway
	sess *fakeEvalSessions
	use  *usage.Store
	feed *EvalFeed
	host *EvalHost
}

func newEvalEnv(t *testing.T) *evalEnv {
	t.Helper()
	se := newSafetyEnv(t, nil)
	us, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { us.Close() })
	ee := &evalEnv{safetyEnv: se, use: us, feed: NewEvalFeed(), sess: &fakeEvalSessions{root: t.TempDir(), infos: map[string]session.Info{}}}
	ee.gw = &fakeJudgeGateway{use: us}
	host := NewEvalHost(EvalHostDeps{Config: func() *config.Config { return se.srv.cfg }, Gateway: ee.gw, Usage: us,
		Sessions: ee.sess, Audit: se.audit, Feed: ee.feed,
		Models: func(context.Context) []engine.ModelInfo { return []engine.ModelInfo{{ID: "m-local", Local: true}} }})
	eng, err := evals.Open(host.Options(filepath.Join(t.TempDir(), "evals")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	ee.eng, ee.host = eng, host
	se.srv.evals, se.srv.evalFeed = eng, ee.feed
	se.h = se.srv.handler()
	return ee
}

func (e *evalEnv) call(t *testing.T, method, target, body string, want int, out any) {
	t.Helper()
	w := e.do(method, target, body, withBearer(testToken))
	if w.Code != want {
		t.Fatalf("%s %s: %d %s (want %d)", method, target, w.Code, w.Body, want)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, target, err, w.Body)
		}
	}
}

const healthEval = `{"name":"Health endpoint","task":"Add GET /health","workspace":"scratch",
 "gates":[{"id":"build","max_iterations":1},{"id":"test","max_iterations":1}],
 "criteria":[{"id":"has_main","prompt":"main.go has a main function","gate":"build"},
             {"id":"nope","prompt":"There is a Dockerfile","gate":"build","depends_on":["has_main"]},
             {"id":"tests_pass","prompt":"The tests pass","gate":"test","evidence":["checks"]}],
 "setups":[{"label":"local","agent":"cerberus","model":"m-local"}],
 "judge_model":"m-local"}`

func TestEvalsAPI(t *testing.T) {
	env := newEvalEnv(t)
	evCh, stop := env.feed.Subscribe()
	defer stop()

	var list []EvalSummary
	env.call(t, "GET", "/api/evals", "", 200, &list)
	if list == nil || len(list) != 0 {
		t.Fatalf("empty list %#v", list)
	}
	var v EvalView
	env.call(t, "POST", "/api/evals", healthEval, 201, &v)
	if v.Eval.ID != "ev-health-endpoint" || v.Eval.Version != 1 || v.JudgeModel != "m-local" || len(v.Gates) != 2 || v.Problems == nil {
		t.Fatalf("created %+v", v)
	}
	id := v.Eval.ID

	// A draft with a cycle and an unknown model: problems, nothing saved.
	var val struct {
		Problems []evals.Problem `json:"problems"`
		Order    [][]string      `json:"order"`
	}
	env.call(t, "POST", "/api/evals/validate", `{"name":"x","task":"t","criteria":[{"id":"a","prompt":"p","depends_on":["b"]},
	  {"id":"b","prompt":"q","depends_on":["a"]}],"setups":[{"agent":"cerberus","model":"ghost"}]}`, 200, &val)
	if !evals.HasErrors(val.Problems) || len(val.Order) != 0 {
		t.Fatalf("validate %+v", val)
	}
	env.call(t, "POST", "/api/evals/validate", healthEval, 200, &val)
	if len(val.Problems) != 0 || len(val.Order) != 2 || strings.Join(val.Order[0], ",") != "has_main,nope" {
		t.Fatalf("validate clean %+v", val)
	}
	env.call(t, "POST", "/api/evals", `{"name":"bad","task":"t","criteria":[],"setups":[]}`, 400, nil)

	// Optimistic versions.
	v.Eval.Name = "Health endpoint v2"
	b, _ := json.Marshal(v.Eval)
	env.call(t, "PUT", "/api/evals/"+id, string(b), 200, &v)
	if v.Eval.Version != 2 {
		t.Fatalf("version %d", v.Eval.Version)
	}
	w := env.do("PUT", "/api/evals/"+id, string(b), withBearer(testToken))
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"eval"`) {
		t.Fatalf("stale put: %d %s", w.Code, w.Body)
	}
	env.call(t, "GET", "/api/evals/ev-nope", "", 404, nil)
	env.call(t, "GET", "/api/evals/..%2Fx", "", 404, nil)

	// A run: 202, then judged.
	var run evals.Run
	env.call(t, "POST", "/api/evals/"+id+"/run", `{}`, 202, &run)
	if !evals.ValidRunID(run.ID) || len(run.Slots) != 1 {
		t.Fatalf("run %+v", run)
	}
	var d evals.RunDetail
	deadline := time.Now().Add(10 * time.Second)
	for {
		env.call(t, "GET", "/api/evals/runs/"+run.ID, "", 200, &d)
		if evals.Final(d.Run.State) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d.Run.State != evals.RunDone || len(d.Reports) != 1 {
		t.Fatalf("run detail %+v", d)
	}
	rep := d.Reports[0]
	got := map[string]string{}
	for _, x := range rep.Results {
		got[x.Criterion] = x.State
	}
	if got["has_main"] != "pass" || got["nope"] != "fail" || got["tests_pass"] != "skipped" || rep.FirstFailingGate != "build" {
		t.Fatalf("report %+v", rep)
	}
	if rep.SetupHash != "abcdef123456" || rep.WallMs < 2900 || rep.JudgeUsage.Tokens != 80 || rep.SessionUsage.EnergyWh != 1 {
		t.Fatalf("report facts %+v", rep)
	}
	if rep.JudgeUsage.EnergyWh != 0.2 {
		t.Fatalf("judge energy from the usage record: %+v", rep.JudgeUsage)
	}
	if d.Run.Slots[0].Live != session.StatePassed || len(d.Summary) != 1 {
		t.Fatalf("live %+v summary %+v", d.Run.Slots, d.Summary)
	}
	// The session: tagged eval + the eval id, scratch, the setup's agent and model.
	env.sess.mu.Lock()
	spec := env.sess.specs[0]
	env.sess.mu.Unlock()
	if !slices.Equal(spec.Tags, []string{"eval", "ev-health-endpoint"}) || spec.Workspace != session.WorkspaceScratch ||
		spec.Agent != "cerberus" || spec.Model != "m-local" || spec.Task != "Add GET /health" {
		t.Fatalf("spec %+v", spec)
	}
	// The judge call: client cerberus-eval, the run as session, temperature
	// 0, 600 tokens, thinking off for a local model, the secret masked.
	env.gw.mu.Lock()
	body, hdr := env.gw.seen[0], env.gw.hdr[0]
	env.gw.mu.Unlock()
	if hdr.Get("X-Cerberus-Client") != evals.ClientName || hdr.Get("X-Cerberus-Session") != run.ID {
		t.Fatalf("headers %v", hdr)
	}
	if body["temperature"] != float64(0) || body["max_tokens"] != float64(600) || body["reasoning_effort"] != "none" {
		t.Fatalf("body %v", body)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "sk-secret-1") {
		t.Fatal("a secret reached the judge")
	}

	// Lists, cancel of a finished run, rejudge.
	var runs []map[string]any
	env.call(t, "GET", "/api/evals/runs?eval="+id, "", 200, &runs)
	if len(runs) != 1 || runs[0]["snapshot"] != nil {
		t.Fatalf("runs %+v", runs)
	}
	env.call(t, "GET", "/api/evals/runs?eval=bad/../x", "", 400, nil)
	env.call(t, "GET", "/api/evals", "", 200, &list)
	if len(list) != 1 || list[0].LastRun == nil || list[0].LastRun.Judged != 1 {
		t.Fatalf("list %+v", list)
	}
	env.call(t, "POST", "/api/evals/runs/"+run.ID+"/cancel", "", 409, nil)
	env.call(t, "POST", "/api/evals/runs/"+run.ID+"/rejudge", "", 202, nil)
	env.call(t, "GET", "/api/evals/runs/er-20261105-100000-abcdef", "", 404, nil)

	// Events reached the feed.
	select {
	case ev := <-evCh:
		if ev.Run != run.ID || ev.Eval != id {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no evalrun event")
	}

	// The audit log names the engine's events too.
	var names []string
	for _, rec := range env.records(t) {
		names = append(names, rec.Route)
	}
	for _, want := range []string{"eval.create", "eval.update", "eval.run.start", "eval.run.rejudge"} {
		if !slices.Contains(names, want) {
			t.Errorf("no %s in the audit log", want)
		}
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if r, _ := env.eng.GetRun(run.ID); evals.Final(r.Run.State) {
			break
		}
	}
	env.call(t, "DELETE", "/api/evals/"+id, "", 200, nil)
	env.call(t, "GET", "/api/evals/"+id, "", 404, nil)
}

func TestEvalsUnavailable(t *testing.T) {
	env := newSafetyEnv(t, nil)
	w := env.do("GET", "/api/evals", "", withBearer(testToken))
	if w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
}

func TestEvalSessionTags(t *testing.T) {
	if got := evalSessionTags("ev-a-very-long-eval-name-that-goes-on"); len(got[1]) > 24 || got[0] != "eval" {
		t.Fatalf("%v", got)
	}
}

// TestEvalsOwnerScope: a user sees and runs only their own evals and runs;
// an admin's are 404 to them.
func TestEvalsOwnerScope(t *testing.T) {
	env := newEvalEnv(t)
	creds := withRoles(t, env.safetyEnv)
	env.host.d.Users = env.srv.users // as the daemon wires it (before any run reads it)
	adm, usr := creds["admin"], creds["user"]
	csrf := withHeader(csrfHeader, "1")
	w := env.do("POST", "/api/evals", healthEval, withCookie(adm.cookie), csrf)
	if w.Code != 201 {
		t.Fatalf("admin create %d %s", w.Code, w.Body)
	}
	var v EvalView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if w := env.do("GET", "/api/evals/"+v.Eval.ID, "", withCookie(usr.cookie)); w.Code != 404 {
		t.Fatalf("user reads the admin's eval: %d", w.Code)
	}
	if w := env.do("POST", "/api/evals/"+v.Eval.ID+"/run", "{}", withCookie(usr.cookie), csrf); w.Code != 404 {
		t.Fatalf("user runs the admin's eval: %d", w.Code)
	}
	var list []EvalSummary
	w = env.do("GET", "/api/evals", "", withCookie(usr.cookie))
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("user lists %+v", list)
	}
	if w := env.do("GET", "/api/evals", "", withCookie(creds["viewer"].cookie)); w.Code != 403 {
		t.Fatalf("viewer %d", w.Code)
	}
	w = env.do("POST", "/api/evals/"+v.Eval.ID+"/run", "{}", withCookie(adm.cookie), csrf)
	var run evals.Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	if w.Code != 202 || run.Owner != adm.user.ID {
		t.Fatalf("admin run %d %s", w.Code, w.Body)
	}
	if w := env.do("GET", "/api/evals/runs/"+run.ID, "", withCookie(usr.cookie)); w.Code != 404 {
		t.Fatalf("user reads the admin's run: %d", w.Code)
	}
	var owner string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		env.sess.mu.Lock()
		if len(env.sess.specs) > 0 {
			owner = env.sess.specs[0].Owner
		}
		env.sess.mu.Unlock()
		if owner != "" {
			break
		}
	}
	if owner != adm.user.ID {
		t.Fatalf("session owner %q", owner)
	}

	// A user's repo eval naming a repo not shared with them is refused
	// before git looks at it: no path, no head commit comes back.
	repoDir := t.TempDir()
	repoEval := strings.Replace(healthEval, `"workspace":"scratch"`, `"workspace":"repo","repo":`+strconv.Quote(repoDir), 1)
	w = env.do("POST", "/api/evals", repoEval, withCookie(usr.cookie), csrf)
	if w.Code != 201 {
		t.Fatalf("user create %d %s", w.Code, w.Body)
	}
	var uv EvalView
	_ = json.Unmarshal(w.Body.Bytes(), &uv)
	w = env.do("POST", "/api/evals/"+uv.Eval.ID+"/run", "{}", withCookie(usr.cookie), csrf)
	if w.Code != 403 || strings.Contains(w.Body.String(), repoDir) || strings.Contains(w.Body.String(), "commit") {
		t.Fatalf("user probes an unshared repo: %d %s", w.Code, w.Body)
	}
}
