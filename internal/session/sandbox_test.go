//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/sandbox"
)

func swapAvailable(t *testing.T, fn func(context.Context) (string, error)) {
	t.Helper()
	old := sandboxAvailable
	sandboxAvailable = fn
	t.Cleanup(func() { sandboxAvailable = old })
}

func auditIn(t *testing.T, e *testEnv) *audit.Logger {
	t.Helper()
	l, err := audit.Open(filepath.Join(e.home, "audit"), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	e.opts = append(e.opts, WithAudit(l))
	return l
}

func TestSandboxFallbackWarns(t *testing.T) {
	swapAvailable(t, func(context.Context) (string, error) { return "", errors.New("bwrap is not installed") })
	var al *audit.Logger
	e := newTestEnv(t, nil, func(e *testEnv) {
		al = auditIn(t, e)
		e.profiles(config.AgentProfile{Name: "boxed", Sandbox: "bwrap", Command: []string{"bash", "-c", "echo agent ran"}})
	})
	id := e.scratch("boxed", nil)
	e.waitState(id, StatePassed)
	tx, _ := e.m.Transcript(id)
	if !strings.Contains(string(tx), "agent ran") || !strings.Contains(string(tx), "sandbox unavailable") {
		t.Fatalf("transcript %q", tx)
	}
	warned := false
	for _, it := range e.m.Feed(time.Time{}, 0) {
		if it.SessionID == id && it.Kind == FeedWarning && strings.Contains(it.Text, "bwrap is not installed") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no feed warning: %+v", e.m.Feed(time.Time{}, 0))
	}
	recs, _ := al.Query(time.Time{}, audit.ActorSystem, 0)
	if len(recs) != 1 || recs[0].Route != "sandbox.fallback" || len(recs[0].Targets) != 1 || recs[0].Targets[0] != id {
		t.Errorf("audit %+v", recs)
	}
}

func TestSandboxStrictRefuses(t *testing.T) {
	swapAvailable(t, func(context.Context) (string, error) { return "", errors.New("bwrap is not installed") })
	var al *audit.Logger
	e := newTestEnv(t, nil, func(e *testEnv) {
		al = auditIn(t, e)
		e.cfg.Sandbox.Default = "bwrap"
		e.cfg.Sandbox.Strict = true
		e.profiles(config.AgentProfile{Name: "plain", Command: []string{"bash", "-c", "touch should-not-exist"}})
	})
	id := e.scratch("plain", nil)
	info := e.waitState(id, StateError)
	if !strings.Contains(info.Error, "sandbox.strict") {
		t.Errorf("error %q", info.Error)
	}
	if _, err := os.Stat(filepath.Join(info.Worktree, "should-not-exist")); err == nil {
		t.Error("the agent ran without its sandbox")
	}
	if recs, _ := al.Query(time.Time{}, "", 0); len(recs) != 1 || recs[0].Route != "sandbox.refused" {
		t.Errorf("audit %+v", recs)
	}
}

func TestSandboxRepoOverrideAndNone(t *testing.T) {
	calls := 0
	swapAvailable(t, func(context.Context) (string, error) { calls++; return "", errors.New("missing") })
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.cfg.Sandbox.Default = "bwrap"
		e.cfg.Sandbox.Strict = true
		e.cfg.Repos[0].Sandbox = "none" // the repo override beats the default
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo fine"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "t", Agent: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(infos[0].ID, StatePassed)
	if calls != 0 {
		t.Errorf("availability probed %d times for an unsandboxed repo", calls)
	}
}

// TestSandboxRealBwrap runs an agent and its checks under a real bwrap
// (temp HOME and data folders; skipped without a usable bwrap).
func TestSandboxRealBwrap(t *testing.T) {
	if _, err := sandbox.Available(context.Background()); err != nil {
		t.Skipf("no usable bwrap: %v", err)
	}
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "boxed", Sandbox: "bwrap", Command: []string{"bash", "-c", `
cat /proc/1/comm > comm.txt
echo "$TMPDIR" > tmpdir.txt
echo x > "$HOME/escape.txt" 2>/dev/null
ls "$XDG_DATA_HOME/cerberus" > data.txt 2>&1
echo '{"checks":["cat /proc/1/comm > check.txt"]}' > .cerberus.json
`}})
	})
	id := e.scratch("boxed", nil)
	info := e.waitState(id, StatePassed)
	read := func(name string) string {
		raw, _ := os.ReadFile(filepath.Join(info.Worktree, name))
		return strings.TrimSpace(string(raw))
	}
	if read("comm.txt") != "bwrap" {
		t.Errorf("agent not in a pid namespace under bwrap: %q", read("comm.txt"))
	}
	if read("check.txt") != "bwrap" {
		t.Errorf("checks from .cerberus.json ran outside the sandbox: %q", read("check.txt"))
	}
	if read("tmpdir.txt") != "/tmp" {
		t.Errorf("TMPDIR %q", read("tmpdir.txt"))
	}
	if _, err := os.Stat(filepath.Join(e.home, "escape.txt")); err == nil {
		t.Error("the agent wrote into the home folder")
	}
	// The data folder is hidden except the session's own folder.
	if d := read("data.txt"); strings.Contains(d, "feed.jsonl") || strings.Contains(d, "worktrees") {
		t.Errorf("data folder visible: %q", d)
	}
}

func TestWriteAgentConfig(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.cfg.Agent.Policy.Deny = []string{"terraform destroy"}
		e.profiles(config.AgentProfile{Name: "x", Command: []string{"true"}})
	})
	dir := t.TempDir()
	p, err := e.m.writeAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "test-token") {
		t.Fatal("snapshot holds the server token")
	}
	var c config.Config
	if err := json.Unmarshal(raw, &c); err != nil || len(c.Agent.Policy.Deny) != 1 || c.Files.UploadDir == "" {
		t.Errorf("snapshot %s (%v)", raw, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}

func TestSafeWriteAndLogsIgnoreSymlinks(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "bashrc")
	if err := os.WriteFile(victim, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"meta.json", "meta.json.tmp", "scrollback.log"} {
		if err := os.Symlink(victim, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := safeWrite(filepath.Join(dir, "meta.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := capFile{path: filepath.Join(dir, "scrollback.log"), cap: 1 << 20}
	c.write([]byte("agent output\n"))
	c.close()
	if raw, _ := os.ReadFile(victim); string(raw) != "original\n" {
		t.Fatalf("write followed a symlink: %q", raw)
	}
	if st, _ := os.Lstat(filepath.Join(dir, "meta.json")); st.Mode()&os.ModeSymlink != 0 {
		t.Error("meta.json is still the symlink")
	}
}

func TestVerifyGitLink(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo hi > f.txt"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "t", Agent: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	info := e.waitState(infos[0].ID, StatePassed)
	ctx := context.Background()
	if err := checkWorktree(ctx, e.repo, info.Worktree); err != nil {
		t.Fatalf("a genuine worktree failed: %v", err)
	}
	if _, err := e.m.Diff(info.ID); err != nil {
		t.Fatalf("diff: %v", err)
	}
	link := filepath.Join(info.Worktree, ".git")
	orig, _ := os.ReadFile(link)

	// A fake git folder inside the workspace, with a hostile config.
	fake := filepath.Join(info.Worktree, "evil")
	gitT(t, info.Worktree, "init", "-q", "--bare", fake)
	pwned := filepath.Join(e.home, "pwned")
	if err := os.WriteFile(filepath.Join(fake, "config"), []byte("[core]\n\tfsmonitor = touch "+pwned+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("gitdir: "+fake+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Diff(info.ID); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("diff with a forged .git: %v", err)
	}
	if _, err := e.m.Accept(ctx, info.ID, AcceptOpts{}); err == nil {
		t.Error("accept with a forged .git succeeded")
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("the forged git config ran a command")
	}
	// A symlink in place of the file.
	_ = os.Remove(link)
	if err := os.Symlink(fake, link); err != nil {
		t.Fatal(err)
	}
	if err := checkWorktree(ctx, e.repo, info.Worktree); err == nil {
		t.Error("a symlinked .git passed")
	}
	_ = os.Remove(link)
	if err := os.WriteFile(link, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkWorktree(ctx, e.repo, info.Worktree); err != nil {
		t.Errorf("restored link failed: %v", err)
	}
}

func TestRemoveWorkspaceOnlyInsideWorktrees(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"true"}})
	})
	precious := filepath.Join(e.home, "precious")
	if err := os.MkdirAll(precious, 0o755); err != nil {
		t.Fatal(err)
	}
	// What an edited meta.json would hand to Discard.
	if err := e.m.removeWorkspace(WorkspaceRepo, e.repo, precious, filepath.Join(e.home, "x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatal("removed a folder outside the worktrees folder")
	}
}
