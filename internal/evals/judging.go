package evals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// sessionGone are session states with nothing left to judge.
func sessionGone(state string) bool { return state == "removed" }

// Job is one session to judge outside an eval run: a flow's judge step
// (E2). It follows the rules of a run: gates in order, the feedback loop
// within each gate's max_iterations, later gates skipped after a gate
// that fails.
type Job struct {
	Session    string
	Task       string // the task the session was given ("" = none shown to the judge)
	Gates      []Gate
	Criteria   []Criterion
	Persona    string
	JudgeModel string
	Run        string // recorded as the usage session of the judge's calls (a flow run id)
	Owner      string // charged for the judge's calls
	Setup      string // the report's setup label
	// Feedback runs the loop: a failed gate with iterations left resumes
	// the session with feedback (Sessions must be a Resumer). False
	// judges each gate once (a rejudge).
	Feedback bool
	// Charge is called with the usage of every judge and feedback call;
	// an error (a limit) stops the judging, and Judge answers it.
	Charge func(Usage) error
}

// JudgeDeps are what Judge needs: Sessions (a Resumer for the feedback
// loop) and the judge's ModelCaller.
type JudgeDeps struct {
	Sessions Sessions
	Models   ModelCaller
	Now      func() time.Time
}

// Judge judges one session (a flow's judge step): it waits for the session
// to end, then runs the gates with the feedback loop and answers the
// report. The error is a validation error, ErrNoJudge, ctx's end, the
// Charge error that stopped it, or the reason the report could not be
// made (the session is gone); the report is filled as far as it got.
func Judge(ctx context.Context, d JudgeDeps, s Settings, job Job) (Report, error) {
	if ps := ValidateCriteria(job.Gates, job.Criteria, job.Persona, nil); HasErrors(ps) {
		return Report{}, &ValidationError{Problems: ps}
	}
	if strings.TrimSpace(job.JudgeModel) == "" {
		return Report{}, ErrNoJudge
	}
	if d.Sessions == nil {
		return Report{}, errors.New("agent sessions are not available")
	}
	if d.Models == nil {
		return Report{}, errors.New("the judge model cannot be called here (no gateway)")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	info, err := d.Sessions.Wait(ctx, job.Session)
	if err != nil {
		return Report{}, err
	}
	ev := Normalize(Eval{Task: job.Task, Gates: job.Gates, Criteria: job.Criteria, Persona: job.Persona})
	job.Gates, job.Criteria, job.Persona = ev.Gates, ev.Criteria, ev.Persona
	j := newJudging(d, s.withDefaults(), job)
	rep := Report{Run: job.Run, Session: job.Session, Setup: job.Setup, Repeat: 1}
	rep = j.run(ctx, rep, info, 0)
	if c := j.charged(); c != nil {
		return rep, c
	}
	if ctx.Err() != nil {
		return rep, ctx.Err()
	}
	if rep.State == ReportError {
		return rep, errors.New(rep.Error)
	}
	return rep, nil
}

// judging is one session's judgement in progress.
type judging struct {
	sessions Sessions
	models   ModelCaller
	now      func() time.Time
	settings Settings
	job      Job

	// Engine hooks (nil outside a run): attempt reports a resumed attempt
	// starting (true) and ending (false); acquire and release hold a place
	// of evals.max_parallel while it runs; stop stops the session when the
	// judging is canceled during an attempt.
	attempt func(running bool)
	acquire func(ctx context.Context) bool
	release func()
	stop    func(id string)

	cancel context.CancelCauseFunc
	mu     sync.Mutex
	cause  error
}

func newJudging(d JudgeDeps, s Settings, job Job) *judging {
	return &judging{sessions: d.Sessions, models: d.Models, now: d.Now, settings: s, job: job}
}

func (j *judging) mask(text string) string {
	if j.sessions == nil {
		return text
	}
	return j.sessions.Mask(text)
}

// charge passes a call's usage to Job.Charge; an error stops the judging.
func (j *judging) charge(u Usage) {
	if j.job.Charge == nil {
		return
	}
	if err := j.job.Charge(u); err != nil {
		j.mu.Lock()
		if j.cause == nil {
			j.cause = err
		}
		j.mu.Unlock()
		if j.cancel != nil {
			j.cancel(err)
		}
	}
}

func (j *judging) charged() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cause
}

// evidence reads what the criteria need of one attempt of the session.
func (j *judging) evidence(info SessionInfo, criteria []Criterion) evidenceInput {
	in := evidenceInput{Info: info, Mask: j.sessions.Mask}
	need := map[string]bool{}
	for _, c := range criteria {
		for _, k := range EvidenceOf(c) {
			need[k] = true
		}
	}
	if need[EvidenceTranscript] {
		if t, err := j.sessions.Transcript(info.ID); err == nil {
			in.Transcript = string(t)
		}
	}
	if need[EvidenceDiff] && info.Workspace == WorkspaceRepo {
		if d, err := j.sessions.Diff(info.ID); err == nil {
			in.Diff = d
		} else {
			in.Diff = "(the diff could not be read: " + err.Error() + ")"
		}
	}
	return in
}

// attemptWall is how long the latest attempt of the session ran:
// RunEnded - AttemptStarted, or RunEnded - RunStarted when the host does
// not know the attempt's start (the first attempt, where they are equal).
// Zero when it cannot be told.
func attemptWall(info SessionInfo) time.Duration {
	start := info.AttemptStarted
	if start.IsZero() {
		start = info.RunStarted
	}
	if !start.IsZero() && info.RunEnded.After(start) {
		return info.RunEnded.Sub(start)
	}
	return 0
}

// resumedWall is how long a resumed attempt ran: its own start to its end
// when the host knows the attempt's start, else measured (from Resume
// returning to the attempt ending). RunStarted is never used here: it is
// the first attempt's start, so RunEnded - RunStarted would count every
// earlier attempt and the judging between them again.
func resumedWall(info SessionInfo, measured time.Duration) time.Duration {
	if !info.AttemptStarted.IsZero() && info.RunEnded.After(info.AttemptStarted) {
		return info.RunEnded.Sub(info.AttemptStarted)
	}
	return max(measured, 0)
}

// run judges the session gate by gate into rep (whose run, session and
// setup fields the caller set). Within a gate, criteria are judged in
// waves: a criterion whose dependencies all have a result is ready; it is
// skipped ("needs <dep>") when one of them did not pass, else judged, at
// most JudgeParallel at once. A gate passes when every criterion in it
// passed. A gate that fails with iterations left (and Job.Feedback)
// becomes feedback: the criteria that failed on their own, written up by
// the judge model in the persona's voice and sent with Resume; the gate is
// judged again when the new attempt ends, its earlier results moved to
// History. A gate that fails with no iterations left skips the later gates
// ("gate <id> did not pass"); a gate that passes moves on to the next with
// no new attempt.
func (j *judging) run(ctx context.Context, rep Report, info SessionInfo, wall time.Duration) (out Report) {
	ctx, j.cancel = context.WithCancelCause(ctx)
	defer j.cancel(nil)
	ev := Eval{Task: j.job.Task, Gates: j.job.Gates, Criteria: j.job.Criteria, Persona: j.job.Persona}
	if rep.Session == "" {
		rep.Session = info.ID
	}
	if info.ID == "" {
		info.ID = rep.Session
	}
	rep.SetupHash = orStr(info.SetupHash, rep.SetupHash)
	rep.Agent, rep.Model = orStr(info.Agent, rep.Agent), orStr(info.Model, rep.Model)
	rep.SessionState, rep.State = info.State, ReportJudged
	rep.Results, rep.Gates = []Result{}, []GateResult{}
	rep.JudgeModel, rep.JudgePrompt = j.job.JudgeModel, JudgePromptHash()
	if w := attemptWall(info); w > 0 {
		wall = w
	}
	defer func() {
		rep.WallMs = wall.Milliseconds()
		rep.SessionUsage = j.sessions.Usage(rep.Session)
		rep.SessionState = info.State
		rep.Judged = j.now().UTC()
		out = rep
	}()
	if sessionGone(info.State) {
		rep.State, rep.Error = ReportError, "the session is gone: nothing to judge"
		return rep
	}

	in := j.evidence(info, ev.Criteria)
	results := map[string]Result{}
	var mu sync.Mutex
	gates := EffectiveGates(ev)
	order := Order(ev)
	resumer, canResume := j.sessions.(Resumer)
	stopped := ""
	for gi, g := range gates {
		crits := order[gi]
		if stopped != "" {
			for _, c := range crits {
				results[c.ID] = Result{Criterion: c.ID, Gate: g.ID, Iteration: 0, State: StateSkipped,
					Rationale: "gate " + stopped + " did not pass", Evidence: []string{}}
			}
			rep.Gates = append(rep.Gates, GateResult{Gate: g.ID, Name: g.Name, State: StateSkipped})
			continue
		}
		iteration := 1
		pass := false
		for {
			j.judgeGate(ctx, in, crits, g.ID, iteration, results, &mu, &rep)
			if ctx.Err() != nil {
				return rep
			}
			pass = true
			for _, c := range crits {
				if results[c.ID].State != StatePass {
					pass = false
				}
			}
			if pass || iteration >= g.MaxIterations || !j.job.Feedback || !canResume {
				break
			}
			roots := failureRoots(crits, results)
			if len(roots) == 0 {
				break // only judge errors: nothing to tell the agent
			}
			fb, use := j.writeFeedback(ctx, g.ID, iteration, roots, results)
			rep.JudgeUsage.add(use)
			j.charge(use)
			if ctx.Err() != nil {
				rep.Feedback = append(rep.Feedback, fb)
				return rep
			}
			next, took, err := j.resume(ctx, resumer, info.ID, &fb)
			rep.Feedback = append(rep.Feedback, fb)
			if ctx.Err() != nil {
				return rep
			}
			if err != nil {
				break // the gate fails with the results it has
			}
			info = next
			wall += resumedWall(next, took)
			in = j.evidence(info, ev.Criteria)
			for _, c := range crits {
				if r, ok := results[c.ID]; ok {
					rep.History = append(rep.History, r)
					delete(results, c.ID)
				}
			}
			iteration++
		}
		gr := GateResult{Gate: g.ID, Name: g.Name, State: StatePass, Iterations: iteration}
		if !pass {
			gr.State = StateFail
			stopped = g.ID
		}
		rep.Gates = append(rep.Gates, gr)
	}
	for _, c := range ev.Criteria {
		if r, ok := results[c.ID]; ok {
			rep.Results = append(rep.Results, r)
		}
	}
	rep.tally()
	return rep
}

// resume sends the feedback as a new attempt of the session and waits for
// it to end. fb records whether it was sent and why not. took is the time
// from Resume returning to the attempt ending (no place wait, no judging).
func (j *judging) resume(ctx context.Context, rs Resumer, id string, fb *Feedback) (SessionInfo, time.Duration, error) {
	if j.acquire != nil {
		if !j.acquire(ctx) {
			fb.Error = "not sent: the run was stopped"
			return SessionInfo{}, 0, ctx.Err()
		}
		defer j.release()
	}
	if err := rs.Resume(ctx, id, fb.Text); err != nil {
		msg := "not sent: " + clipRunes(strings.TrimSpace(err.Error()), 300)
		if fb.Error != "" {
			msg = fb.Error + "; " + msg
		}
		fb.Error = msg
		return SessionInfo{}, 0, err
	}
	started := j.now()
	fb.Sent = true
	if j.attempt != nil {
		j.attempt(true)
		defer j.attempt(false)
	}
	info, err := j.sessions.Wait(ctx, id)
	took := j.now().Sub(started)
	if err != nil {
		if ctx.Err() != nil && j.stop != nil {
			j.stop(id)
		}
		return SessionInfo{}, took, err
	}
	if sessionGone(info.State) {
		return info, took, fmt.Errorf("the session is gone")
	}
	if info.ID == "" {
		info.ID = id
	}
	return info, took, nil
}

// judgeGate fills results for the criteria of one gate (in topological
// order) at one iteration.
func (j *judging) judgeGate(ctx context.Context, in evidenceInput, crits []Criterion, gate string, iteration int,
	results map[string]Result, mu *sync.Mutex, rep *Report) {
	pending := crits
	for len(pending) > 0 && ctx.Err() == nil {
		var ready, rest []Criterion
		mu.Lock()
		for _, c := range pending {
			ok := true
			for _, d := range c.DependsOn {
				if _, have := results[d]; !have {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, c)
			} else {
				rest = append(rest, c)
			}
		}
		var judge []Criterion
		for _, c := range ready {
			failed := ""
			for _, d := range c.DependsOn {
				if results[d].State != StatePass {
					failed = d
					break
				}
			}
			if failed != "" {
				results[c.ID] = Result{Criterion: c.ID, Gate: gate, Iteration: iteration, State: StateSkipped,
					Rationale: "needs " + failed, Evidence: []string{}}
				continue
			}
			judge = append(judge, c)
		}
		mu.Unlock()
		if len(ready) == 0 {
			// Cannot happen with a valid eval (Order and Validate refuse cycles).
			mu.Lock()
			for _, c := range rest {
				results[c.ID] = Result{Criterion: c.ID, Gate: gate, Iteration: iteration, State: StateError,
					Rationale: "its dependencies could not be ordered", Evidence: []string{}}
			}
			mu.Unlock()
			return
		}
		sem := make(chan struct{}, JudgeParallel)
		var wg sync.WaitGroup
		for _, c := range judge {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				res, use := j.judgeCriterion(ctx, in, c, gate, iteration)
				mu.Lock()
				results[c.ID] = res
				rep.JudgeUsage.add(use)
				mu.Unlock()
				j.charge(use)
			}()
		}
		wg.Wait()
		pending = rest
	}
}

// judgeCriterion asks the judge about one criterion.
func (j *judging) judgeCriterion(ctx context.Context, in evidenceInput, c Criterion, gate string, iteration int) (Result, Usage) {
	evidence := buildEvidence(in, c, j.settings.EvidenceKB<<10)
	req := ModelRequest{Model: j.job.JudgeModel, Messages: JudgeMessages(j.job.Task, c, evidence),
		MaxTokens: JudgeMaxTokens, Client: ClientName, Run: j.job.Run, Owner: j.job.Owner}
	v, reply, err := judgeOne(ctx, j.models, req)
	use := callUsage(reply)
	res := Result{Criterion: c.ID, Gate: gate, Iteration: iteration, Model: orStr(reply.Model, j.job.JudgeModel),
		Tokens: use.Tokens, Evidence: []string{}}
	switch {
	case err == nil:
		// The judge may quote the evidence back: mask what it says before
		// the report keeps it.
		res.State, res.Rationale = v.Verdict, j.mask(v.Rationale)
		for _, q := range v.Evidence {
			res.Evidence = append(res.Evidence, j.mask(q))
		}
	case errors.Is(err, errParse):
		res.State, res.Rationale = StateError, "the judge did not answer with the JSON verdict (asked twice)"
	default:
		res.State, res.Rationale = StateError, "the judge call failed: "+clipRunes(strings.TrimSpace(err.Error()), 400)
	}
	return res, use
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
