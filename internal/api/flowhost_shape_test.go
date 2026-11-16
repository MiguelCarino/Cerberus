package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/evals"
	"carino.systems/cerberus/internal/flows"
)

// bodyGateway records each request body and answers a fixed completion,
// or status with text when status is set.
type bodyGateway struct {
	mu     sync.Mutex
	bodies []map[string]any
	status int
	text   string
}

func (g *bodyGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	g.mu.Lock()
	g.bodies = append(g.bodies, b)
	status, text := g.status, g.text
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": text}})
		return
	}
	_, _ = w.Write([]byte(`{"model":"x","choices":[{"message":{"content":"[]"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
}

func (g *bodyGateway) last() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bodies[len(g.bodies)-1]
}

func localModels(context.Context) []engine.ModelInfo {
	return []engine.ModelInfo{{ID: "qwen", Local: true}, {ID: "openrouter/big"}}
}

// A flow call sends temperature and max_tokens to every model, and the
// answer's schema (as json_schema, never json_object) and the thinking
// switch to local models only; "default" resolves to engines.default_model.
func TestFlowModelsBodyPerModel(t *testing.T) {
	gw := &bodyGateway{}
	cfg := config.Defaults()
	cfg.Engines.DefaultModel = "qwen"
	m := NewFlowHost(FlowHostDeps{Gateway: gw, Models: localModels, Config: func() *config.Config { return cfg }}).
		Options(t.TempDir()).Models
	temp := 0.2
	schema := json.RawMessage(`{"type":"array","maxItems":3}`)
	req := flows.ModelRequest{Model: "qwen", Run: "fr-1", Messages: []flows.Message{{Role: "user", Content: "plan"}},
		JSON: true, Schema: schema, Temperature: &temp, MaxTokens: 900, Thinking: flows.ThinkingOff}
	if _, err := m.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b := gw.last()
	rf, _ := b["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["schema"].(map[string]any)["maxItems"] != 3.0 {
		t.Fatalf("response_format %v", b["response_format"])
	}
	if b["temperature"] != 0.2 || b["max_tokens"] != 900.0 || b["reasoning_effort"] != "none" {
		t.Fatalf("body %v", b)
	}
	if kw, _ := b["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
		t.Fatalf("kwargs %v", b["chat_template_kwargs"])
	}

	req.Model, req.Thinking = "default", flows.ThinkingOn
	if _, err := m.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b = gw.last()
	if kw, _ := b["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != true || b["response_format"] == nil || b["reasoning_effort"] != nil {
		t.Fatalf("default model body %v", b)
	}

	req.Model = "openrouter/big"
	if _, err := m.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b = gw.last()
	if b["response_format"] != nil || b["chat_template_kwargs"] != nil || b["temperature"] != 0.2 || b["max_tokens"] != 900.0 {
		t.Fatalf("cloud body %v", b)
	}

	// Nothing set: the body is as before.
	if _, err := m.Complete(context.Background(), flows.ModelRequest{Model: "qwen", Messages: req.Messages}); err != nil {
		t.Fatal(err)
	}
	b = gw.last()
	for _, k := range []string{"response_format", "temperature", "max_tokens", "chat_template_kwargs", "reasoning_effort"} {
		if _, ok := b[k]; ok {
			t.Errorf("plain call sent %s", k)
		}
	}
}

// The judge sends its verdict schema to a local model only, and a gateway
// error reaches the judge typed with its status.
func TestEvalModelsSchemaAndTypedError(t *testing.T) {
	gw := &bodyGateway{}
	h := NewEvalHost(EvalHostDeps{Gateway: gw, Models: localModels})
	m := h.Options(t.TempDir()).Models
	req := evals.ModelRequest{Model: "qwen", Run: "er-1", Schema: evals.VerdictSchema,
		Messages: []evals.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}}
	if _, err := m.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rf, _ := gw.last()["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["name"] != "verdict" {
		t.Fatalf("response_format %v", rf)
	}
	req.Model = "openrouter/big"
	if _, err := m.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if gw.last()["response_format"] != nil {
		t.Fatal("a cloud judge got a response_format")
	}
	gw.status, gw.text = http.StatusServiceUnavailable, "the local engine failed and is restarting"
	_, err := m.Complete(context.Background(), req)
	var me *evals.ModelError
	if !errors.As(err, &me) || me.Status != http.StatusServiceUnavailable {
		t.Fatalf("err %#v", err)
	}
}
