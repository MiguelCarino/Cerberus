//go:build linux

package platform

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestKillSessionReachesOtherGroups(t *testing.T) {
	// The leader starts a child in its own process group (as run_command
	// does) that ignores SIGHUP and SIGTERM, then the leader exits.
	pidFile := t.TempDir() + "/pid"
	cmd := exec.Command("bash", "-c", `set -m; (trap '' HUP TERM; exec sleep 300) & echo $! > `+pidFile+`; sleep 300`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	deadline := time.Now().Add(5 * time.Second)
	for child == 0 && time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(raw)) != "" {
			child = atoi(strings.TrimSpace(string(raw)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("child pid not written")
	}
	if pg, _ := syscall.Getpgid(child); pg == cmd.Process.Pid {
		t.Fatal("test setup: child should be in its own group")
	}
	leader := cmd.Process.Pid
	_ = syscall.Kill(leader, syscall.SIGKILL)
	_ = cmd.Wait()
	KillSession(leader)
	for time.Now().Before(deadline) {
		if s, ok := procSession(child); !ok || s != leader {
			return // gone (or reaped by init and its pid reused)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	t.Fatal("child in another process group survived KillSession")
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
