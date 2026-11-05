package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/flows"
)

type flowCtxKey struct{}

// A flow's model call hands its own context (with the engine wait hook
// the runner put on it) to the gateway as the request context, and a
// gateway error comes back as a *flows.ModelError the engine's transient
// rule can read.
func TestFlowModelsContextAndModelError(t *testing.T) {
	var sawCtx bool
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCtx = r.Context().Value(flowCtxKey{}) == "flow"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"terminated","type":"server_error"}}`))
	})
	m := NewFlowHost(FlowHostDeps{Gateway: gw}).Options(t.TempDir()).Models
	ctx := engine.WithWaitHook(context.Background(), func(string) {})
	ctx = context.WithValue(ctx, flowCtxKey{}, "flow")
	_, err := m.Complete(ctx, flows.ModelRequest{Model: "qwen", Run: "fr-1", Messages: []flows.Message{{Role: "user", Content: "hi"}}})
	if !sawCtx {
		t.Fatal("the gateway did not get the flow's context")
	}
	var me *flows.ModelError
	if !errors.As(err, &me) || me.Status != http.StatusBadRequest || me.Message != "terminated" {
		t.Fatalf("err %#v", err)
	}
	if err.Error() != "model call failed (400): terminated" {
		t.Fatalf("text %q", err.Error())
	}
	if !engine.IsTransientText(me.Status, me.Message) {
		t.Fatal("an engine crash is not transient")
	}
}
