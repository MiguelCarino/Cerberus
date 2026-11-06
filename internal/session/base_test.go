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

// TestPinnedBase: a repo session with TaskSpec.Base gets its worktree on
// that commit, not on main's head (evals pin the base so every setup
// starts from the same tree); a base that is no commit, or on a scratch
// session, is refused.
func TestPinnedBase(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("agent.sh", "exit 0\n")
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{agent}})
	})
	first := gitT(t, e.repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(e.repo, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, e.repo, "add", "-A")
	gitT(t, e.repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "later")

	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "x", Base: strings.ToUpper(first[:10])})
	if err != nil {
		t.Fatal(err)
	}
	info := e.waitState(infos[0].ID, StatePassed)
	if info.BaseCommit != first {
		t.Fatalf("base %s, want %s", info.BaseCommit, first)
	}
	if _, err := os.Stat(filepath.Join(info.Worktree, "later.txt")); !os.IsNotExist(err) {
		t.Fatal("the worktree is on main's head, not the pinned base")
	}
	for _, bad := range []TaskSpec{
		{Repo: e.repo, Task: "x", Base: "--output=/tmp/x"},
		{Repo: e.repo, Task: "x", Base: "main"},
		{Repo: e.repo, Task: "x", Base: "0123456789abcdef0123456789abcdef01234567"},
		{Workspace: WorkspaceScratch, Task: "x", Base: first},
	} {
		if _, err := e.m.Create(context.Background(), bad); err == nil {
			t.Errorf("base %q on %q accepted", bad.Base, bad.Workspace)
		}
	}
}
