package flows

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/evals"
)

func TestSessionSettingsReachTheRequest(t *testing.T) {
	h := newHarness(t)
	f := h.create(Flow{Name: "S", Steps: []Step{
		{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "do {input}", MaxAttempts: 3, EscalateTo: "cerberus", MaxMinutes: 30},
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", As: KindSession, Agent: "cerberus", Template: "{item.task}", MaxAttempts: 2,
			EscalateTo: "cerberus", MaxMinutes: 5},
	}})
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: `[{"title":"a","kind":"x","task":"t"}]`}, nil
	}
	r := h.wait(h.run(f.ID, "it").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	h.sessions.mu.Lock()
	defer h.sessions.mu.Unlock()
	if len(h.sessions.reqs) != 2 {
		t.Fatalf("%d sessions", len(h.sessions.reqs))
	}
	a, b := h.sessions.reqs[0], h.sessions.reqs[1]
	if a.MaxAttempts != 3 || a.EscalateTo != "cerberus" || a.MaxMinutes != 30 {
		t.Fatalf("session step request %+v", a)
	}
	if b.MaxAttempts != 2 || b.EscalateTo != "cerberus" || b.MaxMinutes != 5 {
		t.Fatalf("fan-out item request %+v", b)
	}
}

func TestStepRetryPassesOnALaterTry(t *testing.T) {
	h := newHarness(t)
	var n atomic.Int32
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if n.Add(1) < 3 {
			return ModelReply{}, errBoom // not transient: only the step retry helps
		}
		return ModelReply{Text: "done", PromptTokens: 7, CompletionTokens: 3}, nil
	}
	f := h.create(Flow{Name: "R", Steps: []Step{
		{ID: "p", Kind: KindPrompt, Model: "big", Template: "x", Retry: &Retry{Max: 2}},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "p")
	if rec.Attempts != 3 || rec.RetryError != "boom" || rec.Output != "done" || rec.PromptTokens != 7 {
		t.Fatalf("record %+v", rec)
	}
	if r.StepsRun != 3 {
		t.Fatalf("steps run %d, want 3 (each retry counts)", r.StepsRun)
	}
	g, _ := h.e.Graph(r.ID)
	for _, nd := range g.Nodes {
		if nd.ID == "st:p" && !strings.Contains(nd.Detail, "retried 2 times") {
			t.Fatalf("graph detail %q", nd.Detail)
		}
	}
}

func TestStepRetryStopsAtMaxStepsAndOnLimits(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) { return ModelReply{}, errBoom }
	f := h.create(Flow{Name: "R", Limits: Limits{MaxSteps: 2}, Steps: []Step{
		{ID: "p", Kind: KindPrompt, Model: "big", Template: "x", Retry: &Retry{Max: 3}, OnError: OnErrorContinue},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateLimited || !strings.Contains(r.Reason, "more than 2 steps") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	// A limit is never retried (and on_error does not catch it).
	h2 := newHarness(t)
	h2.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: "x", PromptTokens: 500}, nil
	}
	f2 := h2.create(Flow{Name: "L", Limits: Limits{MaxTokens: 100}, Steps: []Step{
		{ID: "p", Kind: KindPrompt, Model: "big", Template: "x", Retry: &Retry{Max: 3}, OnError: OnErrorContinue},
	}})
	r2 := h2.wait(h2.run(f2.ID, "").ID)
	if r2.State != StateLimited || len(h2.models.Calls()) != 1 {
		t.Fatalf("%s %q after %d calls", r2.State, r2.Reason, len(h2.models.Calls()))
	}
}

func TestOnErrorContinueAndJump(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if req.Step == "bad" {
			return ModelReply{}, errBoom
		}
		return ModelReply{Text: "ok:" + req.Messages[len(req.Messages)-1].Content}, nil
	}
	f := h.create(Flow{Name: "E", Steps: []Step{
		{ID: "bad", Kind: KindPrompt, Model: "big", Template: "x", OnError: OnErrorContinue},
		{ID: "next", Kind: KindPrompt, Model: "big", Template: "after [{steps.bad.output.error}]"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	bad := h.stepRec(r.ID, "bad")
	if bad.State != StepFailed || bad.OnError != "next" || bad.Error != "boom" || !strings.Contains(bad.Jump, "on error") {
		t.Fatalf("bad %+v", bad)
	}
	if out, _ := h.stepRec(r.ID, "next").Output.(string); out != "ok:after [boom]" {
		t.Fatalf("next output %q", out)
	}

	// A jump skips the steps in between.
	f2 := h.create(Flow{Name: "J", Steps: []Step{
		{ID: "bad", Kind: KindPrompt, Model: "big", Template: "x", OnError: "tell"},
		{ID: "skipped", Kind: KindPrompt, Model: "big", Template: "never"},
		{ID: "tell", Kind: KindOutput, To: ToInbox, Template: "failed: {steps.bad.output.error}"},
	}})
	r2 := h.wait(h.run(f2.ID, "").ID)
	if r2.State != StatePassed {
		t.Fatalf("%s %q", r2.State, r2.Reason)
	}
	for _, rec := range h.detail(r2.ID).Steps {
		if rec.ID == "skipped" {
			t.Fatal("the jump did not skip")
		}
	}
	if rec := h.stepRec(r2.ID, "tell"); rec.Output != "failed: boom" {
		t.Fatalf("tell %+v", rec)
	}
	g, _ := h.e.Graph(r2.ID)
	found := false
	for _, e := range g.Edges {
		if e.From == "st:bad" && e.To == "st:tell" && e.Label == "on error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no on-error edge in %+v", g.Edges)
	}
}

func TestFanOutMinOK(t *testing.T) {
	plan := `[{"title":"a","kind":"x","task":"ta"},{"title":"b","kind":"x","task":"tb"},{"title":"c","kind":"x","task":"tc"}]`
	run := func(minOK int, retry *Retry) (*harness, Run, *atomic.Int32) {
		h := newHarness(t)
		var bTries atomic.Int32
		h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
			task := req.Messages[len(req.Messages)-1].Content
			switch {
			case req.Step == "p":
				return ModelReply{Text: plan}, nil
			case task == "tb":
				if bTries.Add(1) == 1 || retry == nil {
					return ModelReply{}, errBoom
				}
			}
			return ModelReply{Text: "did " + task}, nil
		}
		f := h.create(Flow{Name: "M", Limits: Limits{MaxParallel: 1}, Steps: []Step{
			{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
			{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}", MinOK: minOK, Retry: retry},
		}})
		return h, h.wait(h.run(f.ID, "").ID), &bTries
	}
	h, r, _ := run(2, nil)
	if r.State != StatePassed {
		t.Fatalf("min_ok 2: %s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "f")
	out, _ := rec.Output.([]any)
	if len(out) != 3 || out[1] != nil || out[0] != "did ta" || out[2] != "did tc" {
		t.Fatalf("output %#v", rec.Output)
	}
	if rec.Items[1].State != StepFailed || rec.Items[2].State != StepPassed {
		t.Fatalf("items %+v", rec.Items)
	}
	// min_ok 3: the step fails, but the other items still ran.
	h, r, _ = run(3, nil)
	if r.State != StateFailed || !strings.Contains(r.Reason, "3 had to pass") {
		t.Fatalf("min_ok 3: %s %q", r.State, r.Reason)
	}
	if rec := h.stepRec(r.ID, "f"); rec.Items[2].State != StepPassed {
		t.Fatalf("a sibling was stopped: %+v", rec.Items)
	}
	// An item retry: b passes on its second try, and counts as a step.
	h, r, tries := run(0, &Retry{Max: 1})
	if r.State != StatePassed || tries.Load() != 2 {
		t.Fatalf("retry: %s %q (%d tries)", r.State, r.Reason, tries.Load())
	}
	if it := h.stepRec(r.ID, "f").Items[1]; it.Attempts != 2 || it.RetryError != "boom" {
		t.Fatalf("item %+v", it)
	}
	if r.StepsRun != 6 { // p, f, 3 items, 1 retry
		t.Fatalf("steps run %d", r.StepsRun)
	}
}

func TestErrorSettingsValidation(t *testing.T) {
	bad := Flow{Name: "V", Steps: []Step{
		{ID: "a", Kind: KindPrompt, Model: "big", Template: "x", Retry: &Retry{Max: 9, BackoffS: -1}, OnError: "a",
			MaxAttempts: 2, MinOK: 1},
		{ID: "b", Kind: KindSession, Agent: "cerberus", Task: "t", MaxAttempts: 9, EscalateTo: "ghost", MaxMinutes: -1,
			Retry: &Retry{Max: 1}, OnError: "nope"},
		{ID: "c", Kind: KindIf, Test: TestContains, On: "a", Value: "x", OnError: OnErrorContinue},
	}}
	fields := map[string]bool{}
	for _, p := range Validate(bad, fakeCatalog{}) {
		if p.Severity == SevError {
			fields[p.Step+"."+p.Field] = true
		}
	}
	for _, want := range []string{"a.retry.max", "a.retry.backoff_s", "a.on_error", "a.max_attempts", "a.min_ok",
		"b.max_attempts", "b.escalate_to", "b.max_minutes", "b.retry", "b.on_error", "c.on_error"} {
		if !fields[want] {
			t.Errorf("missing %s in %v", want, fields)
		}
	}
	good := Flow{Name: "V", Steps: []Step{
		{ID: "a", Kind: KindPrompt, Model: "big", Template: "x", Retry: &Retry{Max: 3, BackoffS: 5}, OnError: "c"},
		{ID: "b", Kind: KindSession, Agent: "cerberus", Task: "t", MaxAttempts: 5, EscalateTo: "cerberus", MaxMinutes: 60,
			OnError: OnErrorContinue},
		{ID: "c", Kind: KindOutput, To: ToInbox, Template: "x", OnError: End},
	}}
	if ps := Validate(good, fakeCatalog{}); HasErrors(ps) {
		t.Fatalf("%+v", ps)
	}
	// clone copies the retry setting
	cl := good.clone()
	cl.Steps[0].Retry.Max = 0
	if good.Steps[0].Retry.Max != 3 {
		t.Fatal("clone shares Retry")
	}
}

// textJudge is a JudgeRunner for text mode: verdict answers per call
// (true = pass); on a fail with revisions left it calls Revise first.
type textJudge struct {
	mu        sync.Mutex
	reqs      []JudgeRequest
	revisions int
	texts     []string
	verdict   func(text string) bool
}

func (j *textJudge) Judge(ctx context.Context, req JudgeRequest) (JudgeResult, error) {
	j.mu.Lock()
	j.reqs = append(j.reqs, req)
	j.mu.Unlock()
	if err := req.Charge(ModelReply{Model: "judge", PromptTokens: 20, CompletionTokens: 5}); err != nil {
		return JudgeResult{}, err
	}
	text := req.Text
	var fb []evals.Feedback
	for i := 0; ; i++ {
		j.mu.Lock()
		j.texts = append(j.texts, text)
		j.mu.Unlock()
		if j.verdict(text) {
			return JudgeResult{Passed: true, GatesPassed: 1, GatesTotal: 1, Report: evals.Report{Feedback: fb}}, nil
		}
		if i >= j.revisions {
			return JudgeResult{GatesTotal: 1, Failed: []string{"good"}, Report: evals.Report{Feedback: fb}}, nil
		}
		fb = append(fb, evals.Feedback{Gate: "main", Iteration: i + 1, Text: "make it better"})
		next, err := req.Revise(ctx, "make it better")
		if err != nil {
			return JudgeResult{}, err
		}
		text = next
	}
}

func TestJudgeOnPromptStepRevises(t *testing.T) {
	h := newHarness(t)
	var n atomic.Int32
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if req.Step == "draft" {
			return ModelReply{Text: "draft v1", PromptTokens: 10, CompletionTokens: 5}, nil
		}
		return ModelReply{Text: "draft v" + string(rune('1'+n.Add(1))), PromptTokens: 10, CompletionTokens: 5}, nil
	}
	tj := &textJudge{revisions: 2, verdict: func(s string) bool { return s == "draft v2" }}
	h.e.o.Judge = tj
	f := h.create(Flow{Name: "D", Steps: []Step{
		{ID: "draft", Kind: KindPrompt, Model: "big", System: "be brief", Template: "write about {input}"},
		{ID: "gate", Kind: KindJudge, On: "draft", Gates: []evals.Gate{{ID: "main", MaxIterations: 3}},
			Criteria: []evals.Criterion{{ID: "good", Prompt: "It is good"}}},
		{ID: "tell", Kind: KindOutput, To: ToInbox, Template: "{steps.draft.output}"},
	}})
	r := h.wait(h.run(f.ID, "cats").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	req := tj.reqs[0]
	if req.Session != "" || req.Text != "draft v1" || req.Task != "write about cats" {
		t.Fatalf("judge request %+v", req)
	}
	// The revise call repeats the step's call with the answer and the
	// feedback as the last two turns.
	calls := h.models.Calls()
	last := calls[len(calls)-1]
	if last.Step != "gate" || last.Model != "big" || len(last.Messages) != 4 || last.Messages[0].Content != "be brief" ||
		last.Messages[2].Role != "assistant" || last.Messages[2].Content != "draft v1" ||
		last.Messages[3].Role != "user" || last.Messages[3].Content != "make it better" {
		t.Fatalf("revise call %+v", last)
	}
	rec := h.stepRec(r.ID, "gate")
	out, _ := rec.Output.(map[string]any)
	if out["passed"] != true || out["revised"] != true || out["output"] != "draft v2" {
		t.Fatalf("judge output %#v", out)
	}
	if len(rec.Revisions) != 1 || rec.Revisions[0].Output != "draft v2" || rec.Revisions[0].Gate != "main" ||
		rec.Revisions[0].Iteration != 1 || rec.Revisions[0].Tokens != 15 {
		t.Fatalf("revisions %+v", rec.Revisions)
	}
	if rec.PromptTokens != 30 { // the judge's 20 plus the revise call's 10
		t.Fatalf("judge usage %+v", rec)
	}
	if got := h.stepRec(r.ID, "tell").Output; got != "draft v2" {
		t.Fatalf("a later step read %q, want the revised answer", got)
	}
	if r.StepsRun != 4 {
		t.Fatalf("steps run %d (a revision counts)", r.StepsRun)
	}
}

func TestJudgeOnFanOutItems(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		last := req.Messages[len(req.Messages)-1].Content
		switch {
		case req.Step == "p":
			return ModelReply{Text: `[{"title":"a","kind":"x","task":"ta"},{"title":"b","kind":"x","task":"tb"}]`}, nil
		case req.Step == "gate":
			return ModelReply{Text: "fixed " + req.Messages[0].Content}, nil
		}
		return ModelReply{Text: "did " + last}, nil
	}
	tj := &textJudge{revisions: 1, verdict: func(s string) bool { return s != "did tb" }}
	h.e.o.Judge = tj
	f := h.create(Flow{Name: "F", Steps: []Step{
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}"},
		{ID: "gate", Kind: KindJudge, On: "f", Criteria: []evals.Criterion{{ID: "good", Prompt: "It is good"}}},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	if len(tj.reqs) != 2 || tj.reqs[1].Task != "tb" {
		t.Fatalf("judge requests %+v", tj.reqs)
	}
	rec := h.stepRec(r.ID, "gate")
	out, _ := rec.Output.(map[string]any)
	list, _ := out["output"].([]any)
	if out["passed"] != true || len(list) != 2 || list[0] != "did ta" || list[1] != "fixed tb" {
		t.Fatalf("output %#v", out)
	}
	if len(rec.Revisions) != 1 || rec.Revisions[0].Item != 2 {
		t.Fatalf("revisions %+v", rec.Revisions)
	}
	calls := h.models.Calls()
	if last := calls[len(calls)-1]; last.Model != "small" || len(last.Messages) != 3 {
		t.Fatalf("revise call %+v", last)
	}
}

func TestJudgeRevisionCountsTowardMaxSteps(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) { return ModelReply{Text: "v"}, nil }
	tj := &textJudge{revisions: 3, verdict: func(string) bool { return false }}
	h.e.o.Judge = tj
	f := h.create(Flow{Name: "D", Limits: Limits{MaxSteps: 3}, Steps: []Step{
		{ID: "draft", Kind: KindPrompt, Model: "big", Template: "x"},
		{ID: "gate", Kind: KindJudge, On: "draft", Gates: []evals.Gate{{ID: "main", MaxIterations: 5}},
			Criteria: []evals.Criterion{{ID: "good", Prompt: "It is good"}}},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateLimited || !strings.Contains(r.Reason, "more than 3 steps") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
}
