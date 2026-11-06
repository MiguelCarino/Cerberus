package evals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options open an Engine. Dir is DATA/evals. Sessions and Models may be
// nil (runs then fail with a clear error); Settings nil means the
// defaults.
type Options struct {
	Dir      string
	Sessions Sessions
	Models   ModelCaller
	Catalog  Catalog
	Auditor  Auditor
	Events   Publisher
	Settings func() Settings
	Now      func() time.Time
	Log      *slog.Logger
}

// Engine owns the eval store and the runs: it starts their sessions,
// judges them when they end and writes the reports. It is safe for
// concurrent use.
type Engine struct {
	o       Options
	store   *Store
	runsDir string

	mu     sync.Mutex
	runs   map[string]*Run
	active map[string]*activeRun
	closed bool
	wg     sync.WaitGroup
}

// activeRun is a run (or a rejudge) going now.
type activeRun struct {
	cancel   context.CancelFunc
	canceled bool // by a person (else the daemon is closing)
	actor    string
}

// ErrNoJudge refuses a run with no judge model to use.
var ErrNoJudge = errors.New("no judge model: set the eval's judge_model, evals.judge_model or engines.default_model")

// Open loads the evals and runs.
func Open(o Options) (*Engine, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	st, err := OpenStore(o.Dir, o.Now)
	if err != nil {
		return nil, err
	}
	e := &Engine{o: o, store: st, runsDir: filepath.Join(o.Dir, "runs"), runs: map[string]*Run{}, active: map[string]*activeRun{}}
	if err := e.loadRuns(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) now() time.Time { return e.o.Now() }

// Store is the definition store.
func (e *Engine) Store() *Store { return e.store }

// Settings are the evals.* keys now.
func (e *Engine) Settings() Settings {
	s := Settings{Enabled: true, MaxParallel: DefaultMaxParallel, EvidenceKB: DefaultEvidenceKB}
	if e.o.Settings != nil {
		s = e.o.Settings()
	}
	return s.withDefaults()
}

// JudgeModelFor is the model that judges e now: its own judge_model,
// else evals.judge_model, else engines.default_model.
func (e *Engine) JudgeModelFor(ev Eval) string {
	s := e.Settings()
	for _, m := range []string{ev.JudgeModel, s.JudgeModel, s.DefaultModel} {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return ""
}

func (e *Engine) audit(actor, event, detail string, targets ...string) {
	if e.o.Auditor != nil {
		_ = e.o.Auditor.Event(actor, event, detail, targets...)
	}
}

func (e *Engine) publish(r *Run, session string) {
	if e.o.Events != nil {
		e.o.Events.Publish(Event{Run: r.ID, Eval: r.Eval, State: r.State, Session: session, Owner: r.Owner})
	}
}

// ---------- definitions ----------

// Validate checks a draft against the catalog.
func (e *Engine) Validate(ev Eval) []Problem { return nonNilProblems(Validate(ev, e.o.Catalog)) }

func nonNilProblems(ps []Problem) []Problem {
	if ps == nil {
		return []Problem{}
	}
	return ps
}

// validateFull refuses a definition with errors (catalog included).
func (e *Engine) validateFull(ev Eval) ([]Problem, error) {
	ps := e.Validate(ev)
	if HasErrors(ps) {
		return ps, &ValidationError{Problems: ps}
	}
	return ps, nil
}

// List is every eval.
func (e *Engine) List() []Eval { return e.store.List() }

// Get is one eval.
func (e *Engine) Get(id string) (Eval, error) { return e.store.Get(id) }

// Create saves a new eval (owner as set on ev).
func (e *Engine) Create(actor string, ev Eval) (Eval, []Problem, error) {
	if ps, err := e.validateFull(ev); err != nil {
		return Eval{}, ps, err
	}
	out, err := e.store.Create(ev)
	if err != nil {
		return Eval{}, nil, err
	}
	e.audit(actor, "eval.create", out.Name, out.ID)
	return out, e.Validate(out), nil
}

// Update replaces an eval (ev.Version is the version it was read at).
func (e *Engine) Update(actor string, ev Eval) (Eval, []Problem, error) {
	if ps, err := e.validateFull(ev); err != nil {
		return Eval{}, ps, err
	}
	out, err := e.store.Update(ev)
	if err != nil {
		return Eval{}, nil, err
	}
	e.audit(actor, "eval.update", fmt.Sprintf("%s v%d", out.Name, out.Version), out.ID)
	return out, e.Validate(out), nil
}

// Delete removes an eval; its runs stay.
func (e *Engine) Delete(actor, id string) error {
	if err := e.store.Delete(id); err != nil {
		return err
	}
	e.audit(actor, "eval.delete", "", id)
	return nil
}

// ---------- runs ----------

// Runs lists runs newest first: of one eval (eval != "") or all, at most
// limit (0 = all).
func (e *Engine) Runs(eval string, limit int) []Run {
	e.mu.Lock()
	var list []*Run
	for _, r := range e.runs {
		if eval == "" || r.Eval == eval {
			list = append(list, r)
		}
	}
	sortNewest(list)
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	out := make([]Run, len(list))
	for i, r := range list {
		out[i] = r.clone()
	}
	e.mu.Unlock()
	return out
}

// LastRun is the newest run of an eval.
func (e *Engine) LastRun(eval string) (Run, bool) {
	rs := e.Runs(eval, 1)
	if len(rs) == 0 {
		return Run{}, false
	}
	return rs[0], true
}

// GetRun is one run with its reports, the summary and the sessions' live
// states.
func (e *Engine) GetRun(id string) (RunDetail, error) {
	e.mu.Lock()
	r, ok := e.runs[id]
	var run Run
	if ok {
		run = r.clone()
	}
	e.mu.Unlock()
	if !ok {
		return RunDetail{}, ErrNotFound
	}
	if e.o.Sessions != nil {
		for i, s := range run.Slots {
			if s.Session == "" {
				continue
			}
			if in, ok := e.o.Sessions.Info(s.Session); ok {
				run.Slots[i].Live = in.State
			} else {
				run.Slots[i].Live = "removed"
			}
		}
	}
	reps := e.readReports(run)
	return RunDetail{Run: run, Reports: reps, Summary: Summarize(run, reps)}, nil
}

// RunOwner is the stored owner of a run (owner checks).
func (e *Engine) RunOwner(id string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.runs[id]
	if !ok {
		return "", false
	}
	return r.Owner, true
}

// Start runs an eval: one session per setup (only the setup labelled
// setupLabel when set) and repeat, owned by owner. It answers at once with
// the run; sessions start in the background, at most evals.max_parallel
// at a time.
func (e *Engine) Start(actor, owner, id, setupLabel string) (Run, error) {
	s := e.Settings()
	if !s.Enabled {
		return Run{}, ErrDisabled
	}
	ev, err := e.store.Get(id)
	if err != nil {
		return Run{}, err
	}
	if _, err := e.validateFull(ev); err != nil {
		return Run{}, err
	}
	judge := e.JudgeModelFor(ev)
	if judge == "" {
		return Run{}, ErrNoJudge
	}
	if e.o.Sessions == nil {
		return Run{}, errors.New("agent sessions are not available")
	}
	if e.o.Models == nil {
		return Run{}, errors.New("the judge model cannot be called here (no gateway)")
	}
	setupLabel = strings.TrimSpace(setupLabel)
	var slots []Slot
	for _, st := range ev.Setups {
		if setupLabel != "" && !strings.EqualFold(st.Label, setupLabel) {
			continue
		}
		for rep := 1; rep <= ev.Repeat; rep++ {
			slots = append(slots, Slot{Setup: st.Label, Repeat: rep, Agent: st.Agent, Model: st.Model, State: SlotPending})
		}
	}
	if len(slots) == 0 {
		return Run{}, &ValidationError{Problems: []Problem{{Field: "setup", Severity: SeverityError,
			Message: fmt.Sprintf("no setup labelled %q", setupLabel)}}}
	}
	base := ""
	if ev.Workspace == WorkspaceRepo {
		br, ok := e.o.Sessions.(BaseResolver)
		if !ok {
			return Run{}, errors.New("repo evals need the session host to pin the base commit")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		base, err = br.ResolveBase(ctx, owner, ev.Repo, ev.Base)
		cancel()
		if err != nil {
			return Run{}, fmt.Errorf("base commit: %w", err)
		}
	}
	now := e.now()
	r := &Run{ID: newRunID(now), Eval: ev.ID, EvalName: ev.Name, EvalVersion: ev.Version, Owner: owner, Actor: actor,
		State: RunRunning, SetupFilter: setupLabel, Base: base, JudgeModel: judge, JudgePrompt: JudgePromptHash(),
		MaxParallel: s.MaxParallel, Created: now.UTC(), Started: now.UTC(), Slots: slots, Snapshot: ev}
	r.Snapshot.Base = base
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		cancel()
		return Run{}, ErrClosed
	}
	for e.runs[r.ID] != nil {
		r.ID = newRunID(now)
	}
	if err := e.saveRunLocked(r); err != nil {
		e.mu.Unlock()
		cancel()
		return Run{}, err
	}
	e.runs[r.ID] = r
	act := &activeRun{cancel: cancel}
	e.active[r.ID] = act
	e.pruneLocked(r.Eval)
	out := r.clone()
	e.wg.Add(1)
	e.mu.Unlock()
	e.audit(actor, "eval.run.start", fmt.Sprintf("%s: %d sessions, judge %s", ev.Name, len(slots), judge), ev.ID, r.ID)
	e.publish(&out, "")
	go func() {
		defer e.wg.Done()
		e.execute(ctx, r.ID, act, s)
	}()
	return out, nil
}

// update changes a run under the lock, saves it and publishes the change.
func (e *Engine) update(id, session string, fn func(r *Run)) {
	e.mu.Lock()
	r, ok := e.runs[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	fn(r)
	if err := e.saveRunLocked(r); err != nil {
		e.o.Log.Warn("evals: saving the run failed", "run", id, "err", err)
	}
	snap := r.clone()
	e.mu.Unlock()
	e.publish(&snap, session)
}

func (e *Engine) slot(id string, i int) (Slot, Run) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.runs[id]
	return r.Slots[i], r.clone()
}

// execute starts the run's sessions in order, at most MaxParallel at a
// time; each is judged as soon as it ends.
func (e *Engine) execute(ctx context.Context, id string, act *activeRun, s Settings) {
	_, run := e.slot(id, 0)
	sem := make(chan struct{}, max(run.MaxParallel, 1))
	var wg sync.WaitGroup
	for i := range run.Slots {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			e.update(id, "", func(r *Run) {
				if r.Slots[i].State == SlotPending {
					r.Slots[i].State = SlotCanceled
				}
			})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.runSlot(ctx, id, i, sem, s)
		}()
	}
	wg.Wait()
	e.finish(id, act)
}

// finish records the end of a run (or a rejudge).
func (e *Engine) finish(id string, act *activeRun) {
	e.mu.Lock()
	canceled, closing := act.canceled, e.closed && !act.canceled
	delete(e.active, id)
	e.mu.Unlock()
	e.update(id, "", func(r *Run) {
		switch {
		case canceled:
			r.State, r.Reason = RunCanceled, "canceled"
		case closing:
			r.State, r.Reason = RunFailed, "the daemon stopped during the run (rejudge judges the sessions that ended)"
		default:
			r.State, r.Reason = RunDone, ""
		}
		r.Ended = e.now().UTC()
	})
}

// runSlot starts one session, waits for it and judges it.
func (e *Engine) runSlot(ctx context.Context, id string, i int, sem chan struct{}, s Settings) {
	released := false
	release := func() {
		if !released {
			released = true
			<-sem
		}
	}
	defer release()
	sl, run := e.slot(id, i)
	ev := run.Snapshot
	name := ev.Name + " · " + sl.Setup
	if ev.Repeat > 1 {
		name += fmt.Sprintf(" #%d", sl.Repeat)
	}
	sid, err := e.o.Sessions.Start(ctx, StartRequest{Agent: sl.Agent, Model: sl.Model, Workspace: ev.Workspace, Repo: ev.Repo,
		Base: run.Base, Task: ev.Task, Name: name, Run: id, Eval: ev.ID, Owner: run.Owner})
	if err != nil {
		e.update(id, "", func(r *Run) {
			if ctx.Err() != nil {
				r.Slots[i].State = SlotCanceled
				return
			}
			r.Slots[i].State, r.Slots[i].Error = SlotError, "the session did not start: "+err.Error()
		})
		return
	}
	started := e.now()
	e.update(id, sid, func(r *Run) { r.Slots[i].Session, r.Slots[i].State = sid, SlotRunning })
	info, err := e.o.Sessions.Wait(ctx, sid)
	release()
	if err != nil {
		e.mu.Lock()
		closing := e.closed && !e.active[id].canceledSafe()
		e.mu.Unlock()
		if ctx.Err() != nil && !closing {
			_ = e.o.Sessions.Stop(sid)
		}
		e.update(id, sid, func(r *Run) {
			if ctx.Err() != nil {
				r.Slots[i].State = SlotCanceled
				return
			}
			r.Slots[i].State, r.Slots[i].Error = SlotError, "waiting for the session: "+err.Error()
		})
		return
	}
	wall := e.now().Sub(started)
	e.judgeSlot(ctx, id, i, info, wall, s, sem)
}

func (a *activeRun) canceledSafe() bool { return a != nil && a.canceled }

// judgeSlot judges one ended session and writes its report. sem is the
// run's evals.max_parallel places: a resumed attempt (the feedback loop)
// takes one while it runs. A rejudge passes nil and never resumes.
func (e *Engine) judgeSlot(ctx context.Context, id string, i int, info SessionInfo, wall time.Duration, s Settings, sem chan struct{}) {
	sl, run := e.slot(id, i)
	e.update(id, sl.Session, func(r *Run) { r.Slots[i].State, r.Slots[i].Error = SlotJudging, "" })
	rep := e.judgeSession(ctx, run, sl, info, wall, s, sem)
	if sem == nil {
		if prev, ok := e.readReport(id, sl.Session); ok {
			rep = carryOver(prev, rep)
		}
	}
	if ctx.Err() != nil {
		e.update(id, sl.Session, func(r *Run) {
			r.JudgeUsage.add(rep.JudgeUsage)
			r.Slots[i].State = SlotCanceled
		})
		return
	}
	saveErr := e.saveReport(rep)
	e.update(id, sl.Session, func(r *Run) {
		r.JudgeUsage.add(rep.JudgeUsage)
		switch {
		case saveErr != nil:
			r.Slots[i].State, r.Slots[i].Error = SlotError, "saving the report: "+saveErr.Error()
		case rep.State == ReportError:
			r.Slots[i].State, r.Slots[i].Error = SlotError, rep.Error
		default:
			r.Slots[i].State = SlotJudged
		}
	})
}

// judgeSession judges one ended session of a run (see judging.run), with
// the feedback loop unless sem is nil (a rejudge).
func (e *Engine) judgeSession(ctx context.Context, run Run, sl Slot, info SessionInfo, wall time.Duration, s Settings,
	sem chan struct{}) Report {
	ev := run.Snapshot
	j := newJudging(JudgeDeps{Sessions: e.o.Sessions, Models: e.o.Models, Now: e.now}, s, Job{Session: sl.Session,
		Task: ev.Task, Gates: ev.Gates, Criteria: ev.Criteria, Persona: ev.Persona, JudgeModel: run.JudgeModel,
		Run: run.ID, Owner: run.Owner, Setup: sl.Setup, Feedback: sem != nil})
	if sem != nil {
		i := slotIndex(run, sl)
		j.acquire = func(ctx context.Context) bool {
			select {
			case sem <- struct{}{}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		j.release = func() { <-sem }
		j.attempt = func(running bool) {
			st := SlotJudging
			if running {
				st = SlotRunning
			}
			e.update(run.ID, sl.Session, func(r *Run) {
				if i >= 0 {
					r.Slots[i].State = st
				}
			})
		}
		j.stop = func(sid string) {
			e.mu.Lock()
			closing := e.closed && !e.active[run.ID].canceledSafe()
			e.mu.Unlock()
			if !closing {
				_ = e.o.Sessions.Stop(sid)
			}
		}
	}
	rep := Report{Run: run.ID, Eval: run.Eval, Session: sl.Session, Setup: sl.Setup, Repeat: sl.Repeat,
		Agent: sl.Agent, Model: sl.Model}
	return j.run(ctx, rep, info, wall)
}

// slotIndex is the index of sl in run (-1 when it is not there).
func slotIndex(run Run, sl Slot) int {
	for i, x := range run.Slots {
		if x.Session == sl.Session && x.Setup == sl.Setup && x.Repeat == sl.Repeat {
			return i
		}
	}
	return -1
}

// Cancel stops a run: sessions not started yet are not started, running
// ones are stopped, and judging stops.
func (e *Engine) Cancel(actor, id string) (Run, error) {
	e.mu.Lock()
	r, ok := e.runs[id]
	if !ok {
		e.mu.Unlock()
		return Run{}, ErrNotFound
	}
	act := e.active[id]
	if act == nil || Final(r.State) {
		e.mu.Unlock()
		return Run{}, ErrState
	}
	act.canceled, act.actor = true, actor
	act.cancel()
	out := r.clone()
	e.mu.Unlock()
	e.audit(actor, "eval.run.cancel", r.EvalName, r.Eval, id)
	return out, nil
}

// Rejudge judges a finished run's sessions again with the judge model of
// now, without running them again (a changed judge). Sessions still going
// are waited for; sessions that are gone are recorded as errors.
func (e *Engine) Rejudge(actor, id string) (Run, error) {
	e.mu.Lock()
	r, ok := e.runs[id]
	if !ok {
		e.mu.Unlock()
		return Run{}, ErrNotFound
	}
	if e.closed {
		e.mu.Unlock()
		return Run{}, ErrClosed
	}
	if e.active[id] != nil || !Final(r.State) {
		e.mu.Unlock()
		return Run{}, ErrState
	}
	if e.o.Sessions == nil || e.o.Models == nil {
		e.mu.Unlock()
		return Run{}, errors.New("sessions or the judge model are not available here")
	}
	judge := e.JudgeModelFor(r.Snapshot)
	if judge == "" {
		e.mu.Unlock()
		return Run{}, ErrNoJudge
	}
	started := false
	for _, s := range r.Slots {
		if s.Session != "" {
			started = true
		}
	}
	if !started {
		e.mu.Unlock()
		return Run{}, fmt.Errorf("%w: no session of this run started", ErrState)
	}
	ctx, cancel := context.WithCancel(context.Background())
	act := &activeRun{cancel: cancel}
	e.active[id] = act
	r.State, r.Reason, r.JudgeModel, r.JudgePrompt = RunJudging, "", judge, JudgePromptHash()
	r.JudgeUsage = Usage{}
	r.Ended = time.Time{}
	if err := e.saveRunLocked(r); err != nil {
		e.o.Log.Warn("evals: saving the run failed", "run", id, "err", err)
	}
	out := r.clone()
	e.wg.Add(1)
	e.mu.Unlock()
	e.audit(actor, "eval.run.rejudge", r.EvalName+" with "+judge, r.Eval, id)
	e.publish(&out, "")
	s := e.Settings()
	go func() {
		defer e.wg.Done()
		for i, sl := range out.Slots {
			if sl.Session == "" || ctx.Err() != nil {
				continue
			}
			info, err := e.o.Sessions.Wait(ctx, sl.Session)
			if err != nil {
				continue
			}
			if info.State == "removed" {
				e.update(id, sl.Session, func(r *Run) {
					r.Slots[i].State, r.Slots[i].Error = SlotError, "the session is gone: nothing to judge"
				})
				continue
			}
			// The stored report's wall time (summed over the attempts) is
			// carried over by judgeSlot; without one it is the latest
			// attempt's own (attemptWall).
			e.judgeSlot(ctx, id, i, info, 0, s, nil)
		}
		e.finish(id, act)
	}()
	return out, nil
}

// Close stops every run going (recorded as failed: the daemon stopped)
// without stopping their sessions, and waits for the goroutines.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	for _, a := range e.active {
		a.cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
	return nil
}
