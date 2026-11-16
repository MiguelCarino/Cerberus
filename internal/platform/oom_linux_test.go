//go:build linux

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestCgroupUnit(t *testing.T) {
	root := t.TempDir()
	write := func(who, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, who), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, who, "cgroup"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("self", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/cerberus.service\n")
	write("42", "0::/user.slice/user-1000.slice/user@1000.service/agents.slice/run-p109029-i105992.scope\n")
	write("7", "12:memory:/user.slice\n") // cgroup v1
	write("8", "0::/\n")
	for pid, want := range map[int]string{0: "cerberus.service", 42: "run-p109029-i105992.scope", 7: "", 8: "", 99: "", -1: ""} {
		if got := cgroupUnit(root, pid); got != want {
			t.Errorf("pid %d: %q, want %q", pid, got, want)
		}
	}
}

func TestOOMScoreAdjAndExitSignal(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	pid := cmd.Process.Pid
	if !ProcessAlive(pid) {
		t.Fatal("not alive")
	}
	if err := SetOOMScoreAdj(pid, 500); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/oom_score_adj")
	if strings.TrimSpace(string(raw)) != "500" {
		t.Fatalf("oom_score_adj = %q", raw)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if name, n := ExitSignal(cmd.ProcessState); name != "SIGTERM" || !OutsideStopSignal(n) {
		t.Fatalf("ExitSignal = %q %d", name, n)
	}
	if OutsideStopSignal(int(syscall.SIGSEGV)) || OutsideStopSignal(int(syscall.SIGABRT)) {
		t.Fatal("a crash signal counted as a stop from outside")
	}
	if name, _ := ExitSignal(nil); name != "" {
		t.Fatal("nil state named a signal")
	}
}
