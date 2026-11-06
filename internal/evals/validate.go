package evals

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Severities of a Problem.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Problem is one validation finding. Field names the part of the
// definition ("criteria[2].depends_on", "setups[0].model", "" for the
// whole eval).
type Problem struct {
	Field    string `json:"field"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// ValidationError carries the problems that blocked a save or a run.
type ValidationError struct{ Problems []Problem }

func (v *ValidationError) Error() string {
	for _, p := range v.Problems {
		if p.Severity == SeverityError {
			if p.Field != "" {
				return "invalid eval: " + p.Field + ": " + p.Message
			}
			return "invalid eval: " + p.Message
		}
	}
	return "invalid eval"
}

// HasErrors reports whether any problem is an error.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Validate checks a (normalized) definition. Structure is always checked;
// with a catalog, agents, models and the persona are checked too (unknown
// is an error, known but not installed a warning).
func Validate(e Eval, cat Catalog) []Problem {
	e = e.clone()
	normalize(&e)
	var ps []Problem
	errf := func(field, format string, a ...any) {
		ps = append(ps, Problem{Field: field, Message: fmt.Sprintf(format, a...), Severity: SeverityError})
	}
	warnf := func(field, format string, a ...any) {
		ps = append(ps, Problem{Field: field, Message: fmt.Sprintf(format, a...), Severity: SeverityWarning})
	}

	if e.Name == "" {
		errf("name", "a name is required")
	} else if utf8.RuneCountInString(e.Name) > MaxNameRunes {
		errf("name", "at most %d characters", MaxNameRunes)
	}
	if e.ID != "" && !ValidID(e.ID) {
		errf("id", "an eval id is ev- and lowercase letters, digits and dashes")
	}
	if e.Task == "" {
		errf("task", "the task the agents get is required")
	} else if len(e.Task) > MaxTaskBytes {
		errf("task", "at most %d KiB", MaxTaskBytes>>10)
	}
	switch e.Workspace {
	case WorkspaceScratch:
	case WorkspaceRepo:
		if e.Repo == "" {
			errf("repo", "a repo eval names its repository")
		}
		if e.Base != "" && !ValidCommit(e.Base) {
			errf("base", "a commit id (7 to 64 hex characters)")
		}
	default:
		errf("workspace", "scratch or repo")
	}
	if e.Repeat < 1 || e.Repeat > MaxRepeat {
		errf("repeat", "1 to %d", MaxRepeat)
	}
	if e.Persona != "" && !personaRe.MatchString(e.Persona) {
		errf("persona", "a persona name is lowercase letters, digits, - and _")
	}

	ps = append(ps, graphProblems(e)...)

	// Setups.
	if len(e.Setups) == 0 {
		errf("setups", "at least one setup (an agent, and a model for agents that take one)")
	}
	if len(e.Setups) > MaxSetups {
		errf("setups", "at most %d setups", MaxSetups)
	}
	labels := map[string]bool{}
	for i, s := range e.Setups {
		f := fmt.Sprintf("setups[%d]", i)
		if s.Agent == "" {
			errf(f+".agent", "an agent profile is required")
		}
		if s.Label == "" {
			errf(f+".label", "a label is required")
		} else if utf8.RuneCountInString(s.Label) > MaxLabelRunes {
			errf(f+".label", "at most %d characters", MaxLabelRunes)
		} else if labels[strings.ToLower(s.Label)] {
			errf(f+".label", "label %q is used twice", s.Label)
		}
		labels[strings.ToLower(s.Label)] = true
		if strings.HasPrefix(s.Agent, "-") || strings.HasPrefix(s.Model, "-") {
			errf(f, "an agent or model cannot start with -")
		}
		if cat == nil {
			continue
		}
		if s.Agent != "" {
			if known, installed := cat.Agent(s.Agent); !known {
				errf(f+".agent", "no agent profile %q", s.Agent)
			} else if !installed {
				warnf(f+".agent", "agent %q is not installed on this machine", s.Agent)
			}
		}
		if s.Model != "" {
			if known, installed := cat.Model(s.Model); !known {
				errf(f+".model", "no model %q (not configured, not listed by an engine, no provider)", s.Model)
			} else if !installed {
				warnf(f+".model", "model %q is configured but not installed now", s.Model)
			}
		}
	}
	if cat != nil && e.JudgeModel != "" {
		if known, installed := cat.Model(e.JudgeModel); !known {
			errf("judge_model", "no model %q", e.JudgeModel)
		} else if !installed {
			warnf("judge_model", "model %q is configured but not installed now", e.JudgeModel)
		}
	}
	if cat != nil && e.Persona != "" && personaRe.MatchString(e.Persona) && !cat.Persona(e.Persona) {
		warnf("persona", "no persona %q in evals.personas: the built-in reviewer is used", e.Persona)
	}
	return ps
}

// graphProblems checks the gates and the criteria graph of a normalized
// definition: ids, iteration budgets, prompts, gates of criteria,
// dependencies (same or earlier gate, no cycle) and evidence kinds.
func graphProblems(e Eval) []Problem {
	var ps []Problem
	errf := func(field, format string, a ...any) {
		ps = append(ps, Problem{Field: field, Message: fmt.Sprintf(format, a...), Severity: SeverityError})
	}
	// Gates.
	gateIdx := map[string]int{}
	if len(e.Gates) > MaxGates {
		errf("gates", "at most %d gates", MaxGates)
	}
	for i, g := range e.Gates {
		f := fmt.Sprintf("gates[%d]", i)
		switch {
		case !ValidCriterionID(g.ID):
			errf(f+".id", "a gate id is a lowercase letter, then letters, digits or _ (at most 48)")
		case hasKey(gateIdx, g.ID):
			errf(f+".id", "gate %q is listed twice", g.ID)
		default:
			gateIdx[g.ID] = i
		}
		if g.MaxIterations < 1 || g.MaxIterations > MaxIterations {
			errf(f+".max_iterations", "1 to %d", MaxIterations)
		}
		if utf8.RuneCountInString(g.Name) > MaxNameRunes {
			errf(f+".name", "at most %d characters", MaxNameRunes)
		}
	}
	if len(e.Gates) == 0 {
		gateIdx[ImplicitGate] = 0
	}

	// Criteria.
	if len(e.Criteria) == 0 {
		errf("criteria", "at least one criterion")
	}
	if len(e.Criteria) > MaxCriteria {
		errf("criteria", "at most %d criteria", MaxCriteria)
	}
	crit := map[string]int{}
	for i, c := range e.Criteria {
		f := fmt.Sprintf("criteria[%d]", i)
		switch {
		case !ValidCriterionID(c.ID):
			errf(f+".id", "a criterion id is a lowercase letter, then letters, digits or _ (at most 48)")
		case hasKey(crit, c.ID):
			errf(f+".id", "criterion %q is listed twice", c.ID)
		default:
			crit[c.ID] = i
		}
		if c.Prompt == "" {
			errf(f+".prompt", "say what the judge should decide")
		} else if utf8.RuneCountInString(c.Prompt) > MaxPromptRunes {
			errf(f+".prompt", "at most %d characters", MaxPromptRunes)
		}
		if _, ok := gateIdx[GateOf(e, c)]; !ok {
			errf(f+".gate", "no gate %q in this eval", c.Gate)
		}
		for _, k := range c.Evidence {
			if !contains(EvidenceKinds, k) {
				errf(f+".evidence", "%q is not an evidence kind (%s)", k, strings.Join(EvidenceKinds, ", "))
			}
		}
	}
	for i, c := range e.Criteria {
		f := fmt.Sprintf("criteria[%d].depends_on", i)
		seen := map[string]bool{}
		for _, d := range c.DependsOn {
			j, ok := crit[d]
			switch {
			case !ok:
				errf(f, "no criterion %q in this eval", d)
				continue
			case d == c.ID:
				errf(f, "a criterion cannot depend on itself")
				continue
			case seen[d]:
				errf(f, "%q is listed twice", d)
				continue
			}
			seen[d] = true
			gi, ok1 := gateIdx[GateOf(e, c)]
			gd, ok2 := gateIdx[GateOf(e, e.Criteria[j])]
			if ok1 && ok2 && gd > gi {
				errf(f, "%q is in a later gate (%s): a dependency's gate must be the same or an earlier one",
					d, GateOf(e, e.Criteria[j]))
			}
		}
	}
	if cyc := cycle(e); len(cyc) > 0 {
		errf("criteria", "the dependencies go round in a circle: %s", strings.Join(cyc, " → "))
	}

	return ps
}

// ValidateCriteria checks gates and criteria outside an eval (a flow's
// judge step) with the rules of Validate; persona is checked for its form
// and, with a catalog, that evals.personas has it (a warning).
func ValidateCriteria(gates []Gate, criteria []Criterion, persona string, cat Catalog) []Problem {
	e := Eval{Gates: append([]Gate(nil), gates...), Criteria: append([]Criterion(nil), criteria...), Persona: persona}
	e = e.clone()
	normalize(&e)
	ps := graphProblems(e)
	if e.Persona != "" && !personaRe.MatchString(e.Persona) {
		ps = append(ps, Problem{Field: "persona", Message: "a persona name is lowercase letters, digits, - and _", Severity: SeverityError})
	} else if cat != nil && e.Persona != "" && !cat.Persona(e.Persona) {
		ps = append(ps, Problem{Field: "persona", Message: fmt.Sprintf("no persona %q in evals.personas: the built-in reviewer is used", e.Persona), Severity: SeverityWarning})
	}
	return nonNilProblems(ps)
}

func hasKey[V any](m map[string]V, k string) bool { _, ok := m[k]; return ok }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// cycle finds one dependency cycle (criterion ids, the first repeated at
// the end), or nil.
func cycle(e Eval) []string {
	deps := map[string][]string{}
	for _, c := range e.Criteria {
		deps[c.ID] = c.DependsOn
	}
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var found []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range deps[id] {
			if _, ok := deps[d]; !ok || d == id {
				continue // reported elsewhere
			}
			switch color[d] {
			case grey:
				for i, s := range stack {
					if s == d {
						found = append(append([]string(nil), stack[i:]...), d)
						return true
					}
				}
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	ids := make([]string, 0, len(deps))
	for _, c := range e.Criteria {
		ids = append(ids, c.ID)
	}
	for _, id := range ids {
		if color[id] == white && visit(id) {
			return found
		}
	}
	return nil
}

// Order is the criteria of each gate, in gate order, each gate's in a
// topological order (a criterion after its dependencies; ties keep the
// definition's order). The eval must be valid.
func Order(e Eval) [][]Criterion {
	gates := EffectiveGates(e)
	pos := map[string]int{}
	for i, c := range e.Criteria {
		pos[c.ID] = i
	}
	out := make([][]Criterion, len(gates))
	for gi, g := range gates {
		var in []Criterion
		for _, c := range e.Criteria {
			if GateOf(e, c) == g.ID {
				in = append(in, c)
			}
		}
		inGate := map[string]bool{}
		for _, c := range in {
			inGate[c.ID] = true
		}
		done := map[string]bool{}
		var order []Criterion
		for len(order) < len(in) {
			var ready []Criterion
			for _, c := range in {
				if done[c.ID] {
					continue
				}
				ok := true
				for _, d := range c.DependsOn {
					if inGate[d] && !done[d] {
						ok = false
						break
					}
				}
				if ok {
					ready = append(ready, c)
				}
			}
			if len(ready) == 0 {
				break // a cycle: Validate refuses these
			}
			sort.SliceStable(ready, func(i, j int) bool { return pos[ready[i].ID] < pos[ready[j].ID] })
			for _, c := range ready {
				done[c.ID] = true
			}
			order = append(order, ready...)
		}
		out[gi] = order
	}
	return out
}
