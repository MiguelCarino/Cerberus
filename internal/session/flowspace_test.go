//go:build !windows

package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// Flows' workspace "step": SameAs works in a done scratch session's
// folder in place; Seeds copy done sessions' files into a new scratch
// folder; a running or foreign source, or a bad folder, is refused.
func TestSameAsAndSeeds(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo hi > a.txt; mkdir -p sub; echo deep > sub/b.txt; ln -sf /etc/hostname link"}},
			config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "read -r x"}})
		e.cfg.Sessions.IdleWaitSec = 0
		e.cfg.Guard.MaxSessions = 10
	})
	ctx := context.Background()
	src := e.scratch("ok", nil)
	si := e.waitState(src, StatePassed)

	infos, err := e.m.Create(ctx, TaskSpec{Workspace: WorkspaceSame, SameAs: src, Task: "go on", Agent: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	in := infos[0]
	if in.Workspace != WorkspaceFolder || in.Worktree != si.Worktree || in.Parent != "" {
		t.Fatalf("same-as session %+v (source worktree %s)", in, si.Worktree)
	}
	e.waitState(in.ID, StatePassed)

	// A chain A -> B -> C: C borrows the scratch folder B borrowed from A
	// (flows-agent-1), and it is not refused as Cerberus' own data.
	infos, err = e.m.Create(ctx, TaskSpec{Workspace: WorkspaceSame, SameAs: in.ID, Task: "and on", Agent: "ok"})
	if err != nil {
		t.Fatalf("step of a step: %v", err)
	}
	if c := infos[0]; c.Workspace != WorkspaceFolder || c.Worktree != si.Worktree {
		t.Fatalf("chained session %+v (source worktree %s)", c, si.Worktree)
	}
	e.waitState(infos[0].ID, StatePassed)

	infos, err = e.m.Create(ctx, TaskSpec{Workspace: WorkspaceScratch, Task: "join", Agent: "ok",
		Seeds: []Seed{{Session: src, Into: "parts/1-home"}, {Session: in.ID, Into: "parts/2-again"}}})
	if err != nil {
		t.Fatal(err)
	}
	wt := infos[0].Worktree
	if wt == si.Worktree {
		t.Fatal("seeded session shares the source folder")
	}
	for _, rel := range []string{"parts/1-home/a.txt", "parts/1-home/sub/b.txt", "parts/2-again/a.txt"} {
		b, err := os.ReadFile(filepath.Join(wt, rel))
		if err != nil || len(b) == 0 {
			t.Fatalf("%s: %q %v", rel, b, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(wt, "parts/1-home/link")); err == nil {
		t.Fatal("a symlink was copied")
	}
	e.waitState(infos[0].ID, StatePassed)

	running := e.scratch("wait", nil)
	e.waitState(running, StateRunning)
	for name, spec := range map[string]TaskSpec{
		"running source":  {Workspace: WorkspaceSame, SameAs: running, Task: "x", Agent: "ok"},
		"unknown source":  {Workspace: WorkspaceSame, SameAs: "nope", Task: "x", Agent: "ok"},
		"other owner":     {Workspace: WorkspaceSame, SameAs: src, Task: "x", Agent: "ok", Owner: "u-other"},
		"seed escapes":    {Workspace: WorkspaceScratch, Seeds: []Seed{{Session: src, Into: "../out"}}, Task: "x", Agent: "ok"},
		"seed into .git":  {Workspace: WorkspaceScratch, Seeds: []Seed{{Session: src, Into: ".git/x"}}, Task: "x", Agent: "ok"},
		"seed in folder":  {Workspace: WorkspaceFolder, Dir: si.Worktree, Seeds: []Seed{{Session: src, Into: "p"}}, Task: "x", Agent: "ok"},
		"seed running":    {Workspace: WorkspaceScratch, Seeds: []Seed{{Session: running, Into: "p"}}, Task: "x", Agent: "ok"},
		"same, no source": {Workspace: WorkspaceSame, Task: "x", Agent: "ok"},
	} {
		if _, err := e.m.Create(ctx, spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	e.m.Bulk(ctx, []string{running}, BulkOp{Kind: BulkStop})
}

func TestLastReply(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo hi"}})
	})
	id := e.scratch("ok", nil)
	e.waitState(id, StatePassed)
	if got, err := e.m.LastReply(id); err != nil || got != "" {
		t.Fatalf("no conversation: %q %v", got, err)
	}
	conv := `{"model":"m","cwd":"/x","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"},` +
		`{"role":"assistant","content":"  I made <index.html>.\n"},{"role":"assistant","content":"","tool_calls":[{"id":"1"}]},{"role":"tool","content":"ok"}]}`
	if err := os.WriteFile(filepath.Join(e.m.sessDir, id, conversationFile), []byte(conv), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := e.m.LastReply(id); err != nil || got != "I made <index.html>." {
		t.Fatalf("last reply %q %v", got, err)
	}
	if _, err := e.m.LastReply("nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown session: %v", err)
	}
}
