package api

import (
	"context"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/flows"
)

func TestFlowCatalogModelHints(t *testing.T) {
	cfg := config.Defaults()
	cfg.Engines.DefaultModel = "lfm3-1.7b"
	list := []engine.ModelInfo{
		{ID: "lfm3-1.7b", Local: true, SizeMiB: 1700},
		{ID: "qwen/qwen3.8-27b", Local: true, SizeMiB: 17000},
		{ID: "cloud/big", Local: false},
	}
	host := NewFlowHost(FlowHostDeps{Config: func() *config.Config { return cfg },
		Models: func(context.Context) []engine.ModelInfo { return list }})
	var cat flows.Catalog = flowCatalog{host}
	h, ok := cat.(flows.ModelHints)
	if !ok {
		t.Fatal("flowCatalog is not a flows.ModelHints")
	}
	if h.ModelSizeMiB("qwen/qwen3.8-27b") != 17000 || h.ModelSizeMiB("nope") != 0 {
		t.Error("sizes")
	}
	if h.DefaultModel() != "lfm3-1.7b" || h.AgentModel("cerberus") != "lfm3-1.7b" || h.AgentModel("claude") != "" || h.AgentModel("nope") != "" {
		t.Errorf("default %q, cerberus %q, claude %q", h.DefaultModel(), h.AgentModel("cerberus"), h.AgentModel("claude"))
	}
	if got := h.LocalModels(); len(got) != 2 || got[0] != "lfm3-1.7b" || got[1] != "qwen/qwen3.8-27b" {
		t.Errorf("local %v", got)
	}
	// The owner's Website flow: the session on the default model warns.
	f := flows.Flow{Name: "W", Steps: []flows.Step{
		{ID: "plan", Kind: flows.KindPlan, Model: "lfm3-1.7b", Template: "plan {input}"},
		{ID: "build", Kind: flows.KindSession, Agent: "cerberus", Task: "build {steps.plan.output}"},
	}}
	ps := flows.Validate(f, cat)
	if flows.HasErrors(ps) {
		t.Fatalf("errors %+v", ps)
	}
	n := 0
	for _, p := range ps {
		if p.Severity == flows.SevWarning && (p.Step == "plan" && p.Field == "model" || p.Step == "build" && p.Field == "agent") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("warnings %+v", ps)
	}
}
