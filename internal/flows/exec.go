package flows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// timeoutUnit is the unit of limits.timeout_min (tests shorten it).
var timeoutUnit = time.Minute

// runCtx is what a running run's steps share.
type runCtx struct {
	id      string
	live    *liveRun // this execution's live entry (another execution may replace it after an approval)
	snap    Flow
	input   string
	lim     Limits
	outputs map[string]any
	known   map[string]bool
	// items are the passed fan-out steps' item records (their titles and
	// kinds title the sections of {steps.<id>.output}).
	items map[string][]ItemRecord
	// prior is, per step id, the newest record of an attempt that did not
	// pass (failed, limited or canceled) when no later attempt passed: a
	// continued fan-out reuses its passed items. A step takes its entry
	// once.
	prior map[string]StepRecord
	// failed are the steps whose latest record failed under on_error: their
	// output is only {"error"}, nothing a judge can judge.
	failed map[string]bool
	// resume is the session the step at resumePC continues (Continue), once.
	resume   string
	resumePC int
	// bases are the totals when the run was last continued (see Run).
	baseTokens int
	baseCost   float64
	baseSteps  int
	// smu guards the steps count while fan-out items retry in parallel.
	smu sync.Mutex
}

func (rc *runCtx) link() string { return "#/flows/runs/" + rc.id }

// outcome is how a pass of the loop ended.
type outcome struct {
	state  string // a final state, or StateWaiting
	reason string
}

func (e *Engine) execute(ctx context.Context, id string, cancel context.CancelCauseFunc) {
	defer e.wg.Done()
	defer cancel(nil)
	e.mu.Lock()
	r := copyRun(e.index[id])
	lr := e.live[id]
	e.mu.Unlock()
	out := e.loop(ctx, r, lr)
	if out.state == StateWaiting {
		return // ask already gave the slot back, under the same lock
	}
	e.finish(id, lr, out)
}

// finish records a run's final outcome and gives its slot back. lr is the
// execution's own live entry: only that one is removed.
func (e *Engine) finish(id string, lr *liveRun, out outcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.releaseLocked(id, lr)
	r := e.index[id]
	if r == nil { // removed meanwhile (never expected: running runs are not pruned)
		e.startQueuedLocked()
		return
	}
	now := e.clock.Now().UTC()
	r.Updated = now
	if !Final(r.State) {
		r.State, r.Ended = out.state, now
		if !(out.state == StateCanceled && r.Reason != "") {
			r.Reason = out.reason
		}
	}
	e.persistLocked(r)
	e.changedLocked(r, r.Step)
	if Final(r.State) {
		e.pruneLocked(r.Flow, r.ID)
	}
	e.startQueuedLocked()
}

// releaseLocked gives an execution's slot back and drops its live entry
// when it is still the current one.
func (e *Engine) releaseLocked(id string, lr *liveRun) {
	e.active--
	if lr != nil && e.live[id] == lr {
		delete(e.live, id)
	}
	delete(e.waitPub, id)
	if r := e.index[id]; r != nil {
		r.Waiting = ""
	}
}

// loop runs steps from r.PC until the run ends or waits.
func (e *Engine) loop(ctx context.Context, r Run, lr *liveRun) outcome {
	rc := &runCtx{id: r.ID, live: lr, snap: r.Snapshot, input: r.Input, lim: r.Snapshot.Limits.Effective(),
		outputs: map[string]any{}, known: map[string]bool{}, items: map[string][]ItemRecord{},
		prior: map[string]StepRecord{}, failed: map[string]bool{}, resume: r.Resume, resumePC: r.PC,
		baseTokens: r.BaseTokens, baseCost: r.BaseCostUSD, baseSteps: r.BaseSteps}
	for _, s := range rc.snap.Steps {
		rc.known[s.ID] = true
	}
	recs, err := e.runs.steps(r.ID)
	if err != nil {
		return outcome{StateFailed, "steps not read: " + err.Error()}
	}
	for _, rec := range recs {
		switch rec.State {
		case StepPassed:
			rc.outputs[rec.ID] = rec.Output
			if len(rec.Items) > 0 {
				rc.items[rec.ID] = rec.Items
			}
			delete(rc.prior, rec.ID)
			delete(rc.failed, rec.ID)
			// a judge step that revised a prompt step's answer
			if out, ok := rec.Output.(map[string]any); ok && rec.Kind == KindJudge && out["revised"] == true {
				if i := rc.snap.stepIndex(rec.ID); i >= 0 {
					rc.outputs[rc.snap.Steps[i].On] = out["output"]
				}
			}
		case StepFailed, StepLimited, StepCanceled:
			if rec.State == StepFailed && rec.OnError != "" {
				// a failure on_error let the run pass: later steps read
				// its {"error"} output
				rc.outputs[rec.ID] = rec.Output
				delete(rc.prior, rec.ID)
				rc.failed[rec.ID] = true
				continue
			}
			rc.prior[rec.ID] = rec
			// a judge step that stopped part-way kept the revisions it made
			// (judgeText): its fan-out reads them, and Continue does not
			// pay for them again
			if out, ok := rec.Output.(map[string]any); ok && rec.Kind == KindJudge && out["partial"] == true && out["revised"] == true {
				if i := rc.snap.stepIndex(rec.ID); i >= 0 {
					rc.outputs[rc.snap.Steps[i].On] = out["output"]
				}
			}
		}
	}
	n := nextN(recs) - 1
	stepsRun, active := r.StepsRun, r.ActiveMS
	timeout := time.Duration(rc.lim.TimeoutMin) * timeoutUnit
	for pc := r.PC; pc < len(rc.snap.Steps); {
		s := rc.snap.Steps[pc]
		if ctx.Err() != nil {
			return stopOutcome(context.Cause(ctx), rc)
		}
		stepsRun++
		if stepsRun-r.BaseSteps > rc.lim.MaxSteps {
			return outcome{StateLimited, fmt.Sprintf("more than %d steps", rc.lim.MaxSteps)}
		}
		if s.Kind == KindApprove {
			if !e.ask(ctx, rc, pc, s, stepsRun) {
				return stopOutcome(context.Cause(ctx), rc)
			}
			return outcome{state: StateWaiting}
		}
		remaining := timeout - time.Duration(active)*time.Millisecond
		if remaining <= 0 {
			return outcome{StateLimited, fmt.Sprintf("timeout after %d min", rc.lim.TimeoutMin)}
		}
		if pc != rc.resumePC {
			rc.resume = ""
		}
		e.update(r.ID, func(x *Run) { x.Step, x.PC, x.StepsRun, x.Resume = s.ID, pc, stepsRun, rc.resume })
		sctx, scancel := context.WithTimeoutCause(ctx, remaining, errTimeout)
		n++
		rec := StepRecord{N: n, ID: s.ID, Kind: s.Kind, Name: s.Name, Started: e.clock.Now().UTC(), Items: []ItemRecord{}}
		begin := time.Now()
		jump, err := e.runStep(sctx, rc, s, &rec, &stepsRun)
		elapsed := time.Since(begin)
		var cause error
		if err != nil && sctx.Err() != nil {
			cause = context.Cause(sctx)
		}
		scancel()
		rec.Ended = e.clock.Now().UTC()
		rec.DurationMS = elapsed.Milliseconds()
		active += elapsed.Milliseconds()
		e.update(r.ID, func(x *Run) { x.ActiveMS, x.StepsRun, x.Waiting = active, stepsRun, "" })
		if err != nil {
			out := classify(err, cause, rc, s)
			rec.State, rec.Error = stepState(out.state), err.Error()
			if cause != nil && !errors.Is(err, cause) {
				rec.Error = out.reason
			}
			if !handledError(s, out, cause, err) {
				e.writeStep(r.ID, rec)
				return out
			}
			// on_error: the step failed, the run goes on.
			jump = ""
			if s.OnError != OnErrorContinue {
				jump = s.OnError
			}
			rec.OnError = "next"
			if jump != "" {
				rec.OnError = jump
			}
			rec.Jump = "on error → " + rec.OnError
			rec.Output = map[string]any{"error": rec.Error}
			rec.Output = generic(rec.Output)
			rc.outputs[s.ID] = rec.Output
			rc.failed[s.ID] = true
			e.writeStep(r.ID, rec)
			e.log.Info("flows: step failed, the run goes on (on_error)", "run", r.ID, "step", s.ID, "err", err)
		} else {
			rec.State = StepPassed
			rec.Output = generic(rec.Output)
			rc.outputs[s.ID] = rec.Output
			delete(rc.failed, s.ID)
			if len(rec.Items) > 0 {
				rc.items[s.ID] = rec.Items
			}
			e.writeStep(r.ID, rec)
		}
		switch jump {
		case "":
			pc++
		case End:
			pc = len(rc.snap.Steps)
		default:
			pc = rc.snap.stepIndex(jump)
		}
		// The next step to run, so a run stopped between steps continues
		// from there (Continue), not from the step that just passed.
		next := min(pc, len(rc.snap.Steps))
		e.update(r.ID, func(x *Run) { x.PC = next })
	}
	return outcome{StatePassed, ""}
}

func (e *Engine) writeStep(id string, rec StepRecord) {
	if err := e.runs.writeStep(id, rec); err != nil {
		e.log.Warn("flows: step record not saved", "run", id, "step", rec.ID, "err", err)
	}
}

func stepState(runState string) string {
	switch runState {
	case StateLimited:
		return StepLimited
	case StateCanceled:
		return StepCanceled
	}
	return StepFailed
}

// handledError reports whether step s's failure lets the run go on
// (on_error continue or a jump): a plain failure, never a limit, a
// cancel, the timeout or a shutdown.
func handledError(s Step, out outcome, cause, err error) bool {
	if !s.handlesErrors() || out.state != StateFailed || cause != nil || out.reason == ReasonRestarted {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// retryable reports whether a failed step (or item) may run again under
// its retry setting: anything but a limit, a cancel or the step's end.
func retryable(ctx context.Context, err error) bool {
	var le *limitError
	return err != nil && ctx.Err() == nil && !errors.As(err, &le) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// countStep counts one more executed step (a retry) toward max_steps.
func (e *Engine) countStep(rc *runCtx, stepsRun *int, what string) error {
	rc.smu.Lock()
	defer rc.smu.Unlock()
	*stepsRun++
	if *stepsRun-rc.baseSteps > rc.lim.MaxSteps {
		return &limitError{fmt.Sprintf("more than %d steps (%s)", rc.lim.MaxSteps, what)}
	}
	return nil
}

// addUsage adds the usage of an earlier try to rec.
func addUsage(rec *StepRecord, o StepRecord) {
	rec.PromptTokens += o.PromptTokens
	rec.CompletionTokens += o.CompletionTokens
	rec.Calls += o.Calls
	rec.CostUSD += o.CostUSD
	rec.EnergyWh += o.EnergyWh
	rec.Retries += o.Retries
}

// withRetry runs a prompt or plan step, and runs it again (up to
// s.retries() times, s.backoff() apart) when it fails for a reason
// retryable allows. Each retry counts as a step toward max_steps; the
// record keeps the usage of every try and the first error.
func (e *Engine) withRetry(ctx context.Context, rc *runCtx, s Step, rec *StepRecord, stepsRun *int, run func(*StepRecord) error) error {
	base := *rec
	var spent StepRecord
	first := ""
	for try := 0; ; try++ {
		cur := base
		err := run(&cur)
		own := cur
		addUsage(&cur, spent)
		if try > 0 {
			cur.Attempts = try + 1
			if cur.RetryError == "" {
				cur.RetryError = first
			}
		}
		*rec = cur
		if err == nil || try >= s.retries() || !retryable(ctx, err) {
			return err
		}
		if first == "" {
			first = err.Error()
		}
		addUsage(&spent, own)
		if cerr := e.countStep(rc, stepsRun, "a retry of "+s.ID); cerr != nil {
			return cerr
		}
		e.log.Info("flows: retrying a step", "run", rc.id, "step", s.ID, "try", try+2, "err", err)
		if d := s.backoff(); d > 0 && !e.sleep(ctx, d) {
			return ctx.Err()
		}
	}
}

// stopOutcome is the outcome for a run stopped between steps.
func stopOutcome(cause error, rc *runCtx) outcome {
	return classify(cause, cause, rc, Step{})
}

// classify maps a step error (and the step context's cause) to a run
// outcome.
func classify(err, cause error, rc *runCtx, s Step) outcome {
	var le *limitError
	switch {
	case errors.As(err, &le) || errors.As(cause, &le):
		return outcome{StateLimited, le.reason}
	case errors.Is(cause, errShutdown):
		return outcome{StateFailed, ReasonRestarted}
	case errors.Is(cause, errCanceled):
		return outcome{StateCanceled, "canceled"}
	case errors.Is(cause, errTimeout):
		return outcome{StateLimited, fmt.Sprintf("timeout after %d min", rc.lim.TimeoutMin)}
	}
	if s.ID != "" {
		return outcome{StateFailed, fmt.Sprintf("step %s: %v", s.ID, err)}
	}
	return outcome{StateFailed, err.Error()}
}

// generic is v in its JSON-decoded form, so outputs read back from disk
// equal the ones in memory.
func generic(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func (e *Engine) env(rc *runCtx, item map[string]string) renderEnv {
	now := e.clock.Now().In(e.loc)
	return renderEnv{input: rc.input, flowName: rc.snap.Name, date: now.Format("2006-01-02"),
		time: now.Format("15:04"), outputs: rc.outputs, known: rc.known, item: item, items: rc.items}
}

// ask puts the approval request out and marks the run waiting. In the
// same locked section it gives the execution's slot back, so an Approve
// that comes at once never sees a waiting run still holding a slot (its
// new execution would race this one's end). It answers false (and
// changes nothing; the inbox item it posted is resolved) when the run was
// stopped meanwhile.
func (e *Engine) ask(ctx context.Context, rc *runCtx, pc int, s Step, stepsRun int) bool {
	msg := strings.TrimSpace(s.Message)
	if msg != "" {
		if m, err := render(msg, e.env(rc, nil)); err == nil {
			msg = m
		}
	}
	if msg == "" {
		msg = "Approve to continue?"
	}
	n := Notice{Kind: "flow-approve", Level: "warn", Title: "Approve: " + rc.snap.Name, Body: msg,
		Link: rc.link(), Run: rc.id, Flow: rc.snap.ID, Step: s.ID, Owner: rc.snap.Owner}
	var inboxID string
	if e.o.Notifier != nil {
		var err error
		if inboxID, err = e.o.Notifier.Post(n); err != nil {
			e.log.Warn("flows: approval not put in the inbox", "run", rc.id, "err", err)
		}
		if err := e.o.Notifier.Notify(n); err != nil {
			e.log.Warn("flows: approval not notified", "run", rc.id, "err", err)
		}
	}
	now := e.clock.Now().UTC()
	e.mu.Lock()
	r := e.index[rc.id]
	if r == nil || r.State != StateRunning || ctx.Err() != nil {
		e.mu.Unlock()
		e.resolve(inboxID) // nobody will decide it: the run is stopping
		return false
	}
	r.State, r.Step, r.PC, r.StepsRun, r.Updated, r.Waiting = StateWaiting, s.ID, pc, stepsRun, now, ""
	r.Approval = &Approval{Step: s.ID, Message: msg, Inbox: inboxID, Asked: now}
	e.persistLocked(r)
	e.releaseLocked(rc.id, rc.live)
	e.changedLocked(r, s.ID)
	e.startQueuedLocked()
	e.mu.Unlock()
	return true
}

// charge adds a model reply's usage to the run and checks the limits.
func (e *Engine) charge(rc *runCtx, reply ModelReply, model string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.index[rc.id]
	if r == nil {
		return nil
	}
	r.Tokens += reply.PromptTokens + reply.CompletionTokens
	r.CostUSD += reply.CostUSD
	r.EnergyWh += reply.EnergyWh
	if model != "" && !slices.Contains(r.Models, model) {
		r.Models = append(r.Models, model)
	}
	r.Updated = e.clock.Now().UTC()
	e.persistLocked(r)
	switch {
	case r.Tokens-rc.baseTokens > rc.lim.MaxTokens:
		return &limitError{fmt.Sprintf("used %d tokens, over the limit of %d", r.Tokens-rc.baseTokens, rc.lim.MaxTokens)}
	case r.CostUSD-rc.baseCost > rc.lim.MaxCostUSD:
		return &limitError{fmt.Sprintf("cost $%.4f, over the limit of $%.2f", r.CostUSD-rc.baseCost, rc.lim.MaxCostUSD)}
	}
	return nil
}

// usage accumulates one or more model calls.
type usageSum struct {
	model            string
	prompt, complete int
	cost, energy     float64
	calls            int
}

func (u *usageSum) add(r ModelReply, model string) {
	u.model = model
	u.prompt += r.PromptTokens
	u.complete += r.CompletionTokens
	u.cost += r.CostUSD
	u.energy += r.EnergyWh
	u.calls++
}

// callShape is a call's JSON wish and model settings (Step.shape).
type callShape struct {
	json        bool
	schema      json.RawMessage
	temperature *float64
	maxTokens   int
	thinking    string
}

// call runs one completion and charges it.
func (e *Engine) call(ctx context.Context, rc *runCtx, step, model string, msgs []Message, sh callShape, u *usageSum) (string, error) {
	if e.o.Models == nil {
		return "", errors.New("no model caller is available")
	}
	reply, err := e.o.Models.Complete(ctx, ModelRequest{Model: model, Messages: msgs, JSON: sh.json,
		Schema: sh.schema, Temperature: sh.temperature, MaxTokens: sh.maxTokens, Thinking: sh.thinking,
		Client: ClientName, Run: rc.id, Flow: rc.snap.ID, Step: step, Owner: rc.snap.Owner})
	if err != nil {
		return "", err
	}
	used := reply.Model
	if used == "" {
		used = model
	}
	u.add(reply, used)
	if err := e.charge(rc, reply, used); err != nil {
		return reply.Text, err
	}
	return reply.Text, nil
}

// retryBackoff is the pause before the one retry of a model call that
// failed with a transient engine error (tests shorten it or move the
// fake clock).
var retryBackoff = 2 * time.Second

// retryState counts a step's (or item's) retries of model calls after a
// transient engine failure: at most one per step or item.
type retryState struct {
	n     int
	first string // the error that caused the retry
}

// callRetry is call, retried once (after retryBackoff, on the engine's
// clock) when the model call fails with a transient engine error (the
// local engine crashed, ran out of memory, dropped the connection, or
// the call waited too long for room) and the step is still going. A
// limit, a validation error or the caller's cancel is never retried.
// ctx should carry the step's wait hook (withWaitHook).
func (e *Engine) callRetry(ctx context.Context, rc *runCtx, step, model string, msgs []Message, sh callShape, u *usageSum, rt *retryState) (string, error) {
	text, err := e.call(ctx, rc, step, model, msgs, sh, u)
	if err == nil || rt == nil || rt.n >= 1 || ctx.Err() != nil || !transientModelErr(err) {
		return text, err
	}
	rt.n++
	rt.first = err.Error()
	e.log.Info("flows: retrying a model call after a transient engine error", "run", rc.id, "step", step, "model", model, "err", err)
	if !e.sleep(ctx, retryBackoff) {
		return "", ctx.Err()
	}
	return e.call(ctx, rc, step, model, msgs, sh, u)
}

// sleep waits d on the engine's clock; false when ctx ends first.
func (e *Engine) sleep(ctx context.Context, d time.Duration) bool {
	t := e.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-ctx.Done():
		return false
	}
}

// transientModelErr: err is a temporary engine failure worth one retry.
// A limit or a context error never is.
func transientModelErr(err error) bool {
	var le *limitError
	if err == nil || errors.As(err, &le) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var me *ModelError
	if errors.As(err, &me) {
		return engineTransientText(me.Status, me.Message)
	}
	return engineTransient(err)
}

// stepWait is the wait hook of a prompt or plan step: the run's Waiting
// follows the engine's queue.
func (e *Engine) stepWait(ctx context.Context, rc *runCtx, step string) context.Context {
	return withWaitHook(ctx, func(reason string) { e.setStepWaiting(rc.id, step, reason) })
}

func (rt *retryState) fill(rec *StepRecord) { rec.Retries, rec.RetryError = rt.n, rt.first }

func (u *usageSum) fill(rec *StepRecord) {
	rec.Model, rec.PromptTokens, rec.CompletionTokens, rec.CostUSD, rec.EnergyWh, rec.Calls =
		u.model, u.prompt, u.complete, u.cost, u.energy, u.calls
}

// runStep runs one step (not approve) and fills rec. It answers the jump
// target of an if step ("" = the next step).
func (e *Engine) runStep(ctx context.Context, rc *runCtx, s Step, rec *StepRecord, stepsRun *int) (string, error) {
	switch s.Kind {
	case KindPrompt:
		return "", e.withRetry(ctx, rc, s, rec, stepsRun, func(r *StepRecord) error { return e.stepPrompt(ctx, rc, s, r) })
	case KindPlan:
		return "", e.withRetry(ctx, rc, s, rec, stepsRun, func(r *StepRecord) error { return e.stepPlan(ctx, rc, s, r) })
	case KindFanOut:
		return "", e.stepFanOut(ctx, rc, s, rec, stepsRun)
	case KindSession:
		task, err := render(s.Task, e.env(rc, nil))
		if err != nil {
			return "", err
		}
		rec.Input = task
		var u usageSum
		// s.Model "" keeps the agent profile's model.
		out, err := e.session(ctx, rc, s, s.Agent, s.Model, task, s.Label(), &u)
		u.fill(rec)
		if out != nil {
			rec.Session = out["session"].(string)
			rec.Output = out
		}
		return "", err
	case KindJudge:
		return "", e.stepJudge(ctx, rc, s, rec, stepsRun)
	case KindIf:
		return e.stepIf(rc, s, rec)
	case KindOutput:
		return "", e.stepOutput(rc, s, rec)
	}
	return "", fmt.Errorf("unknown step kind %q", s.Kind)
}

func (e *Engine) stepPrompt(ctx context.Context, rc *runCtx, s Step, rec *StepRecord) error {
	env := e.env(rc, nil)
	user, err := render(s.Template, env)
	if err != nil {
		return err
	}
	var msgs []Message
	if strings.TrimSpace(s.System) != "" {
		sys, err := render(s.System, env)
		if err != nil {
			return err
		}
		msgs = append(msgs, Message{Role: "system", Content: sys})
	}
	msgs = append(msgs, Message{Role: "user", Content: user})
	rec.Input = user
	var u usageSum
	var rt retryState
	defer u.fill(rec)
	defer rt.fill(rec)
	ctx = e.stepWait(ctx, rc, s.ID)
	sh := s.shape(false, "")
	if s.JSON {
		sh = s.shape(true, jsonAnswerSchema)
	}
	text, err := e.callRetry(ctx, rc, s.ID, s.Model, msgs, sh, &u, &rt)
	if err != nil {
		return err
	}
	if !s.JSON {
		rec.Output = text
		return nil
	}
	v, perr := extractJSON(text, false)
	if perr != nil { // one retry with the parse error
		msgs = append(msgs, Message{Role: "assistant", Content: text},
			Message{Role: "user", Content: "That was not valid JSON (" + perr.Error() + "). Reply with only the JSON."})
		if text, err = e.callRetry(ctx, rc, s.ID, s.Model, msgs, sh, &u, &rt); err != nil {
			return err
		}
		if v, perr = extractJSON(text, false); perr != nil {
			return fmt.Errorf("the reply is not JSON: %v", perr)
		}
	}
	rec.Output = v
	return nil
}

func planInstruction(n int) string {
	return fmt.Sprintf("Answer with only a JSON array (no other text) of at most %d objects, each "+
		`{"title": a short title, "kind": one word for the kind of work (for example html, css, copy, code, research), `+
		`"task": the full instructions for whoever does it}.`, n)
}

// jsonAnswerSchema is what a JSON prompt step asks for: an object or an
// array (a fan-out can read its items).
const jsonAnswerSchema = `{"anyOf":[{"type":"object"},{"type":"array"}]}`

// planSchema is the JSON schema of a plan of at most limit items.
func planSchema(limit int) string {
	return fmt.Sprintf(`{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","properties":{`+
		`"title":{"type":"string"},"kind":{"type":"string"},"task":{"type":"string"}},`+
		`"required":["title","kind","task"]}}`, limit)
}

func (e *Engine) stepPlan(ctx context.Context, rc *runCtx, s Step, rec *StepRecord) error {
	user, err := render(s.Template, e.env(rc, nil))
	if err != nil {
		return err
	}
	rec.Input = user
	limit := s.maxItems()
	msgs := []Message{{Role: "system", Content: planInstruction(limit)}, {Role: "user", Content: user}}
	var u usageSum
	var rt retryState
	defer u.fill(rec)
	defer rt.fill(rec)
	ctx = e.stepWait(ctx, rc, s.ID)
	sh := s.shape(true, planSchema(limit))
	text, err := e.callRetry(ctx, rc, s.ID, s.Model, msgs, sh, &u, &rt)
	if err != nil {
		return err
	}
	items, perr := planItems(text, limit)
	if perr != nil {
		msgs = append(msgs, Message{Role: "assistant", Content: text},
			Message{Role: "user", Content: "That was not a usable plan (" + perr.Error() + "). " + planInstruction(limit)})
		if text, err = e.callRetry(ctx, rc, s.ID, s.Model, msgs, sh, &u, &rt); err != nil {
			return err
		}
		if items, perr = planItems(text, limit); perr != nil {
			return fmt.Errorf("the reply is not a plan: %v", perr)
		}
	}
	rec.Output = items
	return nil
}

// planItems extracts the first JSON array of {title, kind, task} from a
// reply, at most max items.
func planItems(text string, limit int) ([]any, error) {
	v, err := extractJSON(text, true)
	if err != nil {
		return nil, err
	}
	arr := v.([]any)
	if len(arr) == 0 {
		return nil, errors.New("the array is empty")
	}
	out := make([]any, 0, min(len(arr), limit))
	for i, x := range arr {
		if i >= limit {
			break
		}
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d is not an object", i+1)
		}
		it := itemFields(m)
		if it["task"] == "" && it["title"] == "" {
			return nil, fmt.Errorf("item %d has no title or task", i+1)
		}
		out = append(out, map[string]any{"title": it["title"], "kind": it["kind"], "task": it["task"]})
	}
	return out, nil
}

func itemFields(m map[string]any) map[string]string {
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case nil:
			return ""
		default:
			return outputText(v)
		}
	}
	return map[string]string{"title": str("title"), "kind": str("kind"), "task": str("task")}
}

// asItem is one fan-out source element as {title, kind, task}: an object
// gives its fields, anything else becomes the title and task.
func asItem(x any) map[string]string {
	if m, ok := x.(map[string]any); ok {
		it := itemFields(m)
		if it["task"] == "" {
			it["task"] = outputText(x)
		}
		return it
	}
	t := outputText(x)
	return map[string]string{"title": t, "kind": "", "task": t}
}

// extractJSON finds the first JSON value in text (the first array when
// wantArray): it tries each "[" (and "{") in order and decodes from there.
func extractJSON(text string, wantArray bool) (any, error) {
	tries := 0
	for i := 0; i < len(text) && tries < 200; i++ {
		c := text[i]
		if c != '[' && (wantArray || c != '{') {
			continue
		}
		tries++
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var v any
		if dec.Decode(&v) != nil {
			continue
		}
		if wantArray {
			if _, ok := v.([]any); !ok {
				continue
			}
		}
		return v, nil
	}
	if wantArray {
		return nil, errors.New("no JSON array found")
	}
	return nil, errors.New("no JSON object or array found")
}

func (e *Engine) stepFanOut(ctx context.Context, rc *runCtx, s Step, rec *StepRecord, stepsRun *int) error {
	src, ok := rc.outputs[s.From].([]any)
	if !ok {
		return fmt.Errorf("the output of step %s is not a list", s.From)
	}
	items := make([]ItemRecord, len(src))
	results := make([]any, len(src))
	// A continued run keeps the items an earlier attempt of this step
	// passed, when they would run the same input on the same model: no
	// model call, no charge, and they do not count toward max_steps.
	prev, hadPrev := rc.prior[s.ID]
	delete(rc.prior, s.ID)
	reused := make([]bool, len(src))
	nReused := 0
	for i, x := range src {
		it := asItem(x)
		model := routeModel(s, it["kind"])
		items[i] = ItemRecord{Index: i, Title: it["title"], Kind: it["kind"], Model: model, Route: model, State: StepPending}
		if !hadPrev || i >= len(prev.Items) {
			continue
		}
		if p, ok := e.reusable(rc, s, prev.Items[i], it, model); ok {
			items[i], results[i], reused[i] = p, p.Output, true
			nReused++
		}
	}
	rec.Input = fmt.Sprintf("%d items from %s", len(src), s.From)
	if nReused > 0 {
		rec.Input += fmt.Sprintf(" (%d kept from the last attempt)", nReused)
	}
	*stepsRun += len(src) - nReused
	if *stepsRun-rc.baseSteps > rc.lim.MaxSteps {
		return &limitError{fmt.Sprintf("more than %d steps (%d fan-out items)", rc.lim.MaxSteps, len(src)-nReused)}
	}
	par := rc.lim.MaxParallel
	if s.MaxParallel > 0 {
		par = min(par, s.MaxParallel)
	}
	gctx, gcancel := context.WithCancelCause(ctx)
	defer gcancel(nil)
	var mu sync.Mutex
	var firstErr, cancelErr error
	failed := 0
	// publish hands the items to the engine with mu held, so two
	// snapshots never land out of order (lock order: mu, then e.mu).
	publishLocked := func(waitOnly bool) { e.setItems(rc.id, s.ID, slices.Clone(items), waitOnly) }
	publish := func() {
		mu.Lock()
		publishLocked(false)
		mu.Unlock()
	}
	publish()
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, x := range src {
		if reused[i] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				mu.Lock()
				items[i].State = StepCanceled
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			if gctx.Err() != nil {
				mu.Lock()
				items[i].State = StepCanceled
				mu.Unlock()
				return
			}
			mu.Lock()
			items[i].State, items[i].Started = StepRunning, e.clock.Now().UTC()
			mu.Unlock()
			publish()
			// While the engine holds this item's model call, the item
			// (and the run) show why.
			ictx := withWaitHook(gctx, func(reason string) {
				mu.Lock()
				defer mu.Unlock()
				if items[i].State != StepRunning || items[i].Waiting == reason {
					return
				}
				items[i].Waiting = reason
				publishLocked(true)
			})
			ir, out, err := e.fanItemRetry(ictx, rc, s, i, asItem(x), items[i], stepsRun)
			mu.Lock()
			ir.Ended, ir.Waiting = e.clock.Now().UTC(), ""
			switch {
			case err == nil:
				ir.State = StepPassed
				results[i] = out
			case gctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
				// stopped because a sibling failed (or the step ended)
				ir.State, ir.Error = StepCanceled, err.Error()
			default:
				ir.State, ir.Error = StepFailed, err.Error()
				failed++
				if firstErr == nil {
					firstErr = err
				}
				// A transient engine failure (it persisted after the
				// retry) fails only this item; anything else stops the
				// siblings, as before. With min_ok only a limit does.
				var le *limitError
				stopAll := errors.As(err, &le) ||
					(s.MinOK <= 0 && (s.fanAs() == KindSession || !transientModelErr(err)))
				if cancelErr == nil && stopAll {
					cancelErr = err
					gcancel(err)
				}
			}
			items[i] = ir
			publishLocked(false)
			mu.Unlock()
		}()
	}
	wg.Wait()
	rec.Items = items
	for _, it := range items {
		rec.PromptTokens += it.PromptTokens
		rec.CompletionTokens += it.CompletionTokens
		rec.CostUSD += it.CostUSD
		rec.EnergyWh += it.EnergyWh
		rec.Retries += it.Retries
		if rec.RetryError == "" {
			rec.RetryError = it.RetryError
		}
	}
	if firstErr != nil {
		var le *limitError
		if errors.As(cancelErr, &le) {
			// the run is limited (also when an earlier item had failed for
			// good with a transient error): keep the limit's own reason
			return cancelErr
		}
		if s.MinOK > 0 {
			passed := 0
			for _, it := range items {
				if it.State == StepPassed {
					passed++
				}
			}
			if passed >= s.MinOK && ctx.Err() == nil {
				// enough passed: failed items give null
				rec.Input += fmt.Sprintf(" (%d failed, %d had to pass)", failed, s.MinOK)
				rec.Output = results
				return nil
			}
			return fmt.Errorf("%d of %d items failed, %d had to pass: %w", failed, len(items), s.MinOK, firstErr)
		}
		return fmt.Errorf("%d of %d items failed: %w", failed, len(items), firstErr)
	}
	if err := context.Cause(ctx); ctx.Err() != nil {
		return err
	}
	rec.Output = results
	return nil
}

// reusable answers p as the record of a kept item when a continued
// fan-out can keep it: it passed, it is the same item, it asked the same
// model, and the template renders the same input now. The usage is not
// charged again, so the kept record carries none.
func (e *Engine) reusable(rc *runCtx, s Step, p ItemRecord, it map[string]string, model string) (ItemRecord, bool) {
	route := p.Route
	if route == "" {
		route = p.Model // a record from before Route
	}
	if p.State != StepPassed || route != model || p.Title != it["title"] || p.Kind != it["kind"] {
		return ItemRecord{}, false
	}
	text, err := render(s.Template, e.env(rc, it))
	if err != nil || text != p.Input {
		return ItemRecord{}, false
	}
	p.Route, p.Reused, p.Waiting = model, true, ""
	p.PromptTokens, p.CompletionTokens, p.Calls, p.CostUSD, p.EnergyWh = 0, 0, 0, 0, 0
	p.Retries, p.RetryError = 0, ""
	return p, true
}

// routeModel is the model of the first route whose kind matches (case
// does not matter), else the default model.
func routeModel(s Step, kind string) string {
	for _, r := range s.Routes {
		if strings.EqualFold(strings.TrimSpace(r.Kind), strings.TrimSpace(kind)) && r.Model != "" {
			return r.Model
		}
	}
	return s.DefaultModel
}

// fanItemRetry is fanItem, run again under the step's retry setting
// (each retry counts toward max_steps; the record keeps every try's usage
// and the first error).
func (e *Engine) fanItemRetry(ctx context.Context, rc *runCtx, s Step, i int, it map[string]string, ir ItemRecord, stepsRun *int) (ItemRecord, any, error) {
	base := ir
	var spent ItemRecord
	first := ""
	for try := 0; ; try++ {
		cur, out, err := e.fanItem(ctx, rc, s, i, it, base)
		own := cur
		cur.PromptTokens += spent.PromptTokens
		cur.CompletionTokens += spent.CompletionTokens
		cur.Calls += spent.Calls
		cur.CostUSD += spent.CostUSD
		cur.EnergyWh += spent.EnergyWh
		cur.Retries += spent.Retries
		if try > 0 {
			cur.Attempts = try + 1
			if cur.RetryError == "" {
				cur.RetryError = first
			}
		}
		if err == nil || try >= s.retries() || !retryable(ctx, err) {
			return cur, out, err
		}
		if first == "" {
			first = err.Error()
		}
		spent.PromptTokens += own.PromptTokens
		spent.CompletionTokens += own.CompletionTokens
		spent.Calls += own.Calls
		spent.CostUSD += own.CostUSD
		spent.EnergyWh += own.EnergyWh
		spent.Retries += own.Retries
		if cerr := e.countStep(rc, stepsRun, fmt.Sprintf("a retry of item %d of %s", i+1, s.ID)); cerr != nil {
			return cur, nil, cerr
		}
		e.log.Info("flows: retrying a fan-out item", "run", rc.id, "step", s.ID, "item", i+1, "try", try+2, "err", err)
		if d := s.backoff(); d > 0 && !e.sleep(ctx, d) {
			return cur, nil, ctx.Err()
		}
	}
}

func (e *Engine) fanItem(ctx context.Context, rc *runCtx, s Step, i int, it map[string]string, ir ItemRecord) (ItemRecord, any, error) {
	text, err := render(s.Template, e.env(rc, it))
	if err != nil {
		return ir, nil, err
	}
	ir.Input = text
	if s.fanAs() == KindSession {
		name := s.Label() + " · " + it["title"]
		if it["title"] == "" {
			name = s.Label() + " · " + strconv.Itoa(i+1)
		}
		var u usageSum
		out, err := e.session(ctx, rc, s, s.Agent, ir.Model, text, name, &u)
		ir.PromptTokens, ir.CompletionTokens, ir.CostUSD, ir.EnergyWh, ir.Calls = u.prompt, u.complete, u.cost, u.energy, u.calls
		if out != nil {
			ir.Session = out["session"].(string)
			ir.Output = out
		}
		return ir, generic(out), err
	}
	var u usageSum
	var rt retryState
	reply, err := e.callRetry(ctx, rc, s.ID, ir.Model, []Message{{Role: "user", Content: text}}, s.shape(false, ""), &u, &rt)
	ir.PromptTokens, ir.CompletionTokens, ir.CostUSD, ir.EnergyWh, ir.Calls = u.prompt, u.complete, u.cost, u.energy, u.calls
	ir.Retries, ir.RetryError = rt.n, rt.first
	if u.model != "" {
		ir.Model = u.model
	}
	if err != nil {
		return ir, nil, err
	}
	ir.Output = reply
	return ir, reply, nil
}

// session starts an agent session and waits for its final state. When
// the runner is a SessionMeter, the session's model usage is charged to
// the run as it happens (and added to u): a session that takes the run
// past a limit is stopped and the step ends with the limit error.
func (e *Engine) session(ctx context.Context, rc *runCtx, s Step, agent, model, task, name string, u *usageSum) (map[string]any, error) {
	if e.o.Sessions == nil {
		return nil, errors.New("agent sessions are not available")
	}
	id, err := "", error(nil)
	// Only a session step resumes (fan-out items run in parallel and
	// never touch rc.resume).
	resume := ""
	if s.Kind == KindSession {
		resume, rc.resume = rc.resume, ""
	}
	if rs, ok := e.o.Sessions.(SessionResumer); ok && resume != "" {
		// Continue: the step stopped inside this session; carry on in its
		// workspace and conversation rather than starting over.
		id = resume
		if err = rs.Resume(ctx, id); err != nil {
			return nil, fmt.Errorf("session %s not continued: %w", id, err)
		}
	} else {
		req := SessionRequest{
			Agent: agent, Model: model, Workspace: s.workspace(), Repo: s.Repo, Folder: s.Folder, Task: task,
			Name: rc.snap.Name + " · " + name, Tags: []string{"flow:" + rc.snap.ID}, Run: rc.id, Flow: rc.snap.ID, Step: s.ID,
			Owner: rc.snap.Owner, MaxAttempts: s.MaxAttempts, EscalateTo: s.EscalateTo, MaxMinutes: s.MaxMinutes,
		}
		if s.Kind == KindSession && s.workspace() == WorkspaceStep {
			if err := stepWorkspace(rc, s, &req); err != nil {
				return nil, err
			}
		}
		id, err = e.o.Sessions.Start(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("session not started: %w", err)
		}
	}
	e.update(rc.id, func(x *Run) {
		if !slices.Contains(x.Sessions, id) {
			x.Sessions = append(x.Sessions, id)
		}
	})
	out := map[string]any{"session": id, "state": "", "summary": "", "workspace": "", "files": []SessionFile{},
		"checks": "", "diff_stat": "", "final": ""}
	wctx, stopMeter := ctx, func() {}
	if m, ok := e.o.Sessions.(SessionMeter); ok {
		var mcancel context.CancelCauseFunc
		wctx, mcancel = context.WithCancelCause(ctx)
		defer mcancel(nil)
		var umu sync.Mutex
		stop := m.Meter(id, func(rep ModelReply) {
			used := rep.Model
			if used == "" {
				used = model
			}
			umu.Lock()
			u.add(rep, used)
			umu.Unlock()
			if err := e.charge(rc, rep, used); err != nil {
				mcancel(err)
			}
		})
		var once sync.Once
		stopMeter = func() { once.Do(stop) }
		defer stopMeter()
	}
	res, err := e.o.Sessions.Wait(wctx, id)
	if err == nil {
		out["state"], out["summary"], out["workspace"] = res.State, res.Summary, res.Workspace
		out["checks"], out["diff_stat"], out["final"] = res.Checks, res.DiffStat, res.Final
		if res.Files != nil {
			out["files"] = res.Files
		}
	} else if wctx.Err() != nil && !errors.Is(context.Cause(wctx), errShutdown) {
		if serr := e.o.Sessions.Stop(id); serr != nil {
			e.log.Warn("flows: session not stopped", "session", id, "err", serr)
		}
	}
	stopMeter() // no usage is charged after this
	var le *limitError
	if errors.As(context.Cause(wctx), &le) {
		err = le // over a limit (also when the last record came as the session ended)
	}
	return out, err
}

// stepWorkspace points a session step with workspace "step" at the
// session(s) of step s.From: one session's workspace is used in place
// (SameAs); a fan-out run as sessions has each item's files copied into a
// new scratch folder under parts/<n>-<slug>/ (Seeds).
func stepWorkspace(rc *runCtx, s Step, req *SessionRequest) error {
	switch src := rc.outputs[s.From].(type) {
	case map[string]any:
		id, _ := src["session"].(string)
		if id == "" {
			return fmt.Errorf("step %s has no session to work in", s.From)
		}
		req.SameAs = id
		return nil
	case []any:
		recs := rc.items[s.From]
		for i, x := range src {
			m, _ := x.(map[string]any)
			id, _ := m["session"].(string)
			if id == "" {
				continue
			}
			title := ""
			if i < len(recs) {
				title = recs[i].Title
			}
			req.Seeds = append(req.Seeds, SessionSeed{Session: id, Into: fmt.Sprintf("parts/%d-%s", i+1, partSlug(title))})
		}
		if len(req.Seeds) == 0 {
			return fmt.Errorf("step %s has no sessions to take files from", s.From)
		}
		return nil
	}
	return fmt.Errorf("step %s has no session output (was it skipped?)", s.From)
}

// partSlug is a short folder name for a part's title: lower-case letters
// and digits with "-" between words, at most 40 bytes; "part" when empty.
func partSlug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			if b.Len() >= 40 {
				break
			}
			continue
		}
		dash = true
	}
	if b.Len() == 0 {
		return "part"
	}
	return b.String()
}

// stepJudge judges the workspace of the session step s.On (E2). The
// judge's calls are charged to the run as they happen, and so is the usage
// of attempts the feedback resumes (SessionMeter): past a limit the
// judging stops and the session is stopped. The step passes whatever the
// verdict; its output is {passed, gates_passed, gates_total, failed,
// session, report}, for an if step (equals, path "passed"). When s.On is
// a prompt step or a fan-out run as prompts, the answer is judged instead
// (judgeText).
func (e *Engine) stepJudge(ctx context.Context, rc *runCtx, s Step, rec *StepRecord, stepsRun *int) error {
	if e.o.Judge == nil {
		return errors.New("judging is not available here")
	}
	if i := rc.snap.stepIndex(s.On); i >= 0 {
		switch src := rc.snap.Steps[i]; {
		case src.Kind == KindPrompt:
			return e.judgeText(ctx, rc, s, src, rec, stepsRun)
		case src.Kind == KindFanOut && src.fanAs() == KindPrompt:
			return e.judgeText(ctx, rc, s, src, rec, stepsRun)
		}
	}
	prev, _ := rc.outputs[s.On].(map[string]any)
	sid, _ := prev["session"].(string)
	if sid == "" {
		return fmt.Errorf("step %s has no session to judge", s.On)
	}
	rec.Session = sid
	rec.Input = fmt.Sprintf("judge %s (%s) on %d criteria", s.On, sid, len(s.Criteria))
	var u usageSum
	var umu sync.Mutex
	defer func() {
		umu.Lock()
		u.fill(rec)
		umu.Unlock()
	}()
	jctx, jcancel := context.WithCancelCause(ctx)
	defer jcancel(nil)
	charge := func(rep ModelReply, fallback string) error {
		used := rep.Model
		if used == "" {
			used = fallback
		}
		umu.Lock()
		u.add(rep, used)
		umu.Unlock()
		return e.charge(rc, rep, used)
	}
	if m, ok := e.o.Sessions.(SessionMeter); ok {
		stop := m.Meter(sid, func(rep ModelReply) {
			if err := charge(rep, ""); err != nil {
				jcancel(err)
			}
		})
		defer stop()
	}
	res, err := e.o.Judge.Judge(jctx, JudgeRequest{Session: sid, Criteria: s.Criteria, Gates: s.Gates, Persona: s.Persona,
		Model: s.Model, Run: rc.id, Flow: rc.snap.ID, Step: s.ID, Owner: rc.snap.Owner,
		Charge: func(rep ModelReply) error {
			err := charge(rep, s.Model)
			if err != nil {
				jcancel(err)
			}
			return err
		}})
	if err != nil {
		if jctx.Err() != nil && !errors.Is(context.Cause(jctx), errShutdown) && e.o.Sessions != nil {
			_ = e.o.Sessions.Stop(sid) // a resumed attempt may be going
		}
		var le *limitError
		if errors.As(context.Cause(jctx), &le) {
			return le
		}
		return err
	}
	rec.Output = map[string]any{"passed": res.Passed, "gates_passed": res.GatesPassed, "gates_total": res.GatesTotal,
		"failed": nonNilStrings(res.Failed), "session": sid, "report": res.Report}
	return nil
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// maxRevisionText bounds the texts a Revision record keeps.
const maxRevisionText = 4000

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// judgeText judges the answer of the prompt step src (or each item of
// the fan-out src, one after another) with the rules of a judge step. On a
// failed gate with iterations left, the revise call asks the same model
// again with [the original messages, its answer, the feedback as a user
// turn]; it is charged to the run and counts as a step toward max_steps.
// A revised answer replaces the step's output (or the item's) for the
// steps that follow, and the judge step's output carries it as "output"
// with "revised": true, so a continued run reads it back. Every revise
// call is recorded in rec.Revisions. Fan-out items that have no output
// (a failed item under min_ok) are skipped and left out of the verdict.
func (e *Engine) judgeText(ctx context.Context, rc *runCtx, s, src Step, rec *StepRecord, stepsRun *int) error {
	if _, ok := rc.outputs[s.On]; !ok {
		return fmt.Errorf("step %s has no answer to judge (was it skipped?)", s.On)
	}
	if rc.failed[s.On] {
		// on_error let the run go on; its output is the error, which a
		// judge must not judge, nor revise into an answer
		return fmt.Errorf("step %s failed; nothing to judge", s.On)
	}
	var u usageSum
	var umu sync.Mutex
	defer func() {
		umu.Lock()
		u.fill(rec)
		umu.Unlock()
	}()
	jctx, jcancel := context.WithCancelCause(ctx)
	defer jcancel(nil)
	ctxErr := func(err error) error {
		var le *limitError
		if errors.As(context.Cause(jctx), &le) {
			return le
		}
		return err
	}
	charge := func(rep ModelReply, fallback string) error {
		used := rep.Model
		if used == "" {
			used = fallback
		}
		umu.Lock()
		u.add(rep, used)
		umu.Unlock()
		err := e.charge(rc, rep, used)
		if err != nil {
			jcancel(err)
		}
		return err
	}
	// one judges one answer; item is 1-based (0 = the step itself). Each
	// answer gets its own transient retry (retryState: one per step or
	// item), and its retries are added to the record.
	one := func(item int, msgs []Message, model string, sh callShape, answer any) (JudgeResult, any, bool, error) {
		var rt retryState
		defer func() {
			umu.Lock()
			rec.Retries += rt.n
			if rec.RetryError == "" {
				rec.RetryError = rt.first
			}
			umu.Unlock()
		}()
		text := outputText(answer)
		latest, revised := answer, false
		var revs []Revision
		revise := func(rctx context.Context, feedback string) (string, error) {
			rev := Revision{Item: item, Feedback: clipText(feedback, maxRevisionText), Model: model}
			defer func() { revs = append(revs, rev) }()
			if err := e.countStep(rc, stepsRun, "a revision for "+s.ID); err != nil {
				rev.Error = err.Error()
				jcancel(err)
				return "", err
			}
			m := append(slices.Clone(msgs), Message{Role: "assistant", Content: text}, Message{Role: "user", Content: feedback})
			var ru usageSum
			reply, err := e.callRetry(rctx, rc, s.ID, model, m, sh, &ru, &rt)
			umu.Lock()
			u.prompt, u.complete, u.cost, u.energy, u.calls = u.prompt+ru.prompt, u.complete+ru.complete, u.cost+ru.cost, u.energy+ru.energy, u.calls+ru.calls
			if ru.model != "" {
				u.model = ru.model
				rev.Model = ru.model
			}
			umu.Unlock()
			rev.Tokens = ru.prompt + ru.complete
			if err != nil {
				rev.Error = err.Error()
				var le *limitError
				if errors.As(err, &le) {
					jcancel(le)
				}
				return "", err
			}
			out := any(reply)
			if sh.json {
				v, perr := extractJSON(reply, false)
				if perr != nil {
					rev.Error = "the revised answer is not JSON: " + perr.Error()
					return "", errors.New(rev.Error)
				}
				out = v
			}
			rev.Output = clipText(reply, maxRevisionText)
			text, latest, revised = reply, out, true
			return reply, nil
		}
		res, err := e.o.Judge.Judge(jctx, JudgeRequest{Criteria: s.Criteria, Gates: s.Gates, Persona: s.Persona,
			Model: s.Model, Run: rc.id, Flow: rc.snap.ID, Step: s.ID, Owner: rc.snap.Owner,
			Charge: func(rep ModelReply) error { return charge(rep, s.Model) },
			Text:   text, Task: lastUser(msgs), Revise: revise})
		for k := range revs {
			if k < len(res.Report.Feedback) {
				revs[k].Gate, revs[k].Iteration = res.Report.Feedback[k].Gate, res.Report.Feedback[k].Iteration
			}
		}
		rec.Revisions = append(rec.Revisions, revs...)
		return res, latest, revised, err
	}
	if src.Kind == KindPrompt {
		msgs, err := e.promptMessages(rc, src)
		if err != nil {
			return err
		}
		sh := src.shape(false, "")
		if src.JSON {
			sh = src.shape(true, jsonAnswerSchema)
		}
		rec.Input = fmt.Sprintf("judge the answer of %s on %d criteria", s.On, len(s.Criteria))
		res, latest, revised, err := one(0, msgs, src.Model, sh, rc.outputs[s.On])
		if err != nil {
			return ctxErr(err)
		}
		out := map[string]any{"passed": res.Passed, "gates_passed": res.GatesPassed, "gates_total": res.GatesTotal,
			"failed": nonNilStrings(res.Failed), "session": "", "report": res.Report, "revised": revised}
		if revised {
			rc.outputs[s.On] = generic(latest)
			out["output"] = latest
		}
		rec.Output = out
		return nil
	}
	list, _ := rc.outputs[s.On].([]any)
	if list == nil {
		return fmt.Errorf("step %s has no items to judge", s.On)
	}
	recs := rc.items[s.On]
	list = slices.Clone(list)
	rec.Input = fmt.Sprintf("judge %d answers of %s on %d criteria", len(list), s.On, len(s.Criteria))
	passed, anyRevised, judged := true, false, 0
	gp, gt := 0, 0
	var failed []string
	items := make([]any, len(list))
	// A continued judge keeps the verdicts an earlier attempt reached for
	// answers that are still the same (their revisions are already in the
	// fan-out's output, see loop): no judge call, no revision paid twice.
	prev := priorJudged(rc, s.ID)
	sums := make([]string, len(list))
	nKept := 0
	for i, v := range list {
		if v == nil || i >= len(recs) {
			items[i] = map[string]any{"index": i, "skipped": true}
			continue
		}
		if p, ok := prev[i]; ok && p.sum == answerSum(v) {
			items[i], sums[i] = p.item, p.sum
			judged++
			nKept++
			passed = passed && p.passed
			gp += p.gp
			gt += p.gt
			for _, f := range p.failed {
				if !slices.Contains(failed, f) {
					failed = append(failed, f)
				}
			}
			continue
		}
		ir := recs[i]
		model := ir.Route
		if model == "" {
			model = ir.Model
		}
		msgs := []Message{{Role: "user", Content: ir.Input}}
		res, latest, revised, err := one(i+1, msgs, model, src.shape(false, ""), v)
		if err != nil {
			// Keep what was reached so far: the revisions (paid for) and
			// the verdicts, for Continue.
			kept := make([]any, i)
			for k := range kept {
				if m, ok := items[k].(map[string]any); ok && sums[k] != "" {
					m = maps.Clone(m)
					m["answer_sum"] = sums[k]
					kept[k] = m
				} else {
					kept[k] = items[k]
				}
			}
			rec.Output = generic(map[string]any{"partial": true, "revised": anyRevised, "output": list, "items": kept})
			return ctxErr(fmt.Errorf("item %d: %w", i+1, err))
		}
		judged++
		passed = passed && res.Passed
		gp += res.GatesPassed
		gt += res.GatesTotal
		for _, f := range res.Failed {
			if !slices.Contains(failed, f) {
				failed = append(failed, f)
			}
		}
		if revised {
			list[i], anyRevised = generic(latest), true
		}
		sums[i] = answerSum(list[i])
		items[i] = map[string]any{"index": i, "title": ir.Title, "passed": res.Passed, "gates_passed": res.GatesPassed,
			"gates_total": res.GatesTotal, "failed": nonNilStrings(res.Failed), "revised": revised, "report": res.Report}
	}
	if nKept > 0 {
		rec.Input += fmt.Sprintf(" (%d kept from the last attempt)", nKept)
	}
	out := map[string]any{"passed": passed && judged > 0, "gates_passed": gp, "gates_total": gt,
		"failed": nonNilStrings(failed), "session": "", "items": items, "revised": anyRevised}
	if anyRevised {
		rc.outputs[s.On] = list
		out["output"] = list
	}
	rec.Output = out
	return nil
}

// judgedItem is one verdict a stopped judge step reached (priorJudged).
type judgedItem struct {
	item   any
	sum    string
	passed bool
	gp, gt int
	failed []string
}

// priorJudged takes the earlier attempt of judge step id (rc.prior, once)
// and returns the verdicts its partial output kept, by item index.
func priorJudged(rc *runCtx, id string) map[int]judgedItem {
	prev, ok := rc.prior[id]
	delete(rc.prior, id)
	out, _ := prev.Output.(map[string]any)
	if !ok || out == nil || out["partial"] != true {
		return nil
	}
	list, _ := out["items"].([]any)
	got := map[int]judgedItem{}
	for i, x := range list {
		m, _ := x.(map[string]any)
		sum, _ := m["answer_sum"].(string)
		p, isBool := m["passed"].(bool)
		if sum == "" || !isBool {
			continue
		}
		num := func(k string) int { f, _ := m[k].(float64); return int(f) }
		var failed []string
		if fl, ok := m["failed"].([]any); ok {
			for _, f := range fl {
				if s, ok := f.(string); ok {
					failed = append(failed, s)
				}
			}
		}
		item := maps.Clone(m)
		delete(item, "answer_sum")
		got[i] = judgedItem{item: item, sum: sum, passed: p, gp: num("gates_passed"), gt: num("gates_total"), failed: failed}
	}
	return got
}

// answerSum fingerprints an answer, so a kept verdict is reused only for
// the same text.
func answerSum(v any) string {
	h := sha256.Sum256([]byte(outputText(v)))
	return hex.EncodeToString(h[:12])
}

// lastUser is the content of the last user message.
func lastUser(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// promptMessages are the messages of prompt step s as it ran (rendered
// again: templates read earlier steps only, so the text is the same but
// for {date} and {time} across midnight or a minute).
func (e *Engine) promptMessages(rc *runCtx, s Step) ([]Message, error) {
	env := e.env(rc, nil)
	user, err := render(s.Template, env)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if strings.TrimSpace(s.System) != "" {
		sys, err := render(s.System, env)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, Message{Role: "system", Content: sys})
	}
	return append(msgs, Message{Role: "user", Content: user}), nil
}

func (e *Engine) stepIf(rc *runCtx, s Step, rec *StepRecord) (string, error) {
	v := rc.outputs[s.On]
	ok, err := evalTest(s, v)
	if err != nil {
		return "", err
	}
	rec.Input = fmt.Sprintf("%s %s %q", s.On, s.Test, s.Value)
	if s.Test == TestJSON || (s.Test == TestEquals && strings.TrimSpace(s.Path) != "") {
		rec.Input = fmt.Sprintf("%s.%s == %q", s.On, s.Path, s.Value)
	}
	rec.Output = ok
	branch, to := "then", s.Then
	if !ok {
		branch, to = "else", s.Else
	}
	target := to
	if target == "" {
		target = "next"
	}
	rec.Jump = branch + " → " + target
	return to, nil
}

// evalTest evaluates an if step's test against an output.
func evalTest(s Step, v any) (bool, error) {
	text := outputText(v)
	switch s.Test {
	case TestContains:
		return strings.Contains(strings.ToLower(text), strings.ToLower(s.Value)), nil
	case TestEquals:
		if strings.TrimSpace(s.Path) != "" { // a field of the output (a judge step's "passed")
			got, ok := lookup(parsed(v), s.Path)
			if !ok {
				return false, nil
			}
			return strings.TrimSpace(outputText(got)) == strings.TrimSpace(s.Value), nil
		}
		return strings.TrimSpace(text) == strings.TrimSpace(s.Value), nil
	case TestMatches:
		re, err := regexp.Compile(s.Value)
		if err != nil {
			return false, err
		}
		return re.MatchString(text), nil
	case TestJSON:
		got, ok := lookup(parsed(v), s.Path)
		if !ok {
			return false, nil
		}
		if str, isStr := got.(string); isStr {
			return str == s.Value, nil
		}
		return outputText(got) == strings.TrimSpace(s.Value), nil
	case TestState:
		m, _ := v.(map[string]any)
		st, _ := m["state"].(string)
		return st == strings.TrimSpace(s.Value), nil
	}
	return false, fmt.Errorf("unknown test %q", s.Test)
}

// parsed is v, or the JSON in it when v is a reply text (nil when it
// holds none).
func parsed(v any) any {
	str, ok := v.(string)
	if !ok {
		return v
	}
	out, err := extractJSON(str, false)
	if err != nil {
		return nil
	}
	return out
}

// lookup follows a dotted path (numbers index arrays) into a JSON value.
func lookup(v any, path string) (any, bool) {
	for _, part := range strings.Split(strings.Trim(path, "."), ".") {
		switch x := v.(type) {
		case map[string]any:
			n, ok := x[part]
			if !ok {
				return nil, false
			}
			v = n
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func (e *Engine) stepOutput(rc *runCtx, s Step, rec *StepRecord) error {
	env := e.env(rc, nil)
	text, err := render(s.Template, env)
	if err != nil {
		return err
	}
	rec.Input = text
	n := Notice{Kind: "flow", Level: "info", Title: rc.snap.Name, Body: text, Link: rc.link(),
		Run: rc.id, Flow: rc.snap.ID, Step: s.ID, Owner: rc.snap.Owner}
	switch s.To {
	case ToNotify, ToInbox:
		if e.o.Notifier == nil {
			return errors.New("notifications are not available")
		}
		if s.To == ToNotify {
			err = e.o.Notifier.Notify(n)
		} else {
			_, err = e.o.Notifier.Post(n)
		}
		if err != nil {
			return err
		}
		rec.Output = text
	case ToFile:
		name := "output.txt"
		if strings.TrimSpace(s.File) != "" {
			fn, err := render(s.File, env)
			if err != nil {
				return err
			}
			if c := sanitizeFile(fn); c != "" {
				name = c
			}
		}
		rel, err := e.runs.writeFile(rc.id, e.runs.uniqueFile(rc.id, name), []byte(text))
		if err != nil {
			return err
		}
		rec.File = rel
		rec.Output = map[string]any{"file": rel, "bytes": len(text)}
	default:
		return fmt.Errorf("unknown output target %q", s.To)
	}
	return nil
}
