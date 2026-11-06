package flows

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"carino.systems/cerberus/internal/evals"
)

// fakeJudgeRunner answers a fixed result, charging usage per call first.
type fakeJudgeRunner struct {
	mu      sync.Mutex
	reqs    []JudgeRequest
	result  JudgeResult
	charges []ModelReply // charged one by one before answering
	meter   ModelReply   // usage of a resumed attempt, through the session meter
	h       *harness
}

func (j *fakeJudgeRunner) Judge(ctx context.Context, req JudgeRequest) (JudgeResult, error) {
	j.mu.Lock()
	j.reqs = append(j.reqs, req)
	charges, res, meter := j.charges, j.result, j.meter
	j.mu.Unlock()
	if meter.PromptTokens > 0 && j.h != nil {
		j.h.sessions.use(req.Session, meter)
		if ctx.Err() != nil {
			return JudgeResult{}, context.Cause(ctx)
		}
	}
	for _, c := range charges {
		if err := req.Charge(c); err != nil {
			return JudgeResult{}, err
		}
	}
	return res, nil
}

func judgeSteps() []Step {
	return []Step{
		{ID: "work", Kind: KindSession, Agent: "cerberus", Task: "build {input}"},
		{ID: "check", Kind: KindJudge, On: "work", Persona: "strict",
			Gates:    []evals.Gate{{ID: "build", MaxIterations: 2}},
			Criteria: []evals.Criterion{{ID: "builds", Prompt: "It builds", Gate: "build"}}},
		{ID: "ok", Kind: KindIf, Test: TestEquals, On: "check", Path: "passed", Value: "true", Else: End},
		{ID: "tell", Kind: KindOutput, To: ToNotify, Template: "passed {steps.check.output}"},
	}
}

func TestJudgeStepValidation(t *testing.T) {
	f := Flow{Name: "J", Steps: judgeSteps()}
	if ps := Validate(f, fakeCatalog{}); HasErrors(ps) {
		t.Fatalf("valid flow: %+v", ps)
	}
	bad := f.clone()
	bad.Steps[1].On = "nope"
	bad.Steps[1].Criteria = nil
	bad.Steps[1].Gates[0].MaxIterations = 9
	bad.Steps[1].Model = "ghost"
	fields := map[string]bool{}
	for _, p := range Validate(bad, fakeCatalog{}) {
		if p.Step == "check" && p.Severity == SevError {
			fields[p.Field] = true
		}
	}
	for _, want := range []string{"on", "criteria", "gates[0].max_iterations", "model"} {
		if !fields[want] {
			t.Fatalf("missing %s in %v", want, fields)
		}
	}
	// on must be a session step
	notSession := Flow{Name: "J", Steps: []Step{
		{ID: "p", Kind: KindPrompt, Model: "small", Template: "x"},
		{ID: "check", Kind: KindJudge, On: "p", Criteria: []evals.Criterion{{ID: "a", Prompt: "b"}}},
	}}
	ps := Validate(notSession, fakeCatalog{})
	if !HasErrors(ps) || !strings.Contains(ps[0].Message, "session step") {
		t.Fatalf("%+v", ps)
	}
	// an unknown persona is a warning when the catalog knows personas
	ps = Validate(f, personaCatalog{})
	if HasErrors(ps) || len(ps) != 1 || ps[0].Field != "persona" || ps[0].Severity != SevWarning {
		t.Fatalf("persona: %+v", ps)
	}
	if !slices.Contains(Kinds, KindJudge) {
		t.Fatal("judge is not a listed kind")
	}
}

type personaCatalog struct{ fakeCatalog }

func (personaCatalog) Persona(name string) bool { return name == "kind" }

func TestJudgeStepRunsAndBranches(t *testing.T) {
	h := newHarness(t)
	fj := &fakeJudgeRunner{h: h, result: JudgeResult{Passed: true, GatesPassed: 1, GatesTotal: 1,
		Report: evals.Report{Session: "s-1", GatesPassed: 1, GatesTotal: 1}},
		charges: []ModelReply{{Model: "judge", PromptTokens: 40, CompletionTokens: 10, CostUSD: 0.01}}}
	h.e.o.Judge = fj
	f := h.create(Flow{Name: "J", Steps: judgeSteps()})
	r := h.wait(h.run(f.ID, "x").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	req := fj.reqs[0]
	if req.Session != "s-1" || req.Persona != "strict" || len(req.Criteria) != 1 || req.Run != r.ID || req.Step != "check" {
		t.Fatalf("request %+v", req)
	}
	rec := h.stepRec(r.ID, "check")
	out, _ := rec.Output.(map[string]any)
	if out["passed"] != true || out["gates_passed"] != float64(1) || out["session"] != "s-1" || rec.Session != "s-1" {
		t.Fatalf("output %#v", out)
	}
	if _, ok := out["report"].(map[string]any); !ok {
		t.Fatalf("report %#v", out["report"])
	}
	if f, ok := out["failed"].([]any); !ok || len(f) != 0 {
		t.Fatalf("failed %#v", out["failed"])
	}
	if r.Tokens != 50 || rec.PromptTokens != 40 || !slices.Contains(r.Models, "judge") {
		t.Fatalf("usage run %d rec %+v models %v", r.Tokens, rec, r.Models)
	}
	if n, _ := h.notifier.counts(); n != 1 {
		t.Fatal("passed did not branch to the output")
	}
	if h.sessions.metered("s-1") {
		t.Fatal("the session meter was not stopped")
	}
	g, _ := h.e.Graph(r.ID)
	if g.Nodes[2].Sub != "judge · on work · 1 criterion" || g.Nodes[2].Href != "#/s/s-1" {
		t.Fatalf("graph node %+v", g.Nodes[2])
	}

	// Not passed: the if step goes to end.
	fj.mu.Lock()
	fj.result = JudgeResult{Passed: false, GatesTotal: 1, Failed: []string{"builds"}}
	fj.mu.Unlock()
	r = h.wait(h.run(f.ID, "x").ID)
	if n, _ := h.notifier.counts(); n != 1 || r.State != StatePassed {
		t.Fatalf("failed verdict branched: %d %s", n, r.State)
	}
	out, _ = h.stepRec(r.ID, "check").Output.(map[string]any)
	if fl, _ := out["failed"].([]any); len(fl) != 1 || fl[0] != "builds" {
		t.Fatalf("failed %#v", out["failed"])
	}
}

func TestJudgeStepLimits(t *testing.T) {
	h := newHarness(t)
	fj := &fakeJudgeRunner{h: h, charges: []ModelReply{{Model: "judge", PromptTokens: 500, CompletionTokens: 1}},
		result: JudgeResult{Passed: true}}
	h.e.o.Judge = fj
	f := h.create(Flow{Name: "J", Limits: Limits{MaxTokens: 100}, Steps: judgeSteps()})
	r := h.wait(h.run(f.ID, "x").ID)
	if r.State != StateLimited || !strings.Contains(r.Reason, "tokens") {
		t.Fatalf("judge calls over the limit: %s %q", r.State, r.Reason)
	}
	if rec := h.stepRec(r.ID, "check"); rec.State != StepLimited {
		t.Fatalf("record %+v", rec)
	}
	if !slices.Contains(h.sessions.Stopped(), "s-1") {
		t.Fatal("the judged session was not stopped")
	}

	// The usage of a resumed attempt (session meter) counts too.
	fj.mu.Lock()
	fj.charges, fj.meter = nil, ModelReply{Model: "agent", PromptTokens: 900}
	fj.mu.Unlock()
	r = h.wait(h.run(f.ID, "x").ID)
	if r.State != StateLimited || r.Tokens != 900 {
		t.Fatalf("metered attempt: %s %q %d", r.State, r.Reason, r.Tokens)
	}

	// No judge runner: the step fails clearly.
	h.e.o.Judge = nil
	r = h.wait(h.run(f.ID, "x").ID)
	if r.State != StateFailed || !strings.Contains(r.Reason, "judging is not available") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
}

func TestEqualsWithPath(t *testing.T) {
	s := Step{Test: TestEquals, Path: "passed", Value: "true"}
	if ok, _ := evalTest(s, map[string]any{"passed": true}); !ok {
		t.Fatal("passed true")
	}
	if ok, _ := evalTest(s, map[string]any{"passed": false}); ok {
		t.Fatal("passed false")
	}
	if ok, _ := evalTest(s, `{"passed": true}`); !ok {
		t.Fatal("JSON text")
	}
	if ok, _ := evalTest(Step{Test: TestEquals, Value: "hi"}, " hi "); !ok {
		t.Fatal("plain equals")
	}
}
