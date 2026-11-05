package flows

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// --- clock ---

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	made   chan time.Duration // every NewTimer, with its duration
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t, made: make(chan time.Duration, 100)}
}

type fakeTimer struct {
	c       chan time.Time
	at      time.Time
	stopped bool
	fired   bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	t := &fakeTimer{c: make(chan time.Time, 1), at: c.now.Add(d)}
	c.timers = append(c.timers, t)
	if d <= 0 {
		t.fired = true
		t.c <- c.now
	}
	c.mu.Unlock()
	select {
	case c.made <- d:
	default:
	}
	return &fakeTimerHandle{c: c, t: t}
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			t.c <- c.now
		}
	}
}

type fakeTimerHandle struct {
	c *fakeClock
	t *fakeTimer
}

func (h *fakeTimerHandle) C() <-chan time.Time { return h.t.c }
func (h *fakeTimerHandle) Stop() bool {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	was := !h.t.stopped && !h.t.fired
	h.t.stopped = true
	return was
}

// waitTimer waits for the scheduler to arm a timer and answers its duration.
func (c *fakeClock) waitTimer(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.made:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no timer armed")
		return 0
	}
}

// settle waits for a timer, then until the scheduler arms no new one for
// 30 ms (it may re-plan after a kick), and answers the last duration.
func (c *fakeClock) settle(t *testing.T) time.Duration {
	t.Helper()
	d := c.waitTimer(t)
	for {
		select {
		case d = <-c.made:
		case <-time.After(30 * time.Millisecond):
			return d
		}
	}
}

// --- models ---

type fakeModels struct {
	mu        sync.Mutex
	fn        func(ctx context.Context, req ModelRequest) (ModelReply, error)
	calls     []ModelRequest
	cur, peak int
}

func (m *fakeModels) Complete(ctx context.Context, req ModelRequest) (ModelReply, error) {
	m.mu.Lock()
	m.calls = append(m.calls, req)
	m.cur++
	m.peak = max(m.peak, m.cur)
	fn := m.fn
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.cur--; m.mu.Unlock() }()
	if fn == nil {
		return ModelReply{Text: "ok:" + req.Messages[len(req.Messages)-1].Content, PromptTokens: 10, CompletionTokens: 5}, nil
	}
	return fn(ctx, req)
}

func (m *fakeModels) Calls() []ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ModelRequest(nil), m.calls...)
}

// --- sessions ---

type fakeSessions struct {
	mu      sync.Mutex
	n       int
	reqs    []SessionRequest
	result  SessionResult
	block   bool
	release chan struct{}
	stopped []string
	started chan string
	failErr error

	mmu    sync.Mutex // guards meters; held while a meter fn runs
	meters map[string]func(ModelReply)
}

// Meter makes fakeSessions a SessionMeter: use delivers usage.
func (s *fakeSessions) Meter(id string, fn func(ModelReply)) func() {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if s.meters == nil {
		s.meters = map[string]func(ModelReply){}
	}
	s.meters[id] = fn
	return func() {
		s.mmu.Lock()
		defer s.mmu.Unlock()
		delete(s.meters, id)
	}
}

// use reports one model call of session id (false when nobody meters it).
func (s *fakeSessions) use(id string, rep ModelReply) bool {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	fn := s.meters[id]
	if fn == nil {
		return false
	}
	fn(rep)
	return true
}

func (s *fakeSessions) metered(id string) bool {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	return s.meters[id] != nil
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{result: SessionResult{State: "passed", Summary: "all good"},
		release: make(chan struct{}), started: make(chan string, 100)}
}

func (s *fakeSessions) Start(ctx context.Context, req SessionRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil {
		return "", s.failErr
	}
	s.n++
	id := fmt.Sprintf("s-%d", s.n)
	s.reqs = append(s.reqs, req)
	s.started <- id
	return id, nil
}

func (s *fakeSessions) Wait(ctx context.Context, id string) (SessionResult, error) {
	s.mu.Lock()
	block, res := s.block, s.result
	s.mu.Unlock()
	if block {
		select {
		case <-ctx.Done():
			return SessionResult{}, ctx.Err()
		case <-s.release:
		}
	}
	return res, nil
}

func (s *fakeSessions) Stop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = append(s.stopped, id)
	return nil
}

func (s *fakeSessions) Stopped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stopped...)
}

// --- notifier, auditor, events, guard ---

type fakeNotifier struct {
	mu       sync.Mutex
	notified []Notice
	posted   []Notice
	resolved []string
	onPost   func(Notice) // called after a Post, without n.mu held
}

func (n *fakeNotifier) Resolve(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.resolved = append(n.resolved, id)
}

func (n *fakeNotifier) resolvedIDs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.resolved)
}

func (n *fakeNotifier) Notify(x Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notified = append(n.notified, x)
	return nil
}

func (n *fakeNotifier) Post(x Notice) (string, error) {
	n.mu.Lock()
	n.posted = append(n.posted, x)
	id, hook := fmt.Sprintf("in-%d", len(n.posted)), n.onPost
	n.mu.Unlock()
	if hook != nil {
		hook(x)
	}
	return id, nil
}

func (n *fakeNotifier) counts() (int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.notified), len(n.posted)
}

type fakeAuditor struct {
	mu     sync.Mutex
	events []string
}

func (a *fakeAuditor) Event(actor, event, detail string, targets ...string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, actor+" "+event)
	return nil
}

func (a *fakeAuditor) has(s string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if e == s {
			return true
		}
	}
	return false
}

type fakeEvents struct {
	mu  sync.Mutex
	evs []Event
}

func (f *fakeEvents) Publish(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, ev)
}

func (f *fakeEvents) states(run string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.evs {
		if e.Run == run && (len(out) == 0 || out[len(out)-1] != e.State) {
			out = append(out, e.State)
		}
	}
	return out
}

type fakeGuard struct {
	mu  sync.Mutex
	why string
}

func (g *fakeGuard) CanStart() (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.why == "", g.why
}

// --- harness ---

type harness struct {
	t        *testing.T
	dir      string
	e        *Engine
	clock    *fakeClock
	models   *fakeModels
	sessions *fakeSessions
	notifier *fakeNotifier
	auditor  *fakeAuditor
	events   *fakeEvents
	guard    *fakeGuard
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, dir: t.TempDir(),
		clock:  newFakeClock(time.Date(2026, 11, 2, 7, 59, 0, 0, time.UTC)),
		models: &fakeModels{}, sessions: newFakeSessions(), notifier: &fakeNotifier{},
		auditor: &fakeAuditor{}, events: &fakeEvents{}, guard: &fakeGuard{}}
	h.open()
	return h
}

func (h *harness) open() {
	h.t.Helper()
	e, err := Open(Options{Dir: h.dir, Models: h.models, Sessions: h.sessions, Notifier: h.notifier,
		Auditor: h.auditor, Events: h.events, Guard: h.guard, Catalog: fakeCatalog{}, Clock: h.clock,
		Location: time.UTC})
	if err != nil {
		h.t.Fatal(err)
	}
	h.e = e
	h.t.Cleanup(func() { _ = e.Close() })
}

func (h *harness) create(f Flow) Flow {
	h.t.Helper()
	out, _, err := h.e.Create("owner:test", f)
	if err != nil {
		h.t.Fatalf("create: %v", err)
	}
	return out
}

func (h *harness) run(id, input string) Run {
	h.t.Helper()
	r, err := h.e.RunFlow("owner:test", id, input)
	if err != nil {
		h.t.Fatalf("run: %v", err)
	}
	return r
}

// wait waits for run id to be final or waiting.
func (h *harness) wait(id string) Run {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := h.e.Wait(ctx, id)
	if err != nil {
		h.t.Fatalf("wait %s: %v", id, err)
	}
	return r
}

func (h *harness) detail(id string) RunDetail {
	h.t.Helper()
	d, err := h.e.GetRun(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) stepRec(id, step string) StepRecord {
	h.t.Helper()
	for _, s := range h.detail(id).Steps {
		if s.ID == step {
			return s
		}
	}
	h.t.Fatalf("no record for step %s", step)
	return StepRecord{}
}

// eventually polls cond (tests only) for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var errBoom = errors.New("boom")
