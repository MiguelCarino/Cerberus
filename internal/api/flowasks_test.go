package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/users"
)

// askSessions is a session manager fake for the server and the flow host
// alike: sessions it creates run until set otherwise, every change goes to
// each subscriber in order, and AnswerApproval releases a waiting prompt
// as the real manager does (state back to running; a stale seq is 409).
type askSessions struct {
	*fakeSessions
	mu      sync.Mutex
	infos   map[string]session.Info
	subs    map[chan session.Event]bool
	n       int
	created chan string
}

func newAskSessions() *askSessions {
	return &askSessions{fakeSessions: newFakeSessions(), infos: map[string]session.Info{},
		subs: map[chan session.Event]bool{}, created: make(chan string, 8)}
}

func (a *askSessions) List() []session.Info {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]session.Info, 0, len(a.infos))
	for _, in := range a.infos {
		out = append(out, in)
	}
	return out
}

func (a *askSessions) Get(id string) (session.Info, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, ok := a.infos[id]
	return in, ok
}

func (a *askSessions) Create(ctx context.Context, t session.TaskSpec) ([]session.Info, error) {
	a.mu.Lock()
	a.n++
	in := session.Info{ID: "fs" + string(rune('0'+a.n)), Owner: t.Owner, Name: t.Name, Tags: t.Tags,
		State: session.StateRunning, Updated: time.Now()}
	a.infos[in.ID] = in
	a.mu.Unlock()
	a.created <- in.ID
	return []session.Info{in}, nil
}

func (a *askSessions) Stop(id string) error {
	in, ok := a.Get(id)
	if !ok {
		return session.ErrNotFound
	}
	in.State = session.StateStopped
	a.set(in)
	return nil
}

func (a *askSessions) Subscribe() (<-chan session.Event, func()) {
	ch := make(chan session.Event, 64)
	a.mu.Lock()
	a.subs[ch] = true
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.subs[ch] {
			delete(a.subs, ch)
			close(ch)
		}
	}
}

// set stores a session and sends the change to every subscriber.
func (a *askSessions) set(in session.Info) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in.Updated = time.Now()
	a.infos[in.ID] = in
	for ch := range a.subs {
		ch <- session.Event{Type: "state", Session: in}
	}
}

func (a *askSessions) AnswerApproval(id string, seq int, answer string) (session.Info, error) {
	in, ok := a.Get(id)
	if !ok {
		return session.Info{}, session.ErrNotFound
	}
	if in.State != session.StateWaiting || in.WaitingReason != "approval" || in.AskSeq != seq {
		return session.Info{}, session.ErrNoApproval
	}
	before := in
	in.State, in.WaitingReason, in.WaitingAsk, in.WaitingAlways = session.StateRunning, "", "", nil
	a.set(in)
	return before, nil
}

type askEnv struct {
	*safetyEnv
	eng  *flows.Engine
	ss   *askSessions
	tok  map[string]string
	user map[string]users.User
}

func newAskEnv(t *testing.T) *askEnv {
	t.Helper()
	t.Cleanup(users.SetCostForTest(1000))
	se := newSafetyEnv(t, nil)
	st, err := users.Open(se.srv.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	se.srv.users = st
	e := &askEnv{safetyEnv: se, ss: newAskSessions(), tok: map[string]string{}, user: map[string]users.User{}}
	for _, x := range []struct{ name, role string }{{"root", users.RoleAdmin}, {"bob", users.RoleUser}, {"cy", users.RoleUser}} {
		u, err := st.Add(x.name, "", x.role, "a long password")
		if err != nil {
			t.Fatal(err)
		}
		tok, _, err := st.Logins.Create(u.ID, "192.0.2.10", "test")
		if err != nil {
			t.Fatal(err)
		}
		e.user[x.name], e.tok[x.name] = u, tok
	}
	feed := NewFlowFeed()
	host := NewFlowHost(FlowHostDeps{Config: func() *config.Config { return se.srv.cfg }, Sessions: e.ss,
		Audit: se.audit, Feed: feed, MemAvail: func() int { return -1 }})
	eng, err := flows.Open(host.Options(filepath.Join(t.TempDir(), "flows")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	e.eng = eng
	se.srv.sessions = e.ss
	se.srv.flows, se.srv.flowFeed = eng, feed
	se.h = se.srv.handler()
	return e
}

func (e *askEnv) as(name, method, target, body string) *httptest.ResponseRecorder {
	opts := []reqOpt{withCookie(e.tok[name]), withHeader(csrfHeader, "1")}
	if body != "" {
		opts = append(opts, withHeader("Content-Type", "application/json"))
	}
	return e.do(method, target, body, opts...)
}

// sse opens /api/events as name; reads fail after 10 s instead of hanging.
func (e *askEnv) sse(t *testing.T, name string) sseReader {
	t.Helper()
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: e.tok[name]})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events as %s: %d %q", name, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return sseReader{bufio.NewReader(resp.Body)}
}

// nextFlowrun reads frames until a flowrun event (or a session event for
// stopAt, which then returns ok false).
func nextFlowrun(t *testing.T, r sseReader, stopAt string) (flows.Event, bool) {
	t.Helper()
	for {
		ev, data := r.next(t)
		switch ev {
		case flows.EventType:
			var fe flows.Event
			if err := json.Unmarshal([]byte(data), &fe); err != nil {
				t.Fatalf("flowrun %s: %v", data, err)
			}
			return fe, true
		case "session":
			if stopAt != "" && strings.Contains(data, `"ID":"`+stopAt+`"`) {
				return flows.Event{}, false
			}
		}
	}
}

type runReply struct {
	Run   flows.Run `json:"run"`
	Asks  []FlowAsk `json:"asks"`
	Steps []any     `json:"steps"`
}

func (e *askEnv) run(t *testing.T, name, id string) runReply {
	t.Helper()
	w := e.as(name, "GET", "/api/flows/runs/"+id, "")
	var got runReply
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("run as %s: %d %s", name, w.Code, w.Body)
	}
	return got
}

// TestFlowRunShowsSessionApprovals: a session step's tool prompt (and a
// child's spawn request) shows on the run, in the run list and on the
// graph; answering it through the session route releases it, audited; the
// event stream tells the owner when it starts and stops waiting; another
// user neither sees nor answers it.
func TestFlowRunShowsSessionApprovals(t *testing.T) {
	e := newAskEnv(t)
	bob := e.user["bob"]
	f, probs, err := e.eng.Create("owner:test", flows.Flow{Name: "Site", Owner: bob.ID, Trigger: flows.Trigger{Kind: "manual"},
		Steps: []flows.Step{{ID: "build", Kind: flows.KindSession, Name: "Assemble", Agent: "cerberus",
			Workspace: flows.WorkspaceScratch, Task: "Build {input}"}}})
	if err != nil {
		t.Fatalf("create: %v %+v", err, probs)
	}
	run, err := e.eng.RunFlow("owner:test", f.ID, "a page")
	if err != nil {
		t.Fatal(err)
	}
	var sid string
	select {
	case sid = <-e.ss.created:
	case <-time.After(5 * time.Second):
		t.Fatal("no session started")
	}
	// The run lists the session before anything waits.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if d, _ := e.eng.GetRun(run.ID); len(d.Run.Sessions) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never listed its session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.run(t, "bob", run.ID); len(got.Asks) != 0 {
		t.Fatalf("asks before any wait: %+v", got.Asks)
	}

	bobEv, cyEv := e.sse(t, "bob"), e.sse(t, "cy")
	in, _ := e.ss.Get(sid)
	in.State, in.WaitingReason, in.WaitingAsk, in.AskSeq = session.StateWaiting, "approval", "run_command: npm test", 1
	in.WaitingAlways = []session.AlwaysOption{{Text: "Bash(npm test)", Tool: "Bash", Content: "npm test"}}
	e.ss.set(in)

	// The owner's stream says the run changed.
	fe, ok := nextFlowrun(t, bobEv, "")
	if !ok || fe.Run != run.ID || fe.Step != "build" || fe.State != flows.StateRunning {
		t.Fatalf("bob's event %+v", fe)
	}
	// The run shows the ask, placed on its step.
	got := e.run(t, "bob", run.ID)
	if len(got.Asks) != 1 {
		t.Fatalf("asks %+v", got.Asks)
	}
	a := got.Asks[0]
	if a.Session != sid || a.Kind != "tool" || a.Step != "build" || a.Item != nil || a.Via != "" ||
		a.Ask != "run_command: npm test" || a.Seq != 1 || len(a.Always) != 1 || a.Name != "Site · Assemble" {
		t.Fatalf("ask %+v", a)
	}
	// The run list counts it.
	w := e.as("bob", "GET", "/api/flows/runs", "")
	var list []flowRunItem
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].Asks != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	// The graph marks the step.
	w = e.as("bob", "GET", "/api/flows/runs/"+run.ID+"/flow", "")
	var g flows.Graph
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &g) != nil {
		t.Fatalf("graph: %d %s", w.Code, w.Body)
	}
	marked := false
	for _, n := range g.Nodes {
		if n.ID == "st:build" {
			marked = n.State == flows.StepWaiting && n.Sub == "approve: run_command" &&
				strings.Contains(n.Detail, "npm test") && n.Href == "#/s/"+sid
		}
	}
	if !marked {
		t.Fatalf("graph nodes %+v", g.Nodes)
	}

	// Another user: no run, no list entry, no answer, no event.
	if w := e.as("cy", "GET", "/api/flows/runs/"+run.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("cy reads the run: %d %s", w.Code, w.Body)
	}
	if w := e.as("cy", "GET", "/api/flows/runs", ""); w.Code != 200 || strings.Contains(w.Body.String(), run.ID) {
		t.Fatalf("cy's list: %d %s", w.Code, w.Body)
	}
	if w := e.as("cy", "GET", "/api/flows/runs/"+run.ID+"/flow", ""); w.Code != http.StatusNotFound {
		t.Fatalf("cy reads the graph: %d", w.Code)
	}
	if w := e.as("cy", "POST", "/api/sessions/"+sid+"/answer", `{"answer":"y","seq":1}`); w.Code != http.StatusNotFound {
		t.Fatalf("cy answers: %d %s", w.Code, w.Body)
	}
	// A session of cy's own comes after bob's change on cy's stream: any
	// flowrun event for bob's run would have come first.
	e.ss.mu.Lock()
	e.ss.infos["cy1"] = session.Info{ID: "cy1", Owner: e.user["cy"].ID, State: session.StateRunning}
	e.ss.mu.Unlock()
	e.ss.set(session.Info{ID: "cy1", Owner: e.user["cy"].ID, State: session.StateRunning})
	if fe, ok := nextFlowrun(t, cyEv, "cy1"); ok {
		t.Fatalf("cy got a flowrun event %+v", fe)
	}
	if got := e.run(t, "bob", run.ID); len(got.Asks) != 1 {
		t.Fatalf("the ask went away: %+v", got.Asks)
	}

	// A child the session spawned waits to be let start: listed too.
	e.ss.set(session.Info{ID: "fc1", Owner: bob.ID, Parent: sid, Name: "helper", State: session.StateQueued,
		WaitingReason: session.ReasonNeedsApproval})
	if fe, ok := nextFlowrun(t, bobEv, ""); !ok || fe.Run != run.ID {
		t.Fatalf("spawn event %+v", fe)
	}
	got = e.run(t, "bob", run.ID)
	if len(got.Asks) != 2 || got.Asks[1].Session != "fc1" || got.Asks[1].Kind != "spawn" || got.Asks[1].Via != sid ||
		got.Asks[1].Step != "build" {
		t.Fatalf("asks with the child %+v", got.Asks)
	}
	e.ss.set(session.Info{ID: "fc1", Owner: bob.ID, Parent: sid, Name: "helper", State: session.StateRunning})
	if fe, ok := nextFlowrun(t, bobEv, ""); !ok || fe.Run != run.ID {
		t.Fatalf("child released event %+v", fe)
	}

	// The owner answers through the session route: audited, released.
	if w := e.as("bob", "POST", "/api/sessions/"+sid+"/answer", `{"answer":"y","seq":2}`); w.Code != http.StatusConflict {
		t.Fatalf("stale seq: %d %s", w.Code, w.Body)
	}
	if w := e.as("bob", "POST", "/api/sessions/"+sid+"/answer", `{"answer":"y","seq":1}`); w.Code != 200 {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	if fe, ok := nextFlowrun(t, bobEv, ""); !ok || fe.Run != run.ID {
		t.Fatalf("release event %+v", fe)
	}
	if got := e.run(t, "bob", run.ID); len(got.Asks) != 0 {
		t.Fatalf("asks after the answer: %+v", got.Asks)
	}
	audited := false
	for _, r := range e.records(t) {
		if r.Route == "POST /api/sessions/{id}/{action}" && r.User == "bob" && strings.Contains(r.Detail, "approval yes: run_command: npm test") {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("no audit record of the answer: %+v", e.records(t))
	}

	// The session ends: the run goes on and passes.
	in, _ = e.ss.Get(sid)
	in.State = session.StatePassed
	e.ss.set(in)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if r, err := e.eng.Wait(ctx, run.ID); err != nil || r.State != flows.StatePassed {
		t.Fatalf("run end %+v %v", r, err)
	}
}

// TestAskStepOfFanOutItem: a waiting session of a running fan-out is put
// on its live item by the name the engine gives an item's session.
func TestAskStepOfFanOutItem(t *testing.T) {
	d := flows.RunDetail{
		Run: flows.Run{State: flows.StateRunning, Step: "parts", Sessions: []string{"a", "b"},
			Snapshot: flows.Flow{Name: "Site", Steps: []flows.Step{{ID: "parts", Kind: flows.KindFanOut, Name: "Write parts"}}}},
		Live: []flows.ItemRecord{{Index: 0, Title: "Home", State: "running"}, {Index: 1, Title: "Style", State: "running"}},
	}
	step, item := askStep(d, "b", session.Info{ID: "b", Name: "Site · Write parts · Style"})
	if step != "parts" || item == nil || *item != 1 {
		t.Fatalf("step %q item %v", step, item)
	}
	step, item = askStep(d, "a", session.Info{ID: "a", Name: "something else"})
	if step != "parts" || item != nil {
		t.Fatalf("unknown name: step %q item %v", step, item)
	}
	// A finished record names the session directly.
	d.Steps = []flows.StepRecord{{ID: "parts", Items: []flows.ItemRecord{{Index: 2, Session: "a"}}}}
	if step, item = askStep(d, "a", session.Info{ID: "a"}); step != "parts" || item == nil || *item != 2 {
		t.Fatalf("record: step %q item %v", step, item)
	}
	// markAsks puts it on the item node.
	g := flows.Graph{Nodes: []flows.Node{{ID: "st:parts", State: "running"}, {ID: "it:parts:1", State: "running"}}}
	markAsks(&g, []FlowAsk{{Session: "b", Kind: "tool", Step: "parts", Item: intPtr(1), Ask: "write_file: a.css"}})
	if g.Nodes[0].State != "running" || g.Nodes[1].State != flows.StepWaiting || g.Nodes[1].Sub != "approve: write_file" {
		t.Fatalf("nodes %+v", g.Nodes)
	}
}
