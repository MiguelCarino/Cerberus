package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"carino.systems/cerberus/internal/config"
)

// Internal sessions (train.go): Cerberus itself starts a session to run a
// command it was configured with, so the run gets the terminal, replay,
// notifications, Stop and the Wall like any agent. Its agent profile is
// internal: it is stored in the session folder (internalProfileFile), never
// in the config, is never listed by Agents and cannot be picked by a person
// or an agent (normalize refuses its name).

// FinetuneTrainAgent is the internal agent profile of a fine-tuning run:
// its command is the owner's finetune.train_command, expanded for the run.
const FinetuneTrainAgent = "finetune-train"

// internalProfileFile holds an internal session's profile, in its folder.
const internalProfileFile = "internal-agent.json"

// ErrTrainerMissing is wrapped when the trainer's program is not found.
var ErrTrainerMissing = errors.New("trainer not found")

// internalAgent reports whether name is an internal profile.
func internalAgent(name string) bool { return name == FinetuneTrainAgent }

// TrainSpec starts a fine-tuning run as a scratch session.
type TrainSpec struct {
	Title string // e.g. "Fine-tune qwen3:8b"
	Task  string // what the session shows as its task (the trainer does not read it)
	// Command is the trainer's command line. The session placeholders
	// ({cwd} is the scratch workspace, {session}, {self}…) are expanded at
	// launch like any profile's, so a file in Files is "{cwd}/<name>".
	Command []string
	Env     map[string]string
	// Files are copied into the scratch workspace before the session is
	// queued: workspace file name (no folders) → source path.
	Files map[string]string
	Tags  []string // default ["finetune"]
}

// StartTrain queues a scratch session that runs t.Command under the
// internal FinetuneTrainAgent profile. The trainer runs without the bwrap
// sandbox (it is the owner's own configured program and usually needs the
// GPU, its caches and the run folder outside the workspace), only once (no
// retries, no escalation), and is stopped, resumed and replayed like any
// session.
func (m *Manager) StartTrain(ctx context.Context, t TrainSpec) (Info, error) {
	if len(t.Command) == 0 || strings.TrimSpace(t.Command[0]) == "" {
		return Info{}, fmt.Errorf("%w: no command", ErrTrainerMissing)
	}
	p := config.AgentProfile{Name: FinetuneTrainAgent, Command: append([]string(nil), t.Command...), Env: t.Env,
		Description: "Fine-tuning run (internal)", Sandbox: "none"}
	if !profileAvailable(p) {
		return Info{}, fmt.Errorf("%w: %q is not on PATH", ErrTrainerMissing, profileBinary(p))
	}
	for name := range t.Files {
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			return Info{}, fmt.Errorf("bad workspace file name %q", name)
		}
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return Info{}, err
	}
	task := strings.TrimSpace(t.Task)
	if task == "" {
		task = "Fine-tune"
	}
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = titleFrom(task)
	}
	tags := t.Tags
	if tags == nil {
		tags = []string{"finetune"}
	}
	spec := TaskSpec{Workspace: WorkspaceScratch, Task: task, Agent: FinetuneTrainAgent, Title: title,
		MaxAttempts: 1, Count: 1, Tags: tags}
	if err := normalizeMeta(&spec.Name, &spec.Tags, spec.Priority, spec.Limits); err != nil {
		return Info{}, err
	}
	spec.prepare = func(dir, wt string) error {
		if err := safeWrite(filepath.Join(dir, internalProfileFile), raw, 0o600); err != nil {
			return err
		}
		for name, src := range t.Files {
			if err := copyInto(src, filepath.Join(wt, name)); err != nil {
				return fmt.Errorf("copy %s into the workspace: %w", name, err)
			}
		}
		return nil
	}
	info, err := m.enqueue(ctx, spec, title, "", "", true)
	if err != nil {
		return Info{}, err
	}
	m.signal()
	return info, nil
}

// sessionProfile is the profile a session runs: an internal one from its
// folder, else the configured profile (m.profile).
func (m *Manager) sessionProfile(name, dir string) (config.AgentProfile, error) {
	if !internalAgent(name) {
		return m.profile(name)
	}
	var p config.AgentProfile
	raw, err := os.ReadFile(filepath.Join(dir, internalProfileFile))
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	if err != nil || p.Name != name || len(p.Command) == 0 {
		return p, fmt.Errorf("internal agent %q: its profile is missing from the session folder", name)
	}
	if !profileAvailable(p) {
		return p, fmt.Errorf("%w: %q is not on PATH", ErrTrainerMissing, profileBinary(p))
	}
	return p, nil
}

// copyInto copies src to a new file dst (0600).
func copyInto(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
