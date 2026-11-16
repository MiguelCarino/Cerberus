package railguard

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRead(t *testing.T) {
	write := func(t *testing.T, name, body string) string {
		t.Helper()
		dir := t.TempDir()
		if name != "" {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	def := Defaults()
	cases := []struct {
		name, file, body string
		present          bool
		vram             bool
		pct              float64
	}{
		{"no files", "", "", false, true, 97},
		{"history only", HistoryFile, `{"kind":"vram"}` + "\n", true, true, 97},
		{"empty object", SettingsFile, `{}`, true, true, 97},
		{"vram off", SettingsFile, `{"vram": false}`, true, false, 97},
		{"pct 90", SettingsFile, `{"vram_pct": 90}`, true, true, 90},
		{"pct 99.5", SettingsFile, `{"vram_pct": 99.5}`, true, true, 99.5},
		{"pct above range clamps", SettingsFile, `{"vram_pct": 140}`, true, true, 100},
		{"pct below range clamps", SettingsFile, `{"vram_pct": 40}`, true, true, 80},
		{"pct as a string", SettingsFile, `{"vram_pct": "95"}`, true, true, 95},
		{"vram 0 is off, other keys kept", SettingsFile, `{"vram": 0, "vram_pct": 90}`, true, false, 90},
		{"wrong type keeps that key only", SettingsFile, `{"vram_pct": [1], "vram": "yes"}`, true, true, 97},
		{"not an object", SettingsFile, `[1,2]`, true, true, 97},
		{"malformed", SettingsFile, `{"vram_pct": `, true, true, 97},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, present := Read(write(t, tc.file, tc.body))
			if present != tc.present || s.VRAM != tc.vram || s.VRAMPct != tc.pct {
				t.Fatalf("got %+v present %v", s, present)
			}
			if s.VRAMS != def.VRAMS || s.ManagedGraceS != 20 || s.VRAMHardPct != 99.5 ||
				!slices.Equal(s.ManagedUnits, []string{"cerberus-lmstudio.scope", "cerberus.service", "lmstudio.service"}) {
				t.Fatalf("defaults lost: %+v", s)
			}
		})
	}
	t.Run("new keys", func(t *testing.T) {
		s, _ := Read(write(t, SettingsFile, `{"managed_units":["x.service"],"managed_grace_s":30,"vram_hard_pct":99,"vram_s":5}`))
		if !slices.Equal(s.ManagedUnits, []string{"x.service"}) || s.ManagedGraceS != 30 || s.VRAMHardPct != 99 || s.VRAMS != 5 {
			t.Fatalf("got %+v", s)
		}
		s, _ = Read(write(t, SettingsFile, `{"managed_grace_s":2}`))
		if s.ManagedGraceS != 5 {
			t.Fatalf("grace below range not clamped to 5: %+v", s)
		}
	})
	// The shared fixture: what railguard.py's read_settings gives for the
	// same file (python3 -c 'import railguard; print(railguard.read_settings())'
	// with STATE_DIR pointed at it).
	t.Run("same as railguard's read_settings", func(t *testing.T) {
		s, _ := Read(write(t, SettingsFile, `{"vram":1,"vram_pct":"99.9","vram_s":200.7,"managed_grace_s":"12.9",`+
			`"vram_hard_pct":null,"managed_units":["lmstudio.service"," ","a.scope","a.scope",{"x":1}],"unknown":5}`))
		want := Settings{VRAM: true, VRAMPct: 99.9, VRAMS: 120, ManagedGraceS: 12, VRAMHardPct: 99.5,
			ManagedUnits: []string{"a.scope", "lmstudio.service", "{'x': 1}"}}
		if !slices.Equal(s.ManagedUnits, want.ManagedUnits) || s.VRAM != want.VRAM || s.VRAMPct != want.VRAMPct ||
			s.VRAMS != want.VRAMS || s.ManagedGraceS != want.ManagedGraceS || s.VRAMHardPct != want.VRAMHardPct {
			t.Fatalf("got %+v, want %+v", s, want)
		}
		s, _ = Read(write(t, SettingsFile, `{"managed_units":"cerberus.service","vram_s":true,"vram_hard_pct":"NaN"}`))
		if len(s.ManagedUnits) != 3 || s.VRAMS != 3 || s.VRAMHardPct != 80 {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("empty dir name", func(t *testing.T) {
		if _, present := Read(""); present {
			t.Fatal("present with no dir")
		}
	})
	t.Run("Dir follows XDG_STATE_HOME", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/x/state")
		if got := Dir(); got != filepath.Join("/x/state", "hypr") {
			t.Fatalf("Dir %q", got)
		}
	})
}
