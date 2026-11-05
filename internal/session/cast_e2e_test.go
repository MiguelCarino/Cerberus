//go:build !windows

package session

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestRecordingMaskedNoInput: a run is recorded to runs/1.cast with the
// vault value masked, what the user typed absent, and it is listed and
// opened through Runs/OpenRun.
func TestRecordingMaskedNoInput(t *testing.T) {
	const secret = "sk-rec-0123456789abcdef"
	const typed = "hunter2-typed-input"
	e := newTestEnv(t, nil, func(e *testEnv) {
		withFileVault(e, map[string]string{"api-token": secret})
		agent := e.script("rec.sh", `printf 'token=%s\n' "$MY_TOKEN"
printf 'password: '
read -rs pw
[ "$pw" = "`+typed+`" ] && printf '\ngot the input\n'
exit 0
`)
		e.profiles(config.AgentProfile{Name: "rec", Command: []string{"bash", agent},
			SecretEnv: map[string]string{"MY_TOKEN": "api-token"}})
	})
	id := e.scratch("rec", nil)
	backlog, ch, detach, err := e.m.Attach(id)
	if err != nil {
		t.Fatal(err)
	}
	live := string(backlog)
	deadline := time.After(waitLong)
	for !strings.Contains(live, "password:") {
		select {
		case b, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed; got %q", live)
			}
			live += string(b)
		case <-deadline:
			t.Fatalf("no prompt; got %q", live)
		}
	}
	detach()
	if err := e.m.Input(id, []byte(typed+"\r")); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StatePassed)

	runs, err := e.m.Runs(id)
	if err != nil || len(runs) != 1 || runs[0].Attempt != 1 || runs[0].Size == 0 || runs[0].Recording {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	f, err := e.m.OpenRun(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(f)
	f.Close()
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), typed) {
		t.Fatalf("cast leaks: %s", raw)
	}
	hdr, evs := readCast(t, filepath.Join(e.m.sessDir, id, "runs", "1.cast"))
	if hdr.Version != 2 || hdr.Width < 20 || hdr.Height < 5 {
		t.Fatalf("header %+v", hdr)
	}
	var out string
	for _, ev := range evs {
		if ev[1] != "o" {
			t.Fatalf("non-output event %v", ev)
		}
		out += ev[2].(string)
	}
	if !strings.Contains(out, "token="+redactMask) || !strings.Contains(out, "got the input") {
		t.Fatalf("recorded output %q", out)
	}
	if _, err := e.m.OpenRun(id, 2); !os.IsNotExist(err) {
		t.Fatalf("missing attempt: %v", err)
	}
	if _, err := e.m.Runs("nope"); err == nil {
		t.Fatal("unknown session listed")
	}
}

// TestRecordingOff: RecordMaxMiB 0 writes no runs folder.
func TestRecordingOff(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.cfg.Sessions.RecordMaxMiB = 0
		agent := e.script("quiet.sh", "echo hi\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "quiet", Command: []string{"bash", agent}})
	})
	id := e.scratch("quiet", nil)
	e.waitState(id, StatePassed)
	if _, err := os.Stat(filepath.Join(e.m.sessDir, id, "runs")); !os.IsNotExist(err) {
		t.Fatalf("runs folder with recording off: %v", err)
	}
	if runs, err := e.m.Runs(id); err != nil || len(runs) != 0 {
		t.Fatalf("runs = %v, %v", runs, err)
	}
}
