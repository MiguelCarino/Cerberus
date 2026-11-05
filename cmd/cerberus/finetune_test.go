//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/platform"
)

func TestFinetuneCLI(t *testing.T) {
	bin := t.TempDir()
	trainer := filepath.Join(bin, "trainer.sh")
	// The fake trainer records its arguments and the dataset it got.
	os.WriteFile(trainer, []byte("#!/bin/sh\necho \"$@\" > \"$3/args\"\nwc -l < \"$1\" > \"$3/rows\"\n"), 0o700)
	path := cliConfig(t, func(c *config.Config) {
		c.Finetune.Capture = config.CaptureDelegated
		c.Finetune.BaseModel = "qwen3:8b"
		c.Finetune.TrainCommand = []string{trainer, "{dataset}", "{base_model}", "{out}"}
	})
	var out, errb bytes.Buffer
	if code := run([]string{"finetune", "-config", path}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "Examples:     0") {
		t.Fatalf("stats: %d %s %s", code, out.String(), errb.String())
	}
	// no usable rows yet: train refuses
	out.Reset()
	errb.Reset()
	if code := run([]string{"finetune", "train", "-config", path}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no usable examples") {
		t.Fatalf("train with nothing: %d %s", code, errb.String())
	}
	data, _ := platform.DataDir()
	ft, err := finetune.New(filepath.Join(data, "finetune"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := []finetune.Message{{Role: "user", Content: "q"}}
	a, _ := ft.Add(finetune.Example{Kind: finetune.KindDelegated, Model: "m", Messages: p, Answer: "a1"})
	ft.Add(finetune.Example{Kind: finetune.KindCloud, Model: "c", Messages: p, Answer: "a2"})
	ft.Rate(finetune.Rating{ID: a, Good: true})

	out.Reset()
	exp := filepath.Join(t.TempDir(), "x.jsonl")
	if code := run([]string{"finetune", "export", "-config", path, "--out", exp}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "Wrote 2 rows") {
		t.Fatalf("export: %d %s %s", code, out.String(), errb.String())
	}
	if fi, _ := os.Stat(exp); fi.Mode().Perm() != 0o600 {
		t.Errorf("export mode %v", fi.Mode().Perm())
	}
	runDir := filepath.Join(t.TempDir(), "run")
	out.Reset()
	if code := run([]string{"finetune", "train", "-config", path, "--dry-run", "--out", runDir}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "qwen3:8b") {
		t.Fatalf("dry run: %d %s %s", code, out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(runDir, "args")); err == nil {
		t.Error("dry run ran the trainer")
	}
	out.Reset()
	if code := run([]string{"finetune", "train", "-config", path, "--out", runDir}, nil, &out, &errb); code != 0 {
		t.Fatalf("train: %d %s %s", code, out.String(), errb.String())
	}
	args, _ := os.ReadFile(filepath.Join(runDir, "args"))
	rows, _ := os.ReadFile(filepath.Join(runDir, "rows"))
	if !strings.Contains(string(args), filepath.Join(runDir, "dataset.jsonl")+" qwen3:8b "+runDir) || strings.TrimSpace(string(rows)) != "2" {
		t.Errorf("trainer got %q, %q rows", args, rows)
	}
	// Default run folders are made exclusively: two runs (even in the same
	// second) never share or truncate one, and the empty run above left none.
	if ents, _ := os.ReadDir(ft.RunsDir()); len(ents) != 0 {
		t.Fatalf("the refused run left %d folders", len(ents))
	}
	stale := filepath.Join(ft.RunsDir(), "keep.txt")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(stale, nil, 0o600))
	for i := 0; i < 2; i++ {
		out.Reset()
		if code := run([]string{"finetune", "train", "-config", path, "--dry-run"}, nil, &out, &errb); code != 0 {
			t.Fatalf("default dry run %d: %d %s", i, code, errb.String())
		}
	}
	ents, _ := os.ReadDir(ft.RunsDir())
	dirs := 0
	for _, e := range ents {
		if e.IsDir() {
			dirs++
			if _, err := os.Stat(filepath.Join(ft.RunsDir(), e.Name(), "dataset.jsonl")); err != nil {
				t.Errorf("run %s: %v", e.Name(), err)
			}
		}
	}
	if dirs != 2 {
		t.Errorf("two default runs made %d folders", dirs)
	}
	if code := run([]string{"finetune", "nope"}, nil, &out, &errb); code != 2 {
		t.Errorf("unknown subcommand: %d", code)
	}
}
