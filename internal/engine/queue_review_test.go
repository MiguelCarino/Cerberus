package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// A request that failed from a crash but reports it only after the model
// was reloaded (LM Studio's reload kills the leftovers of the old engine)
// must not mark the fresh engine crashed or cut the limit a second time.
func TestMarkCrashedIgnoresStaleGeneration(t *testing.T) {
	f := newFake("lmstudio", true, "m")
	m, _ := testManager(nil, -1, slotFake{f, 4})
	ctx := context.Background()
	ep1, rel1, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	ep2, rel2, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	m.MarkCrashedGen("m", ep1.Gen)
	rel1()
	_, rel3, err := m.Acquire(ctx, "m") // reloads after the crash
	if err != nil {
		t.Fatal(err)
	}
	m.MarkCrashedGen("m", ep2.Gen) // the same crash, reported late
	rel2()
	got := modelInfo(m, "m")
	if !got.Loaded || got.MaxParallel != 3 {
		t.Fatalf("after a stale crash report: loaded %v limit %d, want true 3", got.Loaded, got.MaxParallel)
	}
	rel3()
	if _, rel, err := m.Acquire(ctx, "m"); err != nil {
		t.Fatal(err)
	} else {
		rel()
	}
	if n := f.loadCount("m"); n != 2 {
		t.Fatalf("loads %d, want 2 (one reload for one crash)", n)
	}
	// A crash of the current engine still counts.
	ep, rel, _ := m.Acquire(ctx, "m")
	m.MarkCrashedGen("m", ep.Gen)
	rel()
	if got := modelInfo(m, "m"); got.Loaded || got.MaxParallel != 2 {
		t.Fatalf("current crash: loaded %v limit %d, want false 2", got.Loaded, got.MaxParallel)
	}
}

// A load queued behind another load that is waiting for room must give up
// when its caller does (not sit out wait_for_room_sec), and its wait hook
// must say it is waiting rather than leave it looking live.
func TestRoomLockWaitIsCancellableAndReported(t *testing.T) {
	m, _, _ := roomSetup(t, time.Minute)
	waiting := make(chan error, 1)
	go func() { _, _, err := m.Acquire(context.Background(), "new"); waiting <- err }()
	waitFor(t, "new waiting for room", func() bool { return modelInfo(m, "new").InFlight == 1 })
	time.Sleep(20 * time.Millisecond) // let it reach the room wait (holds the room lock)

	h := &hookLog{}
	ctx, cancel := context.WithTimeout(WithWaitHook(context.Background(), h.hook), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := m.Acquire(ctx, "z"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v, want the caller's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled load stayed blocked on the room lock")
	}
	got := h.get()
	if len(got) < 2 || got[0] == "" || got[len(got)-1] != "" {
		t.Fatalf("hook calls %q, want a reason then \"\"", got)
	}
	if info := modelInfo(m, "z"); info.InFlight != 0 {
		t.Fatalf("slot leaked: %+v", info)
	}
}

// A second request for a model that another request is loading waits on
// the load; when that load is itself waiting for room, the second must be
// cancellable and reported as waiting too.
func TestLoadLockWaitIsCancellableAndReported(t *testing.T) {
	m, _, _ := roomSetup(t, time.Minute)
	go func() { _, _, _ = m.Acquire(context.Background(), "new") }()
	waitFor(t, "new waiting for room", func() bool { return modelInfo(m, "new").InFlight == 1 })
	time.Sleep(20 * time.Millisecond)

	h := &hookLog{}
	ctx, cancel := context.WithTimeout(WithWaitHook(context.Background(), h.hook), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := m.Acquire(ctx, "new"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled request stayed blocked on the model's load lock")
	}
	if got := h.get(); len(got) < 2 || got[0] == "" || got[len(got)-1] != "" {
		t.Fatalf("hook calls %q", got)
	}
}

func TestIsTransientCallerContext(t *testing.T) {
	for _, msg := range []string{"context canceled", "context deadline exceeded"} {
		if IsTransientText(503, msg) {
			t.Errorf("IsTransientText(503, %q) = true, want false (the caller's own context)", msg)
		}
		if IsTransient(fmt.Errorf("model call failed (503): %s", msg)) {
			t.Errorf("IsTransient(503 %q) = true", msg)
		}
	}
	if !IsTransientText(503, "local engine failed: upstream: connection refused (it will be reloaded)") {
		t.Error("crash 503 no longer transient")
	}
}

// unloadHook is a fake backend that runs onUnload during each unload.
type unloadHook struct {
	*fakeBackend
	onUnload func(id string)
}

func (u unloadHook) Unload(ctx context.Context, m ModelInfo) error {
	if u.onUnload != nil {
		u.onUnload(m.ID)
	}
	return u.fakeBackend.Unload(ctx, m)
}

// A busy model that goes idle while makeRoom is evicting others must be
// evicted too, not missed: missing it either waits on a release that has
// already happened or (with nothing else busy) loads the target spilled
// into RAM next to a model that could have made room.
func TestMakeRoomSeesReleaseDuringEviction(t *testing.T) {
	cfg := config.Defaults()
	cfg.Models = []config.ModelConfig{{ID: "new", SizeMiB: 5000}}
	f := newFake("llamacpp", true, "c", "y", "new")
	var relY func()
	hb := unloadHook{fakeBackend: f}
	hb.onUnload = func(id string) {
		if id == "c" && relY != nil {
			relY() // y finishes while c is being unloaded
		}
	}
	m, v := testManager(cfg, 100000, hb)
	m.roomWaitOverride = time.Minute
	m.memAvail = func() int { return 64000 } // spilling would be allowed
	ctx := context.Background()
	if err := m.Load(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	var err error
	if _, relY, err = m.Acquire(ctx, "y"); err != nil {
		t.Fatal(err)
	}
	v.onRefresh = func() { v.free.Store(int64(4300 + 1000*len(f.unloaded()))) }
	done := make(chan error, 1)
	go func() {
		_, rel, err := m.Acquire(ctx, "new")
		if err == nil {
			rel()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("load never proceeded after the busy model went idle")
	}
	if got := f.unloaded(); !slices.Contains(got, "y") {
		t.Fatalf("unloaded %v: y went idle during eviction but was not evicted", got)
	}
}
