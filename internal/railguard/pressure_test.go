package railguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadPressure(t *testing.T) {
	now := time.Unix(1794745087, 0)
	ok := `{"v":1,"time":1794745080.5,"resource":"vram","used_mib":23900,"total_mib":24564,"target_mib":22000,"act_at":1794745100,"managed_units":["cerberus.service","lmstudio.service"]}`
	p, good := parsePressure([]byte(ok), now)
	if !good || p.TargetMiB != 22000 || p.UsedMiB != 23900 || len(p.ManagedUnits) != 2 || p.At().Unix() != 1794745080 {
		t.Fatalf("parse = %+v %v", p, good)
	}
	for name, raw := range map[string]string{
		"stale":     `{"v":1,"time":1794745020,"resource":"vram","target_mib":22000}`,
		"future":    `{"v":1,"time":1794745200,"resource":"vram","target_mib":22000}`,
		"version":   `{"v":2,"time":1794745080,"resource":"vram","target_mib":22000}`,
		"resource":  `{"v":1,"time":1794745080,"resource":"ram","target_mib":22000}`,
		"no target": `{"v":1,"time":1794745080,"resource":"vram"}`,
		"malformed": `{"v":1,"time":`,
	} {
		if _, good := parsePressure([]byte(raw), now); good {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	if _, good := ReadPressure(dir, now); good {
		t.Fatal("no file accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, PressureFile), []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, good := ReadPressure(dir, now); !good {
		t.Fatal("file not read")
	}
	if _, good := ReadPressure("", now); good {
		t.Fatal("empty dir read")
	}
}

func TestHistory(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"time": 1793670797, "kind": "vram", "why": "VRAM 98% full", "name": "llama-server", "pid": 1946621, "unit": "run-p109029-i105992.scope", "gb": 2.3, "vram_gb": 18.8}`,
		`not json`,
		`{"time": 1793686032, "kind": "vram", "why": "VRAM 98% full", "name": "llama-server", "pid": 2813161, "unit": "run-p109029-i105992.scope", "gb": 2.4, "vram_gb": 20.5}`,
		`{"time": 1794745087, "kind": "vram", "why": "VRAM 97% full", "name": "llama-server", "pid": 141931, "unit": "cerberus.service", "gb": 3.5, "vram_gb": 18.7}`,
	}
	if err := os.WriteFile(filepath.Join(dir, HistoryFile), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1793686032, 0)
	got := History(dir, at.Add(-15*time.Second), at.Add(5*time.Second))
	if len(got) != 1 || got[0].PID != 2813161 || got[0].Unit != "run-p109029-i105992.scope" || got[0].VRAMGB != 20.5 ||
		got[0].Why != "VRAM 98% full" || !got[0].Time.Equal(at) {
		t.Fatalf("window = %+v", got)
	}
	if all := History(dir, time.Unix(0, 0), time.Unix(1<<40, 0)); len(all) != 3 {
		t.Fatalf("all = %d entries, want 3 (the bad line skipped)", len(all))
	}
	if none := History(t.TempDir(), time.Unix(0, 0), time.Unix(1<<40, 0)); none != nil {
		t.Fatalf("no file: %+v", none)
	}

	// A file longer than the tail: only the end is read, and the cut first
	// line is skipped.
	big := strings.Repeat(lines[0]+"\n", (historyTail/len(lines[0]))+10) + lines[3] + "\n"
	if err := os.WriteFile(filepath.Join(dir, HistoryFile), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	last := History(dir, time.Unix(1794745000, 0), time.Unix(1794745100, 0))
	if len(last) != 1 || last[0].Unit != "cerberus.service" {
		t.Fatalf("tail = %+v", last)
	}
}
