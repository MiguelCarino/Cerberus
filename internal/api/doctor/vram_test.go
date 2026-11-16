package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVRAMReserveCheck(t *testing.T) {
	cases := []struct {
		name     string
		setting  int
		total    int
		railJSON string // "": no railguard files
		noLocal  bool
		want     Status
		text     string
	}{
		{"auto with railguard", 0, 24564, `{"vram_pct": 97}`, false, OK, "railguard's line at 97%"},
		{"auto without railguard", 0, 24564, "", false, OK, "keeps 1024 MiB of 24564 MiB free (default)"},
		{"fixed too small for railguard", 300, 24564, `{"vram_pct": 97}`, false, Warn, "at least 1505"},
		{"railguard with its VRAM watch off", 300, 24564, `{"vram": false}`, false, Warn, "under 3%"},
		{"fixed too small, no railguard", 500, 24564, "", false, Warn, "under 3%"},
		{"off with railguard", -1, 24564, `{}`, false, Warn, "never_kill"},
		{"off without railguard", -1, 24564, "", false, OK, "off"},
		{"total unknown", 0, -1, `{}`, false, OK, "not known"},
		{"no local engine", 0, 24564, `{}`, true, OK, "no local engine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.cfg.Engines.VRAMReserveMiB = tc.setting
			if tc.noLocal {
				f.cfg.Engines.LlamaCpp.Enabled = false
				f.cfg.Engines.LMStudio.Enabled = false
				f.cfg.Engines.Ollama.Enabled = false
			}
			e := f.envFor()
			e.RailguardDir = t.TempDir()
			if tc.railJSON != "" {
				if err := os.WriteFile(filepath.Join(e.RailguardDir, "railguard.json"), []byte(tc.railJSON), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e.VRAMTotalMiB = func(context.Context) int { return tc.total }
			c := vramReserveCheck(context.Background(), Defaults(e))
			if c.Status != tc.want || !strings.Contains(c.Detail+" "+c.Fix, tc.text) {
				t.Fatalf("got %s %q fix %q", c.Status, c.Detail, c.Fix)
			}
		})
	}
}

func TestRailguardDirFromEnv(t *testing.T) {
	e := Env{Home: "/home/x", Getenv: func(string) string { return "" }}
	if got := railguardDir(e); got != filepath.Join("/home/x", ".local", "state", "hypr") {
		t.Fatalf("dir %q", got)
	}
	e.Getenv = func(k string) string {
		if k == "XDG_STATE_HOME" {
			return "/s"
		}
		return ""
	}
	if got := railguardDir(e); got != filepath.Join("/s", "hypr") {
		t.Fatalf("dir %q", got)
	}
}

func TestNvidiaTotalMiB(t *testing.T) {
	f := newFixture(t)
	e := f.envFor()
	e.Output = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "nvidia-smi" && len(args) > 0 && args[0] == "--query-gpu=memory.total" {
			return "24564\n8192\n", nil
		}
		return "", errors.New("no")
	}
	if got := nvidiaTotalMiB(context.Background(), e); got != 32756 {
		t.Fatalf("total %d", got)
	}
	f.cfg.GPU.Backend = "none"
	if got := nvidiaTotalMiB(context.Background(), e); got != -1 {
		t.Fatalf("backend none: %d", got)
	}
	f.cfg.GPU.Backend = "auto"
	e.Output = func(context.Context, string, ...string) (string, error) { return "N/A\n", nil }
	if got := nvidiaTotalMiB(context.Background(), e); got != -1 {
		t.Fatalf("garbage: %d", got)
	}
}
