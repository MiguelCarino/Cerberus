package sysmon

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
)

// Every kill and renice attempt on a host process is audited with the
// actor, the pid, the process name and the outcome, refused ones included.
func TestSignalAndReniceAreAudited(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	al, err := audit.Open(t.TempDir(), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	m := New(config.Defaults(), nil, nil, WithAudit(al))
	if err := m.SignalAs("owner:phone", 1, "kill"); !errors.Is(err, ErrProtected) {
		t.Fatalf("pid 1: %v", err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid := cmd.Process.Pid
	if err := m.ReniceAs("cli", pid, 7); err != nil {
		t.Fatalf("renice: %v", err)
	}
	if err := m.Signal(pid, "term"); err != nil {
		t.Fatalf("term: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child did not exit")
	}
	recs, err := al.Query(time.Time{}, "", 0)
	if err != nil || len(recs) != 3 {
		t.Fatalf("records %+v %v", recs, err)
	}
	// newest first
	want := []struct{ actor, route, detail string }{
		{audit.ActorSystem, "process.signal", "term: ok"},
		{"cli", "process.renice", "nice 7: ok"},
		{"owner:phone", "process.signal", "kill: " + ErrProtected.Error()},
	}
	for i, w := range want {
		r := recs[i]
		if r.Actor != w.actor || r.Route != w.route || r.Detail != w.detail {
			t.Errorf("record %d = %+v, want %+v", i, r, w)
		}
	}
	if recs[0].Targets[0] != strconv.Itoa(pid) || len(recs[0].Targets) != 2 || recs[0].Targets[1] != "sleep" {
		t.Fatalf("targets %v", recs[0].Targets)
	}
}
