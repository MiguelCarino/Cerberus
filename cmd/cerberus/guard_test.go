package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/guardbench"
)

func TestGuardPrintRun(t *testing.T) {
	r := &guardbench.Run{
		ID: "20261105-100000-qg", Model: "qg", Threshold: 0.5, State: "done",
		Attacks: 36, Benign: 25, Detection: 0.92, FalseAlarm: 0.08, HasScores: true,
		Categories: []guardbench.CategoryRate{
			{Category: "direct_override", Total: 8, Scored: 8, Flagged: 8, Rate: 1},
			{Category: "benign_chat", Benign: true, Total: 8, Scored: 8, Flagged: 1, Rate: 0.125},
		},
		Sweep:  []guardbench.SweepPoint{{Threshold: 0.5, Detection: 0.92, FalseAlarm: 0.08}},
		Misses: []string{"encoded_rot13_readme", "benign_chat_recipe"},
	}
	var out bytes.Buffer
	printGuardRun(&out, r)
	s := out.String()
	for _, want := range []string{"model qg", "threshold 0.50", "92% on 36 attacks", "8% on 25 benign",
		"direct_override", "detection", "false alarm", "Threshold sweep", "Misses (2)", "encoded_rot13_readme"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestGuardCompareOffline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	cfgPath := filepath.Join(home, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"server":{"listen":"127.0.0.1:1","token":"t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, err := guardbenchDir()
	if err != nil {
		t.Fatal(err)
	}
	a := &guardbench.Run{ID: "20261105-100000-a", Model: "a", Threshold: 0.5, State: "done", Detection: 0.9, FalseAlarm: 0.1,
		Categories: []guardbench.CategoryRate{{Category: "direct_override", Rate: 1}, {Category: "benign_chat", Benign: true, Rate: 0.1}}}
	b := &guardbench.Run{ID: "20261105-110000-b", Model: "a", Threshold: 0.5, State: "done", Detection: 0.5, FalseAlarm: 0.1,
		Categories: []guardbench.CategoryRate{{Category: "direct_override", Rate: 0.5}, {Category: "benign_chat", Benign: true, Rate: 0.1}}}
	for _, r := range []*guardbench.Run{a, b} {
		if err := guardbench.Save(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"guard", "bench", "compare", a.ID, b.ID}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	s := out.String()
	for _, want := range []string{"direct_override", "-50 pts", "detection 90% → 50%", "false alarms 10% → 10%"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}

	// list shows both runs.
	out.Reset()
	if code := run([]string{"guard", "bench", "list"}, nil, &out, &errb); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(out.String(), a.ID) || !strings.Contains(out.String(), b.ID) {
		t.Errorf("list missing a run:\n%s", out.String())
	}
}
