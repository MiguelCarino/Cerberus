package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/evals"
)

func TestEvalReportPrint(t *testing.T) {
	d := evals.RunDetail{
		Run: evals.Run{ID: "er-20261105-100000-abcdef", EvalName: "Health", EvalVersion: 3, State: evals.RunDone, JudgeModel: "judge",
			Slots: []evals.Slot{{Setup: "claude", Repeat: 1, Error: "the session did not start: \x1b[2Jboom"}}},
		Reports: []evals.Report{{Setup: "qwen", Repeat: 1, Session: "s1", SetupHash: "abc123def456", SessionState: "passed",
			Results: []evals.Result{
				{Criterion: "has_server", Gate: "build", Iteration: 1, State: evals.StatePass},
				{Criterion: "health_route", Gate: "build", Iteration: 2, State: evals.StateFail, Rationale: "no route\x1b]52;c;x\x07"},
				{Criterion: "tests_pass", Gate: "test", State: evals.StateSkipped, Rationale: "gate build did not pass"},
			}}},
		Summary: []evals.Rank{{Rank: 1, Setup: "qwen", Reports: 1, GatesPassed: 0, GatesTotal: 2, CriteriaPassed: 1, CriteriaTotal: 3, WallMs: 12000},
			{Setup: "claude"}},
	}
	var out bytes.Buffer
	printEvalReport(&out, d)
	s := out.String()
	for _, want := range []string{"Health (v3) · done · judge judge", "0/2", "1/3", "✓ has_server", "✗ health_route", "(iteration 2)",
		"– tests_pass", "gate build did not pass", "abc123def456", "claude #1: the session did not start"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.ContainsRune(s, 0x1b) || strings.ContainsRune(s, 0x07) {
		t.Fatalf("control characters reached the terminal:\n%q", s)
	}
}

// TestEvalValidateOffline: without a daemon, validate checks the
// structure and says so.
func TestEvalValidateOffline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/cfg")
	t.Setenv("XDG_DATA_HOME", home+"/data")
	cfgPath := filepath.Join(home, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"server":{"listen":"127.0.0.1:1","token":"t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(home, "good.json")
	_ = os.WriteFile(good, []byte(`{"name":"x","task":"t","criteria":[{"id":"a","prompt":"p"}],"setups":[{"agent":"cerberus"}]}`), 0o600)
	bad := filepath.Join(home, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"name":"x","task":"t","criteria":[{"id":"a","prompt":"p","depends_on":["a"]}],"setups":[]}`), 0o600)
	var out, errb bytes.Buffer
	if code := run([]string{"eval", "validate", "-config", cfgPath, good}, nil, &out, &errb); code != 0 {
		t.Fatalf("good: exit %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "structure only") || !strings.Contains(out.String(), "Valid.") {
		t.Fatalf("good output:\n%s%s", out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"eval", "validate", "-config", cfgPath, bad}, nil, &out, &errb); code != 1 {
		t.Fatalf("bad: exit %d", code)
	}
	if !strings.Contains(out.String(), "depends_on") || !strings.Contains(out.String(), "setups") {
		t.Fatalf("bad output:\n%s", out.String())
	}
}
