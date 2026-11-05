package flows

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Problem is one validation finding. Step is the step id ("" for the
// flow itself), Field the JSON field name ("" when it is about the whole
// step), Severity "error" (blocks save and run) or "warning".
type Problem struct {
	Step     string `json:"step"`
	Field    string `json:"field"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// Severities.
const (
	SevError   = "error"
	SevWarning = "warning"
)

// HasErrors reports whether any problem is an error.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if p.Severity == SevError {
			return true
		}
	}
	return false
}

// ValidationError is returned by saves and runs of a flow with errors.
type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	for _, p := range e.Problems {
		if p.Severity == SevError {
			where := p.Step
			if p.Field != "" {
				where = strings.TrimPrefix(where+"."+p.Field, ".")
			}
			if where != "" {
				return "invalid flow: " + where + ": " + p.Message
			}
			return "invalid flow: " + p.Message
		}
	}
	return "invalid flow"
}

// Validate checks a flow (SCOPE: Validation): known kinds, unique step
// ids, references to earlier steps only, forward jumps, schedule, limits,
// template placeholders, fan-out sources, and, when cat is not nil, models
// and agent profiles that exist (a warning when a model is not installed
// now). It never changes f. The result is never nil.
func Validate(f Flow, cat Catalog) []Problem {
	v := validator{cat: cat, ps: []Problem{}}
	v.flow(f)
	return v.ps
}

type validator struct {
	cat Catalog
	ps  []Problem
}

func (v *validator) add(step, field, sev, format string, a ...any) {
	v.ps = append(v.ps, Problem{Step: step, Field: field, Message: fmt.Sprintf(format, a...), Severity: sev})
}

func (v *validator) flow(f Flow) {
	if strings.TrimSpace(f.Name) == "" {
		v.add("", "name", SevError, "give the flow a name")
	} else if len([]rune(f.Name)) > maxNameLen {
		v.add("", "name", SevError, "the name is longer than %d characters", maxNameLen)
	}
	if len(f.Description) > maxDescLen {
		v.add("", "description", SevError, "the description is longer than %d characters", maxDescLen)
	}
	if len(f.InputHint) > maxHintLen {
		v.add("", "input_hint", SevError, "the input hint is longer than %d characters", maxHintLen)
	}
	switch f.Trigger.Kind {
	case TriggerManual, "":
	case TriggerSchedule:
		if _, err := ParseSchedule(f.Trigger.Schedule); err != nil {
			v.add("", "trigger.schedule", SevError, "%v", err)
		}
	default:
		v.add("", "trigger.kind", SevError, "the trigger is manual or schedule, not %q", f.Trigger.Kind)
	}
	v.limits(f.Limits)
	lim := f.Limits.Effective()
	if len(f.Steps) == 0 {
		v.add("", "steps", SevError, "add at least one step")
	}
	if len(f.Steps) > lim.MaxSteps {
		v.add("", "steps", SevError, "%d steps is more than the limit of %d", len(f.Steps), lim.MaxSteps)
	}
	index := map[string]int{}
	for i, s := range f.Steps {
		switch {
		case !ValidStepID(s.ID):
			v.add(s.ID, "id", SevError, "step %d: the id is a short slug (a-z, 0-9, - and _; starts with a letter; not \"end\")", i+1)
		case index[s.ID] != 0:
			v.add(s.ID, "id", SevError, "the id %q is used twice", s.ID)
		}
		if _, dup := index[s.ID]; !dup {
			index[s.ID] = i + 1 // 1-based so 0 means absent
		}
	}
	for i, s := range f.Steps {
		earlier := func(id string) bool { n := index[id]; return n > 0 && n-1 < i }
		v.step(f, i, s, earlier)
	}
}

func (v *validator) limits(l Limits) {
	if l.MaxSteps < 0 || l.MaxSteps > maxMaxSteps {
		v.add("", "limits.max_steps", SevError, "max steps is 1 to %d", maxMaxSteps)
	}
	if l.MaxTokens < 0 {
		v.add("", "limits.max_tokens", SevError, "max tokens cannot be negative")
	}
	if l.MaxCostUSD < 0 {
		v.add("", "limits.max_cost_usd", SevError, "max cost cannot be negative")
	}
	if l.MaxParallel < 0 || l.MaxParallel > maxMaxParallel {
		v.add("", "limits.max_parallel", SevError, "max parallel is 1 to %d", maxMaxParallel)
	}
	if l.TimeoutMin < 0 || l.TimeoutMin > maxTimeoutMin {
		v.add("", "limits.timeout_min", SevError, "the timeout is 1 to %d minutes", maxTimeoutMin)
	}
}

func (v *validator) template(step, field, t string, required bool, earlier func(string) bool, inItem bool) {
	if strings.TrimSpace(t) == "" {
		if required {
			v.add(step, field, SevError, "write the %s", strings.ReplaceAll(field, "_", " "))
		}
		return
	}
	if len(t) > maxTemplateLen {
		v.add(step, field, SevError, "longer than %d KiB", maxTemplateLen>>10)
	}
	for _, name := range Placeholders(t) {
		if msg := checkPlaceholder(name, earlier, inItem); msg != "" {
			v.add(step, field, SevError, "%s", msg)
		}
	}
}

func (v *validator) model(step, field, id string) {
	if strings.TrimSpace(id) == "" {
		v.add(step, field, SevError, "pick a model")
		return
	}
	if v.cat == nil || id == "default" {
		return
	}
	known, installed := v.cat.Model(id)
	switch {
	case !known:
		v.add(step, field, SevError, "no model %q", id)
	case !installed:
		v.add(step, field, SevWarning, "the model %q is not installed now", id)
	}
}

func (v *validator) agent(step, field, name string) {
	if strings.TrimSpace(name) == "" {
		v.add(step, field, SevError, "pick an agent")
		return
	}
	if v.cat != nil && !v.cat.Agent(name) {
		v.add(step, field, SevError, "no agent profile %q", name)
	}
}

func (v *validator) workspace(s Step) {
	switch s.workspace() {
	case WorkspaceScratch:
	case WorkspaceRepo:
		if strings.TrimSpace(s.Repo) == "" {
			v.add(s.ID, "repo", SevError, "name the repository")
		}
	case WorkspaceFolder:
		if !filepath.IsAbs(s.Folder) && !strings.HasPrefix(s.Folder, "/") {
			v.add(s.ID, "folder", SevError, "the folder is an absolute path")
		}
	default:
		v.add(s.ID, "workspace", SevError, "the workspace is scratch, repo or folder")
	}
}

func (v *validator) step(f Flow, i int, s Step, earlier func(string) bool) {
	switch s.Kind {
	case KindPrompt:
		v.model(s.ID, "model", s.Model)
		v.template(s.ID, "system", s.System, false, earlier, false)
		v.template(s.ID, "template", s.Template, true, earlier, false)
	case KindPlan:
		v.model(s.ID, "model", s.Model)
		v.template(s.ID, "template", s.Template, true, earlier, false)
		if s.MaxItems < 0 || s.MaxItems > maxMaxItems {
			v.add(s.ID, "max_items", SevError, "max items is 1 to %d", maxMaxItems)
		}
	case KindFanOut:
		switch {
		case s.From == "":
			v.add(s.ID, "from", SevError, "pick the step whose items fan out")
		case !earlier(s.From):
			v.add(s.ID, "from", SevError, "%q is not an earlier step", s.From)
		default:
			src := f.Steps[f.stepIndex(s.From)]
			if src.Kind != KindPlan && !(src.Kind == KindPrompt && src.JSON) {
				v.add(s.ID, "from", SevError, "fan out from a plan step or a JSON prompt step")
			}
		}
		for n, r := range s.Routes {
			if strings.TrimSpace(r.Kind) == "" {
				v.add(s.ID, fmt.Sprintf("routes.%d.kind", n), SevError, "route %d needs a kind", n+1)
			}
			if s.fanAs() == KindPrompt {
				v.model(s.ID, fmt.Sprintf("routes.%d.model", n), r.Model)
			} else if r.Model != "" {
				v.model(s.ID, fmt.Sprintf("routes.%d.model", n), r.Model)
			}
		}
		switch s.fanAs() {
		case KindPrompt:
			v.model(s.ID, "default_model", s.DefaultModel)
		case KindSession:
			if s.DefaultModel != "" {
				v.model(s.ID, "default_model", s.DefaultModel)
			}
			v.agent(s.ID, "agent", s.Agent)
			v.workspace(s)
		default:
			v.add(s.ID, "as", SevError, "fan out as prompt or session")
		}
		v.template(s.ID, "template", s.Template, true, earlier, true)
		if s.MaxParallel < 0 || s.MaxParallel > maxMaxParallel {
			v.add(s.ID, "max_parallel", SevError, "max parallel is 1 to %d", maxMaxParallel)
		}
	case KindSession:
		v.agent(s.ID, "agent", s.Agent)
		v.workspace(s)
		v.template(s.ID, "task", s.Task, true, earlier, false)
	case KindIf:
		v.ifStep(f, i, s, earlier)
	case KindApprove:
		v.template(s.ID, "message", s.Message, false, earlier, false)
	case KindOutput:
		switch s.To {
		case ToNotify, ToInbox:
		case ToFile:
			v.template(s.ID, "file", s.File, false, earlier, false)
			if strings.TrimSpace(s.File) != "" && len(Placeholders(s.File)) == 0 && sanitizeFile(s.File) == "" {
				v.add(s.ID, "file", SevError, "the file name has no usable characters")
			}
		default:
			v.add(s.ID, "to", SevError, "send the output to notify, inbox or file")
		}
		v.template(s.ID, "template", s.Template, true, earlier, false)
	case "":
		v.add(s.ID, "kind", SevError, "pick a step kind")
	default:
		v.add(s.ID, "kind", SevError, "unknown step kind %q", s.Kind)
	}
}

// SessionStates are the final session states a state test can compare
// with (what a SessionRunner's Wait answers).
var SessionStates = []string{"passed", "failed", "stopped", "error", "accepted", "discarded", "removed"}

func (v *validator) ifStep(f Flow, i int, s Step, earlier func(string) bool) {
	switch {
	case s.On == "":
		v.add(s.ID, "on", SevError, "pick the step to test")
	case !earlier(s.On):
		v.add(s.ID, "on", SevError, "%q is not an earlier step", s.On)
	}
	switch s.Test {
	case TestContains, TestEquals:
	case TestMatches:
		if _, err := regexp.Compile(s.Value); err != nil {
			v.add(s.ID, "value", SevError, "not a regular expression: %v", err)
		}
	case TestJSON:
		if strings.TrimSpace(s.Path) == "" {
			v.add(s.ID, "path", SevError, "write the dotted path to compare")
		}
	case TestState:
		if earlier(s.On) && f.Steps[f.stepIndex(s.On)].Kind != KindSession {
			v.add(s.ID, "on", SevError, "a state test needs a session step")
		}
		switch st := strings.TrimSpace(s.Value); {
		case st == "":
			v.add(s.ID, "value", SevError, "pick the session state to test for")
		case !slices.Contains(SessionStates, st):
			v.add(s.ID, "value", SevError, "%q is not a session state (%s)", st, strings.Join(SessionStates, ", "))
		}
	case "":
		v.add(s.ID, "test", SevError, "pick a test")
	default:
		v.add(s.ID, "test", SevError, "the test is contains, matches, equals, json or state")
	}
	for _, j := range []struct{ field, to string }{{"then", s.Then}, {"else", s.Else}} {
		if j.to == "" || j.to == End {
			continue
		}
		n := f.stepIndex(j.to)
		switch {
		case n < 0:
			v.add(s.ID, j.field, SevError, "no step %q", j.to)
		case n <= i:
			v.add(s.ID, j.field, SevError, "jumps go forward only (%q comes before)", j.to)
		}
	}
}
