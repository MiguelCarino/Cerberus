package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestReloadIdleTTL proves a live TTL change by behaviour: a model kept
// forever (TTL 0) is reaped once Reload sets a short TTL, without a restart.
func TestReloadIdleTTL(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.IdleTTLSec = 0 // keep loaded
	f := newFake("lmstudio", true, "m1")
	m, _ := testManager(cfg, -1, f)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	_, rel, err := m.Acquire(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	now = now.Add(time.Hour)
	m.reap(ctx)
	if len(f.unloaded()) != 0 {
		t.Fatalf("TTL 0 unloaded %v", f.unloaded())
	}

	next := cfg.Clone()
	next.Engines.IdleTTLSec = 60
	next.Engines.DefaultModel = "m1"
	m.Reload(next)
	m.reap(ctx)
	if got := f.unloaded(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("after reload to 60s unloaded %v, want [m1]", got)
	}
	if st := m.match("default"); st == nil || st.info.ID != "m1" {
		t.Fatalf("default model not applied live: %v", st)
	}
	m.Reload(nil) // ignored
}

// TestImplicitDefault: with engines.default_model unset, "default" is the
// only loaded model, else the only model; with several it stays unknown.
func TestImplicitDefault(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.DefaultModel = ""
	m, _ := testManager(cfg, -1, newFake("lmstudio", true, "solo"))
	if st, err := m.resolve(ctx, "default"); err != nil || st.info.ID != "solo" {
		t.Fatalf("single model: %v %v", st, err)
	}

	m, _ = testManager(cfg, -1, newFake("lmstudio", true, "a", "b"))
	if _, err := m.resolve(ctx, "default"); err == nil {
		t.Fatal("two models, nothing loaded: default should be unknown")
	}
	_, rel, err := m.Acquire(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if st, err := m.resolve(ctx, "default"); err != nil || st.info.ID != "b" {
		t.Fatalf("one loaded of two: %v %v", st, err)
	}
}

// TestForgetReloads: an engine that dropped a model behind Cerberus' back
// gets it loaded again on the next Acquire after Forget.
func TestForgetReloads(t *testing.T) {
	ctx := context.Background()
	f := newFake("lmstudio", true, "a")
	m, _ := testManager(config.Defaults(), -1, f)
	_, rel, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if n := f.loadCount("a"); n != 1 {
		t.Fatalf("loads %d, want 1", n)
	}
	_, rel, _ = m.Acquire(ctx, "a")
	rel()
	if n := f.loadCount("a"); n != 1 {
		t.Fatalf("a loaded model was loaded again: %d", n)
	}
	if !m.Forget("a") {
		t.Fatal("Forget(a) = false for a local model")
	}
	_, rel, err = m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if n := f.loadCount("a"); n != 2 {
		t.Errorf("after Forget the model was not loaded again: %d loads", n)
	}
	if m.Forget("nope") {
		t.Error("Forget of an unknown model reported true")
	}
}
