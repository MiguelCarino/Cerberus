package flows

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// A session step's output carries the workspace, files, checks, diff
// summary and final answer; later templates read single fields, and the
// state test still works.
func TestSessionOutputFields(t *testing.T) {
	h := newHarness(t)
	h.sessions.result = SessionResult{State: "passed", Summary: "done", Workspace: "/ws/s-1",
		Files: []SessionFile{{Path: "index.html", Bytes: 120}}, Checks: "ok 3 tests", DiffStat: "", Final: "I wrote <index.html>."}
	f := h.create(Flow{Name: "S", Steps: []Step{
		{ID: "work", Kind: KindSession, Agent: "cerberus", Task: "build {input}"},
		{ID: "ok", Kind: KindIf, Test: TestState, On: "work", Value: "passed", Else: End},
		{ID: "next", Kind: KindPrompt, Model: "small", Template: "Folder {steps.work.output.workspace}; first file {steps.work.output.files.0.path}; agent said: {steps.work.output.final}"},
	}})
	r := h.wait(h.run(f.ID, "a site").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	out, _ := h.stepRec(r.ID, "work").Output.(map[string]any)
	files, _ := out["files"].([]any)
	if out["session"] != "s-1" || out["state"] != "passed" || out["summary"] != "done" || out["workspace"] != "/ws/s-1" ||
		out["final"] != "I wrote <index.html>." || out["checks"] != "ok 3 tests" || len(files) != 1 {
		t.Fatalf("session output %#v", out)
	}
	next := h.stepRec(r.ID, "next")
	if next.Input != "Folder /ws/s-1; first file index.html; agent said: I wrote <index.html>." {
		t.Fatalf("next input %q", next.Input)
	}
}

// Workspace "step": a session step works in an earlier session step's
// workspace (SameAs), or starts with a fan-out's session files (Seeds).
func TestSessionStepWorkspace(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: `[{"title":"Home Page!","kind":"html","task":"a"},{"title":"","kind":"css","task":"b"}]`}, nil
	}
	f := h.create(Flow{Name: "W", Steps: []Step{
		{ID: "first", Kind: KindSession, Agent: "cerberus", Task: "start"},
		{ID: "again", Kind: KindSession, Agent: "cerberus", Workspace: WorkspaceStep, From: "first", Task: "go on"},
		{ID: "plan", Kind: KindPlan, Model: "big", Template: "plan"},
		{ID: "parts", Kind: KindFanOut, From: "plan", As: KindSession, Agent: "cerberus", Template: "{item.task}"},
		{ID: "join", Kind: KindSession, Agent: "cerberus", Workspace: WorkspaceStep, From: "parts", Task: "join"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	h.sessions.mu.Lock()
	reqs := slices.Clone(h.sessions.reqs)
	h.sessions.mu.Unlock()
	if len(reqs) != 5 {
		t.Fatalf("%d sessions", len(reqs))
	}
	if reqs[1].Workspace != WorkspaceStep || reqs[1].SameAs != "s-1" || len(reqs[1].Seeds) != 0 {
		t.Fatalf("again %+v", reqs[1])
	}
	// the fan-out items are s-3 and s-4 (in either order of start)
	join := reqs[4]
	if join.SameAs != "" || len(join.Seeds) != 2 || join.Seeds[0].Into != "parts/1-home-page" || join.Seeds[1].Into != "parts/2-part" {
		t.Fatalf("join %+v", join)
	}
	got := []string{join.Seeds[0].Session, join.Seeds[1].Session}
	slices.Sort(got)
	if !slices.Equal(got, []string{"s-3", "s-4"}) {
		t.Fatalf("seed sessions %v", got)
	}
}

func TestStepWorkspaceValidation(t *testing.T) {
	base := []Step{
		{ID: "p", Kind: KindPrompt, Model: "small", Template: "x"},
		{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "t"},
	}
	cases := []struct {
		step  Step
		field string
	}{
		{Step{ID: "s", Kind: KindSession, Agent: "cerberus", Workspace: WorkspaceStep, Task: "t"}, "from"},
		{Step{ID: "s", Kind: KindSession, Agent: "cerberus", Workspace: WorkspaceStep, From: "p", Task: "t"}, "from"},
		{Step{ID: "s", Kind: KindSession, Agent: "cerberus", Workspace: WorkspaceStep, From: "later", Task: "t"}, "from"},
		{Step{ID: "s", Kind: KindFanOut, From: "p", As: KindSession, Agent: "cerberus", Workspace: WorkspaceStep, Template: "x"}, "workspace"},
	}
	for i, c := range cases {
		steps := append(slices.Clone(base), c.step, Step{ID: "later", Kind: KindSession, Agent: "cerberus", Task: "t"})
		steps[0].JSON = true // a fan-out source
		ps := Validate(Flow{Name: "V", Steps: steps}, nil)
		found := false
		for _, p := range ps {
			if p.Step == "s" && p.Field == c.field && p.Severity == SevError {
				found = true
			}
		}
		if !found {
			t.Errorf("case %d: no %s error in %+v", i, c.field, ps)
		}
	}
	ok := append(slices.Clone(base), Step{ID: "s", Kind: KindSession, Agent: "cerberus", Workspace: WorkspaceStep, From: "w", Task: "{steps.w.output.final}"})
	if ps := Validate(Flow{Name: "V", Steps: ok}, nil); HasErrors(ps) {
		t.Fatalf("valid step workspace refused: %+v", ps)
	}
}

// Continue keeps the fan-out items that passed: only the failed one calls
// its model again, the output keeps its order, and kept items are not
// charged again.
func TestContinueKeepsPassedFanOutItems(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var fail atomic.Bool
	fail.Store(true)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		task := req.Messages[len(req.Messages)-1].Content
		switch {
		case req.Step == "p":
			return ModelReply{Text: `["a","b","c"]`, PromptTokens: 1}, nil
		case task == "b" && fail.Load():
			return ModelReply{}, errTransient
		}
		return ModelReply{Text: "done " + task, PromptTokens: 3, CompletionTokens: 2}, nil
	}
	f := fanFlow(h)
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateFailed {
		t.Fatalf("first: %s %q", r.State, r.Reason)
	}
	tokens := r.Tokens
	before := len(h.models.Calls())
	fail.Store(false)
	if _, err := h.e.Continue("owner:test", r.ID); err != nil {
		t.Fatal(err)
	}
	r = h.wait(r.ID)
	if r.State != StatePassed {
		t.Fatalf("after continue: %s %q", r.State, r.Reason)
	}
	calls := h.models.Calls()[before:]
	if len(calls) != 1 || calls[0].Messages[0].Content != "b" {
		t.Fatalf("continue made %d calls: %+v", len(calls), calls)
	}
	if r.Tokens != tokens+5 {
		t.Fatalf("tokens %d, want %d", r.Tokens, tokens+5)
	}
	var last StepRecord
	for _, rec := range h.detail(r.ID).Steps {
		if rec.ID == "f" {
			last = rec
		}
	}
	outs, _ := last.Output.([]any)
	if last.State != StepPassed || !slices.Equal(outs, []any{"done a", "done b", "done c"}) {
		t.Fatalf("output %#v", last.Output)
	}
	for _, it := range last.Items {
		if (it.Title != "b") != it.Reused {
			t.Fatalf("item %s reused=%v", it.Title, it.Reused)
		}
		if it.Reused && (it.Calls != 0 || it.PromptTokens != 0 || it.State != StepPassed) {
			t.Fatalf("kept item %+v", it)
		}
	}
	if !strings.Contains(last.Input, "2 kept") || last.PromptTokens != 3 {
		t.Fatalf("step record %q tokens %d", last.Input, last.PromptTokens)
	}
}

// An item whose input renders differently now, or whose route picks
// another model, runs again.
func TestContinueRerunsChangedItems(t *testing.T) {
	h := newHarness(t)
	rc := &runCtx{snap: Flow{Name: "F"}, outputs: map[string]any{}, known: map[string]bool{}}
	s := Step{ID: "f", Kind: KindFanOut, DefaultModel: "small", Template: "{item.task} at {time}"}
	it := map[string]string{"title": "a", "kind": "", "task": "a"}
	text, _ := render(s.Template, h.e.env(rc, it))
	p := ItemRecord{Title: "a", Model: "small-q4", Route: "small", State: StepPassed, Input: text, Output: "x", PromptTokens: 9, Calls: 1}
	if kept, ok := h.e.reusable(rc, s, p, it, "small"); !ok || !kept.Reused || kept.Calls != 0 || kept.Model != "small-q4" {
		t.Fatalf("same item not kept: %+v %v", kept, ok)
	}
	if _, ok := h.e.reusable(rc, s, p, it, "big"); ok {
		t.Fatal("kept with another route model")
	}
	p2 := p
	p2.Input = "a at 00:00"
	if _, ok := h.e.reusable(rc, s, p2, it, "small"); ok {
		t.Fatal("kept with another input")
	}
	p3 := p
	p3.State = StepFailed
	if _, ok := h.e.reusable(rc, s, p3, it, "small"); ok {
		t.Fatal("kept a failed item")
	}
}

// The Website template hands the parts to the Assemble session as titled
// sections with the markup as written, not as escaped JSON.
func TestWebsiteTemplateAssembleTask(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if req.Step == "plan" {
			return ModelReply{Text: `[{"title":"Home","kind":"html","task":"page"},{"title":"Look","kind":"css","task":"styles"}]`}, nil
		}
		if strings.Contains(req.Messages[0].Content, "(html)") {
			return ModelReply{Text: "<main>\n  <h1>Tom & Jerry</h1>\n</main>"}, nil
		}
		return ModelReply{Text: "h1 { color: red; }"}, nil
	}
	var site Flow
	for _, tpl := range Templates() {
		if tpl.Key == "website" {
			site = tpl.Flow
		}
	}
	site.Enabled = true
	f := h.create(site)
	r := h.wait(h.run(f.ID, "a bakery").ID)
	if r.State != StateWaiting {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	h.sessions.mu.Lock()
	task := h.sessions.reqs[0].Task
	h.sessions.mu.Unlock()
	if !strings.Contains(task, "### 1. Home (html)\n<main>\n  <h1>Tom & Jerry</h1>\n</main>\n\n### 2. Look (css)\nh1 { color: red; }") {
		t.Fatalf("assemble task:\n%s", task)
	}
	if strings.Contains(task, "\\u003c") || strings.Contains(task, `\n`) {
		t.Fatalf("assemble task is escaped:\n%s", task)
	}
}
