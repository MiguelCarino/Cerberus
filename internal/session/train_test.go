//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// A fine-tuning run is a scratch session under the internal profile: the
// dataset is in its workspace, the fake trainer runs once, and the profile
// is never offered or accepted from a person.
func TestStartTrainSession(t *testing.T) {
	var trainer string
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{"true"}})
		trainer = e.script("fake-trainer", `echo "training on $1 into $2"
test -f "$1" || exit 3
mkdir -p "$2" && cp "$1" "$2/seen.jsonl"
`)
	})
	src := filepath.Join(t.TempDir(), "dataset.jsonl")
	trainMust(t, os.WriteFile(src, []byte(`{"messages":[]}`+"\n"), 0o600))
	out := filepath.Join(t.TempDir(), "run")

	info, err := e.m.StartTrain(context.Background(), TrainSpec{Title: "Fine-tune qwen", Task: "Fine-tune qwen on 1 example",
		Command: []string{trainer, filepath.Join("{cwd}", "dataset.jsonl"), out},
		Files:   map[string]string{"dataset.jsonl": src}})
	trainMust(t, err)
	if info.Agent != FinetuneTrainAgent || info.Workspace != WorkspaceScratch || info.Title != "Fine-tune qwen" ||
		len(info.Tags) != 1 || info.Tags[0] != "finetune" {
		t.Fatalf("info %+v", info)
	}
	done := e.waitState(info.ID, StatePassed)
	if done.Attempt != 1 {
		t.Errorf("attempts %d", done.Attempt)
	}
	if _, err := os.Stat(filepath.Join(done.Worktree, "dataset.jsonl")); err != nil {
		t.Errorf("dataset not in the workspace: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(out, "seen.jsonl")); err != nil || !strings.Contains(string(raw), "messages") {
		t.Errorf("trainer output: %v %q", err, raw)
	}
	// A follow-up would run the trainer again over the same output: refused,
	// from a person and from an agent, and the session stays passed.
	if err := e.m.FollowUp(context.Background(), info.ID, "again"); !errors.Is(err, ErrNoFollowUp) {
		t.Errorf("follow-up on a trainer: %v", err)
	}
	if err := e.m.FollowUpIdle(context.Background(), info.ID, "again"); !errors.Is(err, ErrNoFollowUp) {
		t.Errorf("agent follow-up on a trainer: %v", err)
	}
	if got, _ := e.m.Get(info.ID); got.State != StatePassed || got.Attempt != 1 {
		t.Errorf("after refused follow-ups: %s attempt %d", got.State, got.Attempt)
	}

	for _, a := range e.m.Agents() {
		if a.Name == FinetuneTrainAgent {
			t.Error("the internal profile is listed for pickers")
		}
	}
	if _, err := e.m.Create(context.Background(), TaskSpec{Task: "x", Agent: FinetuneTrainAgent}); !errors.Is(err, ErrAgentArg) {
		t.Errorf("a person picked the internal profile: %v", err)
	}
	if _, err := e.m.Create(context.Background(), TaskSpec{Task: "x", EscalateTo: FinetuneTrainAgent}); !errors.Is(err, ErrAgentArg) {
		t.Errorf("escalation to the internal profile: %v", err)
	}
}

func TestStartTrainFailsAndRefuses(t *testing.T) {
	var trainer string
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{"true"}})
		trainer = e.script("bad-trainer", "echo out of memory; exit 4\n")
	})
	ctx := context.Background()
	info, err := e.m.StartTrain(ctx, TrainSpec{Task: "Fine-tune", Command: []string{trainer}})
	trainMust(t, err)
	failed := e.waitState(info.ID, StateFailed)
	if failed.Attempt != 1 || failed.ExitCode == nil || *failed.ExitCode != 4 {
		t.Errorf("failed run %+v (a trainer is never retried)", failed)
	}

	if _, err := e.m.StartTrain(ctx, TrainSpec{Command: []string{filepath.Join(e.bin, "no-such-trainer")}}); !errors.Is(err, ErrTrainerMissing) {
		t.Errorf("missing trainer: %v", err)
	}
	if _, err := e.m.StartTrain(ctx, TrainSpec{}); !errors.Is(err, ErrTrainerMissing) {
		t.Errorf("no command: %v", err)
	}
	before := len(e.m.List())
	if _, err := e.m.StartTrain(ctx, TrainSpec{Command: []string{trainer}, Files: map[string]string{"../x": "/etc/hostname"}}); err == nil {
		t.Error("a file name with a folder was accepted")
	}
	if _, err := e.m.StartTrain(ctx, TrainSpec{Command: []string{trainer}, Files: map[string]string{"d.jsonl": filepath.Join(e.bin, "missing")}}); err == nil {
		t.Error("a missing source file was accepted")
	}
	if n := len(e.m.List()); n != before {
		t.Errorf("refused runs left %d sessions", n-before)
	}
}

func trainMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
