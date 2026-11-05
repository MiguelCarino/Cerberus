package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/usage"
)

// flowEnv is a safety env (audit log, devices) with a real flow engine
// whose dependencies are the real FlowHost over fakes: a fake gateway
// that answers chat completions and records usage like the real one, a
// temp inbox and usage store, and recorded notifications. No model ever
// runs.
type flowEnv struct {
	*safetyEnv
	eng   *flows.Engine
	ib    *inbox.Store
	use   *usage.Store
	feed  *FlowFeed
	gw    *fakeFlowGateway
	mu    sync.Mutex
	sent  []notify.Message
	push  []notify.Message
	enter chan string // the slow model's requests, as they arrive
}

type fakeFlowGateway struct {
	use   *usage.Store
	enter chan string
	mu    sync.Mutex
	seen  []http.Header
}

func (g *fakeFlowGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string          `json:"model"`
		Messages []flows.Message `json:"messages"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	g.seen = append(g.seen, r.Header.Clone())
	g.mu.Unlock()
	if r.URL.Path != "/chat/completions" {
		http.Error(w, "bad path "+r.URL.Path, 404)
		return
	}
	if body.Model == "slow" {
		g.enter <- r.Header.Get("X-Cerberus-Session")
		<-r.Context().Done()
		w.WriteHeader(499)
		return
	}
	if body.Model == "broken" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"model not loaded","type":"unavailable"}}`)
		return
	}
	model := body.Model
	if model == "default" {
		model = "m-local"
	}
	last := ""
	if n := len(body.Messages); n > 0 {
		last = body.Messages[n-1].Content
	}
	// Like the real gateway: the usage record is added before the reply ends.
	g.use.Add(usage.Record{Time: time.Now(), Model: model, Client: r.Header.Get("X-Cerberus-Client"),
		SessionID: r.Header.Get("X-Cerberus-Session"), PromptTokens: 10, CompletionTokens: 5,
		CostUSD: 0.25, EnergyJ: 7200, Status: 200, Local: true})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   model,
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "<think>hm</think>re: " + last}}},
		"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
	})
}

func newFlowEnv(t *testing.T) *flowEnv {
	t.Helper()
	se := newSafetyEnv(t, nil)
	us, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { us.Close() })
	fe := &flowEnv{safetyEnv: se, ib: newInbox(t), use: us, feed: NewFlowFeed(), enter: make(chan string, 4)}
	fe.gw = &fakeFlowGateway{use: us, enter: fe.enter}
	host := NewFlowHost(FlowHostDeps{
		Config:  func() *config.Config { return se.srv.cfg },
		Gateway: fe.gw, Usage: us, Inbox: fe.ib, Audit: se.audit, Feed: fe.feed,
		Send:     func(m notify.Message) { fe.mu.Lock(); fe.sent = append(fe.sent, m); fe.mu.Unlock() },
		Push:     func(m notify.Message) { fe.mu.Lock(); fe.push = append(fe.push, m); fe.mu.Unlock() },
		MemAvail: func() int { return -1 },
		Models: func(context.Context) []engine.ModelInfo {
			return []engine.ModelInfo{{ID: "m-local", Local: true}, {ID: "slow", Local: true}, {ID: "broken", Local: true}}
		},
	})
	eng, err := flows.Open(host.Options(filepath.Join(t.TempDir(), "flows")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	fe.eng = eng
	se.srv.flows, se.srv.flowFeed = eng, fe.feed
	se.h = se.srv.handler()
	return fe
}

func (e *flowEnv) call(t *testing.T, method, target, body string, want int, out any) {
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

func (e *flowEnv) wait(t *testing.T, run string) flows.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := e.eng.Wait(ctx, run)
	if err != nil {
		t.Fatalf("wait %s: %v", run, err)
	}
	return r
}

const askTwice = `{"name":"Ask twice","description":"two prompts","trigger":{"kind":"manual"},"input_hint":"A question",
 "steps":[{"id":"a","kind":"prompt","name":"First","model":"m-local","template":"Q: {input}"},
          {"id":"b","kind":"prompt","model":"default","template":"R: {steps.a.output}"},
          {"id":"out","kind":"output","to":"inbox","template":"{steps.b.output}"}]}`

func TestFlowsCRUD(t *testing.T) {
	env := newFlowEnv(t)

	var list []flows.Summary
	env.call(t, "GET", "/api/flows", "", 200, &list)
	if list == nil || len(list) != 0 {
		t.Fatalf("empty list = %#v", list)
	}

	var v FlowView
	env.call(t, "POST", "/api/flows", askTwice, 201, &v)
	if v.Flow.ID != "fl-ask-twice" || v.Flow.Version != 1 || v.TriggerText != "Manual" || v.Problems == nil || v.Versions == nil {
		t.Fatalf("created %+v", v)
	}
	id := v.Flow.ID

	// An invalid flow: 400 with its problems, nothing saved.
	w := env.do("POST", "/api/flows", `{"name":"Bad","trigger":{"kind":"manual"},"steps":[{"id":"x","kind":"prompt","model":"nope","template":"{steps.later.output}"}]}`, withBearer(testToken))
	var bad struct {
		Error    string          `json:"error"`
		Problems []flows.Problem `json:"problems"`
	}
	if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &bad) != nil || len(bad.Problems) == 0 || !strings.Contains(bad.Error, "invalid flow") {
		t.Fatalf("bad create: %d %s", w.Code, w.Body)
	}

	// Validate a draft: problems and a preview graph, nothing saved.
	var val struct {
		Problems []flows.Problem `json:"problems"`
		Graph    flows.Graph     `json:"graph"`
	}
	env.call(t, "POST", "/api/flows/validate", `{"name":"D","trigger":{"kind":"schedule","schedule":"every never"},"steps":[]}`, 200, &val)
	if len(val.Problems) == 0 {
		t.Fatalf("validate found nothing: %+v", val)
	}
	// A broken draft still gets a preview graph (no panic on dangling refs).
	env.call(t, "POST", "/api/flows/validate", `{"name":"Odd","steps":[
	  {"id":"f","kind":"fan_out","from":"missing","template":"{item.title}"},
	  {"id":"i","kind":"if","test":"state","on":"ghost","then":"nowhere","else":"end"},
	  {"id":"i","kind":"approve"},{"kind":"weird"}]}`, 200, &val)
	if !flows.HasErrors(val.Problems) {
		t.Fatalf("odd draft: %+v", val.Problems)
	}
	env.call(t, "POST", "/api/flows/validate", `not json`, 400, nil)
	env.call(t, "POST", "/api/flows/validate", askTwice, 200, &val)
	if len(val.Problems) != 0 || len(val.Graph.Nodes) < 4 {
		t.Fatalf("validate clean flow: %+v", val)
	}

	env.call(t, "GET", "/api/flows", "", 200, &list)
	if len(list) != 1 || list[0].ID != id || list[0].TriggerText != "Manual" || list[0].Next != nil {
		t.Fatalf("list %+v", list)
	}

	var got FlowView
	env.call(t, "GET", "/api/flows/"+id, "", 200, &got)
	if got.Flow.Name != "Ask twice" || len(got.Flow.Steps) != 3 {
		t.Fatalf("get %+v", got)
	}
	env.call(t, "GET", "/api/flows/fl-nope", "", 404, nil)
	env.call(t, "GET", "/api/flows/NOT-AN-ID", "", 404, nil)

	// PUT: the version must match.
	f := got.Flow
	f.Name = "Ask twice, renamed"
	f.Trigger = flows.Trigger{Kind: flows.TriggerSchedule, Schedule: "weekdays 08:00"}
	f.Enabled = true
	raw, _ := json.Marshal(f)
	var put FlowView
	env.call(t, "PUT", "/api/flows/"+id, string(raw), 200, &put)
	if put.Flow.Version != 2 || put.Flow.Name != "Ask twice, renamed" || len(put.Versions) != 1 || put.Versions[0] != 1 {
		t.Fatalf("put %+v", put)
	}
	w = env.do("PUT", "/api/flows/"+id, string(raw), withBearer(testToken)) // still version 1
	var stale struct {
		Error string     `json:"error"`
		Flow  flows.Flow `json:"flow"`
	}
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &stale) != nil || stale.Flow.Version != 2 {
		t.Fatalf("stale put: %d %s", w.Code, w.Body)
	}
	f.Version = 0
	raw, _ = json.Marshal(f)
	env.call(t, "PUT", "/api/flows/"+id, string(raw), 400, nil)
	f.Version, f.ID = 2, "fl-other"
	raw, _ = json.Marshal(f)
	env.call(t, "PUT", "/api/flows/"+id, string(raw), 400, nil)

	env.call(t, "GET", "/api/flows", "", 200, &list)
	if len(list) != 1 || list[0].Next == nil || list[0].TriggerText == "Manual" {
		t.Fatalf("scheduled list %+v", list)
	}

	var ok map[string]bool
	env.call(t, "DELETE", "/api/flows/"+id, "", 200, &ok)
	if !ok["ok"] {
		t.Fatalf("delete %v", ok)
	}
	env.call(t, "DELETE", "/api/flows/"+id, "", 404, nil)
	env.call(t, "GET", "/api/flows/"+id, "", 404, nil)
}

func TestFlowsRunThroughGateway(t *testing.T) {
	env := newFlowEnv(t)
	var v FlowView
	env.call(t, "POST", "/api/flows", askTwice, 201, &v)

	var run flows.Run
	env.call(t, "POST", "/api/flows/"+v.Flow.ID+"/run", `{"input":"why?"}`, 202, &run)
	if run.ID == "" || run.Flow != v.Flow.ID || run.Trigger != flows.RunManual || run.Actor != audit.Owner("admin") {
		t.Fatalf("run %+v", run)
	}
	done := env.wait(t, run.ID)
	if done.State != flows.StatePassed {
		t.Fatalf("state %s (%s)", done.State, done.Reason)
	}
	// Usage from the gateway's records: two calls, cost and energy each.
	if done.Tokens != 30 || done.CostUSD != 0.5 || done.EnergyWh != 4 {
		t.Fatalf("usage tokens=%d cost=%v energy=%v", done.Tokens, done.CostUSD, done.EnergyWh)
	}
	env.gw.mu.Lock()
	for _, h := range env.gw.seen {
		if h.Get("X-Cerberus-Client") != flows.ClientName || h.Get("X-Cerberus-Session") != run.ID {
			t.Errorf("gateway headers %v", h)
		}
	}
	env.gw.mu.Unlock()

	var d flows.RunDetail
	env.call(t, "GET", "/api/flows/runs/"+run.ID, "", 200, &d)
	if len(d.Steps) != 3 || d.Steps[0].Output != "re: Q: why?" || d.Steps[1].Model != "m-local" {
		t.Fatalf("steps %+v", d.Steps)
	}
	if d.Steps[0].CostUSD != 0.25 || d.Steps[0].Items == nil {
		t.Fatalf("step usage %+v", d.Steps[0])
	}
	// The output step went to the inbox.
	items := env.ib.List(false, 0)
	if len(items) != 1 || items[0].Kind != "flow" || items[0].Link != "#/flows/runs/"+run.ID || !strings.Contains(items[0].Body, "re: R: re: Q: why?") {
		t.Fatalf("inbox %+v", items)
	}

	// Run list: newest first, no snapshot.
	w := env.do("GET", "/api/flows/runs?flow="+v.Flow.ID+"&limit=5", "", withBearer(testToken))
	if w.Code != 200 || strings.Contains(w.Body.String(), `"snapshot"`) || !strings.Contains(w.Body.String(), run.ID) {
		t.Fatalf("runs: %d %s", w.Code, w.Body)
	}
	env.call(t, "GET", "/api/flows/runs?limit=0", "", 400, nil)
	env.call(t, "GET", "/api/flows/runs?flow=BAD", "", 400, nil)
	var all []json.RawMessage
	env.call(t, "GET", "/api/flows/runs", "", 200, &all)
	if len(all) != 1 {
		t.Fatalf("all runs %d", len(all))
	}

	// The run graph, in the Flow view's shape.
	var g flows.Graph
	env.call(t, "GET", "/api/flows/runs/"+run.ID+"/flow", "", 200, &g)
	ids := map[string]string{}
	for _, n := range g.Nodes {
		ids[n.ID] = n.State
	}
	if ids["st:a"] != "passed" || ids["end"] != "passed" {
		t.Fatalf("graph nodes %+v", g.Nodes)
	}
	if _, ok := ids["m:m-local"]; !ok {
		t.Fatalf("no model node: %+v", g.Nodes)
	}
	env.call(t, "GET", "/api/flows/runs/fr-20200101-000000-zzzzzz", "", 404, nil)
	env.call(t, "GET", "/api/flows/runs/nope/flow", "", 404, nil)

	// The list shows the last run.
	var list []flows.Summary
	env.call(t, "GET", "/api/flows", "", 200, &list)
	if list[0].LastRun == nil || list[0].LastRun.ID != run.ID || list[0].LastRun.State != flows.StatePassed {
		t.Fatalf("last run %+v", list[0].LastRun)
	}

	// Cancelling a finished run is a conflict.
	env.call(t, "POST", "/api/flows/runs/"+run.ID+"/cancel", "", 409, nil)
	// Running an unknown flow is a 404.
	env.call(t, "POST", "/api/flows/fl-nope/run", `{}`, 404, nil)
}

func TestFlowsModelErrorFailsRun(t *testing.T) {
	env := newFlowEnv(t)
	var run flows.Run
	env.call(t, "POST", "/api/flows/test", `{"input":"x","flow":{"name":"Broken","trigger":{"kind":"manual"},
	  "steps":[{"id":"a","kind":"prompt","model":"broken","template":"{input}"}]}}`, 202, &run)
	if !run.Test || run.Trigger != flows.RunTest {
		t.Fatalf("test run %+v", run)
	}
	done := env.wait(t, run.ID)
	var d flows.RunDetail
	env.call(t, "GET", "/api/flows/runs/"+run.ID, "", 200, &d)
	if done.State != flows.StateFailed || len(d.Steps) == 0 ||
		!strings.Contains(done.Reason+" "+d.Steps[0].Error, "model not loaded") {
		t.Fatalf("broken model: %+v steps %+v", done, d.Steps)
	}
	env.call(t, "POST", "/api/flows/test", `{"input":"x"}`, 400, nil)
	env.call(t, "POST", "/api/flows/test", `{"flow":{"name":"Bad","steps":[{"id":"a","kind":"nope"}]}}`, 400, nil)
}

func TestFlowsApproveAndReject(t *testing.T) {
	env := newFlowEnv(t)
	var v FlowView
	env.call(t, "POST", "/api/flows", `{"name":"Gate","trigger":{"kind":"manual"},"steps":[
	  {"id":"a","kind":"prompt","model":"m-local","template":"{input}"},
	  {"id":"ok","kind":"approve","message":"Ship {input}?"},
	  {"id":"tell","kind":"output","to":"notify","template":"shipped {input}"}]}`, 201, &v)

	start := func() flows.Run {
		var run flows.Run
		env.call(t, "POST", "/api/flows/"+v.Flow.ID+"/run", `{"input":"v2"}`, 202, &run)
		r := env.wait(t, run.ID)
		if r.State != flows.StateWaiting || r.Approval == nil || r.Approval.Message != "Ship v2?" || r.Approval.Inbox == "" {
			t.Fatalf("not waiting: %+v", r)
		}
		return r
	}

	r := start()
	// The approval is an inbox item and a desktop push, not a second inbox item.
	it, ok := env.ib.Get(r.Approval.Inbox)
	if !ok || it.Kind != "flow-approve" || it.Link != "#/flows/runs/"+r.ID {
		t.Fatalf("approval inbox item %+v %v", it, ok)
	}
	env.mu.Lock()
	if len(env.push) != 1 || env.push[0].Kind != "flow-approve" || len(env.sent) != 0 {
		t.Fatalf("push %+v sent %+v", env.push, env.sent)
	}
	env.mu.Unlock()

	env.call(t, "POST", "/api/flows/runs/"+r.ID+"/approve", `{"note":"x"}`, 400, nil)
	var after flows.Run
	env.call(t, "POST", "/api/flows/runs/"+r.ID+"/approve", `{"ok":true,"note":"go"}`, 200, &after)
	if done := env.wait(t, r.ID); done.State != flows.StatePassed {
		t.Fatalf("approved run %s (%s)", done.State, done.Reason)
	}
	if it, _ := env.ib.Get(r.Approval.Inbox); !it.Read {
		t.Fatal("the decided approval's inbox item is still unread")
	}
	env.mu.Lock()
	if len(env.sent) != 1 || env.sent[0].Kind != "flow" || env.sent[0].Body != "shipped v2" {
		t.Fatalf("output notify %+v", env.sent)
	}
	env.mu.Unlock()
	env.call(t, "POST", "/api/flows/runs/"+r.ID+"/approve", `{"ok":true}`, 409, nil)

	r = start()
	var rej flows.Run
	env.call(t, "POST", "/api/flows/runs/"+r.ID+"/approve", `{"ok":false,"note":"not yet"}`, 200, &rej)
	if rej.State != flows.StateRejected || !strings.Contains(rej.Reason, "not yet") {
		t.Fatalf("rejected %+v", rej)
	}

	// Cancel while waiting for an approval.
	r = start()
	var c flows.Run
	env.call(t, "POST", "/api/flows/runs/"+r.ID+"/cancel", "", 200, &c)
	if c.State != flows.StateCanceled {
		t.Fatalf("cancel waiting: %+v", c)
	}
	if it, _ := env.ib.Get(r.Approval.Inbox); !it.Read {
		t.Fatal("a canceled run's approval is still unread")
	}

	// Engine events in the audit log, by the HTTP actor.
	events := map[string]int{}
	for _, rec := range env.records(t) {
		if strings.HasPrefix(rec.Route, "flow.") {
			events[rec.Route]++
			if rec.Actor != audit.Owner("admin") {
				t.Errorf("%s by %q", rec.Route, rec.Actor)
			}
		}
	}
	for _, e := range []string{"flow.create", "flow.run.start", "flow.run.approve", "flow.run.reject", "flow.run.cancel"} {
		if events[e] == 0 {
			t.Errorf("no %s audit event (%v)", e, events)
		}
	}
}

func TestFlowsCancelRunning(t *testing.T) {
	env := newFlowEnv(t)
	var run flows.Run
	env.call(t, "POST", "/api/flows/test", `{"flow":{"name":"Slow","trigger":{"kind":"manual"},
	  "steps":[{"id":"a","kind":"prompt","model":"slow","template":"{input}"}]}}`, 202, &run)
	select {
	case got := <-env.enter:
		if got != run.ID {
			t.Fatalf("session header %q, want %q", got, run.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never called")
	}
	env.call(t, "POST", "/api/flows/runs/"+run.ID+"/cancel", "", 200, nil)
	if done := env.wait(t, run.ID); done.State != flows.StateCanceled {
		t.Fatalf("state %s (%s)", done.State, done.Reason)
	}
}

func TestFlowsDisabledAndUnavailable(t *testing.T) {
	env := newFlowEnv(t)
	var v FlowView
	env.call(t, "POST", "/api/flows", askTwice, 201, &v)
	FlowReloader(env.eng).Reload(&config.Config{Flows: config.FlowsConfig{Enabled: false, MaxRuns: 1}})
	w := env.do("POST", "/api/flows/"+v.Flow.ID+"/run", `{}`, withBearer(testToken))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "flows.enabled") {
		t.Fatalf("disabled run: %d %s", w.Code, w.Body)
	}
	FlowReloader(env.eng).Reload(&config.Config{Flows: config.FlowsConfig{Enabled: true, MaxRuns: 2}})
	env.call(t, "POST", "/api/flows/"+v.Flow.ID+"/run", `{}`, 202, nil)
	FlowReloader(nil).Reload(nil) // harmless

	// Without an engine: 503, except the templates.
	plain := newEnv(t, nil)
	for _, rt := range [][2]string{{"GET", "/api/flows"}, {"POST", "/api/flows"}, {"GET", "/api/flows/runs"},
		{"POST", "/api/flows/fl-x/run"}, {"GET", "/api/flows/runs/fr-20200101-000000-abcdef/flow"}} {
		if w := plain.do(rt[0], rt[1], "{}", withBearer(testToken)); w.Code != 503 {
			t.Errorf("%v without engine: %d", rt, w.Code)
		}
	}
	var tpl []flows.Template
	w = plain.do("GET", "/api/flows/templates", "", withBearer(testToken))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tpl) != nil {
		t.Fatalf("templates: %d %s", w.Code, w.Body)
	}
	names := []string{}
	for _, x := range tpl {
		names = append(names, x.Name)
	}
	if strings.Join(names, "|") != "Clarify → Answer|Draft → Critique → Revise|Plan → Delegate → Merge|Website" {
		t.Fatalf("templates %v", names)
	}
	// Every template is a valid flow.
	for _, x := range tpl {
		raw, _ := json.Marshal(x.Flow)
		var val struct {
			Problems []flows.Problem `json:"problems"`
		}
		env.call(t, "POST", "/api/flows/validate", string(raw), 200, &val)
		if flows.HasErrors(val.Problems) {
			t.Errorf("template %s: %+v", x.Key, val.Problems)
		}
	}
}

func TestFlowsAgentScopeRefused(t *testing.T) {
	env := newFlowEnv(t)
	agentTok := env.srv.cfg.SessionAgentToken("s1")
	for _, rt := range [][2]string{{"GET", "/api/flows"}, {"POST", "/api/flows"}, {"GET", "/api/flows/templates"}} {
		w := env.do(rt[0], rt[1], "{}", withBearer(agentTok), withHeader(sessionHeader, "s1"))
		if w.Code != 401 && w.Code != 403 {
			t.Errorf("agent token on %v: %d", rt, w.Code)
		}
	}
}

func TestFlowsEventStream(t *testing.T) {
	env := newFlowEnv(t)
	sse, cancel := openSSE(t, env.testEnv, "/api/events")
	defer cancel()
	env.feed.Publish(flows.Event{Run: "fr-1", Flow: "fl-a", State: "running", Step: "a"})
	ev, data := nextNamed(t, sse)
	var got flows.Event
	if ev != "flowrun" || json.Unmarshal([]byte(data), &got) != nil || got.Run != "fr-1" || got.Step != "a" || got.State != "running" {
		t.Fatalf("frame %q %s", ev, data)
	}
	// A run publishes its own changes.
	var v FlowView
	env.call(t, "POST", "/api/flows", askTwice, 201, &v)
	var run flows.Run
	env.call(t, "POST", "/api/flows/"+v.Flow.ID+"/run", `{"input":"q"}`, 202, &run)
	for {
		ev, data = nextNamed(t, sse)
		if ev != "flowrun" {
			continue
		}
		if json.Unmarshal([]byte(data), &got) != nil || got.Run != run.ID {
			t.Fatalf("frame %s", data)
		}
		if got.State == flows.StatePassed {
			break
		}
	}
}

// TestFlowFeedDropsLaggard: a subscriber that falls behind is closed
// (its event stream then resubscribes and sends a resync).
func TestFlowFeedDropsLaggard(t *testing.T) {
	f := NewFlowFeed()
	ch, cancel := f.Subscribe()
	for range feedBuffer + 1 {
		f.Publish(flows.Event{Run: "r"})
	}
	n := 0
	for range ch {
		n++
	}
	if n != feedBuffer {
		t.Fatalf("got %d before close", n)
	}
	cancel() // after the drop: harmless
	var nilFeed *FlowFeed
	nilFeed.Publish(flows.Event{})
}
