//go:build linux

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// A llama-server killed by SIGTERM from outside (as railguard does) is
// noticed at once: the model is not loaded, its limit is unchanged, one
// notification goes out, a late crash report of that load changes nothing,
// and the reload keeps the dead process' log as .log.1. Cerberus' own
// Unload notifies nothing. The child gets oom_score_adj 500.
func TestLlamaKilledFromOutside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.gguf")
	writeFile(t, path, 1024)
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.ModelDirs = nil
	cfg.Engines.LoadTimeoutSec = 20
	cfg.Models = []config.ModelConfig{{ID: "m", Path: path, Parallel: 4}}
	b, logs := fakeLlama(t, cfg, "ok")
	m, _ := testManager(cfg, -1, b)
	var n notes
	m.notify = n.send
	ctx := context.Background()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m.Shutdown(sctx)
	})

	ep, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	limit := modelInfo(m, "m").MaxParallel
	b.mu.Lock()
	p := b.procs["m"]
	b.mu.Unlock()
	pid := p.cmd.Process.Pid
	if ep.PID != pid {
		t.Errorf("Endpoint.PID = %d, want the llama-server's %d", ep.PID, pid)
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/oom_score_adj")
	if err != nil || strings.TrimSpace(string(raw)) != "500" {
		t.Errorf("oom_score_adj = %q %v, want 500", raw, err)
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the model to count as unloaded", func() bool { return !modelInfo(m, "m").Loaded })
	info := modelInfo(m, "m")
	if info.MaxParallel != limit || info.LastFailure == nil || info.LastFailure.Cause != "signal" ||
		!strings.Contains(info.LastFailure.Text, "SIGTERM") {
		t.Fatalf("limit %d (want %d) failure %+v", info.MaxParallel, limit, info.LastFailure)
	}
	m.MarkCrashedGen("m", ep.Gen) // the request that was in flight
	if got := modelInfo(m, "m").MaxParallel; got != limit {
		t.Fatalf("limit %d after the late crash report, want %d", got, limit)
	}
	if msgs := n.get(); len(msgs) != 1 || msgs[0].Kind != "engine" || !strings.Contains(msgs[0].Body, "SIGTERM") {
		t.Fatalf("notifications = %+v", msgs)
	}
	if !strings.Contains(m.FailureText("m"), "SIGTERM") {
		t.Fatalf("FailureText = %q", m.FailureText("m"))
	}

	// Reload, then Cerberus' own unload: no new notification.
	if _, rel, err = m.Acquire(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	rel()
	if _, err := os.Stat(filepath.Join(logs, "m.log.1")); err != nil {
		t.Fatalf("the killed process' log was not kept: %v", err)
	}
	if err := m.Unload(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(n.get()); got != 1 {
		t.Fatalf("%d notifications after Unload, want still 1", got)
	}
	b.mu.Lock()
	e := b.exits["m"]
	b.mu.Unlock()
	if !e.ByUs {
		t.Fatalf("Unload's exit = %+v, want ByUs", e)
	}
}

// A crash report that arrives before the exit is reaped still sees the
// exit: a killed engine is not a crash.
func TestLlamaCrashReportWaitsForExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.gguf")
	writeFile(t, path, 1024)
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.ModelDirs = nil
	cfg.Engines.LoadTimeoutSec = 20
	cfg.Models = []config.ModelConfig{{ID: "m", Path: path, Parallel: 4}}
	b, _ := fakeLlama(t, cfg, "ok")
	m, _ := testManager(cfg, -1, b)
	b.setOnExit(nil) // only the crash report sees it
	ctx := context.Background()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m.Shutdown(sctx)
	})
	ep, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	limit := modelInfo(m, "m").MaxParallel
	b.mu.Lock()
	p := b.procs["m"]
	b.mu.Unlock()
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	m.MarkCrashedGen("m", ep.Gen)
	rel()
	info := modelInfo(m, "m")
	if info.Loaded || info.MaxParallel != limit || info.LastFailure == nil || !strings.Contains(info.LastFailure.Text, "SIGKILL") {
		t.Fatalf("loaded %v limit %d (want %d) failure %+v", info.Loaded, info.MaxParallel, limit, info.LastFailure)
	}
}
