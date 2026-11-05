//go:build !windows

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/session"
)

// POST /api/finetune/train runs a fake trainer script as a session: never a
// real trainer, never the GPU.
func TestFinetuneTrainSession(t *testing.T) {
	bin := t.TempDir()
	trainer := filepath.Join(bin, "fake-trainer")
	e := newCCEnv(t, func(c *config.Config) {
		c.Finetune.BaseModel = "qwen3:8b"
		c.Finetune.TrainCommand = []string{trainer, "--data", "{dataset}", "--base", "{base_model}", "--out", "{out}"}
	})
	ft, err := finetune.New(filepath.Join(t.TempDir(), "ft"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.finetune = ft

	// No usable rows: 409, and no run folder is left.
	if code, raw := e.call("POST", "/api/finetune/train", `{}`, "admin"); code != 409 || !strings.Contains(string(raw), "no usable examples") {
		t.Fatalf("train with no rows: %d %s", code, raw)
	}
	ids := seedFinetune(t, ft, 3, "an answer")
	if err := ft.RateOwner(ids[0], true, "", ""); err != nil {
		t.Fatal(err)
	}

	// The trainer is not installed: 409, nothing left behind.
	before := len(e.m.List())
	if code, raw := e.call("POST", "/api/finetune/train", `{}`, "admin"); code != 409 || !strings.Contains(string(raw), "not on PATH") {
		t.Fatalf("train with a missing trainer: %d %s", code, raw)
	}
	if runs, _ := ft.Runs(); len(runs) != 0 || len(e.m.List()) != before {
		t.Fatalf("a refused train left %d runs, %d sessions", len(runs), len(e.m.List())-before)
	}

	script := `#!/bin/bash
echo "fake trainer: $*"
while [ $# -gt 0 ]; do case "$1" in --data) data=$2;; --out) out=$2;; --base) base=$2;; esac; shift 2; done
test -f "$data" || { echo "no dataset"; exit 3; }
wc -l < "$data" > "$out/rows.txt"
echo "$base" > "$out/base.txt"
pwd > "$out/cwd.txt"
`
	if err := os.WriteFile(trainer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	code, raw := e.call("POST", "/api/finetune/train", `{"unrated":true}`, "admin")
	if code != 201 {
		t.Fatalf("train: %d %s", code, raw)
	}
	var info session.Info
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info.Agent != session.FinetuneTrainAgent || info.Title != "Fine-tune qwen3:8b" || info.Workspace != session.WorkspaceScratch ||
		!strings.Contains(info.Task, "3 examples") {
		t.Fatalf("info %+v", info)
	}
	e.waitFor("the run to pass", func() bool { return e.state(info.ID) == session.StatePassed })

	runs, err := ft.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %v %+v", err, runs)
	}
	run := runs[0]
	if run.Session != info.ID || run.Rows != 3 || !run.Unrated || run.Base != "qwen3:8b" || len(run.Command) == 0 {
		t.Errorf("run %+v", run)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(run.Out, name))
		return strings.TrimSpace(string(b))
	}
	got, _ := e.m.Get(info.ID)
	if read("rows.txt") != "3" || read("base.txt") != "qwen3:8b" || read("cwd.txt") != got.Worktree {
		t.Errorf("trainer saw rows=%q base=%q cwd=%q (workspace %s)", read("rows.txt"), read("base.txt"), read("cwd.txt"), got.Worktree)
	}
	if _, err := os.Stat(filepath.Join(got.Worktree, "dataset.jsonl")); err != nil {
		t.Errorf("the dataset is not in the workspace: %v", err)
	}

	code, raw = e.call("GET", "/api/finetune/runs", "", "admin")
	var res struct {
		Runs []struct {
			ID, Session, State, Base string
			Rows                     int
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || code != 200 || len(res.Runs) != 1 ||
		res.Runs[0].State != session.StatePassed || res.Runs[0].Session != info.ID || res.Runs[0].Rows != 3 {
		t.Fatalf("runs: %d %s", code, raw)
	}

	// The internal agent is not offered to people.
	code, raw = e.call("GET", "/api/status", "", "admin")
	if code != 200 || !strings.Contains(string(raw), `"echo"`) || strings.Contains(string(raw), `"`+session.FinetuneTrainAgent+`"`) {
		t.Errorf("agents list shows the internal profile: %s", raw)
	}
	if code, _ := e.call("POST", "/api/sessions", `{"task":"x","agent":"finetune-train","workspace":"scratch"}`, "admin"); code != 400 {
		t.Errorf("a person started the internal profile: %d", code)
	}
}
