package flows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// Options configure an Engine. Every dependency is optional: a missing
// one makes the steps that need it fail with a clear error (models,
// sessions, notifier), or turns its check off (guard, catalog, auditor,
// events).
type Options struct {
	Dir      string // DATA/flows (created 0700); runs live in Dir/runs
	Models   ModelCaller
	Sessions SessionRunner
	Notifier Notifier
	Auditor  Auditor
	Guard    Guard
	Catalog  Catalog
	Judge    JudgeRunner // judge steps (nil: they fail with a clear error)
	Events   Publisher
	Clock    Clock          // SystemClock when nil
	Location *time.Location // schedules and {date}/{time}; time.Local when nil
	MaxRuns  int            // flows.max_runs; 2 when 0
	KeepRuns int            // finished runs kept per flow; 100 when 0
	Log      *slog.Logger
	// OwnerActive reports whether a flow's owner may still run it (Phase
	// 4a): scheduled runs of a disabled or removed owner are skipped.
	// Nil: every owner is active.
	OwnerActive func(owner string) bool
}

// Defaults.
const (
	DefaultMaxRuns  = 2
	DefaultKeepRuns = 100
	// ActorSystem is the audit actor of scheduled runs.
	ActorSystem = "system"
	maxSleep    = 15 * time.Minute // the scheduler re-checks the wall clock at least this often
)

// Internal stop causes.
var (
	errShutdown = errors.New("daemon restarted")
	errCanceled = errors.New("canceled")
	errTimeout  = errors.New("timeout")
)

// limitError is a run passing one of its limits.
type limitError struct{ reason string }

func (l *limitError) Error() string { return l.reason }

// Engine owns flows, runs and the scheduler. Create one with Open, call
// Start to run the scheduler, Close to stop.
type Engine struct {
	o     Options
	store *Store
	runs  runStore
	clock Clock
	loc   *time.Location
	log   *slog.Logger

	base context.Context
	stop context.CancelCauseFunc
	wg   sync.WaitGroup
	kick chan struct{}

	mu        sync.Mutex
	index     map[string]*Run
	live      map[string]*liveRun
	queue     []string
	active    int
	maxRuns   int
	disabled  bool
	closed    bool
	started   bool
	changed   chan struct{}
	sched     map[string]*schedEntry
	schedPath string
	waitPub   map[string]*waitThrottle // run id → waiting-change publish throttle
}

// liveRun is the in-process state of a queued or running run.
type liveRun struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	items  map[string][]ItemRecord // fan-out step id → items so far
}

// Open opens the flows folder, loads flows and runs, and recovers from a
// restart: queued and running runs become failed ("daemon restarted");
// runs waiting for an approval keep waiting.
func Open(o Options) (*Engine, error) {
	if o.Dir == "" {
		return nil, errors.New("flows: no folder")
	}
	if o.Clock == nil {
		o.Clock = SystemClock{}
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.MaxRuns <= 0 {
		o.MaxRuns = DefaultMaxRuns
	}
	if o.KeepRuns <= 0 {
		o.KeepRuns = DefaultKeepRuns
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	st, err := OpenStore(o.Dir, o.Clock.Now)
	if err != nil {
		return nil, err
	}
	rd := filepath.Join(o.Dir, "runs")
	if err := os.MkdirAll(rd, 0o700); err != nil {
		return nil, err
	}
	base, stop := context.WithCancelCause(context.Background())
	e := &Engine{
		o: o, store: st, runs: runStore{dir: rd}, clock: o.Clock, loc: o.Location, log: o.Log,
		base: base, stop: stop, kick: make(chan struct{}, 1),
		index: map[string]*Run{}, live: map[string]*liveRun{}, maxRuns: o.MaxRuns,
		changed: make(chan struct{}), schedPath: filepath.Join(o.Dir, "schedule.json"),
		waitPub: map[string]*waitThrottle{},
	}
	now := e.clock.Now().UTC()
	for _, r := range e.runs.list() {
		if r.State == StateQueued || r.State == StateRunning {
			r.State, r.Reason, r.Ended, r.Updated, r.Waiting = StateFailed, ReasonRestarted, now, now, ""
			if err := e.runs.write(r); err != nil {
				e.log.Warn("flows: run not updated after restart", "run", r.ID, "err", err)
			}
		}
		e.index[r.ID] = r
	}
	e.sched = e.loadSchedule()
	return e, nil
}

// Store is the flow store (reads; writes should go through the Engine so
// they are audited and rescheduled).
func (e *Engine) Store() *Store { return e.store }

// Start runs the scheduler until Close. Calling it twice does nothing.
func (e *Engine) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.closed {
		return
	}
	e.started = true
	e.wg.Add(1)
	go e.scheduleLoop()
}

// Close stops the scheduler and every running run (they are recorded as
// failed, "daemon restarted"; their sessions are left alone) and waits.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()
	e.stop(errShutdown)
	e.wg.Wait()
	return nil
}

// SetMaxRuns changes flows.max_runs (at least 1); queued runs start when
// slots free up.
func (e *Engine) SetMaxRuns(n int) {
	if n <= 0 {
		n = DefaultMaxRuns
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.maxRuns = n
	e.startQueuedLocked()
}

// SetEnabled is flows.enabled: when off, new runs are refused
// (ErrDisabled) and the scheduler fires nothing; runs already going
// continue.
func (e *Engine) SetEnabled(on bool) {
	e.mu.Lock()
	e.disabled = !on
	e.mu.Unlock()
	e.reschedule()
}

func (e *Engine) audit(actor, event, detail string, targets ...string) {
	if e.o.Auditor == nil {
		return
	}
	if err := e.o.Auditor.Event(actor, event, detail, targets...); err != nil {
		e.log.Warn("flows: audit", "event", event, "err", err)
	}
}

// --- flows -------------------------------------------------------------

// Validate checks a draft against the catalog (nothing is saved).
func (e *Engine) Validate(f Flow) []Problem {
	normalize(&f)
	return Validate(f, e.o.Catalog)
}

// Create saves a new flow (validated with the catalog); it answers the
// saved flow and any warnings. Errors come back as *ValidationError.
func (e *Engine) Create(actor string, f Flow) (Flow, []Problem, error) {
	normalize(&f)
	ps := Validate(f, e.o.Catalog)
	if HasErrors(ps) {
		return Flow{}, ps, &ValidationError{Problems: ps}
	}
	out, err := e.store.Create(f)
	if err != nil {
		return Flow{}, ps, err
	}
	e.audit(actor, "flow.create", out.Name, out.ID)
	e.reschedule()
	return out, ps, nil
}

// Update replaces a flow; f.Version must match (ErrStale otherwise).
func (e *Engine) Update(actor string, f Flow) (Flow, []Problem, error) {
	normalize(&f)
	ps := Validate(f, e.o.Catalog)
	if HasErrors(ps) {
		return Flow{}, ps, &ValidationError{Problems: ps}
	}
	out, err := e.store.Update(f)
	if err != nil {
		return Flow{}, ps, err
	}
	e.audit(actor, "flow.update", fmt.Sprintf("%s v%d", out.Name, out.Version), out.ID)
	e.reschedule()
	return out, ps, nil
}

// SetFlowEnabled turns a flow's schedule on or off.
func (e *Engine) SetFlowEnabled(actor, id string, on bool) (Flow, error) {
	out, err := e.store.SetEnabled(id, on)
	if err != nil {
		return Flow{}, err
	}
	e.audit(actor, "flow.update", fmt.Sprintf("enabled=%v", on), id)
	e.reschedule()
	return out, nil
}

// Adopt makes owner the flow's stored owner (Phase 4a: an admin re-saving
// the flow of a removed user takes it over).
func (e *Engine) Adopt(actor, id, owner string) (Flow, error) {
	out, err := e.store.SetOwner(id, owner)
	if err != nil {
		return Flow{}, err
	}
	e.audit(actor, "flow.update", "adopted", out.ID)
	e.reschedule()
	return out, nil
}

// Delete removes a flow (its runs stay; runs going continue).
func (e *Engine) Delete(actor, id string) error {
	if err := e.store.Delete(id); err != nil {
		return err
	}
	e.audit(actor, "flow.delete", "", id)
	e.reschedule()
	return nil
}

// Get is one flow.
func (e *Engine) Get(id string) (Flow, error) { return e.store.Get(id) }

// Summary is a flow in the list: the flow, its trigger in words, the next
// scheduled time (nil when none), its last run and the last skipped
// scheduled run.
type Summary struct {
	Flow
	TriggerText string     `json:"trigger_text"`
	Next        *time.Time `json:"next"`
	LastRun     *RunBrief  `json:"last_run"`
	LastSkip    *Skip      `json:"last_skip"`
}

// RunBrief is the short form of a run.
type RunBrief struct {
	ID      string    `json:"id"`
	State   string    `json:"state"`
	Trigger string    `json:"trigger"`
	Created time.Time `json:"created"`
	Ended   time.Time `json:"ended"`
	Note    string    `json:"note"`
}

// Skip is a scheduled run that did not start, and why.
type Skip struct {
	Time   time.Time `json:"time"`
	Reason string    `json:"reason"`
}

// List is every flow with its next scheduled time and last run.
func (e *Engine) List() []Summary {
	flows := e.store.List()
	now := e.clock.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	last := map[string]*Run{}
	for _, r := range e.index {
		if r.Test {
			continue
		}
		if p := last[r.Flow]; p == nil || newer(r, p) {
			last[r.Flow] = r
		}
	}
	out := make([]Summary, 0, len(flows))
	for _, f := range flows {
		s := Summary{Flow: f, TriggerText: DescribeTrigger(f.Trigger)}
		if f.Enabled && f.Trigger.Kind == TriggerSchedule && !e.disabled {
			var next time.Time
			if en := e.sched[f.ID]; en != nil && en.Schedule == f.Trigger.Schedule && !en.next.IsZero() {
				next = en.next
			} else if sc, err := ParseSchedule(f.Trigger.Schedule); err == nil {
				next = sc.Next(now, e.loc)
			}
			if !next.IsZero() {
				s.Next = &next
			}
		}
		if r := last[f.ID]; r != nil {
			s.LastRun = brief(r)
		}
		if en := e.sched[f.ID]; en != nil && en.SkipReason != "" {
			s.LastSkip = &Skip{Time: en.SkipTime, Reason: en.SkipReason}
		}
		out = append(out, s)
	}
	return out
}

func brief(r *Run) *RunBrief {
	return &RunBrief{ID: r.ID, State: r.State, Trigger: r.Trigger, Created: r.Created, Ended: r.Ended, Note: r.Note}
}

func newer(a, b *Run) bool {
	if !a.Created.Equal(b.Created) {
		return a.Created.After(b.Created)
	}
	return a.ID > b.ID
}

// Templates are the built-in flows to start from.
func (e *Engine) Templates() []Template { return Templates() }

// --- runs --------------------------------------------------------------

// RunFlow starts a saved flow by hand (it need not be enabled) with input.
func (e *Engine) RunFlow(actor, id, input string) (Run, error) {
	f, err := e.store.Get(id)
	if err != nil {
		return Run{}, err
	}
	return e.start(actor, f, input, RunManual, "")
}

// DraftID is the flow id of test runs of unsaved drafts; the store never
// gives it to a saved flow.
const DraftID = "fl-draft"

// TestRun runs an unsaved draft once (marked test, never scheduled). A
// draft without a valid id runs as DraftID.
func (e *Engine) TestRun(actor string, f Flow, input string) (Run, error) {
	normalize(&f)
	if !ValidID(f.ID) {
		f.ID = DraftID
	}
	return e.start(actor, f, input, RunTest, "")
}

func (e *Engine) start(actor string, f Flow, input, trigger, note string) (Run, error) {
	if ps := Validate(f, e.o.Catalog); HasErrors(ps) {
		return Run{}, &ValidationError{Problems: ps}
	}
	now := e.clock.Now()
	r := &Run{
		ID: newRunID(now.In(e.loc)), Flow: f.ID, FlowName: f.Name, FlowVersion: f.Version,
		Trigger: trigger, Test: trigger == RunTest, Actor: actor, Input: input, Owner: f.Owner,
		State: StateQueued, Note: note, Created: now.UTC(), Updated: now.UTC(),
		Sessions: []string{}, Models: []string{}, Snapshot: f.clone(),
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Run{}, ErrClosed
	}
	if e.disabled {
		return Run{}, ErrDisabled
	}
	if err := e.runs.write(r); err != nil {
		return Run{}, err
	}
	e.index[r.ID] = r
	e.queue = append(e.queue, r.ID)
	e.audit(actor, "flow.run.start", trigger, f.ID, r.ID)
	e.changedLocked(r, "")
	e.startQueuedLocked()
	return copyRun(r), nil
}

// startQueuedLocked starts queued runs while slots are free.
func (e *Engine) startQueuedLocked() {
	for !e.closed && e.active < e.maxRuns && len(e.queue) > 0 {
		id := e.queue[0]
		e.queue = e.queue[1:]
		r := e.index[id]
		if r == nil || r.State != StateQueued {
			continue
		}
		now := e.clock.Now().UTC()
		r.State, r.Updated = StateRunning, now
		if r.Started.IsZero() {
			r.Started = now
		}
		e.persistLocked(r)
		ctx, cancel := context.WithCancelCause(e.base)
		e.live[id] = &liveRun{ctx: ctx, cancel: cancel, items: map[string][]ItemRecord{}}
		e.active++
		e.changedLocked(r, r.Step)
		e.wg.Add(1)
		go e.execute(ctx, id, cancel)
	}
}

func (e *Engine) persistLocked(r *Run) {
	if err := e.runs.write(r); err != nil {
		e.log.Warn("flows: run not saved", "run", r.ID, "err", err)
	}
}

// changedLocked wakes waiters and publishes a flowrun event.
func (e *Engine) changedLocked(r *Run, step string) {
	close(e.changed)
	e.changed = make(chan struct{})
	if e.o.Events != nil {
		e.o.Events.Publish(Event{Run: r.ID, Flow: r.Flow, State: r.State, Step: step, Owner: r.Owner})
	}
}

// update mutates run id under the lock, saves it and publishes.
func (e *Engine) update(id string, fn func(r *Run)) Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.index[id]
	fn(r)
	r.Updated = e.clock.Now().UTC()
	e.persistLocked(r)
	e.changedLocked(r, r.Step)
	return copyRun(r)
}

func copyRun(r *Run) Run {
	c := *r
	c.Sessions = slices.Clone(r.Sessions)
	c.Models = slices.Clone(r.Models)
	if r.Approval != nil {
		a := *r.Approval
		c.Approval = &a
	}
	c.Snapshot = r.Snapshot.clone()
	return c
}

// Runs lists runs newest first, of one flow when flow is not "", at most
// limit (all when limit <= 0).
func (e *Engine) Runs(flow string, limit int) []Run {
	e.mu.Lock()
	var rs []*Run
	for _, r := range e.index {
		if flow == "" || r.Flow == flow {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return newer(rs[i], rs[j]) })
	if limit > 0 && len(rs) > limit {
		rs = rs[:limit]
	}
	out := make([]Run, len(rs))
	for i, r := range rs {
		out[i] = copyRun(r)
	}
	e.mu.Unlock()
	return out
}

// GetRun is a run with its finished steps.
func (e *Engine) GetRun(id string) (RunDetail, error) {
	e.mu.Lock()
	r, ok := e.index[id]
	var c Run
	var live []ItemRecord
	if ok {
		c = copyRun(r)
		if lr := e.live[id]; lr != nil && c.State == StateRunning {
			live = slices.Clone(lr.items[c.Step])
		}
	}
	e.mu.Unlock()
	if !ok {
		return RunDetail{}, ErrNotFound
	}
	steps, err := e.runs.steps(id)
	if err != nil {
		return RunDetail{}, err
	}
	return RunDetail{Run: c, Steps: steps, Live: live}, nil
}

// RunDir is a run's folder (its output files are under files/).
func (e *Engine) RunDir(id string) (string, error) {
	e.mu.Lock()
	_, ok := e.index[id]
	e.mu.Unlock()
	if !ok {
		return "", ErrNotFound
	}
	return e.runs.runDir(id), nil
}

// Wait blocks until run id is final or waiting for an approval, or ctx
// ends.
func (e *Engine) Wait(ctx context.Context, id string) (Run, error) {
	for {
		e.mu.Lock()
		r, ok := e.index[id]
		if !ok {
			e.mu.Unlock()
			return Run{}, ErrNotFound
		}
		if Final(r.State) || r.State == StateWaiting {
			c := copyRun(r)
			e.mu.Unlock()
			return c, nil
		}
		ch := e.changed
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return Run{}, ctx.Err()
		case <-ch:
		}
	}
}

// Cancel stops a run: a queued or waiting run is canceled at once, a
// running one as soon as its step notices (sessions it started are
// stopped). A final run answers ErrState.
func (e *Engine) Cancel(actor, id string) (Run, error) {
	var asked string // an approval's inbox item, resolved after the unlock
	defer func() { e.resolve(asked) }()
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.index[id]
	if !ok {
		return Run{}, ErrNotFound
	}
	now := e.clock.Now().UTC()
	switch r.State {
	case StateQueued, StateWaiting:
		if r.State == StateWaiting && r.Approval != nil {
			asked = r.Approval.Inbox
		}
		e.queue = slices.DeleteFunc(e.queue, func(q string) bool { return q == id })
		r.State, r.Reason, r.Ended, r.Updated = StateCanceled, "canceled by "+actor, now, now
		if lr := e.live[id]; lr != nil {
			lr.cancel(errCanceled)
			delete(e.live, id)
		}
		e.persistLocked(r)
		e.changedLocked(r, r.Step)
		e.pruneLocked(r.Flow, r.ID)
	case StateRunning:
		if lr := e.live[id]; lr != nil {
			lr.cancel(errCanceled)
		}
		r.Reason = "canceled by " + actor
	default:
		return Run{}, ErrState
	}
	e.audit(actor, "flow.run.cancel", "", r.Flow, id)
	return copyRun(r), nil
}

// Approve decides a run waiting at an approve step: ok continues the run
// (it queues again), !ok ends it as rejected.
func (e *Engine) Approve(actor, id string, ok bool, note string) (Run, error) {
	var asked string // the approval's inbox item, resolved after the unlock
	defer func() { e.resolve(asked) }()
	e.mu.Lock()
	defer e.mu.Unlock()
	r, found := e.index[id]
	if !found {
		return Run{}, ErrNotFound
	}
	if r.State != StateWaiting || r.Approval == nil || r.PC >= len(r.Snapshot.Steps) {
		return Run{}, ErrState
	}
	now := e.clock.Now().UTC()
	a := r.Approval
	a.Decided, a.OK, a.Note, a.By = now, ok, note, actor
	recs, _ := e.runs.steps(id)
	s := r.Snapshot.Steps[r.PC]
	rec := StepRecord{
		N: nextN(recs), ID: s.ID, Kind: s.Kind, Name: s.Name, State: StepPassed, Input: a.Message,
		Output:  map[string]any{"ok": ok, "note": note, "by": actor},
		Started: a.Asked, Ended: now, DurationMS: now.Sub(a.Asked).Milliseconds(), Items: []ItemRecord{},
	}
	if !ok {
		rec.State = StepRejected
	}
	if err := e.runs.writeStep(id, rec); err != nil {
		return Run{}, err
	}
	asked = a.Inbox
	r.Updated = now
	if ok {
		r.PC++
		r.State = StateQueued
		e.queue = append(e.queue, id)
		e.audit(actor, "flow.run.approve", note, r.Flow, id)
	} else {
		r.State, r.Ended = StateRejected, now
		r.Reason = "rejected by " + actor
		if note != "" {
			r.Reason += ": " + note
		}
		e.audit(actor, "flow.run.reject", note, r.Flow, id)
	}
	e.persistLocked(r)
	e.changedLocked(r, r.Step)
	if ok {
		e.startQueuedLocked()
	} else {
		e.pruneLocked(r.Flow, r.ID)
	}
	return copyRun(r), nil
}

// Continue starts a run that stopped (failed, limited or canceled) again
// from the step it stopped at: that step runs again, and the steps before
// it keep their outputs. A step that stopped inside an agent session goes
// on in that session (its workspace and conversation) when the runner is
// a SessionResumer. The run gets a fresh allowance of its limits (time,
// steps, tokens, cost); its totals keep counting. A rejected or passed
// run, or one still going, answers ErrState.
func (e *Engine) Continue(actor, id string) (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Run{}, ErrClosed
	}
	if e.disabled {
		return Run{}, ErrDisabled
	}
	r, found := e.index[id]
	if !found {
		return Run{}, ErrNotFound
	}
	switch r.State {
	case StateFailed, StateLimited, StateCanceled:
	default:
		return Run{}, ErrState
	}
	if r.PC < 0 || r.PC >= len(r.Snapshot.Steps) {
		return Run{}, ErrState
	}
	if _, live := e.live[id]; live {
		return Run{}, ErrState // its execution has not wound down yet
	}
	r.Resume = ""
	if s := r.Snapshot.Steps[r.PC]; s.Kind == KindSession {
		recs, _ := e.runs.steps(id)
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].ID == s.ID {
				if recs[i].State != StepPassed {
					r.Resume = recs[i].Session
				}
				break
			}
		}
	}
	now := e.clock.Now().UTC()
	r.Continued++
	r.BaseTokens, r.BaseCostUSD, r.BaseSteps, r.ActiveMS = r.Tokens, r.CostUSD, r.StepsRun, 0
	r.State, r.Reason, r.Ended, r.Updated, r.Waiting = StateQueued, "", time.Time{}, now, ""
	r.Note = fmt.Sprintf("continued by %s from step %s", actor, r.Snapshot.Steps[r.PC].ID)
	e.persistLocked(r)
	e.queue = append(e.queue, id)
	e.audit(actor, "flow.run.continue", r.Snapshot.Steps[r.PC].ID, r.Flow, id)
	e.changedLocked(r, r.Step)
	e.startQueuedLocked()
	return copyRun(r), nil
}

// resolve tells a Notifier that is also a Resolver that the approval
// behind inbox item id no longer waits.
func (e *Engine) resolve(id string) {
	if id == "" {
		return
	}
	if r, ok := e.o.Notifier.(Resolver); ok {
		r.Resolve(id)
	}
}

func nextN(recs []StepRecord) int {
	n := 0
	for _, r := range recs {
		n = max(n, r.N)
	}
	return n + 1
}

// pruneLocked drops the oldest finished runs of a flow beyond KeepRuns.
// keep is the run that just ended: it is always kept (and counted), even
// when it was created before the runs that make up the newest KeepRuns
// (a run that waited days for an approval must not vanish as it ends).
func (e *Engine) pruneLocked(flow, keep string) {
	var done []*Run
	kept := 0
	for _, r := range e.index {
		if r.Flow != flow || !Final(r.State) {
			continue
		}
		if r.ID == keep {
			kept = 1
			continue
		}
		done = append(done, r)
	}
	room := max(e.o.KeepRuns-kept, 0)
	if len(done) <= room {
		return
	}
	sort.Slice(done, func(i, j int) bool { return newer(done[i], done[j]) })
	for _, r := range done[room:] {
		if err := e.runs.remove(r.ID); err != nil {
			e.log.Warn("flows: old run not removed", "run", r.ID, "err", err)
			continue
		}
		delete(e.index, r.ID)
	}
}

// setItems publishes a fan-out step's items so far (for the live graph
// and the run page) and sets the run's Waiting from the items that wait
// for the engine. waitOnly: only an item's Waiting changed, so the event
// is throttled.
func (e *Engine) setItems(id, step string, items []ItemRecord, waitOnly bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if lr := e.live[id]; lr != nil {
		lr.items[step] = slices.Clone(items)
	}
	r := e.index[id]
	if r == nil {
		return
	}
	if r.Step == step && r.State == StateRunning {
		r.Waiting = itemsWaiting(items)
	}
	if waitOnly {
		e.waitChangedLocked(r)
		return
	}
	e.changedLocked(r, step)
}

// itemsWaiting sums up the items whose model call the engine holds:
// "item 2: <reason>", "3 items waiting: <first reason>", or "".
func itemsWaiting(items []ItemRecord) string {
	n, first, idx := 0, "", 0
	for _, it := range items {
		if it.Waiting == "" {
			continue
		}
		if n == 0 {
			first, idx = it.Waiting, it.Index
		}
		n++
	}
	switch n {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("item %d: %s", idx+1, first)
	}
	return fmt.Sprintf("%d items waiting: %s", n, first)
}

// setStepWaiting sets the run's Waiting while the current step's model
// call is held by the engine ("" once it proceeds) and publishes it,
// throttled. It is not saved: it only means something while the run is
// in this process.
func (e *Engine) setStepWaiting(id, step, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.index[id]
	if r == nil || r.Step != step || r.State != StateRunning || r.Waiting == reason {
		return
	}
	r.Waiting = reason
	e.waitChangedLocked(r)
}

// waitGap is the least time between two events caused only by a waiting
// change of one run; changes inside it are sent once, at its end.
var waitGap = 300 * time.Millisecond

// waitThrottle is one run's waiting-change publishing.
type waitThrottle struct {
	last    time.Time
	pending bool
}

// waitChangedLocked publishes a run's waiting change, at most one event
// per waitGap (a later change inside the gap is sent at its end with the
// state as it is then), so an engine hook that flip-flops does not flood
// the event stream.
func (e *Engine) waitChangedLocked(r *Run) {
	t := e.waitPub[r.ID]
	if t == nil {
		t = &waitThrottle{}
		e.waitPub[r.ID] = t
	}
	if t.pending {
		return
	}
	since := time.Since(t.last)
	if t.last.IsZero() || since >= waitGap {
		t.last = time.Now()
		e.changedLocked(r, r.Step)
		return
	}
	t.pending = true
	id := r.ID
	time.AfterFunc(waitGap-since, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		t.pending = false
		t.last = time.Now()
		if x := e.index[id]; x != nil && !e.closed {
			e.changedLocked(x, x.Step)
		}
	})
}
