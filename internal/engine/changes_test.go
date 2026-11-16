package engine

import (
	"context"
	"testing"
	"time"
)

func gotHint(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func noHint(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	case <-time.After(30 * time.Millisecond):
		return true
	}
}

// TestChangeHints: loads, unloads, requests and an idle cached read
// (web-api-ops-6). An idle Models view costs no listing.
func TestChangeHints(t *testing.T) {
	b := newFake("llamacpp", true, "m1")
	m, _ := testManager(nil, 50000, b)
	ctx := context.Background()
	ch, cancel := m.SubscribeChanges()
	defer cancel()

	// The first cached read lists once (nothing is known yet).
	if got := m.ModelsCached(ctx); len(got) != 1 {
		t.Fatalf("models %+v", got)
	}
	drain := func() {
		time.Sleep(20 * time.Millisecond) // background work settles
		for len(ch) > 0 {
			<-ch
		}
	}
	drain()

	if err := m.Load(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if !gotHint(ch) {
		t.Fatal("no hint on load")
	}
	drain()
	ep, release, err := m.Acquire(ctx, "m1")
	if err != nil || ep.BaseURL == "" {
		t.Fatal(err)
	}
	if !gotHint(ch) {
		t.Fatal("no hint when a request started")
	}
	if got := m.ModelsCached(ctx); got[0].InFlight != 1 || !got[0].Loaded {
		t.Fatalf("cached view while busy: %+v", got[0])
	}
	drain()
	release()
	if !gotHint(ch) {
		t.Fatal("no hint when a request ended")
	}
	drain()
	if err := m.Unload(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if !gotHint(ch) {
		t.Fatal("no hint on unload")
	}

	// Idle: cached reads never list, and nothing is hinted.
	drain()
	b.mu.Lock()
	lists := b.lists
	b.mu.Unlock()
	m.mu.Lock()
	m.listed = m.listed.Add(-time.Hour) // stale: Models would re-list
	m.mu.Unlock()
	for range 5 {
		m.ModelsCached(ctx)
	}
	b.mu.Lock()
	after := b.lists
	b.mu.Unlock()
	if after != lists {
		t.Fatalf("ModelsCached listed %d times", after-lists)
	}
	if !noHint(ch) {
		t.Fatal("a hint while idle")
	}

	// A cancelled subscription gets nothing.
	cancel()
	m.changed()
	if !noHint(ch) {
		t.Fatal("a hint after cancel")
	}
}
