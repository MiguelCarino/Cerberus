package flows

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/evals"
)

// A judge on a prompt step that failed under on_error does not judge, nor
// revise, the {"error"} object (flows-agent-5).
func TestJudgeRefusesAStepThatFailed(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) { return ModelReply{}, errBoom }
	tj := &textJudge{revisions: 2, verdict: func(string) bool { return false }}
	h.e.o.Judge = tj
	f := h.create(Flow{Name: "D", Steps: []Step{
		{ID: "draft", Kind: KindPrompt, Model: "big", Template: "x", OnError: OnErrorContinue},
		{ID: "gate", Kind: KindJudge, On: "draft", Criteria: []evals.Criterion{{ID: "good", Prompt: "It is good"}}},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateFailed || !strings.Contains(r.Reason, "step draft failed; nothing to judge") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	if len(tj.reqs) != 0 {
		t.Fatalf("the error object was judged: %+v", tj.reqs)
	}
}

// A judge that stops part-way through a fan-out keeps the revisions it
// paid for, and Continue reuses the verdicts of the answers it reached
// (flows-agent-6).
func TestJudgeKeepsRevisionsWhenAnItemFails(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		switch req.Step {
		case "p":
			return ModelReply{Text: `[{"title":"a","kind":"x","task":"ta"},{"title":"b","kind":"x","task":"tb"}]`}, nil
		case "gate":
			return ModelReply{Text: "fixed " + req.Messages[0].Content}, nil
		}
		return ModelReply{Text: "did " + req.Messages[len(req.Messages)-1].Content}, nil
	}
	var fail atomic.Bool
	fail.Store(true)
	tj := &failingJudge{textJudge: textJudge{revisions: 1, verdict: func(s string) bool { return strings.HasPrefix(s, "fixed") }},
		fail: func(req JudgeRequest) bool { return req.Task == "tb" && fail.Load() }}
	h.e.o.Judge = tj
	f := h.create(Flow{Name: "F", Steps: []Step{
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}"},
		{ID: "gate", Kind: KindJudge, On: "f", Criteria: []evals.Criterion{{ID: "good", Prompt: "It is good"}}},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateFailed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "gate")
	out, _ := rec.Output.(map[string]any)
	list, _ := out["output"].([]any)
	if out["partial"] != true || out["revised"] != true || len(list) != 2 || list[0] != "fixed ta" {
		t.Fatalf("partial output %#v", rec.Output)
	}
	revisesBefore := countStep(h.models.Calls(), "gate")
	fail.Store(false)
	if _, err := h.e.Continue("owner:test", r.ID); err != nil {
		t.Fatal(err)
	}
	done := h.wait(r.ID)
	if done.State != StatePassed {
		t.Fatalf("after continue: %s %q", done.State, done.Reason)
	}
	// Item a was not judged nor revised again: one more judge request (b)
	// and one more revise call (b's).
	tj.mu.Lock()
	var tasks []string
	for _, q := range tj.reqs {
		tasks = append(tasks, q.Task)
	}
	tj.mu.Unlock()
	if strings.Join(tasks, ",") != "ta,tb,tb" {
		t.Fatalf("judge requests for %v, want ta,tb then tb only", tasks)
	}
	if n := countStep(h.models.Calls(), "gate") - revisesBefore; n != 1 {
		t.Fatalf("%d revise calls after Continue, want 1", n)
	}
	for _, sr := range h.detail(done.ID).Steps {
		if sr.ID == "gate" {
			rec = sr // the last attempt
		}
	}
	out, _ = rec.Output.(map[string]any)
	list, _ = out["output"].([]any)
	if out["passed"] != true || len(list) != 2 || list[0] != "fixed ta" || list[1] != "fixed tb" {
		t.Fatalf("output %#v", out)
	}
}

// Each judged answer gets its own transient retry (flows-agent-10).
func TestJudgeRetriesEachAnswer(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var aFail, bFail atomic.Int32
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		switch req.Step {
		case "p":
			return ModelReply{Text: `[{"title":"a","kind":"x","task":"ta"},{"title":"b","kind":"x","task":"tb"}]`}, nil
		case "gate":
			c := &aFail
			if req.Messages[0].Content == "tb" {
				c = &bFail
			}
			if c.Add(1) == 1 {
				return ModelReply{}, errTransient
			}
			return ModelReply{Text: "fixed " + req.Messages[0].Content}, nil
		}
		return ModelReply{Text: "did " + req.Messages[len(req.Messages)-1].Content}, nil
	}
	h.e.o.Judge = &textJudge{revisions: 1, verdict: func(s string) bool { return strings.HasPrefix(s, "fixed") }}
	f := h.create(Flow{Name: "F", Steps: []Step{
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}"},
		{ID: "gate", Kind: KindJudge, On: "f", Criteria: []evals.Criterion{{ID: "good", Prompt: "It is good"}}},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "gate")
	out, _ := rec.Output.(map[string]any)
	if out["passed"] != true || rec.Retries != 2 || !strings.Contains(rec.RetryError, "terminated") {
		t.Fatalf("retries %d %q, output %#v", rec.Retries, rec.RetryError, out)
	}
}

// failingJudge is textJudge that fails the requests fail picks.
type failingJudge struct {
	textJudge
	fail func(JudgeRequest) bool
}

func (j *failingJudge) Judge(ctx context.Context, req JudgeRequest) (JudgeResult, error) {
	if j.fail(req) {
		j.mu.Lock()
		j.reqs = append(j.reqs, req)
		j.mu.Unlock()
		return JudgeResult{}, errors.New("judge model went away")
	}
	return j.textJudge.Judge(ctx, req)
}

func countStep(calls []ModelRequest, step string) int {
	n := 0
	for _, c := range calls {
		if c.Step == step {
			n++
		}
	}
	return n
}
