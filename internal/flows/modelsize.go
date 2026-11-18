package flows

// The small-model hint (SCOPE "Flow quality (2026-11-16)"): plan, judge
// and agent session steps on a very small local model get a warning (never
// an error) that names a larger installed model.

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ModelHints is optional on a Catalog: what the small-model warning needs
// beyond the model id.
type ModelHints interface {
	// ModelSizeMiB is an installed model's file size in MiB (0 = unknown).
	ModelSizeMiB(id string) int
	// DefaultModel is the model "default" stands for ("" = not known).
	DefaultModel() string
	// AgentModel is the model a session of the agent profile runs on
	// when the step names none: the default model for a profile that
	// takes a model, "" for one that picks its own.
	AgentModel(agent string) string
	// LocalModels lists the installed local model ids.
	LocalModels() []string
}

// Size bounds of the hint.
const (
	smallParamsB  = 5.0  // below this many billion parameters a model is small
	smallSizeMiB  = 3500 // without a size in the name: a file below this is small
	suggestParamB = 7.0  // a model suggested instead has at least this many
)

// paramsRe finds a parameter count in a model id: "1.7b", "27b",
// "e4b" (an effective size), "0.6b", "8x7b" (the expert count is left
// out), each between non-alphanumerics.
var paramsRe = regexp.MustCompile(`(?:^|[^a-z0-9.])(?:\d+x)?e?(\d+(?:\.\d+)?)([bm])(?:$|[^a-z0-9])`)

// ParamsB is the parameter count in billions written in a model id or
// name ("lfm3-1.7b" → 1.7, "qwen/qwen3.8-27b" → 27, "gemma-4-E4B" → 4,
// "smollm-360m" → 0.36), or 0 when the name says none. A mixture of
// experts ("30b-a3b") counts its total, the first size named.
func ParamsB(id string) float64 {
	m := paramsRe.FindStringSubmatch(strings.ToLower(id))
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 {
		return 0
	}
	if m[2] == "m" {
		n /= 1000
	}
	return n
}

// smallModel says whether a model is small for planning, judging and
// agent work, and how its size is described ("about 1.7B parameters",
// "a 1.2 GiB file").
func smallModel(id string, sizeMiB int) (bool, string) {
	if p := ParamsB(id); p > 0 {
		return p < smallParamsB, fmt.Sprintf("about %sB parameters", strconv.FormatFloat(p, 'f', -1, 64))
	}
	if sizeMiB > 0 && sizeMiB < smallSizeMiB {
		return true, fmt.Sprintf("a %.1f GiB file", float64(sizeMiB)/1024)
	}
	return false, ""
}

// largerModels are up to two installed models to suggest instead: the
// largest by the size in their name (at least suggestParamB), then by
// file size when no name says one.
func largerModels(h ModelHints) []string {
	type cand struct {
		id     string
		params float64
		mib    int
	}
	var cs []cand
	for _, id := range h.LocalModels() {
		p, mib := ParamsB(id), h.ModelSizeMiB(id)
		switch {
		case p >= suggestParamB:
		case p == 0 && mib >= 3*smallSizeMiB:
		default:
			continue
		}
		cs = append(cs, cand{id, p, mib})
	}
	slices.SortStableFunc(cs, func(a, b cand) int {
		switch {
		case (a.params > 0) != (b.params > 0):
			if a.params > 0 {
				return -1
			}
			return 1
		case a.params != b.params:
			if a.params > b.params {
				return -1
			}
			return 1
		case a.mib != b.mib:
			return b.mib - a.mib
		}
		return strings.Compare(a.id, b.id)
	})
	var out []string
	for _, c := range cs[:min(2, len(cs))] {
		out = append(out, c.id)
	}
	return out
}

// sizeHint warns when the model of a plan, judge or session step is
// small. model is the step's model ("default" is resolved); role names
// the work ("planning", "judging", "agent work"); viaDefault says the
// model is the default one because the step names none.
func (v *validator) sizeHint(step, field, model, role string, viaDefault bool) {
	h, _ := v.cat.(ModelHints)
	if model == "default" {
		if h == nil {
			return
		}
		model, viaDefault = h.DefaultModel(), true
	}
	if strings.TrimSpace(model) == "" {
		return
	}
	mib := 0
	if h != nil {
		mib = h.ModelSizeMiB(model)
	}
	small, size := smallModel(model, mib)
	if !small {
		return
	}
	what := fmt.Sprintf("%s (%s) is small for %s", model, size, role)
	if viaDefault {
		what = fmt.Sprintf("this runs on the default model %s (%s), which is small for %s", model, size, role)
	}
	advice := "pick a larger model"
	if viaDefault {
		advice = "pick a larger default model in Settings, or a larger model here"
		if field == "agent" {
			advice = "set a larger default model in Settings (engines.default_model)"
		}
	}
	if h != nil {
		if big := largerModels(h); len(big) > 0 {
			advice += " such as " + strings.Join(big, " or ")
		}
	}
	v.add(step, field, SevWarning, "%s: %s. A small model gives thin plans and can loop in agent work.", what, advice)
}

// sessionSizeHint warns when an agent session step (or a fan-out run as
// sessions) runs on a small model: its own (a fan-out's default model and
// routes) or, when it names none, the one its agent profile takes.
func (v *validator) sessionSizeHint(s Step) {
	named := false
	if s.Kind == KindSession && s.Model != "" {
		named = true
		v.sizeHint(s.ID, "model", s.Model, "agent work", false)
	}
	if s.Kind == KindFanOut {
		for n, r := range s.Routes {
			if r.Model != "" {
				v.sizeHint(s.ID, fmt.Sprintf("routes.%d.model", n), r.Model, "agent work", false)
			}
		}
		if s.DefaultModel != "" {
			named = true
			v.sizeHint(s.ID, "default_model", s.DefaultModel, "agent work", false)
		}
	}
	if named {
		return
	}
	if h, ok := v.cat.(ModelHints); ok && s.Agent != "" {
		if m := h.AgentModel(s.Agent); m != "" {
			v.sizeHint(s.ID, "agent", m, "agent work", true)
		}
	}
}
