//go:build !windows

package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

// A pending spawn that is stopped (or killed) stays unapproved: a later
// follow-up, resume or checks run must not run it, also after a restart.
func TestStoppedPendingSpawnNeverRuns(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo child; sleep 0.2"}},
			config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "read -r x"}})
		e.cfg.Guard.MaxSessions = 10
		spawnOn(e, 2, 4, 8, true)
	})
	root := e.scratch("wait", nil)
	e.waitState(root, StateRunning)
	ctx := context.Background()
	mk := func() string {
		infos, err := e.m.Create(ctx, TaskSpec{Workspace: WorkspaceScratch, Task: "sub", Agent: "ok", Parent: root})
		if err != nil {
			t.Fatal(err)
		}
		return infos[0].ID
	}
	a, b := mk(), mk()
	if err := e.m.Stop(a); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Kill(b); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, b} {
		if i, _ := e.m.Get(id); i.State != StateStopped || i.WaitingReason != "stopped before approval" {
			t.Fatalf("stopped pending spawn %s %q", i.State, i.WaitingReason)
		}
		if err := e.m.FollowUpIdle(ctx, id, "now run"); err == nil {
			t.Fatal("agent follow-up requeued an unapproved spawn")
		}
		if err := e.m.FollowUp(ctx, id, "now run"); err == nil {
			t.Fatal("follow-up requeued an unapproved spawn")
		}
		if err := e.m.Resume(ctx, id, ""); err == nil {
			t.Fatal("resume requeued an unapproved spawn")
		}
		if err := e.m.Approve(id, true); err == nil {
			t.Fatal("approved a stopped spawn")
		}
	}
	time.Sleep(300 * time.Millisecond) // the scheduler had its chance
	if i, _ := e.m.Get(a); i.State != StateStopped || i.Attempt != 0 {
		t.Fatalf("unapproved spawn ran: %+v", i)
	}
	// It stays unapproved across a restart, and can still be discarded.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m2, err := NewManager(e.cfg, nil, nil, notify.New(e.cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.FollowUp(ctx, a, "x"); err == nil {
		t.Fatal("reloaded unapproved spawn accepted a follow-up")
	}
	m2.Shutdown(ctx)
	if err := e.m.Discard(a); err != nil {
		t.Fatal(err)
	}
	e.m.Stop(root)
}

// Reject keeps the spawn unrunnable until Discard has taken over.
func TestRejectKeepsSpawnOutOfQueue(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "read -r x"}})
		e.cfg.Guard.MaxSessions = 10
		spawnOn(e, 2, 4, 8, true)
	})
	root := e.scratch("wait", nil)
	e.waitState(root, StateRunning)
	infos, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceScratch, Task: "sub", Agent: "wait", Parent: root})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	events, unsub := e.m.Subscribe()
	defer unsub()
	if err := e.m.Approve(id, false); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-events:
			if ev.Session.ID == id && ev.Session.State == StateStarting {
				t.Fatal("a rejected spawn was admitted")
			}
			continue
		default:
		}
		break
	}
	if i, _ := e.m.Get(id); i.State != StateDiscarded {
		t.Fatalf("rejected spawn is %s", i.State)
	}
	e.m.Stop(root)
}

// The agent scope's follow-up never types into a running child, and an
// agent-chosen task or follow-up cannot be read as a CLI option.
func TestAgentSendAndArgs(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "echo up; read -r x; echo got:$x; read -r y"}})
		e.cfg.Guard.MaxSessions = 10
		e.cfg.Sessions.IdleWaitSec = 0
		spawnOn(e, 2, 4, 8, false)
	})
	ctx := context.Background()
	root := e.scratch("wait", nil)
	e.waitState(root, StateRunning)
	for _, task := range []string{"--dangerously-skip-permissions", "  -p x"} {
		_, err := e.m.Create(ctx, TaskSpec{Workspace: WorkspaceScratch, Task: task, Agent: "wait", Parent: root})
		if !errors.Is(err, ErrAgentArg) {
			t.Fatalf("task %q: %v", task, err)
		}
	}
	if _, err := e.m.Create(ctx, TaskSpec{Workspace: WorkspaceScratch, Task: "-x", Agent: "wait"}); err != nil {
		t.Fatalf("a person's task starting with - refused: %v", err)
	}
	infos, err := e.m.Create(ctx, TaskSpec{Workspace: WorkspaceScratch, Task: "sub", Agent: "wait", Parent: root})
	if err != nil {
		t.Fatal(err)
	}
	child := infos[0].ID
	waitFor(t, "child up", func() bool { i, _ := e.m.Get(child); return i.Activity == "up" })
	if err := e.m.FollowUpIdle(ctx, child, "a"); !errors.Is(err, ErrBusy) {
		t.Fatalf("send into a running child: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if i, _ := e.m.Get(child); i.Activity != "up" {
		t.Fatalf("the child got input: %q", i.Activity)
	}
	if err := e.m.Stop(child); err != nil {
		t.Fatal(err)
	}
	e.waitState(child, StateStopped)
	if err := e.m.FollowUpIdle(ctx, child, "--yolo"); !errors.Is(err, ErrAgentArg) {
		t.Fatalf("option follow-up: %v", err)
	}
	if err := e.m.FollowUpIdle(ctx, child, "next step"); err != nil {
		t.Fatalf("follow-up of a stopped child: %v", err)
	}
	e.m.Bulk(ctx, []string{root, child}, BulkOp{Kind: BulkStop})
}

// A secret that straddles the clip point is masked, not half kept.
func TestFeedMasksBeforeClip(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("Q7", 40)
	fd := newFeed(filepath.Join(t.TempDir(), feedName), func() []string { return []string{secret} })
	for _, it := range []FeedItem{
		{Kind: FeedInput, Text: "export KEY=" + secret + " ⏎"},
		{Kind: FeedFollowUp, Text: strings.Repeat("p", 60) + secret},
		{Kind: FeedSpawned, Text: strings.Repeat("s", 230) + secret},
	} {
		it.SessionID, it.Name = "s1", "n"
		fd.record(it)
	}
	for _, it := range fd.items(time.Time{}, 0) {
		if strings.Contains(it.Text, "Q7Q7") || strings.Contains(it.Text, "sk-proj") {
			t.Fatalf("%s leaks part of the secret: %q", it.Kind, it.Text)
		}
		limit := feedTextClip
		if it.Kind == FeedInput || it.Kind == FeedFollowUp {
			limit = inputClip
		}
		if n := len([]rune(it.Text)); n > limit {
			t.Fatalf("%s not clipped: %d runes", it.Kind, n)
		}
	}
	fd.close()
}

func TestInputLog(t *testing.T) {
	var l InputLog
	cases := []struct {
		in     string
		hidden bool
		want   []string
	}{
		{"he", false, nil},
		{"llo\r", false, []string{"hello ⏎"}},
		{"\x03", false, []string{"Ctrl-C"}},
		{"ab\x7fc\n", false, []string{"ac ⏎"}},
		{"\x1b[A", false, []string{"↑"}},
		{"\x1b", false, []string{"Esc"}},
		{"y\r\n", false, []string{"y ⏎"}},
		{"\r", false, []string{"⏎"}},
		{"ñu\t", false, []string{"ñu Tab"}},
		{"hun", true, nil},
		{"ter2\r", false, []string{"(hidden input) ⏎"}}, // part typed with echo off
		{"ok\r", false, []string{"ok ⏎"}},
	}
	for _, c := range cases {
		if got := l.Observe("s", []byte(c.in), c.hidden); !slices.Equal(got, c.want) {
			t.Errorf("Observe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if len(l.lines) != 0 || len(l.hidden) != 0 {
		t.Fatalf("lines left: %v %v", l.lines, l.hidden)
	}
	for in, want := range map[string][]string{
		"\r": {"⏎"}, "\x03": {"Ctrl-C"}, "y": {"y"}, "deploy\r": {"deploy ⏎"},
	} {
		if got := inputTexts(in, false); !slices.Equal(got, want) {
			t.Errorf("inputTexts(%q) = %q, want %q", in, got, want)
		}
	}
	if got := inputTexts("s3cret\r", true); !slices.Equal(got, []string{"(hidden input) ⏎"}) {
		t.Errorf("hidden bulk input = %q", got)
	}
}

// Input typed while the agent's terminal has echo off never reaches the
// feed as text.
func TestBulkInputHiddenWhenEchoOff(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "pw", Interactive: true,
			Command: []string{"bash", "-c", `stty -echo; echo prompt; read -r pw; stty echo; echo done; read -r x`}})
		e.cfg.Sessions.IdleWaitSec = 0
	})
	id := e.scratch("pw", nil)
	waitFor(t, "prompt", func() bool { i, _ := e.m.Get(id); return i.Activity == "prompt" })
	if !e.m.InputHidden(id) {
		t.Fatal("echo-off terminal not reported hidden")
	}
	if r := e.m.Bulk(context.Background(), []string{id}, BulkOp{Kind: BulkInput, Data: "hunter2\r"}); r[0].Result != BulkOK {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "done", func() bool { i, _ := e.m.Get(id); return i.Activity == "done" })
	if feedHas(e.m, id, FeedInput, "hunter2") || !feedHas(e.m, id, FeedInput, "(hidden input) ⏎") {
		t.Fatal("password text in the feed")
	}
	if e.m.InputHidden(id) {
		t.Fatal("echo-on terminal reported hidden")
	}
	e.m.Bulk(context.Background(), []string{id}, BulkOp{Kind: BulkInput, Data: "\x03"})
	if !feedHas(e.m, id, FeedInput, "Ctrl-C") {
		t.Fatal("bulk Ctrl-C not named in the feed")
	}
	e.m.Stop(id)
}

// A Kill between runs settles as stopped with reason "killed", and the
// reason does not stick to a later plain stop.
func TestKillBetweenRunsReason(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "true"}})
	})
	id := e.scratch("ok", nil)
	e.waitState(id, StatePassed)
	e.m.mu.Lock()
	s := e.m.sessions[id]
	e.m.setStateLocked(s, StateStarting) // as admit leaves it before launch
	s.work.Add(1)
	e.m.mu.Unlock()
	if err := e.m.Kill(id); err != nil {
		t.Fatal(err)
	}
	if err := e.m.launch(s); err != nil {
		t.Fatal(err)
	}
	s.work.Done()
	if i, _ := e.m.Get(id); i.State != StateStopped || i.WaitingReason != "killed" {
		t.Fatalf("killed between runs: %s %q", i.State, i.WaitingReason)
	}
	e.m.mu.Lock()
	stale := s.stopReason
	e.m.mu.Unlock()
	if stale != "" {
		t.Fatalf("stale stop reason %q", stale)
	}
}

// An agent that prints a bus error and fails fast does not turn scopes off
// when systemd-run itself works.
func TestAgentOutputCannotDisableScopes(t *testing.T) {
	var calls string
	e := newTestEnv(t, nil, func(e *testEnv) {
		calls = fakeSystemdRun(e, "while [ \"$1\" != -- ]; do shift; done; shift\nexec \"$@\"\n")
		e.profiles(config.AgentProfile{Name: "liar", Command: []string{"bash", "-c", "echo 'Failed to connect to bus: No medium found'; exit 1"}})
		e.cfg.Sessions.MaxAttempts = 1
	})
	id := e.scratch("liar", func(t *TaskSpec) { t.MaxAttempts = 1 })
	info := e.waitState(id, StateFailed)
	if e.m.noScope.Load() {
		t.Fatal("agent output turned scopes off")
	}
	if info.Attempt != 1 {
		t.Fatalf("attempt %d: the run was rolled back and relaunched", info.Attempt)
	}
	if n := countLines(calls); n != 2 { // the run and the probe
		t.Fatalf("systemd-run calls %d", n)
	}
}

func TestScopesEnforced(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		fakeSystemdRun(e, "exit 1\n")
	})
	if !e.m.ScopesEnforced() {
		t.Fatal("scopes reported off with systemd-run and a slice")
	}
	e.m.noScope.Store(true)
	if e.m.ScopesEnforced() {
		t.Fatal("scopes reported on after the no-bus fallback")
	}
}
