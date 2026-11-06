package evals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFeedbackPromptPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(FeedbackSystemPrompt))
	got := hex.EncodeToString(sum[:])
	const want = "5f961be7d823883914f714d8cc9993e7a5e75657dae2d936061a56ba754f6aa2"
	if got != want {
		t.Fatalf("FeedbackSystemPrompt changed: sha256 %s (update the pin only for a deliberate change)", got)
	}
	if FeedbackPromptHash() != got[:12] {
		t.Fatal("FeedbackPromptHash is not the first 12 hex characters")
	}
}

func TestPersonaVoice(t *testing.T) {
	ps := map[string]string{"strict": "Terse, exacting senior engineer."}
	if v, ok := PersonaVoice(ps, ""); v != ReviewerPersona || !ok {
		t.Fatalf("built-in %q %v", v, ok)
	}
	if v, ok := PersonaVoice(ps, "strict"); v != ps["strict"] || !ok {
		t.Fatalf("named %q %v", v, ok)
	}
	if v, ok := PersonaVoice(ps, "nobody"); v != ReviewerPersona || ok {
		t.Fatalf("unknown %q %v", v, ok)
	}
}

// iterEval has a build gate with two iterations and a test gate.
func iterEval() Eval {
	ev := sample()
	ev.Gates[0].MaxIterations = 2
	ev.Setups = ev.Setups[:1]
	ev.Persona = "strict"
	return ev
}

func TestFeedbackLoopPassesOnSecondIteration(t *testing.T) {
	en := newEnv(t)
	en.sess.auto = true
	en.set.Personas = map[string]string{"strict": "Speak like a terse senior engineer."}
	en.judge.failTimes = map[string]int{"health_route": 1}
	ev, _, err := en.e.Create("owner:admin", iterEval())
	if err != nil {
		t.Fatal(err)
	}
	run, err := en.e.Start("owner:admin", "u1", ev.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	d := en.waitRun(run.ID)
	if d.Run.State != RunDone || len(d.Reports) != 1 {
		t.Fatalf("run %+v", d.Run)
	}
	r := d.Reports[0]
	if r.GatesPassed != 2 || r.Gates[0].Iterations != 2 || r.Gates[0].State != StatePass || r.Gates[1].Iterations != 1 {
		t.Fatalf("gates %+v", r.Gates)
	}
	got := map[string]Result{}
	for _, x := range r.Results {
		got[x.Criterion] = x
	}
	if got["health_route"].Iteration != 2 || got["health_route"].State != StatePass || got["has_server"].Iteration != 2 {
		t.Fatalf("results %+v", r.Results)
	}
	if got["tests_pass"].Iteration != 1 || got["tests_pass"].State != StatePass {
		t.Fatalf("later gate %+v", got["tests_pass"])
	}
	if len(r.History) != 2 || r.History[0].Iteration != 1 {
		t.Fatalf("history %+v", r.History)
	}
	if len(r.Feedback) != 1 {
		t.Fatalf("feedback %+v", r.Feedback)
	}
	fb := r.Feedback[0]
	if !fb.Sent || fb.Fallback || fb.Gate != "build" || fb.Iteration != 1 || fb.Persona != "strict" ||
		strings.Join(fb.Criteria, ",") != "health_route" || fb.Text != "Feedback: fix health_route" {
		t.Fatalf("feedback %+v", fb)
	}
	res := en.sess.resumed()
	if len(res) != 1 || res[0] != r.Session+": Feedback: fix health_route" {
		t.Fatalf("resumes %q", res)
	}
	// The feedback call carries the persona's voice and only the failed root.
	var fbCall *ModelRequest
	en.judge.mu.Lock()
	for i, c := range en.judge.calls {
		if strings.HasPrefix(c.Messages[0].Content, FeedbackSystemPrompt) {
			fbCall = &en.judge.calls[i]
		}
	}
	en.judge.mu.Unlock()
	if fbCall == nil || !strings.Contains(fbCall.Messages[0].Content, "terse senior engineer") ||
		strings.Contains(fbCall.Messages[1].Content, "has_server") || fbCall.Client != ClientName || fbCall.Run != run.ID {
		t.Fatalf("feedback call %+v", fbCall)
	}
	// Wall time is summed over both attempts (12 s each in the fake).
	if r.WallMs != 24000 {
		t.Fatalf("wall %d", r.WallMs)
	}
	// 2 build criteria × 2 iterations + 1 feedback + 1 test criterion.
	if n := en.judge.callCount(); n != 6 {
		t.Fatalf("%d calls", n)
	}
}

func TestFeedbackBudgetExhaustedSkipsLaterGates(t *testing.T) {
	en := newEnv(t)
	en.sess.auto = true
	en.judge.verdicts = map[string]string{"has_server": "fail"}
	ev := iterEval()
	ev.Persona = ""
	ev.Gates[0].MaxIterations = 3
	created, _, err := en.e.Create("owner:admin", ev)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := en.e.Start("owner:admin", "u1", created.ID, "")
	r := en.waitRun(run.ID).Reports[0]
	if r.Gates[0].State != StateFail || r.Gates[0].Iterations != 3 || r.Gates[1].State != StateSkipped {
		t.Fatalf("gates %+v", r.Gates)
	}
	if len(r.Feedback) != 2 || len(en.sess.resumed()) != 2 {
		t.Fatalf("feedback %+v resumes %v", r.Feedback, en.sess.resumed())
	}
	// Roots only: health_route was skipped (needs has_server), not fed back.
	for _, fb := range r.Feedback {
		if strings.Join(fb.Criteria, ",") != "has_server" || fb.Persona != "" {
			t.Fatalf("feedback %+v", fb)
		}
	}
	for _, x := range r.Results {
		if x.Criterion == "tests_pass" && (x.State != StateSkipped || x.Rationale != "gate build did not pass") {
			t.Fatalf("later gate %+v", x)
		}
	}
	if r.FirstFailingGate != "build" || len(r.History) != 4 {
		t.Fatalf("report %+v", r)
	}
}

func TestFeedbackFallbackAndResumeRefused(t *testing.T) {
	en := newEnv(t)
	en.sess.auto = true
	en.judge.failTimes = map[string]int{"has_server": 1}
	en.judge.feedbackErr = errors.New("model exploded")
	created, _, err := en.e.Create("owner:admin", iterEval())
	if err != nil {
		t.Fatal(err)
	}
	run, _ := en.e.Start("owner:admin", "u1", created.ID, "")
	r := en.waitRun(run.ID).Reports[0]
	fb := r.Feedback[0]
	if !fb.Fallback || !fb.Sent || !strings.Contains(fb.Text, "There is an HTTP server entry point") ||
		!strings.Contains(fb.Error, "model exploded") {
		t.Fatalf("fallback %+v", fb)
	}
	if r.Gates[0].State != StatePass || r.Gates[0].Iterations != 2 {
		t.Fatalf("gates %+v", r.Gates)
	}

	// A session that cannot be resumed fails the gate with the results it has.
	en2 := newEnv(t)
	en2.sess.auto = true
	en2.sess.resumeErr = errors.New("the session's workspace no longer exists")
	en2.judge.failTimes = map[string]int{"has_server": 1}
	created, _, _ = en2.e.Create("owner:admin", iterEval())
	run, _ = en2.e.Start("owner:admin", "u1", created.ID, "")
	r = en2.waitRun(run.ID).Reports[0]
	if r.Gates[0].State != StateFail || r.Gates[0].Iterations != 1 || len(r.Feedback) != 1 || r.Feedback[0].Sent ||
		!strings.Contains(r.Feedback[0].Error, "workspace no longer exists") {
		t.Fatalf("refused %+v %+v", r.Gates, r.Feedback)
	}
}

func TestRejudgeNeverResumes(t *testing.T) {
	en := newEnv(t)
	en.sess.auto = true
	en.judge.verdicts = map[string]string{"has_server": "fail"}
	created, _, _ := en.e.Create("owner:admin", iterEval())
	run, _ := en.e.Start("owner:admin", "u1", created.ID, "")
	en.waitRun(run.ID)
	before := len(en.sess.resumed())
	if before != 1 {
		t.Fatalf("resumes %d", before)
	}
	if _, err := en.e.Rejudge("owner:admin", run.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rejudge", func() bool { d, _ := en.e.GetRun(run.ID); return Final(d.Run.State) })
	d, _ := en.e.GetRun(run.ID)
	if len(en.sess.resumed()) != before {
		t.Fatalf("rejudge resumed: %v", en.sess.resumed())
	}
	// The feedback loop's record is carried over from the replaced report:
	// the gate took 2 iterations, one feedback was sent, the earlier
	// iteration is in History, and wall time stays summed (12 s each).
	r := d.Reports[0]
	if r.Gates[0].Iterations != 2 || len(r.Feedback) != 1 || len(r.History) != 2 || r.WallMs != 24000 {
		t.Fatalf("rejudge dropped the E2 record: gates %+v feedback %d history %d wall %d",
			r.Gates, len(r.Feedback), len(r.History), r.WallMs)
	}
	for _, x := range r.Results {
		if x.Gate == r.Gates[0].Gate && x.Iteration != 2 {
			t.Fatalf("result %+v", x)
		}
	}
}

func TestResumedWallWithoutAttemptStart(t *testing.T) {
	en := newEnv(t)
	en.sess.auto = true
	en.sess.noAttemptStart = true
	en.judge.failTimes = map[string]int{"health_route": 1}
	created, _, _ := en.e.Create("owner:admin", iterEval())
	run, _ := en.e.Start("owner:admin", "u1", created.ID, "")
	d := en.waitRun(run.ID)
	// RunEnded - RunStarted of the second attempt would be 112 s (it spans
	// the first attempt); the host gives no attempt start, so the resumed
	// attempt is measured (about 0 s with the fake).
	if w := d.Reports[0].WallMs; w < 12000 || w >= 24000 {
		t.Fatalf("wall %d", w)
	}
}

func TestFeedbackWaitsForAParallelPlace(t *testing.T) {
	en := newEnv(t)
	en.judge.failTimes = map[string]int{"has_server": 1}
	ev := iterEval()
	ev.Repeat = 2
	created, _, _ := en.e.Create("owner:admin", ev)
	run, _ := en.e.Start("owner:admin", "u1", created.ID, "")
	// max_parallel 1: when s1 ends, s2 takes the place; s1's resumed
	// attempt (its build gate failed once) waits until s2 is done.
	waitFor(t, "s1", func() bool { _, ok := en.sess.Info("s1"); return ok })
	en.sess.end("s1", "passed")
	waitFor(t, "s2", func() bool { _, ok := en.sess.Info("s2"); return ok })
	time.Sleep(30 * time.Millisecond)
	if n := len(en.sess.resumed()); n != 0 {
		t.Fatalf("s1 resumed while s2 runs (%d)", n)
	}
	en.sess.end("s2", "passed")
	waitFor(t, "resume", func() bool { return len(en.sess.resumed()) == 1 })
	waitFor(t, "slot running", func() bool { d, _ := en.e.GetRun(run.ID); return d.Run.Slots[0].State == SlotRunning })
	en.sess.end("s1", "passed")
	d := en.waitRun(run.ID)
	if len(d.Reports) != 2 || en.sess.peak != 1 {
		t.Fatalf("reports %d peak %d", len(d.Reports), en.sess.peak)
	}
}

func TestSummarizePenalisesIterations(t *testing.T) {
	run := Run{Snapshot: Eval{Setups: []Setup{{Label: "slow"}, {Label: "fast"}}}}
	gates := func(it int) []GateResult { return []GateResult{{Gate: "main", State: StatePass, Iterations: it}} }
	reps := []Report{
		{Setup: "slow", GatesPassed: 1, GatesTotal: 1, CriteriaPassed: 2, Gates: gates(2), WallMs: 1},
		{Setup: "fast", GatesPassed: 1, GatesTotal: 1, CriteriaPassed: 2, Gates: gates(1), WallMs: 100},
	}
	sum := Summarize(run, reps)
	if sum[0].Setup != "fast" || sum[0].Iterations != 1 || sum[1].Iterations != 2 {
		t.Fatalf("summary %+v", sum)
	}
}

func TestJudgeJob(t *testing.T) {
	sess := newFakeSessions(t)
	sess.auto = true
	id, _ := sess.Start(context.Background(), StartRequest{Agent: "cerberus"})
	judge := &fakeJudge{failTimes: map[string]int{"works": 1}}
	job := Job{Session: id, Task: "Make it work", JudgeModel: "judge", Run: "fr-1", Owner: "u1", Setup: "build", Feedback: true,
		Gates:    []Gate{{ID: "main", MaxIterations: 2}},
		Criteria: []Criterion{{ID: "works", Prompt: "It works"}}}
	rep, err := Judge(context.Background(), JudgeDeps{Sessions: sess, Models: judge}, Settings{}, job)
	if err != nil || rep.GatesPassed != 1 || rep.Gates[0].Iterations != 2 || len(rep.Feedback) != 1 || rep.Run != "fr-1" {
		t.Fatalf("%+v %v", rep, err)
	}

	// Charge stops the judging with its error.
	limit := errors.New("over the limit")
	calls := 0
	job.Charge = func(u Usage) error {
		calls++
		if u.Tokens == 0 {
			t.Error("no usage charged")
		}
		return limit
	}
	sess2 := newFakeSessions(t)
	sess2.auto = true
	job.Session, _ = sess2.Start(context.Background(), StartRequest{Agent: "cerberus"})
	if _, err := Judge(context.Background(), JudgeDeps{Sessions: sess2, Models: &fakeJudge{}}, Settings{}, job); !errors.Is(err, limit) || calls != 1 {
		t.Fatalf("charge: %v (%d calls)", err, calls)
	}

	// Bad criteria and no judge model are refused before anything runs.
	var ve *ValidationError
	if _, err := Judge(context.Background(), JudgeDeps{Sessions: sess, Models: judge}, Settings{},
		Job{Session: id, JudgeModel: "judge"}); !errors.As(err, &ve) {
		t.Fatalf("no criteria: %v", err)
	}
	job.Charge = nil
	job.JudgeModel = ""
	if _, err := Judge(context.Background(), JudgeDeps{Sessions: sess, Models: judge}, Settings{}, job); !errors.Is(err, ErrNoJudge) {
		t.Fatalf("no judge: %v", err)
	}
	// A session that is gone is an error.
	job.JudgeModel, job.Session = "judge", "nope"
	if _, err := Judge(context.Background(), JudgeDeps{Sessions: sess, Models: judge}, Settings{}, job); err == nil {
		t.Fatal("gone session judged")
	}
}

func TestValidateCriteria(t *testing.T) {
	ps := ValidateCriteria([]Gate{{ID: "a", MaxIterations: 9}}, []Criterion{{ID: "x", Prompt: "p", Gate: "b"}}, "Bad Name", nil)
	fields := map[string]bool{}
	for _, p := range ps {
		fields[p.Field] = true
	}
	if !fields["gates[0].max_iterations"] || !fields["criteria[0].gate"] || !fields["persona"] {
		t.Fatalf("%+v", ps)
	}
	if ps := ValidateCriteria(nil, []Criterion{{ID: "x", Prompt: "p"}}, "", nil); HasErrors(ps) {
		t.Fatalf("%+v", ps)
	}
	if ps := ValidateCriteria(nil, []Criterion{{ID: "x", Prompt: "p"}}, "nobody", fakeCatalog{}); HasErrors(ps) || len(ps) != 1 {
		t.Fatalf("unknown persona is a warning: %+v", ps)
	}
}
