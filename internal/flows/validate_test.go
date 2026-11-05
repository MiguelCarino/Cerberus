package flows

import (
	"strings"
	"testing"
)

type fakeCatalog struct{}

func (fakeCatalog) Model(id string) (bool, bool) {
	switch id {
	case "big", "small", "cloud/x":
		return true, true
	case "absent":
		return true, false
	}
	return false, false
}
func (fakeCatalog) Agent(name string) bool { return name == "cerberus" }

func validFlow() Flow {
	return Flow{Name: "Ok", Trigger: Trigger{Kind: TriggerManual}, Steps: []Step{
		{ID: "plan", Kind: KindPlan, Model: "big", Template: "Plan {input}"},
		{ID: "fan", Kind: KindFanOut, From: "plan", DefaultModel: "small", Routes: []Route{{Kind: "css", Model: "cloud/x"}}, Template: "{item.task}"},
		{ID: "work", Kind: KindSession, Agent: "cerberus", Task: "Do {steps.fan.output}"},
		{ID: "check", Kind: KindIf, Test: TestState, On: "work", Value: "passed", Else: End},
		{ID: "ok", Kind: KindApprove, Message: "Fine? {steps.work.output}"},
		{ID: "out", Kind: KindOutput, To: ToFile, File: "x-{date}.txt", Template: "{steps.plan.output}"},
	}}
}

func hasProblem(ps []Problem, step, field, sev, contains string) bool {
	for _, p := range ps {
		if p.Step == step && p.Field == field && p.Severity == sev && strings.Contains(p.Message, contains) {
			return true
		}
	}
	return false
}

func TestValidateOK(t *testing.T) {
	if ps := Validate(validFlow(), fakeCatalog{}); len(ps) != 0 {
		t.Fatalf("problems: %+v", ps)
	}
	for _, tpl := range Templates() {
		if ps := Validate(tpl.Flow, nil); HasErrors(ps) {
			t.Errorf("template %s: %+v", tpl.Key, ps)
		}
	}
}

func TestValidateProblems(t *testing.T) {
	f := Flow{Name: "", Trigger: Trigger{Kind: TriggerSchedule, Schedule: "every 5s"}, Limits: Limits{MaxParallel: 99},
		Steps: []Step{
			{ID: "a", Kind: KindPrompt, Model: "nope", Template: "{steps.b.output} {item.title} {wat}"},
			{ID: "a", Kind: KindPrompt, Model: "absent", Template: "x"},
			{ID: "Bad ID", Kind: "dance"},
			{ID: "b", Kind: KindFanOut, From: "a", Template: "x", DefaultModel: "small"},
			{ID: "c", Kind: KindIf, Test: TestMatches, Value: "(", On: "zz", Then: "a"},
			{ID: "d", Kind: KindIf, Test: TestState, On: "a", Then: "nothere"},
			{ID: "g", Kind: KindIf, Test: TestState, On: "a", Value: "done"},
			{ID: "e", Kind: KindSession, Agent: "claude", Workspace: WorkspaceFolder, Folder: "rel/path", Task: "t"},
			{ID: "f", Kind: KindOutput, To: "printer", Template: ""},
			{ID: "end", Kind: KindApprove},
		}}
	ps := Validate(f, fakeCatalog{})
	for _, want := range []struct{ step, field, sev, msg string }{
		{"", "name", SevError, "name"},
		{"", "trigger.schedule", SevError, "unit"},
		{"", "limits.max_parallel", SevError, "max parallel"},
		{"a", "model", SevError, `no model "nope"`},
		{"a", "template", SevError, `"b" is not an earlier step`},
		{"a", "template", SevError, "only known inside a fan-out"},
		{"a", "template", SevError, "{wat} is not a known placeholder"},
		{"a", "id", SevError, "used twice"},
		{"a", "model", SevWarning, "not installed"},
		{"Bad ID", "id", SevError, "slug"},
		{"Bad ID", "kind", SevError, "unknown step kind"},
		{"b", "from", SevError, "plan step or a JSON prompt"},
		{"c", "value", SevError, "regular expression"},
		{"c", "on", SevError, "not an earlier step"},
		{"c", "then", SevError, "forward only"},
		{"d", "on", SevError, "needs a session step"},
		{"d", "then", SevError, `no step "nothere"`},
		{"d", "value", SevError, "pick the session state"},
		{"g", "value", SevError, `"done" is not a session state`},
		{"e", "agent", SevError, "no agent profile"},
		{"e", "folder", SevError, "absolute"},
		{"f", "to", SevError, "notify, inbox or file"},
		{"f", "template", SevError, "write the template"},
		{"end", "id", SevError, "slug"},
	} {
		if !hasProblem(ps, want.step, want.field, want.sev, want.msg) {
			t.Errorf("missing problem %+v in\n%+v", want, ps)
		}
	}
	var ve *ValidationError = &ValidationError{Problems: ps}
	if !strings.HasPrefix(ve.Error(), "invalid flow:") {
		t.Errorf("Error() = %q", ve.Error())
	}
}

func TestValidateLimitsAndSteps(t *testing.T) {
	f := validFlow()
	f.Limits.MaxSteps = 3
	if !hasProblem(Validate(f, nil), "", "steps", SevError, "more than the limit") {
		t.Error("too many steps accepted")
	}
	f = validFlow()
	f.Steps = nil
	if !hasProblem(Validate(f, nil), "", "steps", SevError, "at least one") {
		t.Error("no steps accepted")
	}
	// a JSON prompt may feed a fan-out
	f = Flow{Name: "j", Steps: []Step{
		{ID: "list", Kind: KindPrompt, Model: "big", JSON: true, Template: "x"},
		{ID: "fan", Kind: KindFanOut, From: "list", As: KindSession, Agent: "cerberus", Template: "{item.task}"},
	}}
	if ps := Validate(f, fakeCatalog{}); HasErrors(ps) {
		t.Errorf("json fan-out: %+v", ps)
	}
	// Effective limits
	if l := (Limits{}).Effective(); l != (Limits{50, 200000, 1.00, 2, 60}) {
		t.Errorf("Effective = %+v", l)
	}
}

func TestSlugAndIDs(t *testing.T) {
	for in, want := range map[string]string{
		"Website builder!": "fl-website-builder",
		"  ":               "fl-flow",
		"Ünïcode → ok":     "fl-n-code-ok",
		"a/../../etc":      "fl-a-etc",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if ValidID("fl-../x") || ValidID("fl-") || !ValidID("fl-a-2") {
		t.Error("ValidID")
	}
	if ValidStepID("end") || ValidStepID("1a") || !ValidStepID("a_b-2") {
		t.Error("ValidStepID")
	}
	if sanitizeFile("../../etc/passwd") != "passwd" || sanitizeFile("..") != "" || sanitizeFile("a b.html") != "a_b.html" || sanitizeFile(".hidden") != "hidden" {
		t.Error("sanitizeFile")
	}
}
