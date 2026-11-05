//go:build linux

package session

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// waitPreviewState polls the managed server of id until it is in state.
func waitPreviewState(t *testing.T, m *Manager, id, state string) ManagedPreview {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		mp, err := m.PreviewManaged(id)
		if err != nil {
			t.Fatal(err)
		}
		if mp.State == state {
			return mp
		}
		if time.Now().After(deadline) {
			t.Fatalf("preview server: want %s, have %q (%s)\n%s", state, mp.State, mp.Error, mp.LogTail)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func procAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestManagedPreviewLifecycle: Run preview starts the configured command
// with {port} in the worktree; it becomes running once it listens, its
// output is kept, its port is a managed live server of the session; it
// stops when idle, on Stop and on Discard.
func TestManagedPreviewLifecycle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.cfg.Repos[0].Preview = &config.PreviewConfig{Command: "echo serving on {port} PORT=$PORT; exec python3 -I -m http.server {port} --bind 127.0.0.1"}
		agent := e.script("quick.sh", "echo done\n")
		e.profiles(config.AgentProfile{Name: "quick", Command: []string{"bash", agent}})
	})
	id := e.createOne("make a site")
	e.waitState(id, StatePassed)

	mp, err := e.m.PreviewManaged(id)
	if err != nil || mp.State != "" || !strings.Contains(mp.Command, "{port}") || mp.Source != "config" {
		t.Fatalf("before start: %+v %v", mp, err)
	}
	mp, err = e.m.StartPreview(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	port := mp.Port
	mp = waitPreviewState(t, e.m, id, PreviewRunning)
	if !strings.Contains(mp.LogTail, "serving on "+strconv.Itoa(port)+" PORT="+strconv.Itoa(port)) {
		t.Fatalf("log tail: %q", mp.LogTail)
	}
	if l, ok := e.m.OwnsPort(id, port); !ok || l.Source != "managed" {
		t.Fatalf("managed port %d not owned: %+v", port, e.m.PreviewLive(id))
	}
	info, _ := e.m.Get(id)
	if info.Preview == nil || info.Preview.Managed != PreviewRunning {
		t.Fatalf("Info.Preview: %+v", info.Preview)
	}
	// A second start is a no-op.
	if again, err := e.m.StartPreview(context.Background(), id); err != nil || again.Port != port {
		t.Fatalf("second start: %+v %v", again, err)
	}

	// Idle: nobody looked for longer than preview_idle_min.
	e.m.mu.Lock()
	s := e.m.sessions[id]
	s.pvUsed = time.Now().Add(-time.Hour)
	pid := s.pv.cmd.Process.Pid
	e.m.mu.Unlock()
	e.m.previewTick()
	mp = waitPreviewState(t, e.m, id, PreviewStopped)
	if !strings.Contains(mp.LogTail, "without visits") || procAlive(pid) {
		t.Fatalf("idle stop: alive=%v log=%q", procAlive(pid), mp.LogTail)
	}
	if err := e.m.StopPreview(id); err == nil {
		t.Fatal("stopping a stopped preview server must fail")
	}

	// Start again, then Discard ends it before the worktree goes.
	if _, err := e.m.StartPreview(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitPreviewState(t, e.m, id, PreviewRunning)
	e.m.mu.Lock()
	pid = e.m.sessions[id].pv.cmd.Process.Pid
	e.m.mu.Unlock()
	if err := e.m.Discard(id); err != nil {
		t.Fatal(err)
	}
	waitPreviewState(t, e.m, id, PreviewStopped)
	if procAlive(pid) {
		t.Fatal("preview server outlived the discard")
	}
	if _, err := e.m.StartPreview(context.Background(), id); err == nil {
		t.Fatal("a discarded session must not start a preview server")
	}
}

// TestPreviewStopAndNoCommand: Stop ends a running server; without any
// command Run preview says so.
func TestPreviewStopAndNoCommand(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("quick.sh", "echo done\n")
		e.profiles(config.AgentProfile{Name: "quick", Command: []string{"bash", agent}})
	})
	id := e.createOne("nothing to serve")
	e.waitState(id, StatePassed)
	if _, err := e.m.StartPreview(context.Background(), id); !errors.Is(err, ErrNoPreviewCommand) {
		t.Fatalf("no command: %v", err)
	}
	next := e.cfg.Clone()
	next.Repos[0].Preview = &config.PreviewConfig{Command: "exec python3 -I -m http.server {port} --bind 127.0.0.1"}
	e.m.Reload(next)
	if _, err := e.m.StartPreview(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitPreviewState(t, e.m, id, PreviewRunning)
	if err := e.m.StopPreview(id); err != nil {
		t.Fatal(err)
	}
	waitPreviewState(t, e.m, id, PreviewStopped)
}

// TestLiveOwnership: a server the agent starts is a detected live server
// of its session (also after it leaves the agent's process tree, through
// the preview tag); a listener of another process (this test) never is.
func TestLiveOwnership(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	stranger, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	strangerPort := stranger.Addr().(*net.TCPAddr).Port

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agentPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	orphanPort := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()

	e := newTestEnv(t, nil, func(e *testEnv) {
		// One server stays a child; the other is double-forked into its own
		// session (setsid), out of the agent's tree and session.
		agent := e.script("serve.sh", "python3 -I -m http.server "+strconv.Itoa(agentPort)+" --bind 127.0.0.1 >/dev/null 2>&1 &\n"+
			"setsid bash -c 'python3 -I -m http.server "+strconv.Itoa(orphanPort)+" --bind 127.0.0.1 >/dev/null 2>&1 &' &\n"+
			"exec sleep 60\n")
		e.profiles(config.AgentProfile{Name: "serve", Command: []string{"bash", agent}})
	})
	id := e.createOne("serve")
	e.waitState(id, StateRunning)
	deadline := time.Now().Add(waitLong)
	for {
		_, a := e.m.OwnsPort(id, agentPort)
		_, o := e.m.OwnsPort(id, orphanPort)
		if a && o {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live servers not found: %+v (want %d and %d)", e.m.PreviewLive(id), agentPort, orphanPort)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, ok := e.m.OwnsPort(id, strangerPort); ok {
		t.Fatalf("port %d of another process counted as the session's", strangerPort)
	}
	for _, l := range e.m.PreviewLive(id) {
		if l.Source != "detected" {
			t.Fatalf("source: %+v", l)
		}
	}
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StateStopped)
	// The orphan escaped the session kill: end it here.
	for _, p := range newProcIndex(false).tags[e.m.previewTag(id)] {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
}
