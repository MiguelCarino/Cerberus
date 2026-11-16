package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSys records systemctl / systemd-run calls. unit is the LoadState of
// lmstudio.service; up is set when a call starts the daemon.
type fakeSys struct {
	unit    string
	startOK bool // systemctl start / systemd-run bring the daemon up
	up      *atomic.Bool

	mu    sync.Mutex
	calls []string
}

func (f *fakeSys) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(line, "systemctl --user show"):
		return []byte(f.unit + "\n"), nil
	case strings.HasPrefix(line, "systemctl --user start"), strings.HasPrefix(line, "systemd-run"):
		if !f.startOK {
			return nil, errors.New("failed")
		}
		f.up.Store(true)
		return nil, nil
	}
	return nil, errors.New("unexpected " + line)
}

func (f *fakeSys) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// daemonLMS is testLMStudio with a fake daemon that is down.
func daemonLMS(t *testing.T, unit string, scope bool) (*lmStudio, *fakeLMS, *fakeSys) {
	t.Helper()
	b, f := testLMStudio(t, "http://127.0.0.1:1/v1")
	var up atomic.Bool
	sys := &fakeSys{unit: unit, startOK: true, up: &up}
	b.daemon.alive = up.Load
	b.daemon.sys = sys.run
	b.daemon.canScope = func() bool { return scope }
	return b, f, sys
}

func TestLMSDaemonPrefersUnit(t *testing.T) {
	b, f, sys := daemonLMS(t, "loaded", true)
	if _, err := b.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl --user show -p LoadState --value lmstudio.service", "systemctl --user start lmstudio.service"}
	if got := sys.got(); !slices.Equal(got, want) {
		t.Fatalf("sys calls %v, want %v", got, want)
	}
	if f.called("ls") == nil || !b.daemon.failedAt.IsZero() {
		t.Fatalf("lms calls %v, failed at %v", f.calls, b.daemon.failedAt)
	}
	// Up now: no more systemd calls.
	b.List(context.Background())
	if got := sys.got(); len(got) != 2 {
		t.Fatalf("sys calls %v after the daemon is up", got)
	}
}

func TestLMSDaemonFallsBackToScope(t *testing.T) {
	b, _, sys := daemonLMS(t, "not-found", true)
	if _, err := b.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := sys.got()
	want := "systemd-run --user --scope --collect --quiet --unit=cerberus-lmstudio --slice=app.slice /fake/lms daemon up"
	if len(got) != 2 || got[1] != want || !b.daemon.failedAt.IsZero() {
		t.Fatalf("sys calls %v (want %q last), failed at %v", got, want, b.daemon.failedAt)
	}
}

func TestLMSDaemonOldWayWithoutSystemd(t *testing.T) {
	b, f, sys := daemonLMS(t, "not-found", false)
	now := time.Unix(1_800_000_000, 0)
	b.daemon.now = func() time.Time { return now }
	if _, err := b.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sys.got(); len(got) != 1 || b.daemon.failedAt.IsZero() || f.called("ls") == nil {
		t.Fatalf("sys calls %v, failed at %v, lms %v", got, b.daemon.failedAt, f.calls)
	}
	// Within lmsPlaceRetry the next call goes the old way without trying.
	b.List(context.Background())
	if got := sys.got(); len(got) != 1 {
		t.Fatalf("sys calls %v: placement retried too soon", got)
	}
	// Not latched for the run: after lmsPlaceRetry it tries again, and a
	// unit that exists now is used (contract-engine-2).
	now = now.Add(lmsPlaceRetry)
	sys.unit = "loaded"
	b.List(context.Background())
	if got := sys.got(); len(got) != 3 || got[2] != "systemctl --user start lmstudio.service" || !b.daemon.failedAt.IsZero() {
		t.Fatalf("sys calls %v, failed at %v: placement not retried", got, b.daemon.failedAt)
	}

	b2, _, sys2 := daemonLMS(t, "loaded", true)
	b2.lc.Unit = "none"
	b2.List(context.Background())
	if got := sys2.got(); len(got) != 0 {
		t.Fatalf("unit none: sys calls %v", got)
	}

	b3, _, sys3 := daemonLMS(t, "loaded", false)
	b3.lc.Unit = "my-llm.service"
	b3.List(context.Background())
	if got := sys3.got(); len(got) != 2 || got[1] != "systemctl --user start my-llm.service" {
		t.Fatalf("named unit: sys calls %v", got)
	}
}

// A slow unit start (lmstudio.service's ExecStartPost waits up to a minute)
// is not cut short by the lms call's own timeout: placement has its own
// deadline, and the caller going away does not stop it (contract-engine-2).
func TestLMSDaemonPlacementOwnDeadline(t *testing.T) {
	b, f, sys := daemonLMS(t, "loaded", false)
	var startErr error
	inner := sys.run
	b.daemon.sys = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && args[1] == "start" {
			// The call's own timeout must not be on this context.
			if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < lmsCmdTimeout+time.Minute {
				startErr = errors.New("placement ran under the lms call's deadline")
			}
			if ctx.Err() != nil {
				startErr = ctx.Err()
			}
		}
		return inner(ctx, name, args...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller already went away
	b.ensureDaemon(ctx, "/fake/lms")
	if startErr != nil {
		t.Fatal(startErr)
	}
	if !b.daemonUp() || !b.daemon.failedAt.IsZero() {
		t.Fatalf("daemon up %v, failed at %v, sys %v, lms %v", b.daemonUp(), b.daemon.failedAt, sys.got(), f.calls)
	}
}

// Shutdown unloads only what Cerberus loaded, and stops the daemon only
// when it runs in Cerberus' own cgroup. With the daemon down it
// calls nothing (an lms call would start it).
func TestLMSShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	b, f, _ := daemonLMS(t, "not-found", false) // the old way
	b.daemon.alive = func() bool { return true }
	b.daemon.inOwnCgroup = func() bool { return true }
	b.noteLoaded("mine")
	b.Shutdown(ctx)
	var lines []string
	for _, c := range f.calls {
		lines = append(lines, strings.Join(c, " "))
	}
	if !slices.Equal(lines, []string{"unload mine", "daemon down"}) {
		t.Fatalf("calls %v, want [unload mine, daemon down]", lines)
	}

	b2, f2, _ := daemonLMS(t, "loaded", true) // its own unit
	b2.daemon.alive = func() bool { return true }
	b2.daemon.inOwnCgroup = func() bool { return false }
	b2.noteLoaded("a")
	b2.noteLoaded("b")
	b2.noteUnloaded("b")
	b2.Shutdown(ctx)
	lines = nil
	for _, c := range f2.calls {
		lines = append(lines, strings.Join(c, " "))
	}
	if !slices.Equal(lines, []string{"unload a"}) {
		t.Fatalf("calls %v, want [unload a] and no daemon down", lines)
	}

	b3, f3, sys3 := daemonLMS(t, "loaded", true) // daemon down
	b3.daemon.inOwnCgroup = func() bool { return true }
	b3.noteLoaded("a")
	b3.Shutdown(ctx)
	if len(f3.calls) != 0 || len(sys3.got()) != 0 {
		t.Fatalf("calls %v / %v with the daemon down", f3.calls, sys3.got())
	}
}

// The manager's Shutdown reaches LM Studio within its grace.
func TestManagerShutdownReachesLMStudio(t *testing.T) {
	b, f, _ := daemonLMS(t, "loaded", true)
	b.daemon.alive = func() bool { return true }
	b.noteLoaded("x")
	m, _ := testManager(nil, -1, b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Shutdown(ctx)
	if f.called("unload x") == nil {
		t.Fatalf("calls %v", f.calls)
	}
}
