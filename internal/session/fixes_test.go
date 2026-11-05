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

// writer is a fake agent that writes name=content and exits 0.
func writer(e *testEnv, name, content string) {
	agent := e.script("writer.sh", "echo '"+content+"' > "+name+"\nexit 0\n")
	e.profiles(config.AgentProfile{Name: "writer", Command: []string{"bash", agent}})
}

func (e *testEnv) createOne(task string) string {
	e.t.Helper()
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: task})
	if err != nil {
		e.t.Fatal(err)
	}
	return infos[0].ID
}

func commitFile(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", name)
	gitT(t, repo, "-c", "user.name=U", "-c", "user.email=u@example.com", "commit", "-q", "-m", "user: "+name)
}

func TestFinalStateIsSticky(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) { writer(e, "a.txt", "a") })
	e.m.mu.Lock()
	s := &session{dir: t.TempDir(), out: e.m.newOutput(t.TempDir()),
		info: Info{ID: "x", Repo: e.repo, State: StateDiscarded, Agent: "writer"}}
	e.m.sessions["x"] = s
	e.m.order = append(e.m.order, "x")
	s.stopping = true
	e.m.setStateLocked(s, StateStopped)
	e.m.mu.Unlock()
	if info, _ := e.m.Get("x"); info.State != StateDiscarded {
		t.Fatalf("a late waiter resurrected a discarded session as %s", info.State)
	}
	// A start that finishes after the discard removes the worktree it made.
	e.m.start(s)
	info, _ := e.m.Get("x")
	if info.State != StateDiscarded || info.Worktree != "" {
		t.Fatalf("after late start: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(e.m.wtDir, "x")); !os.IsNotExist(err) {
		t.Fatal("orphan worktree left on disk")
	}
	if wl := gitT(t, e.repo, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Fatalf("orphan worktree registered: %q", wl)
	}
}

func TestLaggingSubscriberIsClosed(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) { writer(e, "a.txt", "a") })
	ch, unsub := e.m.Subscribe()
	e.m.mu.Lock()
	for range eventBuffer + 1 {
		e.m.emitLocked(EventState, Info{ID: "x"})
	}
	e.m.mu.Unlock()
	n := 0
	for range ch { // ends: the channel was closed instead of dropping silently
		n++
	}
	if n != eventBuffer {
		t.Fatalf("got %d buffered events, want %d", n, eventBuffer)
	}
	unsub() // must not double-close
}

func TestAcceptFailedFastForwardMovesBase(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) { writer(e, "a.txt", "agent") })
	id := e.createOne("write a")
	e.waitState(id, StatePassed)
	commitFile(t, e.repo, "b.txt", "user work\n")
	// An untracked a.txt in the main checkout makes the fast-forward fail.
	if err := os.WriteFile(filepath.Join(e.repo, "a.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Accept(context.Background(), id, AcceptOpts{}); err == nil || !strings.Contains(err.Error(), "fast-forward") {
		t.Fatalf("accept: %v", err)
	}
	diff, err := e.m.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, "b.txt") || !strings.Contains(diff, "+agent") {
		t.Fatalf("diff after a failed fast-forward shows main's commits as the session's work:\n%s", diff)
	}
}

func TestAcceptRefusesWorktreeOnBranch(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("brancher.sh", "git switch -q -c fix\necho x > f.txt\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "brancher", Command: []string{"bash", agent}})
	})
	id := e.createOne("branch")
	e.waitState(id, StatePassed)
	before := gitT(t, e.repo, "rev-parse", "refs/heads/fix")
	if _, err := e.m.Accept(context.Background(), id, AcceptOpts{}); err == nil || !strings.Contains(err.Error(), `branch "fix"`) {
		t.Fatalf("accept on a branch: %v", err)
	}
	if after := gitT(t, e.repo, "rev-parse", "refs/heads/fix"); after != before {
		t.Fatal("accept moved the agent's branch")
	}
	if n := gitT(t, e.repo, "rev-list", "--count", "refs/heads/main"); n != "1" {
		t.Fatalf("main moved: %s commits", n)
	}
}

func TestAcceptPushesOnlyTheSessionCommit(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("writer.sh", "echo \"$CERBERUS_SESSION\" > a.txt\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "writer", Command: []string{"bash", agent}})
	})
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, e.repo, "init", "-q", "--bare", origin)
	gitT(t, e.repo, "remote", "add", "origin", origin)
	gitT(t, e.repo, "push", "-q", "origin", "main")
	pushed := gitT(t, origin, "rev-parse", "refs/heads/main")

	id := e.createOne("write a")
	e.waitState(id, StatePassed)
	commitFile(t, e.repo, "wip.txt", "unpushed\n") // the user's own unpushed commit
	sha, err := e.m.Accept(context.Background(), id, AcceptOpts{Push: true})
	if err == nil || !strings.Contains(err.Error(), "not pushed") || !strings.Contains(err.Error(), "user: wip.txt") {
		t.Fatalf("accept with unrelated unpushed commits: %v", err)
	}
	if sha == "" || gitT(t, e.repo, "rev-parse", "refs/heads/main") != sha {
		t.Fatal("not accepted locally")
	}
	if got := gitT(t, origin, "rev-parse", "refs/heads/main"); got != pushed {
		t.Fatal("pushed the user's unpushed commit")
	}

	gitT(t, e.repo, "push", "-q", "origin", "main") // user publishes; next accept is clean
	id2 := e.createOne("write a again")
	e.waitState(id2, StatePassed)
	sha2, err := e.m.Accept(context.Background(), id2, AcceptOpts{Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if gitT(t, origin, "rev-parse", "refs/heads/main") != sha2 {
		t.Fatal("clean accept did not push")
	}
}

func TestCreateRefusesDeniedRepo(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		writer(e, "a.txt", "a")
		e.cfg.Agent.DenyPaths = []string{"**/repo/**"}
	})
	_, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "read it"})
	if err == nil || !strings.Contains(err.Error(), "deny_paths") {
		t.Fatalf("create on a denied repo: %v", err)
	}
}

func TestAgentEnvHasNoAdminToken(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("envy.sh", "printf '%s\\n%s\\n' \"$OPENAI_API_KEY\" \"$CERBERUS_REPO\" > env.txt\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "envy", Command: []string{"bash", agent}})
	})
	id := e.createOne("env")
	info := e.waitState(id, StatePassed)
	raw, err := os.ReadFile(filepath.Join(info.Worktree, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || lines[0] == e.cfg.Server.Token || lines[0] != e.cfg.SessionAgentToken(id) || lines[1] != e.repo {
		t.Fatalf("agent env = %q", raw)
	}
}

func TestGuardReserve(t *testing.T) {
	g := guardState{MemAvailMiB: 8000, MinFreeMiB: 1000, LocalModel: true, FreeVRAMMiB: 12000, NeedVRAMMiB: 10000}
	if ok, _ := g.decide(); !ok {
		t.Fatal("first should start")
	}
	g.reserve(true)
	if ok, _ := g.decide(); ok {
		t.Fatalf("second 10 GiB model admitted against the same 12 GiB: %+v", g)
	}
	g = guardState{MemAvailMiB: 8000, FreeVRAMMiB: 12000, NeedVRAMMiB: 10000, LocalModel: true}
	g.reserve(false) // same model as an earlier admission: VRAM already counted
	if g.FreeVRAMMiB != 12000 || g.MemAvailMiB != 8000-agentMemMiB {
		t.Fatalf("reserve(false) = %+v", g)
	}
}
