//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

const waitLong = 30 * time.Second

// env is a manager on a temp data dir with one temp repo.
type testEnv struct {
	t    *testing.T
	m    *Manager
	cfg  *config.Config
	repo string
	bin  string // folder for fake agent scripts
	home string
	opts []Option // set by setup, passed to NewManager
}

// newTestEnv builds the repo and config, lets setup adjust them (and write
// fake agents with e.script), then starts the manager.
func newTestEnv(t *testing.T, checks []string, setup func(e *testEnv)) *testEnv {
	t.Helper()
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))

	repo := filepath.Join(home, "repo")
	gitT(t, home, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "initial")

	cfg := config.Defaults()
	cfg.Repos = []config.RepoConfig{{Name: "repo", Path: repo, Checks: checks}}
	cfg.RepoRoots = nil
	cfg.Guard = config.GuardConfig{MaxSessions: 3}
	cfg.Notify = config.NotifyConfig{Disabled: true}
	cfg.Git = config.GitConfig{Name: "Cerberus Test", Email: "cerberus@example.com"}
	cfg.Server.Token = "test-token"

	e := &testEnv{t: t, cfg: cfg, repo: repo, bin: t.TempDir(), home: home}
	setup(e)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewManager(cfg, nil, nil, notify.New(cfg.Notify, log), log, e.opts...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer scancel()
		m.Shutdown(sctx)
	})
	e.m = m
	return e
}

// profiles sets the agent profiles; the first is the default.
func (e *testEnv) profiles(ps ...config.AgentProfile) {
	e.cfg.Agents = ps
	e.cfg.DefaultAgent = ps[0].Name
}

// script writes a fake agent and returns its path.
func (e *testEnv) script(name, body string) string {
	p := filepath.Join(e.bin, name)
	if err := os.WriteFile(p, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// waitState blocks until session id reaches state (or fails on error/timeout).
func (e *testEnv) waitState(id, state string) Info {
	e.t.Helper()
	events, unsub := e.m.Subscribe()
	defer unsub()
	deadline := time.NewTimer(waitLong)
	defer deadline.Stop()
	for {
		info, _ := e.m.Get(id)
		if info.State == state {
			return info
		}
		if info.State == StateError && state != StateError {
			e.t.Fatalf("session errored waiting for %s: %s", state, info.Error)
		}
		select {
		case _, ok := <-events:
			if !ok { // dropped for lagging: resubscribe
				unsub()
				events, unsub = e.m.Subscribe()
			}
		case <-deadline.C:
			e.t.Fatalf("timed out waiting for %s; state %s (%s)", state, info.State, info.Error)
		}
	}
}

func TestEndToEndAccept(t *testing.T) {
	e := newTestEnv(t, []string{"test -f result.txt"}, func(e *testEnv) {
		agent := e.script("agent.sh", `echo "hello from fake agent"
printf '\033]777;cerberus;waiting;approve write?\007'
read -r answer
echo "got:$answer"
echo "done" > result.txt
exit 0
`)
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{"bash", agent, "{task_file}"}})
	})

	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "Write result.txt\nwith details"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Title != "Write result.txt" {
		t.Fatalf("infos %+v", infos)
	}
	id := infos[0].ID

	backlog, ch, detach, err := e.m.Attach(id)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	seen := string(backlog)
	deadline := time.After(waitLong)
	for !strings.Contains(seen, "hello from fake agent") {
		select {
		case b := <-ch:
			seen += string(b)
		case <-deadline:
			t.Fatalf("no agent output; got %q", seen)
		}
	}

	info := e.waitState(id, StateWaiting)
	if info.WaitingReason != "approve write?" {
		t.Fatalf("waiting reason %q", info.WaitingReason)
	}
	if err := e.m.Input(id, []byte("y\r")); err != nil {
		t.Fatal(err)
	}
	info = e.waitState(id, StatePassed)
	if info.ExitCode == nil || *info.ExitCode != 0 || info.Attempt != 1 {
		t.Fatalf("passed info %+v", info)
	}
	if info.RunStarted.IsZero() || info.RunStarted.Before(info.Created) || info.RunEnded.Before(info.RunStarted) ||
		info.RunEnded.After(info.Updated) {
		t.Fatalf("run window %v..%v (created %v, updated %v)", info.RunStarted, info.RunEnded, info.Created, info.Updated)
	}
	if !strings.Contains(info.ChecksOutput, "$ test -f result.txt") {
		t.Fatalf("checks output %q", info.ChecksOutput)
	}

	raw, _ := os.ReadFile(filepath.Join(e.m.sessDir, id, "scrollback.log"))
	if !strings.Contains(string(raw), "got:y") || strings.Contains(string(raw), "777;cerberus") {
		t.Fatalf("scrollback should hold the output without markers: %q", raw)
	}

	diff, err := e.m.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "result.txt") || !strings.Contains(diff, "+done") {
		t.Fatalf("diff misses the untracked file:\n%s", diff)
	}

	// Accept refuses while the main checkout is not on main, and keeps the work.
	gitT(t, e.repo, "switch", "-q", "--detach")
	if _, err := e.m.Accept(context.Background(), id, AcceptOpts{}); err == nil || !strings.Contains(err.Error(), "check out main") {
		t.Fatalf("accept on detached main checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(info.Worktree, "result.txt")); err != nil {
		t.Fatal("worktree lost after a refused accept")
	}
	gitT(t, e.repo, "switch", "-q", "main")

	sha, err := e.m.Accept(context.Background(), id, AcceptOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if head := gitT(t, e.repo, "rev-parse", "refs/heads/main"); head != sha {
		t.Fatalf("main at %s, accept returned %s", head, sha)
	}
	if got, _ := os.ReadFile(filepath.Join(e.repo, "result.txt")); string(got) != "done\n" {
		t.Fatalf("main checkout result.txt = %q", got)
	}
	msg := gitT(t, e.repo, "log", "-1", "--format=%B%n%an <%ae>")
	if !strings.HasPrefix(msg, "Write result.txt") || strings.Contains(msg, "Co-Authored") || !strings.Contains(msg, "Cerberus Test") {
		t.Fatalf("commit message/identity: %q", msg)
	}
	if n := gitT(t, e.repo, "rev-list", "--count", "HEAD"); n != "2" {
		t.Fatalf("want initial + one squashed commit, have %s", n)
	}
	if refs := gitT(t, e.repo, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/main" {
		t.Fatalf("branches after accept: %q", refs)
	}
	if _, err := os.Stat(info.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still present: %v", err)
	}
	if wl := gitT(t, e.repo, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Fatalf("worktree list: %q", wl)
	}
	if info, _ := e.m.Get(id); info.State != StateAccepted {
		t.Fatalf("state %s", info.State)
	}
}

func TestDiscard(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("sleepy.sh", "echo started\ntouch scratch.txt\nexec sleep 60\n")
		e.profiles(config.AgentProfile{Name: "sleepy", Command: []string{"bash", agent}})
	})

	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "sleep"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	info := e.waitState(id, StateRunning)
	start := time.Now()
	if err := e.m.Discard(id); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("discard waited for the kill timeout instead of SIGTERM")
	}
	info, _ = e.m.Get(id)
	if info.State != StateDiscarded || info.Worktree != "" {
		t.Fatalf("after discard: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(e.m.wtDir, id)); !os.IsNotExist(err) {
		t.Fatal("worktree folder still exists")
	}
	if refs := gitT(t, e.repo, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/main" {
		t.Fatalf("branches: %q", refs)
	}
	if err := e.m.Discard(id); err == nil {
		t.Fatal("second discard should fail")
	}
}

func TestRetryThenEscalate(t *testing.T) {
	e := newTestEnv(t, []string{"test -f fixed.txt"}, func(e *testEnv) {
		lazy := e.script("lazy.sh", "echo lazy attempt\nexit 0\n")
		fixer := e.script("fixer.sh", "cat \"$1\" > seen-task.txt\necho ok > fixed.txt\n")
		e.profiles(config.AgentProfile{Name: "lazy", Command: []string{"bash", lazy}},
			config.AgentProfile{Name: "fixer", Command: []string{"bash", fixer, "{task_file}"}})
	})

	events, unsub := e.m.Subscribe()
	defer unsub()
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "make fixed.txt", EscalateTo: "fixer", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	info := e.waitState(id, StatePassed)
	if info.Attempt != 3 || info.Agent != "fixer" {
		t.Fatalf("want attempt 3 by fixer, got %d by %s", info.Attempt, info.Agent)
	}
	var states []string
	for len(events) > 0 {
		ev := <-events
		if ev.Session.ID == id && (len(states) == 0 || states[len(states)-1] != ev.Session.State) {
			states = append(states, ev.Session.State)
		}
	}
	joined := strings.Join(states, ",")
	if !strings.Contains(joined, "retrying") || !strings.Contains(joined, "escalated") {
		t.Fatalf("states %s", joined)
	}
	seen, err := os.ReadFile(filepath.Join(info.Worktree, "seen-task.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seen), "make fixed.txt") || !strings.Contains(string(seen), "previous agent left work") {
		t.Fatalf("escalation task: %q", seen)
	}
}

func TestFailedChecksWithoutEscalation(t *testing.T) {
	e := newTestEnv(t, []string{"echo nope; false"}, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "lazy", Command: []string{"bash", e.script("lazy.sh", "exit 0\n")}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "x", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	info := e.waitState(infos[0].ID, StateFailed)
	if info.Attempt != 1 || !strings.Contains(info.ChecksOutput, "nope") {
		t.Fatalf("%+v", info)
	}
	// A follow-up starts a new run in the same worktree.
	if err := e.m.FollowUp(context.Background(), info.ID, "try harder"); err != nil {
		t.Fatal(err)
	}
	info = e.waitState(info.ID, StateFailed)
	if info.Attempt != 2 {
		t.Fatalf("follow-up attempt %d", info.Attempt)
	}
	if task, _ := os.ReadFile(filepath.Join(e.m.sessDir, info.ID, "task.md")); string(task) != "try harder\n" {
		t.Fatalf("task.md %q", task)
	}
}

func TestCreateValidation(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "roots")
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "true"}},
			config.AgentProfile{Name: "ghost", Command: []string{"cerberus-no-such-binary-xyz", "{task}"}})
		e.cfg.RepoRoots = []string{link}
	})
	ctx := context.Background()
	if _, err := e.m.Create(ctx, TaskSpec{Repo: e.repo, Task: "t", Agent: "ghost"}); err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("unavailable profile: %v", err)
	}
	if _, err := e.m.Create(ctx, TaskSpec{Repo: e.repo, Task: "t", Agent: "ok", EscalateTo: "ghost"}); err == nil {
		t.Fatal("unavailable escalation profile accepted")
	}
	other := t.TempDir()
	gitT(t, other, "init", "-q")
	if _, err := e.m.Create(ctx, TaskSpec{Repo: other, Task: "t", Agent: "ok"}); err == nil {
		t.Fatal("repo outside config accepted")
	}
	if _, err := e.m.Create(ctx, TaskSpec{Repo: filepath.Join(e.repo, ".git"), Task: "t", Agent: "ok"}); err == nil {
		t.Fatal("non-top-level path accepted")
	}
	if _, err := e.m.Create(ctx, TaskSpec{Repo: e.repo, Task: "  ", Agent: "ok"}); err == nil {
		t.Fatal("empty task accepted")
	}
	// A repo directly under a repo root is allowed, through a symlinked root.
	gitT(t, root, "init", "-q", "-b", "main", "child")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.validateRepo(ctx, filepath.Join(link, "child")); err != nil {
		t.Fatalf("repo under root: %v", err)
	}

	agents := e.m.Agents()
	avail := map[string]bool{}
	for _, a := range agents {
		avail[a.Name] = a.Available
	}
	if !avail["ok"] || avail["ghost"] || !avail["cerberus"] {
		t.Fatalf("agents %+v", agents)
	}
}

func TestFanOutAndGuardQueue(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "sleepy", Command: []string{"bash", e.script("sleepy.sh", "exec sleep 60\n")}})
		e.cfg.Guard.MaxSessions = 1
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "fan", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Group == "" || infos[0].Group != infos[1].Group {
		t.Fatalf("fan-out %+v", infos)
	}
	e.waitState(infos[0].ID, StateRunning)
	time.Sleep(100 * time.Millisecond)
	if q, _ := e.m.Get(infos[1].ID); q.State != StateQueued || !strings.Contains(q.WaitingReason, "1 of 1") {
		t.Fatalf("second session %s %q", q.State, q.WaitingReason)
	}
	if err := e.m.Stop(infos[0].ID); err != nil {
		t.Fatal(err)
	}
	e.waitState(infos[0].ID, StateStopped)
	e.waitState(infos[1].ID, StateRunning) // the queue moves on its own
	if err := e.m.Stop(infos[1].ID); err != nil {
		t.Fatal(err)
	}
	e.waitState(infos[1].ID, StateStopped)
}

func TestRestartMarksLiveSessionsStopped(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "true"}})
	})
	dir := filepath.Join(e.m.sessDir, "20260101-000000-abcdef")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(meta{Info: Info{ID: "20260101-000000-abcdef", State: StateRunning, Worktree: "/tmp/wt",
		Attempt: 1, RunStarted: started, LastOutput: started.Add(time.Hour)}, Task: "t", MaxAttempts: 2})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(e.cfg, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := m2.Get("20260101-000000-abcdef")
	if !ok || info.State != StateStopped || info.WaitingReason != "daemon restarted" || info.Worktree != "/tmp/wt" {
		t.Fatalf("restored %+v", info)
	}
	if !info.RunEnded.Equal(started.Add(time.Hour)) { // last seen alive, not the restart time
		t.Fatalf("run end after restart %v", info.RunEnded)
	}
	var md meta
	raw, _ = os.ReadFile(filepath.Join(dir, "meta.json"))
	if json.Unmarshal(raw, &md) != nil || md.Info.State != StateStopped {
		t.Fatalf("meta.json not rewritten: %s", raw)
	}
}

func TestInteractiveIdleWaiting(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("chat.sh", "echo ready\nread -r line\necho \"you said $line\"\nread -r line\n")
		e.profiles(config.AgentProfile{Name: "chat", Interactive: true, Command: []string{"bash", agent}})
		e.cfg.Sessions.IdleWaitSec = 1
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	info := e.waitState(id, StateWaiting)
	if !strings.Contains(info.WaitingReason, "no output") {
		t.Fatalf("reason %q", info.WaitingReason)
	}
	if err := e.m.FollowUp(context.Background(), id, "hi"); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StateRunning)
	e.waitState(id, StateWaiting) // idle again after the reply
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	info = e.waitState(id, StateStopped)
	if err := e.m.RunChecks(context.Background(), id); err == nil {
		t.Fatal("RunChecks without configured checks should fail")
	}
	raw, _ := os.ReadFile(filepath.Join(e.m.sessDir, id, "scrollback.log"))
	if !strings.Contains(string(raw), "you said hi") {
		t.Fatalf("scrollback %q", raw)
	}
}

// TestStopIgnoringSIGTERM: an interactive shell ignores SIGTERM; Stop must
// still end it at once through the PTY hangup, not after the kill grace.
func TestStopIgnoringSIGTERM(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "stubborn", Interactive: true, Command: []string{"bash", "--norc", "--noprofile", "-i"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	e.waitState(id, StateRunning)
	// Wait for the prompt: before bash has set up its signal handling a
	// SIGTERM would still kill it.
	backlog, ch, detach, err := e.m.Attach(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog) == 0 {
		select {
		case <-ch:
		case <-time.After(waitLong):
			t.Fatal("no prompt from the shell")
		}
	}
	detach()
	start := time.Now()
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StateStopped)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stop took %v: SIGHUP did not end the shell", d)
	}
}
