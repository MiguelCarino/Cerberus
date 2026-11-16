package flows

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// Plan steps send an array schema with the item cap, JSON prompt steps an
// object-or-array schema, plain prompts and fan-out items none; the step's
// temperature, max_tokens and thinking reach every call of the step.
func TestStepCallsCarrySchemaAndSettings(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var mu sync.Mutex
	got := map[string][]ModelRequest{}
	h.models.fn = func(_ context.Context, req ModelRequest) (ModelReply, error) {
		mu.Lock()
		got[req.Step] = append(got[req.Step], req)
		mu.Unlock()
		switch req.Step {
		case "plan":
			// A schema-honouring engine: the plan parses on the first call.
			return ModelReply{Text: `[{"title":"A","kind":"x","task":"do a"},{"title":"B","kind":"x","task":"do b"}]`}, nil
		case "facts":
			return ModelReply{Text: `{"n": 2}`}, nil
		}
		return ModelReply{Text: "ok"}, nil
	}
	f := h.create(Flow{Name: "Shape", Steps: []Step{
		{ID: "plan", Kind: KindPlan, Model: "big", Template: "x", MaxItems: 3, Temperature: ptr(0.0), Thinking: ThinkingOn},
		{ID: "fan", Kind: KindFanOut, From: "plan", DefaultModel: "small", Template: "{item.task}",
			Temperature: ptr(0.7), MaxTokens: 500, Thinking: ThinkingOff},
		{ID: "facts", Kind: KindPrompt, Model: "big", JSON: true, Template: "y", MaxTokens: 64},
		{ID: "say", Kind: KindPrompt, Model: "big", Template: "z"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	if len(got["plan"]) != 1 {
		t.Fatalf("plan calls %d, want 1", len(got["plan"]))
	}
	p := got["plan"][0]
	var ps struct {
		Type     string `json:"type"`
		MaxItems int    `json:"maxItems"`
		Items    struct {
			Required []string `json:"required"`
		} `json:"items"`
	}
	if err := json.Unmarshal(p.Schema, &ps); err != nil || ps.Type != "array" || ps.MaxItems != 3 || len(ps.Items.Required) != 3 {
		t.Fatalf("plan schema %s: %v", p.Schema, err)
	}
	if !p.JSON || p.Temperature == nil || *p.Temperature != 0 || p.Thinking != ThinkingOn || p.MaxTokens != 0 {
		t.Fatalf("plan request %+v", p)
	}
	if len(got["fan"]) != 2 {
		t.Fatalf("fan calls %d", len(got["fan"]))
	}
	for _, it := range got["fan"] {
		if it.Schema != nil || it.Temperature == nil || *it.Temperature != 0.7 || it.MaxTokens != 500 || it.Thinking != ThinkingOff {
			t.Fatalf("fan item request %+v", it)
		}
	}
	fj := got["facts"][0]
	if string(fj.Schema) != jsonAnswerSchema || fj.MaxTokens != 64 || fj.Temperature != nil || fj.Thinking != "" {
		t.Fatalf("json prompt request %+v", fj)
	}
	if s := got["say"][0]; s.Schema != nil || s.JSON || s.Temperature != nil || s.MaxTokens != 0 {
		t.Fatalf("plain prompt request %+v", s)
	}
	// The run's own copy of the flow does not share the temperature.
	cl := f.clone()
	*cl.Steps[0].Temperature = 1
	if *f.Steps[0].Temperature != 0 {
		t.Fatal("clone shares the temperature")
	}
}

// A plan the engine did not shape still goes through extractJSON and the
// one parse retry, with the schema sent both times.
func TestPlanSchemaKeepsTheParseFallback(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var mu sync.Mutex
	var calls []ModelRequest
	h.models.fn = func(_ context.Context, req ModelRequest) (ModelReply, error) {
		mu.Lock()
		calls = append(calls, req)
		n := len(calls)
		mu.Unlock()
		if n == 1 {
			return ModelReply{Text: "Sure! Here is the plan."}, nil
		}
		return ModelReply{Text: "```json\n[{\"title\":\"A\",\"task\":\"a\"}]\n```"}, nil
	}
	f := h.create(Flow{Name: "Plan", Steps: []Step{{ID: "plan", Kind: KindPlan, Model: "big", Template: "x"}}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed || len(calls) != 2 {
		t.Fatalf("%s %q calls %d", r.State, r.Reason, len(calls))
	}
	for _, c := range calls {
		if len(c.Schema) == 0 {
			t.Fatal("a plan call without the schema")
		}
	}
}

func TestValidateStepModelSettings(t *testing.T) {
	f := validFlow()
	f.Steps[0].Temperature = ptr(2.5)
	f.Steps[0].MaxTokens = -1
	f.Steps[0].Thinking = "maybe"
	f.Steps[1].Temperature = ptr(0.3)
	f.Steps[1].MaxTokens = 1000
	f.Steps[1].Thinking = ThinkingOff
	ps := Validate(f, fakeCatalog{})
	for _, c := range [][3]string{
		{"plan", "temperature", "0 to 2"},
		{"plan", "max_tokens", "max tokens is 1 to"},
		{"plan", "thinking", "on, off or empty"},
	} {
		if !hasProblem(ps, c[0], c[1], SevError, c[2]) {
			t.Errorf("missing %v in %+v", c, ps)
		}
	}
	for _, p := range ps {
		if p.Step == "fan" {
			t.Errorf("fan-out settings refused: %+v", p)
		}
	}
	// A fan-out run as sessions: the settings do nothing, so a warning.
	f = validFlow()
	f.Steps[1].As, f.Steps[1].Agent, f.Steps[1].Temperature = KindSession, "cerberus", ptr(0.5)
	if ps = Validate(f, fakeCatalog{}); HasErrors(ps) || !hasProblem(ps, "fan", "temperature", SevWarning, "prompt items only") {
		t.Errorf("session fan-out: %+v", ps)
	}
	// Unset is valid; the JSON round trip leaves the fields out.
	raw, _ := json.Marshal(validFlow().Steps[0])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"temperature", "max_tokens", "thinking"} {
		if _, ok := m[k]; ok {
			t.Errorf("unset %s written", k)
		}
	}
}
