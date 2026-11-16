package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

// atomicClock is a fake clock that background listings may read safely.
func atomicClock(m *Manager) func(d time.Duration) {
	var ns atomic.Int64
	ns.Store(time.Unix(1000, 0).UnixNano())
	m.now = func() time.Time { return time.Unix(0, ns.Load()) }
	return func(d time.Duration) { ns.Add(int64(d)) }
}

// An unknown id (a client's claude-* or gpt-* name) costs one forced
// listing per cache period, not one per request.
func TestUnknownIDListsOncePerTTL(t *testing.T) {
	f := newFake("llamacpp", true, "m1")
	m, _ := testManager(nil, -1, f)
	advance := atomicClock(m)
	ctx := context.Background()
	for range 10 {
		if _, err := m.Resolve(ctx, "claude-x"); !errors.Is(err, ErrUnknownModel) {
			t.Fatalf("Resolve(claude-x) = %v", err)
		}
	}
	if n := f.listCount(); n != 2 { // the first listing, then one forced
		t.Fatalf("%d listings for 10 misses, want 2", n)
	}
	// A model that shows up is still found through the cache first.
	if _, err := m.Resolve(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	// After the cache period the miss may list once more.
	advance(6 * time.Second)
	for range 5 {
		_, _ = m.Resolve(ctx, "claude-x")
	}
	waitRelist(t, m)
	if n := f.listCount(); n < 3 || n > 4 {
		t.Fatalf("%d listings after the period, want 3 or 4", n)
	}
}

// The negative cache is bounded.
func TestMissCacheBounded(t *testing.T) {
	f := newFake("llamacpp", true, "m1")
	m, _ := testManager(nil, -1, f)
	atomicClock(m)
	for i := range maxMisses + 50 {
		m.noteMiss(fmt.Sprintf("unknown-%d", i))
	}
	m.mu.Lock()
	n := len(m.missAt)
	m.mu.Unlock()
	if n > maxMisses {
		t.Fatalf("%d entries, bound %d", n, maxMisses)
	}
}

// ResolveCached matches against the listing at hand and never forces one.
func TestResolveCachedNeverForces(t *testing.T) {
	f := newFake("llamacpp", true, "qwen/qwen3-8b")
	m, _ := testManager(nil, -1, f)
	atomicClock(m)
	ctx := context.Background()
	for range 10 {
		if _, ok := m.ResolveCached(ctx, "claude-sonnet-4-5"); ok {
			t.Fatal("an unknown id resolved")
		}
	}
	if n := f.listCount(); n != 1 {
		t.Fatalf("%d listings, want only the first", n)
	}
	if info, ok := m.ResolveCached(ctx, "qwen3-8b"); !ok || info.ID != "qwen/qwen3-8b" {
		t.Fatalf("suffix match: %+v %v", info, ok)
	}
}

// A stale cache is served at once while one listing renews it behind.
func TestStaleCacheServedWhileRelisting(t *testing.T) {
	f := newFake("llamacpp", true, "m1")
	f.listDelay = 300 * time.Millisecond
	m, _ := testManager(nil, -1, f)
	advance := atomicClock(m)
	ctx := context.Background()
	m.Models(ctx) // the first listing blocks: there is nothing to serve
	f.mu.Lock()
	f.models = append(f.models, ModelInfo{ID: "m2", Backend: "llamacpp", Name: "m2", Local: true, SizeMiB: 1000})
	f.mu.Unlock()
	advance(6 * time.Second)
	start := time.Now()
	for range 5 {
		if _, err := m.Resolve(ctx, "m1"); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("Resolve on a stale cache took %v: it waited for the listing", d)
	}
	waitRelist(t, m)
	if n := f.listCount(); n != 2 {
		t.Errorf("%d listings, want 2 (one background renewal for five calls)", n)
	}
	ids := []string{}
	for _, mi := range m.Models(ctx) {
		ids = append(ids, mi.ID)
	}
	if !slices.Contains(ids, "m2") {
		t.Errorf("the renewed listing is not served: %v", ids)
	}
}

// A forced refresh while a background one runs still lists in turn.
func TestForcedRefreshWaitsForBackground(t *testing.T) {
	f := newFake("llamacpp", true, "m1")
	f.listDelay = 100 * time.Millisecond
	m, _ := testManager(nil, -1, f)
	advance := atomicClock(m)
	ctx := context.Background()
	m.Models(ctx)
	advance(6 * time.Second)
	m.Models(ctx) // starts the background listing
	m.ensureFresh(ctx, true)
	waitRelist(t, m)
	if n := f.listCount(); n < 2 || n > 3 {
		t.Errorf("%d listings", n)
	}
}

// LoadedEndpoint takes no slot and counts no request: a token count does
// not queue behind a running generation.
func TestLoadedEndpointTakesNoSlot(t *testing.T) {
	cfg := config.Defaults()
	cfg.Models = []config.ModelConfig{{ID: "m", MaxParallel: 1}}
	f := newFake("llamacpp", true, "m", "cold")
	cloud := newFake("openai", false, "c")
	m, _ := testManager(cfg, -1, slotFake{f, 1}, cloud)
	ctx := context.Background()
	ep, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	before := modelInfo(m, "m")
	done := make(chan struct{})
	var got Endpoint
	var ok bool
	go func() { got, ok = m.LoadedEndpoint(ctx, "m"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("LoadedEndpoint waited for the busy slot")
	}
	if !ok || got.BaseURL != ep.BaseURL || got.Gen != ep.Gen {
		t.Fatalf("LoadedEndpoint = %+v %v, want %+v", got, ok, ep)
	}
	after := modelInfo(m, "m")
	if after.InFlight != before.InFlight || !after.LastUsed.Equal(before.LastUsed) || after.Waiting != 0 {
		t.Errorf("counted as a request: before %+v after %+v", before, after)
	}
	if _, ok := m.LoadedEndpoint(ctx, "cold"); ok {
		t.Error("a model that is not loaded has an endpoint")
	}
	if _, ok := m.LoadedEndpoint(ctx, "c"); ok {
		t.Error("a cloud model has a local endpoint")
	}
	if _, ok := m.LoadedEndpoint(ctx, "nope"); ok {
		t.Error("an unknown model has an endpoint")
	}
	if f.loadCount("cold") != 0 {
		t.Error("LoadedEndpoint loaded a model")
	}
}

// A stall reloads the model but keeps its slots, and the engine sends no
// notification of its own (the gateway does).
func TestMarkStalledKeepsSlots(t *testing.T) {
	f := newFake("lmstudio", true, "m")
	m, _ := testManager(nil, -1, slotFake{f, 4})
	var sent atomic.Int32
	m.notify = func(notify.Message) { sent.Add(1) }
	ctx := context.Background()
	ep, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !m.MarkStalledGen("m", ep.Gen) || !m.MarkStalledGen("m", ep.Gen) {
		t.Fatal("MarkStalledGen(local) = false")
	}
	rel()
	got := modelInfo(m, "m")
	if got.Loaded || got.MaxParallel != 4 {
		t.Fatalf("after a stall: loaded %v limit %d, want false 4", got.Loaded, got.MaxParallel)
	}
	if got.LastFailure == nil || got.LastFailure.Cause != "stalled" {
		t.Errorf("last failure %+v", got.LastFailure)
	}
	ep2, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if f.loadCount("m") != 2 || !slices.Equal(f.unloaded(), []string{"m"}) {
		t.Fatalf("loads %d unloads %v: want a clean reload", f.loadCount("m"), f.unloaded())
	}
	// A report against the hung load, after the reload, changes nothing.
	if !m.MarkStalledGen("m", ep.Gen) || !modelInfo(m, "m").Loaded {
		t.Error("a stall report of an earlier load unloaded the fresh one")
	}
	if ep2.Gen == ep.Gen {
		t.Error("the reload kept the generation")
	}
	if sent.Load() != 0 {
		t.Errorf("%d engine notifications for a stall, want 0", sent.Load())
	}
	if m.MarkStalledGen("nope", 1) {
		t.Error("an unknown model was marked")
	}
}
