package config

import "testing"

func TestEvalsDefaultsAndPersonas(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Defaults()
	if !c.Evals.Enabled || c.Evals.MaxParallel != 1 || c.Evals.EvidenceKB != 64 || c.Evals.JudgeModel != "" || c.Evals.Personas == nil {
		t.Fatalf("defaults %+v", c.Evals)
	}
	c.Evals.Personas = map[string]string{"strict": "Be blunt.", "Bad Name": "x", "long": string(make([]rune, 2001))}
	c.Evals.MaxParallel = 9
	var got []string
	for _, p := range c.Validate() {
		if p.Level == LevelError {
			got = append(got, p.Path)
		}
	}
	want := map[string]bool{"evals.max_parallel": false, "evals.personas.long": false}
	for _, p := range got {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	bad := false
	for _, p := range got {
		if p != "evals.max_parallel" && p != "evals.personas.long" && p != `evals.personas["Bad Name"]` && p != "evals.personas.Bad Name" {
			bad = true
		}
	}
	if !want["evals.max_parallel"] || !want["evals.personas.long"] || len(got) != 3 || bad {
		t.Fatalf("errors %v", got)
	}
}
