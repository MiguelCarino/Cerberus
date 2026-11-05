//go:build linux

package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// readPid waits for the agent to write the pid of a child it started.
func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child pid never written")
	return 0
}

// waitGone fails unless pid exits within d.
func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil || strings.Contains(string(raw), ") Z ") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("process %d outlived its session", pid)
}

// TestStopKillsChildInOwnGroup: like run_command, the agent starts a child in
// its own process group that ignores SIGHUP and SIGTERM. Stop must not leave
// it running.
func TestStopKillsChildInOwnGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("spawner.sh", "set -m\n(trap '' HUP TERM; exec sleep 300) &\necho $! > "+pidFile+"\nexec sleep 300\n")
		e.profiles(config.AgentProfile{Name: "spawner", Command: []string{"bash", agent}})
	})
	id := e.createOne("spawn")
	e.waitState(id, StateRunning)
	child := readPid(t, pidFile)
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StateStopped)
	waitGone(t, child, 5*time.Second)
}

// TestExitKillsBackgroundJobs: an agent that leaves a nohup'd server behind
// and exits must not leak it.
func TestExitKillsBackgroundJobs(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("leaver.sh", "nohup sleep 300 >/dev/null 2>&1 &\necho $! > "+pidFile+"\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "leaver", Command: []string{"bash", agent}})
	})
	id := e.createOne("leave")
	e.waitState(id, StatePassed)
	waitGone(t, readPid(t, pidFile), 5*time.Second)
}
