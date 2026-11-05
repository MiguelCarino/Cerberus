package sandbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestProbeCachesOnlyDefiniteResults covers a timed-out probe (not cached,
// retried) and a cancelled caller (probe keeps running detached).
func TestProbeCachesOnlyDefiniteResults(t *testing.T) {
	oldFn, oldTO := probeFn, probeTimeout
	t.Cleanup(func() {
		probeFn, probeTimeout = oldFn, oldTO
		probeMu.Lock()
		delete(probeCache, "/fake/bwrap")
		probeMu.Unlock()
	})
	var calls atomic.Int32
	slow := true
	probeTimeout = 50 * time.Millisecond
	probeFn = func(ctx context.Context, bin string) error {
		calls.Add(1)
		if slow {
			<-ctx.Done()
			return errors.New("bwrap cannot start a sandbox: signal: killed")
		}
		return nil
	}
	if err := probeOnce(context.Background(), "/fake/bwrap"); err == nil {
		t.Fatal("timed-out probe should report an error")
	}
	probeMu.Lock()
	_, cached := probeCache["/fake/bwrap"]
	probeMu.Unlock()
	if cached {
		t.Fatal("timed-out probe must not be cached")
	}

	// A caller whose context is already cancelled must not poison the
	// cache: the probe runs detached and its definite result sticks.
	slow = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeOnce(ctx, "/fake/bwrap"); err != nil {
		t.Fatalf("detached probe: %v", err)
	}
	if err := probeOnce(context.Background(), "/fake/bwrap"); err != nil {
		t.Fatalf("cached probe: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("probe ran %d times, want 2", n)
	}
}

// TestProbeSharedRun checks concurrent callers share one probe and a waiter
// can leave early without holding a lock.
func TestProbeSharedRun(t *testing.T) {
	oldFn := probeFn
	t.Cleanup(func() {
		probeFn = oldFn
		probeMu.Lock()
		delete(probeCache, "/fake/bwrap2")
		probeMu.Unlock()
	})
	release := make(chan struct{})
	var calls atomic.Int32
	probeFn = func(ctx context.Context, bin string) error {
		calls.Add(1)
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- probeOnce(context.Background(), "/fake/bwrap2") }()
	for {
		probeMu.Lock()
		_, running := probeRunning["/fake/bwrap2"]
		probeMu.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := probeOnce(ctx, "/fake/bwrap2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times, want 1", n)
	}
}
