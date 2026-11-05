package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// hostSessions is a fake session manager with a real subscription model:
// each Subscribe gets its own channel; drop closes them all (a lagging
// subscriber, as the real manager does).
type hostSessions struct {
	mu      sync.Mutex
	infos   map[string]session.Info
	subs    map[chan session.Event]struct{}
	created []session.TaskSpec
	stopped []string
	subbed  chan struct{}
	err     error
}

func newHostSessions() *hostSessions {
	return &hostSessions{infos: map[string]session.Info{}, subs: map[chan session.Event]struct{}{},
		subbed: make(chan struct{}, 16)}
}

func (h *hostSessions) Create(ctx context.Context, t session.TaskSpec) ([]session.Info, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return nil, h.err
	}
	h.created = append(h.created, t)
	in := session.Info{ID: "s" + string(rune('0'+len(h.created))), State: session.StateQueued, Tags: t.Tags}
	h.infos[in.ID] = in
	return []session.Info{in}, nil
}

func (h *hostSessions) Get(id string) (session.Info, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	in, ok := h.infos[id]
	return in, ok
}

func (h *hostSessions) List() []session.Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []session.Info
	for _, in := range h.infos {
		out = append(out, in)
	}
	return out
}

func (h *hostSessions) Stop(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = append(h.stopped, id)
	return nil
}

func (h *hostSessions) Subscribe() (<-chan session.Event, func()) {
	ch := make(chan session.Event, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	h.subbed <- struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// set changes a session (without an event when quiet).
func (h *hostSessions) set(in session.Info, quiet bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.infos[in.ID] = in
	if quiet {
		return
	}
	for ch := range h.subs {
		ch <- session.Event{Type: "state", Session: in}
	}
}

func (h *hostSessions) drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

func waitSub(t *testing.T, h *hostSessions) {
	t.Helper()
	select {
	case <-h.subbed:
	case <-time.After(5 * time.Second):
		t.Fatal("no subscription")
	}
}

func TestFlowHostSessions(t *testing.T) {
	hs := newHostSessions()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "site", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.RepoRoots = []string{repo}
	cfg.Repos = []config.RepoConfig{{Name: "named", Path: "/srv/named"}}
	host := NewFlowHost(FlowHostDeps{Config: func() *config.Config { return cfg }, Sessions: hs})
	sr := host.Options(t.TempDir()).Sessions
	if sr == nil {
		t.Fatal("no session runner")
	}
	ctx := context.Background()

	id, err := sr.Start(ctx, flows.SessionRequest{Agent: "cerberus", Workspace: flows.WorkspaceRepo, Repo: "site",
		Task: "build it", Name: strings.Repeat("long name ", 20), Tags: []string{"flow:fl-website"}, Flow: "fl-website"})
	if err != nil || id != "s1" {
		t.Fatalf("start: %q %v", id, err)
	}
	got := hs.created[0]
	if got.Workspace != session.WorkspaceRepo || got.Dir != filepath.Join(repo, "site") || got.Parent != "" ||
		got.Count != 1 || got.Task != "build it" || len([]rune(got.Name)) > 80 {
		t.Fatalf("task spec %+v", got)
	}
	if !reflect.DeepEqual(got.Tags, []string{"flow", "fl-website"}) {
		t.Fatalf("tags %v", got.Tags)
	}
	if _, err := sr.Start(ctx, flows.SessionRequest{Workspace: flows.WorkspaceRepo, Repo: "named"}); err != nil || hs.created[1].Dir != "/srv/named" {
		t.Fatalf("named repo: %v %+v", err, hs.created[1])
	}
	if _, err := sr.Start(ctx, flows.SessionRequest{Workspace: flows.WorkspaceRepo, Repo: "../etc"}); err == nil {
		t.Fatal("a relative path outside the roots was accepted")
	}
	if _, err := sr.Start(ctx, flows.SessionRequest{Workspace: flows.WorkspaceFolder, Folder: "/tmp/x"}); err != nil ||
		hs.created[2].Workspace != session.WorkspaceFolder || hs.created[2].Dir != "/tmp/x" {
		t.Fatalf("folder: %v %+v", err, hs.created[2])
	}
	if _, err := sr.Start(ctx, flows.SessionRequest{}); err != nil || hs.created[3].Workspace != session.WorkspaceScratch {
		t.Fatalf("scratch: %v %+v", err, hs.created[3])
	}
	hs.err = errors.New("guard says no")
	if _, err := sr.Start(ctx, flows.SessionRequest{}); err == nil {
		t.Fatal("create error lost")
	}
	hs.err = nil

	// Wait follows events, survives a dropped subscription, and ignores
	// other sessions and non-final states.
	res := make(chan flows.SessionResult, 1)
	go func() {
		r, err := sr.Wait(ctx, "s1")
		if err != nil {
			t.Error(err)
		}
		res <- r
	}()
	waitSub(t, hs)
	hs.set(session.Info{ID: "s2", State: session.StatePassed}, false)
	hs.set(session.Info{ID: "s1", State: session.StateRunning}, false)
	hs.drop()
	waitSub(t, hs)
	hs.set(session.Info{ID: "s1", State: session.StateChecking}, false)
	hs.set(session.Info{ID: "s1", State: session.StatePassed, Activity: "all checks pass"}, false)
	select {
	case r := <-res:
		if r.State != session.StatePassed || r.Summary != "all checks pass" {
			t.Fatalf("result %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
	}

	// Already final (changed while unsubscribed): answered at once.
	hs.set(session.Info{ID: "s3", State: session.StateError, Error: "agent crashed"}, true)
	if r, err := sr.Wait(ctx, "s3"); err != nil || r.State != session.StateError || r.Summary != "agent crashed" {
		t.Fatalf("final at once: %+v %v", r, err)
	}
	if r, err := sr.Wait(ctx, "nope"); err != nil || r.State != "removed" {
		t.Fatalf("unknown: %+v %v", r, err)
	}
	// Canceled while waiting.
	for len(hs.subbed) > 0 {
		<-hs.subbed
	}
	hs.set(session.Info{ID: "s4", State: session.StateRunning}, true)
	cctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { _, err := sr.Wait(cctx, "s4"); errc <- err }()
	waitSub(t, hs)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
	if err := sr.Stop("s1"); err != nil || hs.stopped[0] != "s1" {
		t.Fatalf("stop %v %v", err, hs.stopped)
	}
}

func TestFlowSessionTags(t *testing.T) {
	cases := []struct {
		in   []string
		flow string
		want []string
	}{
		{[]string{"flow:fl-a"}, "fl-a", []string{"flow", "fl-a"}},
		{nil, "fl-" + strings.Repeat("x", 40), []string{"flow", "fl-" + strings.Repeat("x", 21)}},
		{[]string{"flow:FL-Ünïcode"}, "", []string{"flow", "fl-ncode"}},
		{[]string{"flow:---"}, "", []string{"flow"}},
	}
	for _, c := range cases {
		if got := flowSessionTags(c.in, c.flow); !reflect.DeepEqual(got, c.want) {
			t.Errorf("flowSessionTags(%v, %q) = %v, want %v", c.in, c.flow, got, c.want)
		}
	}
	for _, st := range []string{session.StatePassed, session.StateFailed, session.StateStopped, session.StateError,
		session.StateAccepted, session.StateDiscarded} {
		if !flowSessionFinal(st) {
			t.Errorf("%s not final", st)
		}
	}
	for _, st := range []string{session.StateQueued, session.StateRunning, session.StateWaiting, session.StateRetrying} {
		if flowSessionFinal(st) {
			t.Errorf("%s final", st)
		}
	}
}

func TestFlowHostGuardAndCatalog(t *testing.T) {
	hs := newHostSessions()
	cfg := config.Defaults()
	cfg.Guard.MaxSessions, cfg.Guard.MinFreeMiB = 2, 1000
	cfg.Providers = []config.ProviderConfig{{Name: "openrouter", Enabled: true}, {Name: "off", Enabled: false}}
	cfg.Models = []config.ModelConfig{{ID: "configured"}}
	mem := 5000
	calls := 0
	host := NewFlowHost(FlowHostDeps{Config: func() *config.Config { return cfg }, Sessions: hs,
		MemAvail: func() int { return mem },
		Models: func(context.Context) []engine.ModelInfo {
			calls++
			return []engine.ModelInfo{{ID: "qwen", Local: true}}
		}})
	o := host.Options(t.TempDir())
	if ok, why := o.Guard.CanStart(); !ok {
		t.Fatalf("guard refused: %s", why)
	}
	hs.set(session.Info{ID: "a", State: session.StateRunning}, true)
	hs.set(session.Info{ID: "b", State: session.StateWaiting}, true)
	hs.set(session.Info{ID: "c", State: session.StatePassed}, true)
	if ok, why := o.Guard.CanStart(); ok || !strings.Contains(why, "2 of 2") {
		t.Fatalf("full guard: %v %q", ok, why)
	}
	cfg.Guard.MaxSessions = 0
	mem = 10
	if ok, why := o.Guard.CanStart(); ok || !strings.Contains(why, "low memory") {
		t.Fatalf("memory guard: %v %q", ok, why)
	}
	mem = -1
	if ok, _ := o.Guard.CanStart(); !ok {
		t.Fatal("unknown memory refused")
	}

	for id, want := range map[string][2]bool{
		"qwen": {true, true}, "openrouter/x/y": {true, true}, "off/m": {false, false},
		"configured": {true, false}, "nope": {false, false},
	} {
		k, in := o.Catalog.Model(id)
		if k != want[0] || in != want[1] {
			t.Errorf("Model(%q) = %v %v, want %v", id, k, in, want)
		}
	}
	if calls != 1 {
		t.Errorf("model list fetched %d times, want 1 (cached)", calls)
	}
	if !o.Catalog.Agent("cerberus") || o.Catalog.Agent("no-such-agent") {
		t.Error("agent lookup")
	}
	// No models func: nothing listed, config still answers.
	bare := NewFlowHost(FlowHostDeps{}).Options(t.TempDir())
	if k, _ := bare.Catalog.Model("qwen"); k {
		t.Error("known without a model list")
	}
	if bare.Sessions != nil || bare.Auditor != nil || bare.Events != nil {
		t.Error("nil deps became non-nil interfaces")
	}
	if _, err := bare.Models.Complete(context.Background(), flows.ModelRequest{Model: "m"}); err == nil {
		t.Error("no gateway, no error")
	}
	if err := bare.Notifier.Notify(flows.Notice{}); err == nil {
		t.Error("notify without a notifier")
	}
	if _, err := bare.Notifier.Post(flows.Notice{}); err == nil {
		t.Error("post without an inbox")
	}
}

func TestUsageClaimsMatchByTokens(t *testing.T) {
	u := newUsageClaims()
	u.add(usage.Record{Client: flows.ClientName, SessionID: "r1", CostUSD: 9}) // nobody waiting: dropped
	u.begin("r1")
	u.begin("r1")
	u.add(usage.Record{Client: "other", SessionID: "r1", CostUSD: 7})
	u.add(usage.Record{Client: flows.ClientName, SessionID: "r1", PromptTokens: 1, CompletionTokens: 2, CostUSD: 1})
	u.add(usage.Record{Client: flows.ClientName, SessionID: "r1", PromptTokens: 3, CompletionTokens: 4, CostUSD: 2})
	if r, ok := u.end("r1", 3, 4); !ok || r.CostUSD != 2 {
		t.Fatalf("first claim %+v %v", r, ok)
	}
	if r, ok := u.end("r1", 99, 99); !ok || r.CostUSD != 1 {
		t.Fatalf("second claim (no exact match takes the first) %+v %v", r, ok)
	}
	if len(u.active) != 0 || len(u.recs) != 0 {
		t.Fatalf("left over %v %v", u.active, u.recs)
	}
	u.begin("r2")
	if _, ok := u.end("r2", 0, 0); ok {
		t.Fatal("claimed a record that never came")
	}
}

func TestParseCompletionErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(402)
	_, _ = rec.WriteString(`{"error":"over budget"}`)
	if _, _, _, _, err := parseCompletion(rec); err == nil || !strings.Contains(err.Error(), "over budget") || !strings.Contains(err.Error(), "402") {
		t.Fatalf("string error: %v", err)
	}
	rec = httptest.NewRecorder()
	rec.WriteHeader(500)
	if _, _, _, _, err := parseCompletion(rec); err == nil || !strings.Contains(err.Error(), "Internal Server Error") {
		t.Fatalf("empty error: %v", err)
	}
	rec = httptest.NewRecorder()
	_, _ = rec.WriteString(`{"choices":[]}`)
	if _, _, _, _, err := parseCompletion(rec); err == nil {
		t.Fatal("no choices accepted")
	}
}

// The session runner meters a session's usage records (and nothing else)
// for the flow engine, and stops once stop returns.
func TestFlowSessionMeter(t *testing.T) {
	us, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { us.Close() })
	h := NewFlowHost(FlowHostDeps{Usage: us, Sessions: newHostSessions(), MemAvail: func() int { return -1 }})
	sr, ok := h.Options(t.TempDir()).Sessions.(flows.SessionMeter)
	if !ok {
		t.Fatal("the flow session runner is not a SessionMeter")
	}
	var got []flows.ModelReply
	stop := sr.Meter("s-1", func(r flows.ModelReply) { got = append(got, r) })
	now := time.Now()
	us.Add(usage.Record{Time: now, Model: "cloud/x", Client: "agent", SessionID: "s-1", PromptTokens: 5, CompletionTokens: 2, CostUSD: 0.25, EnergyJ: 3600})
	us.Add(usage.Record{Time: now, Model: "m", Client: "agent", SessionID: "s-2", PromptTokens: 9})
	us.Add(usage.Record{Time: now, Model: "m", Client: flows.ClientName, SessionID: "s-1", PromptTokens: 9})
	stop()
	us.Add(usage.Record{Time: now, Model: "m", Client: "agent", SessionID: "s-1", PromptTokens: 9})
	want := []flows.ModelReply{{Model: "cloud/x", PromptTokens: 5, CompletionTokens: 2, CostUSD: 0.25, EnergyWh: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metered %+v", got)
	}
	if len(h.meters.fns) != 0 {
		t.Fatalf("meter left %v", h.meters.fns)
	}
}
