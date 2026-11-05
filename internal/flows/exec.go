package flows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		outputs: map[string]any{}, known: map[string]bool{}}
	for _, s := range rc.snap.Steps {
		rc.known[s.ID] = true
	}
	recs, err := e.runs.steps(r.ID)
	if err != nil {
		return outcome{StateFailed, "steps not read: " + err.Error()}
	}
	for _, rec := range recs {
		if rec.State == StepPassed {
			rc.outputs[rec.ID] = rec.Output
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
		if stepsRun > rc.lim.MaxSteps {
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
		e.update(r.ID, func(x *Run) { x.Step, x.PC, x.StepsRun = s.ID, pc, stepsRun })
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
			e.writeStep(r.ID, rec)
			return out
		}
		rec.State = StepPassed
		rec.Output = generic(rec.Output)
		rc.outputs[s.ID] = rec.Output
		e.writeStep(r.ID, rec)
		switch jump {
		case "":
			pc++
		case End:
			pc = len(rc.snap.Steps)
		default:
			pc = rc.snap.stepIndex(jump)
		}
		if pc >= len(rc.snap.Steps) {
			e.update(r.ID, func(x *Run) { x.PC = len(rc.snap.Steps) })
		}
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
		time: now.Format("15:04"), outputs: rc.outputs, known: rc.known, item: item}
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
	case r.Tokens > rc.lim.MaxTokens:
		return &limitError{fmt.Sprintf("used %d tokens, over the limit of %d", r.Tokens, rc.lim.MaxTokens)}
	case r.CostUSD > rc.lim.MaxCostUSD:
		return &limitError{fmt.Sprintf("cost $%.4f, over the limit of $%.2f", r.CostUSD, rc.lim.MaxCostUSD)}
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

// call runs one completion and charges it.
func (e *Engine) call(ctx context.Context, rc *runCtx, step, model string, msgs []Message, wantJSON bool, u *usageSum) (string, error) {
	if e.o.Models == nil {
		return "", errors.New("no model caller is available")
	}
	reply, err := e.o.Models.Complete(ctx, ModelRequest{Model: model, Messages: msgs, JSON: wantJSON,
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
func (e *Engine) callRetry(ctx context.Context, rc *runCtx, step, model string, msgs []Message, wantJSON bool, u *usageSum, rt *retryState) (string, error) {
	text, err := e.call(ctx, rc, step, model, msgs, wantJSON, u)
	if err == nil || rt == nil || rt.n >= 1 || ctx.Err() != nil || !transientModelErr(err) {
		return text, err
	}
	rt.n++
	rt.first = err.Error()
	e.log.Info("flows: retrying a model call after a transient engine error", "run", rc.id, "step", step, "model", model, "err", err)
	if !e.sleep(ctx, retryBackoff) {
		return "", ctx.Err()
	}
	return e.call(ctx, rc, step, model, msgs, wantJSON, u)
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
		return "", e.stepPrompt(ctx, rc, s, rec)
	case KindPlan:
		return "", e.stepPlan(ctx, rc, s, rec)
	case KindFanOut:
		return "", e.stepFanOut(ctx, rc, s, rec, stepsRun)
	case KindSession:
		task, err := render(s.Task, e.env(rc, nil))
		if err != nil {
			return "", err
		}
		rec.Input = task
		var u usageSum
		out, err := e.session(ctx, rc, s, s.Agent, "", task, s.Label(), &u)
		u.fill(rec)
		if out != nil {
			rec.Session = out["session"].(string)
			rec.Output = out
		}
		return "", err
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
	text, err := e.callRetry(ctx, rc, s.ID, s.Model, msgs, s.JSON, &u, &rt)
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
		if text, err = e.callRetry(ctx, rc, s.ID, s.Model, msgs, true, &u, &rt); err != nil {
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
	text, err := e.callRetry(ctx, rc, s.ID, s.Model, msgs, true, &u, &rt)
	if err != nil {
		return err
	}
	items, perr := planItems(text, limit)
	if perr != nil {
		msgs = append(msgs, Message{Role: "assistant", Content: text},
			Message{Role: "user", Content: "That was not a usable plan (" + perr.Error() + "). " + planInstruction(limit)})
		if text, err = e.callRetry(ctx, rc, s.ID, s.Model, msgs, true, &u, &rt); err != nil {
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
	rec.Input = fmt.Sprintf("%d items from %s", len(src), s.From)
	*stepsRun += len(src)
	if *stepsRun > rc.lim.MaxSteps {
		return &limitError{fmt.Sprintf("more than %d steps (%d fan-out items)", rc.lim.MaxSteps, len(src))}
	}
	par := rc.lim.MaxParallel
	if s.MaxParallel > 0 {
		par = min(par, s.MaxParallel)
	}
	gctx, gcancel := context.WithCancelCause(ctx)
	defer gcancel(nil)
	items := make([]ItemRecord, len(src))
	results := make([]any, len(src))
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
	for i, x := range src {
		it := asItem(x)
		items[i] = ItemRecord{Index: i, Title: it["title"], Kind: it["kind"], Model: routeModel(s, it["kind"]), State: StepPending}
	}
	publish()
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, x := range src {
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
			ir, out, err := e.fanItem(ictx, rc, s, i, asItem(x), items[i])
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
				// siblings, as before.
				var le *limitError
				if cancelErr == nil && (errors.As(err, &le) || s.fanAs() == KindSession || !transientModelErr(err)) {
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
		return fmt.Errorf("%d of %d items failed: %w", failed, len(items), firstErr)
	}
	if err := context.Cause(ctx); ctx.Err() != nil {
		return err
	}
	rec.Output = results
	return nil
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
	reply, err := e.callRetry(ctx, rc, s.ID, ir.Model, []Message{{Role: "user", Content: text}}, false, &u, &rt)
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
	id, err := e.o.Sessions.Start(ctx, SessionRequest{
		Agent: agent, Model: model, Workspace: s.workspace(), Repo: s.Repo, Folder: s.Folder, Task: task,
		Name: rc.snap.Name + " · " + name, Tags: []string{"flow:" + rc.snap.ID}, Run: rc.id, Flow: rc.snap.ID, Step: s.ID,
		Owner: rc.snap.Owner,
	})
	if err != nil {
		return nil, fmt.Errorf("session not started: %w", err)
	}
	e.update(rc.id, func(x *Run) {
		if !slices.Contains(x.Sessions, id) {
			x.Sessions = append(x.Sessions, id)
		}
	})
	out := map[string]any{"session": id, "state": "", "summary": ""}
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
		out["state"], out["summary"] = res.State, res.Summary
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

func (e *Engine) stepIf(rc *runCtx, s Step, rec *StepRecord) (string, error) {
	v := rc.outputs[s.On]
	ok, err := evalTest(s, v)
	if err != nil {
		return "", err
	}
	rec.Input = fmt.Sprintf("%s %s %q", s.On, s.Test, s.Value)
	if s.Test == TestJSON {
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
		return strings.TrimSpace(text) == strings.TrimSpace(s.Value), nil
	case TestMatches:
		re, err := regexp.Compile(s.Value)
		if err != nil {
			return false, err
		}
		return re.MatchString(text), nil
	case TestJSON:
		if str, ok := v.(string); ok {
			parsed, err := extractJSON(str, false)
			if err != nil {
				return false, nil
			}
			v = parsed
		}
		got, ok := lookup(v, s.Path)
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
