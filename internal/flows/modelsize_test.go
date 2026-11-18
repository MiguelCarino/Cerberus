package flows

import (
	"strings"
	"testing"

	"carino.systems/cerberus/internal/evals"
)

func TestParamsB(t *testing.T) {
	for id, want := range map[string]float64{
		"lfm3-1.7b":                       1.7,
		"qwen/qwen3.8-27b":                27,
		"gemma-4-E4B":                     4,
		"google/gemma-4-e4b-it":           4,
		"qwen3-0.6b":                      0.6,
		"llama-3.1-8b-instruct":           8,
		"qwen3-30b-a3b":                   30,
		"mixtral-8x7b":                    7,
		"smollm-360m":                     0.36,
		"lfm3-1.7b-q8_0.gguf":             1.7,
		"MiniMax-M2.7":                    0,
		"GLM-4.7-Flash":                   0,
		"qwen3.8":                         0,
		"default":                         0,
		"openrouter/anthropic/model-name": 0,
	} {
		if got := ParamsB(id); got != want {
			t.Errorf("ParamsB(%q) = %v, want %v", id, got, want)
		}
	}
}

// hintCatalog is the owner's machine: a 1.7B default model.
type hintCatalog struct {
	fakeCatalog
	def string
}

func (c hintCatalog) Model(id string) (bool, bool) {
	switch id {
	case "lfm3-1.7b", "qwen/qwen3.8-27b", "gemma-4-e4b", "GLM-4.7-Flash", "MiniMax-M2.7":
		return true, true
	}
	return c.fakeCatalog.Model(id)
}

func (c hintCatalog) ModelSizeMiB(id string) int {
	return map[string]int{"lfm3-1.7b": 1700, "qwen/qwen3.8-27b": 17000, "gemma-4-e4b": 5000,
		"GLM-4.7-Flash": 18000, "MiniMax-M2.7": 120000, "tiny-gguf": 900}[id]
}
func (c hintCatalog) DefaultModel() string { return c.def }
func (c hintCatalog) AgentModel(agent string) string {
	if agent == "cerberus" {
		return c.def
	}
	return ""
}
func (c hintCatalog) LocalModels() []string {
	return []string{"lfm3-1.7b", "gemma-4-e4b", "MiniMax-M2.7", "qwen/qwen3.8-27b", "GLM-4.7-Flash"}
}

func TestSmallModelWarnings(t *testing.T) {
	cat := hintCatalog{def: "lfm3-1.7b"}
	f := Flow{Name: "W", Steps: []Step{
		{ID: "plan", Kind: KindPlan, Model: "lfm3-1.7b", Template: "plan {input}"},
		{ID: "plan2", Kind: KindPlan, Model: "default", Template: "plan {input}"},
		{ID: "plan3", Kind: KindPlan, Model: "qwen/qwen3.8-27b", Template: "plan {input}"},
		{ID: "parts", Kind: KindFanOut, From: "plan", DefaultModel: "lfm3-1.7b", Template: "{item.task}"},
		{ID: "build", Kind: KindSession, Agent: "cerberus", Task: "build {steps.parts.output}"},
		{ID: "review", Kind: KindJudge, On: "build", Model: "gemma-4-e4b",
			Criteria: []evals.Criterion{{ID: "ok", Prompt: "It works"}}},
		{ID: "tell", Kind: KindOutput, To: ToInbox, Template: "{steps.build.output}"},
	}}
	ps := Validate(f, cat)
	if HasErrors(ps) {
		t.Fatalf("a small model is never an error: %+v", ps)
	}
	for _, want := range []struct{ step, field, msg string }{
		{"plan", "model", "lfm3-1.7b (about 1.7B parameters) is small for planning: pick a larger model such as qwen/qwen3.8-27b or MiniMax-M2.7"},
		{"plan2", "model", "this runs on the default model lfm3-1.7b"},
		{"build", "agent", "set a larger default model in Settings (engines.default_model) such as qwen/qwen3.8-27b"},
		{"review", "model", "gemma-4-e4b (about 4B parameters) is small for judging"},
	} {
		if !hasProblem(ps, want.step, want.field, SevWarning, want.msg) {
			t.Errorf("no warning %s.%s %q in %+v", want.step, want.field, want.msg, ps)
		}
	}
	for _, p := range ps {
		// Big models, and prompt parts on a small model, are fine.
		if p.Step == "plan3" || p.Step == "parts" {
			t.Errorf("unexpected %+v", p)
		}
	}
	// A big default model: no warning on the session or on "default".
	if ps := Validate(f, hintCatalog{def: "qwen/qwen3.8-27b"}); hasProblem(ps, "build", "agent", SevWarning, "small") ||
		hasProblem(ps, "plan2", "model", SevWarning, "small") {
		t.Errorf("warned on a big default: %+v", ps)
	}
	// Without hints the name alone still warns; "default" cannot be told.
	ps = Validate(f, nil)
	if !hasProblem(ps, "plan", "model", SevWarning, "small for planning: pick a larger model.") ||
		hasProblem(ps, "plan2", "model", SevWarning, "small") {
		t.Errorf("without hints: %+v", ps)
	}
	// A session fan-out on a small model is agent work too.
	sf := Flow{Name: "S", Steps: []Step{
		{ID: "plan", Kind: KindPlan, Model: "qwen/qwen3.8-27b", Template: "plan {input}"},
		{ID: "parts", Kind: KindFanOut, From: "plan", As: KindSession, Agent: "cerberus", Template: "{item.task}",
			Routes: []Route{{Kind: "code", Model: "lfm3-1.7b"}}},
	}}
	ps = Validate(sf, cat)
	if !hasProblem(ps, "parts", "routes.0.model", SevWarning, "small for agent work") ||
		!hasProblem(ps, "parts", "agent", SevWarning, "default model lfm3-1.7b") {
		t.Errorf("session fan-out: %+v", ps)
	}
}

func TestSmallModelBySize(t *testing.T) {
	if small, why := smallModel("tiny-gguf", 900); !small || !strings.Contains(why, "0.9 GiB") {
		t.Errorf("%v %q", small, why)
	}
	if small, _ := smallModel("MiniMax-M2.7", 120000); small {
		t.Error("a big file is not small")
	}
	if small, _ := smallModel("mystery", 0); small {
		t.Error("an unknown size is not small")
	}
	// The size in the name wins over the file size.
	if small, _ := smallModel("qwen/qwen3.8-27b", 1000); small {
		t.Error("27B is not small")
	}
}
