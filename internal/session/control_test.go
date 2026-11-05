//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

// spawnOn enables agent spawning with the given limits.
func spawnOn(e *testEnv, depth, children, total int, approval bool) {
	e.cfg.Agent.Spawn = config.SpawnConfig{Enabled: true, MaxDepth: depth, MaxChildren: children, MaxTotal: total, RequireApproval: approval}
}

// scratch queues one scratch session with agent and returns its id.
func (e *testEnv) scratch(agent string, mod func(*TaskSpec)) string {
	e.t.Helper()
	t := TaskSpec{Workspace: WorkspaceScratch, Task: "do it", Agent: agent}
	if mod != nil {
		mod(&t)
	}
	infos, err := e.m.Create(context.Background(), t)
	if err != nil {
		e.t.Fatal(err)
	}
	return infos[0].ID
}

// waitFor polls cond (tests only) until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func feedHas(m *Manager, id, kind, text string) bool {
	for _, it := range m.Feed(time.Time{}, 0) {
		if it.SessionID == id && it.Kind == kind && strings.Contains(it.Text, text) {
			return true
		}
	}
	return false
}

func TestUpdatePersistsAndValidates(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "read -r x"}})
	})
	id := e.scratch("wait", func(t *TaskSpec) { t.Name = " First "; t.Tags = []string{"Web"}; t.Priority = 3 })
	info, _ := e.m.Get(id)
	if info.Name != "First" || !slices.Equal(info.Tags, []string{"web"}) || info.Priority != 3 {
		t.Fatalf("create meta %+v", info)
	}
	events, unsub := e.m.Subscribe()
	defer unsub()

	name, tags, pin, prio := "Renamed", []string{"API", "ci"}, true, -4
	lim := Limits{MaxMinutes: 30, MemoryMax: "2G", MaxOutputMiB: 16}
	info, err := e.m.Update(id, Patch{Name: &name, Tags: &tags, Pinned: &pin, Priority: &prio, Limits: &lim})
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "Renamed" || !slices.Equal(info.Tags, []string{"api", "ci"}) || !info.Pinned || info.Priority != -4 || info.Limits != lim {
		t.Fatalf("updated %+v", info)
	}
	got := false
	for !got {
		select {
		case ev := <-events:
			got = ev.Session.ID == id && ev.Session.Name == "Renamed"
		case <-time.After(5 * time.Second):
			t.Fatal("no event for the update")
		}
	}
	if !feedHas(e.m, id, FeedUpdate, "renamed to Renamed") {
		t.Fatal("update not in the feed")
	}
	bad := []Patch{
		{Tags: &[]string{"no spaces"}},
		{Priority: ptr(11)},
		{Limits: &Limits{MemoryMax: "lots"}},
		{Name: ptr(strings.Repeat("x", 100))},
	}
	for _, p := range bad {
		if _, err := e.m.Update(id, p); err == nil {
			t.Errorf("patch %+v accepted", p)
		}
	}
	if _, err := e.m.Update("nope", Patch{Name: &name}); err != ErrNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	// Clearing the name falls back to the title.
	empty := ""
	if info, _ := e.m.Update(id, Patch{Name: &empty}); info.Name != info.Title {
		t.Fatalf("name %q, title %q", info.Name, info.Title)
	}
	if _, err := e.m.Update(id, Patch{Name: &name}); err != nil {
		t.Fatal(err)
	}

	// Another manager on the same data dir reads it all back from meta.json.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m2, err := NewManager(e.cfg, nil, nil, notify.New(e.cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Shutdown(context.Background())
	back, ok := m2.Get(id)
	if !ok || back.Name != "Renamed" || !slices.Equal(back.Tags, []string{"api", "ci"}) || !back.Pinned || back.Priority != -4 || back.Limits != lim {
		t.Fatalf("reloaded %+v", back)
	}
}

func ptr[T any](v T) *T { return &v }

func TestPriorityQueueOrder(t *testing.T) {
	var gate, logf string
	e := newTestEnv(t, nil, func(e *testEnv) {
		gate = filepath.Join(e.home, "gate")
		logf = filepath.Join(e.home, "order.log")
		blocker := e.script("block.sh", "while [ ! -f '"+gate+"' ]; do sleep 0.05; done\n")
		logger := e.script("log.sh", "echo \"$CERBERUS_SESSION\" >> '"+logf+"'\n")
		e.profiles(config.AgentProfile{Name: "log", Command: []string{"bash", logger}},
			config.AgentProfile{Name: "block", Command: []string{"bash", blocker}})
		e.cfg.Guard.MaxSessions = 1
	})
	blk := e.scratch("block", nil)
	e.waitState(blk, StateRunning)
	a := e.scratch("log", nil)
	b := e.scratch("log", func(t *TaskSpec) { t.Priority = 5 })
	c := e.scratch("log", func(t *TaskSpec) { t.Priority = -1 })
	d := e.scratch("log", func(t *TaskSpec) { t.Priority = 5 })
	if _, err := e.m.Update(c, Patch{Pinned: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, b, c, d} {
		e.waitState(id, StatePassed)
	}
	raw, _ := os.ReadFile(logf)
	if got, want := strings.Fields(string(raw)), []string{c, b, d, a}; !slices.Equal(got, want) {
		t.Fatalf("start order %v, want %v", got, want)
	}
}

func TestTimeLimitStops(t *testing.T) {
	old := limitMinute
	limitMinute = 100 * time.Millisecond
	t.Cleanup(func() { limitMinute = old })
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "slow", Command: []string{"bash", "-c", "echo working; sleep 30"}})
	})
	id := e.scratch("slow", func(t *TaskSpec) { t.Limits = Limits{MaxMinutes: 3} })
	start := time.Now()
	info := e.waitState(id, StateStopped)
	if info.WaitingReason != "time limit" {
		t.Fatalf("reason %q", info.WaitingReason)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("stopped after %v", d)
	}
	if !feedHas(e.m, id, FeedLimit, "time limit") {
		t.Fatal("limit not in the feed")
	}

	// Lowering the limit of a running session applies at once.
	id2 := e.scratch("slow", nil)
	e.waitState(id2, StateRunning)
	time.Sleep(150 * time.Millisecond)
	if _, err := e.m.Update(id2, Patch{Limits: &Limits{MaxMinutes: 1}}); err != nil {
		t.Fatal(err)
	}
	if info := e.waitState(id2, StateStopped); info.WaitingReason != "time limit" {
		t.Fatalf("reason %q", info.WaitingReason)
	}
}

func TestOutputLimitStops(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "noisy", Command: []string{"bash", "-c", "head -c 3000000 /dev/zero | tr '\\0' 'a'; sleep 30"}})
	})
	id := e.scratch("noisy", func(t *TaskSpec) { t.Limits = Limits{MaxOutputMiB: 1} })
	info := e.waitState(id, StateStopped)
	if info.WaitingReason != "output limit" {
		t.Fatalf("reason %q", info.WaitingReason)
	}
	if info.OutputBytes <= 1<<20 {
		t.Fatalf("output bytes %d", info.OutputBytes)
	}
}

func TestActivityFields(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "talk", Interactive: true,
			Command: []string{"bash", "-c", `printf '\033[31mfirst\033[0m\n\nlast line  \n\n'; read -r x`}})
		e.cfg.Sessions.IdleWaitSec = 0
	})
	id := e.scratch("talk", nil)
	waitFor(t, "activity", func() bool { i, _ := e.m.Get(id); return i.Activity == "last line" })
	info, _ := e.m.Get(id)
	if info.LastOutput.IsZero() || info.OutputBytes == 0 {
		t.Fatalf("output fields %+v", info)
	}
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StateStopped)
}

func TestBulk(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "echo", Interactive: true,
			Command: []string{"bash", "-c", `echo ready; read -r line; echo "got:$line"; read -r x`}})
		e.cfg.Sessions.IdleWaitSec = 0
	})
	ctx := context.Background()
	var ids []string
	for range 3 {
		ids = append(ids, e.scratch("echo", nil))
	}
	for _, id := range ids {
		waitFor(t, "ready", func() bool { i, _ := e.m.Get(id); return i.Activity == "ready" })
	}
	res := e.m.Bulk(ctx, append(slices.Clone(ids), "missing", ids[0]), BulkOp{Kind: BulkInput, Data: "hello\r"})
	if len(res) != 4 || res[3].ID != "missing" || res[3].Result != BulkError {
		t.Fatalf("results %+v", res)
	}
	for _, r := range res[:3] {
		if r.Result != BulkOK {
			t.Fatalf("input %+v", r)
		}
	}
	for _, id := range ids {
		waitFor(t, "echo", func() bool { i, _ := e.m.Get(id); return i.Activity == "got:hello" })
		if !feedHas(e.m, id, FeedInput, "hello") {
			t.Fatal("input not in the feed")
		}
	}
	for _, op := range []BulkOp{{Kind: BulkTag, Tag: "Night"}, {Kind: BulkPin}, {Kind: BulkPriority, Priority: 7}} {
		for _, r := range e.m.Bulk(ctx, ids, op) {
			if r.Result != BulkOK {
				t.Fatalf("%s: %+v", op.Kind, r)
			}
		}
	}
	for _, id := range ids {
		i, _ := e.m.Get(id)
		if !slices.Equal(i.Tags, []string{"night"}) || !i.Pinned || i.Priority != 7 {
			t.Fatalf("meta %+v", i)
		}
	}
	e.m.Bulk(ctx, ids[:1], BulkOp{Kind: BulkUntag, Tag: "night"})
	if i, _ := e.m.Get(ids[0]); len(i.Tags) != 0 {
		t.Fatalf("untag left %v", i.Tags)
	}
	if r := e.m.Bulk(ctx, ids, BulkOp{Kind: "explode"}); r[0].Result != BulkError {
		t.Fatalf("unknown kind %+v", r[0])
	}
	if r := e.m.Bulk(ctx, ids, BulkOp{Kind: BulkPriority, Priority: 99}); r[0].Result != BulkError {
		t.Fatalf("bad priority %+v", r[0])
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + strings.Repeat("x", i)
	}
	if r := e.m.Bulk(ctx, many, BulkOp{Kind: BulkStop}); len(r) != 101 || r[0].Result != BulkError {
		t.Fatal("more than 100 ids accepted")
	}
	// Accept on running sessions is skipped, then stop all.
	for _, r := range e.m.Bulk(ctx, ids, BulkOp{Kind: BulkAccept}) {
		if r.Result != BulkSkipped {
			t.Fatalf("accept of a running session: %+v", r)
		}
	}
	for _, r := range e.m.Bulk(ctx, ids, BulkOp{Kind: BulkStop}) {
		if r.Result != BulkOK {
			t.Fatalf("stop %+v", r)
		}
	}
	for _, id := range ids {
		e.waitState(id, StateStopped)
	}
	if r := e.m.Bulk(ctx, ids[:1], BulkOp{Kind: BulkStop}); r[0].Result != BulkSkipped {
		t.Fatalf("stop of a stopped session: %+v", r[0])
	}
	for _, r := range e.m.Bulk(ctx, ids, BulkOp{Kind: BulkDiscard}) {
		if r.Result != BulkOK {
			t.Fatalf("discard %+v", r)
		}
	}
}

// TestBulkAcceptSequential: two repo sessions on one repo accepted in one
// bulk call both land on main, which only works one after the other.
func TestBulkAcceptSequential(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("w.sh", "echo \"$CERBERUS_SESSION\" > \"out-$CERBERUS_SESSION.txt\"\n")
		e.profiles(config.AgentProfile{Name: "w", Command: []string{"bash", agent}})
	})
	ctx := context.Background()
	var ids []string
	for range 3 {
		ids = append(ids, e.createOne("write"))
	}
	for _, id := range ids {
		e.waitState(id, StatePassed)
	}
	if err := e.m.Discard(ids[2]); err != nil {
		t.Fatal(err)
	}
	res := e.m.Bulk(ctx, ids, BulkOp{Kind: BulkAccept, Message: "bulk"})
	if res[0].Result != BulkOK || res[1].Result != BulkOK || res[2].Result != BulkSkipped {
		t.Fatalf("results %+v", res)
	}
	log := gitT(t, e.repo, "log", "--format=%s")
	if strings.Count(log, "bulk") != 2 {
		t.Fatalf("main log:\n%s", log)
	}
	for _, id := range ids[:2] {
		if _, err := os.Stat(filepath.Join(e.repo, "out-"+id+".txt")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKill(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "stuck",
			Command: []string{"bash", "-c", `trap '' TERM HUP INT; echo stuck; while :; do sleep 1; done`}})
	})
	id := e.scratch("stuck", nil)
	waitFor(t, "output", func() bool { i, _ := e.m.Get(id); return i.Activity == "stuck" })
	start := time.Now()
	if err := e.m.Kill(id); err != nil {
		t.Fatal(err)
	}
	info := e.waitState(id, StateStopped)
	if info.WaitingReason != "killed" {
		t.Fatalf("reason %q", info.WaitingReason)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("kill took %v", d)
	}
	if err := e.m.Kill(id); err != ErrNotRunning {
		t.Fatalf("second kill: %v", err)
	}
}

// fakeSystemdRun puts a systemd-run on PATH that logs its arguments to
// calls, then runs body.
func fakeSystemdRun(e *testEnv, body string) string {
	if runtime.GOOS != "linux" {
		e.t.Skip("systemd scopes are Linux only")
	}
	dir := filepath.Join(e.home, "fakebin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	calls := filepath.Join(e.home, "systemd-run.calls")
	script := "#!/bin/bash\necho \"$*\" >> '" + calls + "'\n" + body
	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o755); err != nil {
		e.t.Fatal(err)
	}
	// systemctl too, so a Kill never reaches the real user manager.
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	e.cfg.Guard.SystemdSlice = "test.slice"
	e.cfg.Guard.SessionMemoryMax = "6G"
	return calls
}

func countLines(path string) int {
	raw, _ := os.ReadFile(path)
	return strings.Count(string(raw), "\n")
}

func TestNoUserBusFallback(t *testing.T) {
	var calls string
	e := newTestEnv(t, nil, func(e *testEnv) {
		calls = fakeSystemdRun(e, "echo 'Failed to connect to bus: No medium found' >&2\nexit 1\n")
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo agent ran"}})
	})
	id := e.scratch("ok", nil)
	info := e.waitState(id, StatePassed)
	if info.Attempt != 1 {
		t.Fatalf("attempt %d: the failed scope must not count", info.Attempt)
	}
	if !e.m.noScope.Load() || countLines(calls) != 2 { // the scoped run and the probe
		t.Fatalf("noScope %v, systemd-run calls %d", e.m.noScope.Load(), countLines(calls))
	}
	tx, _ := e.m.Transcript(id)
	if !strings.Contains(string(tx), "agent ran") {
		t.Fatalf("transcript %q", tx)
	}
	// Remembered: the next session does not try the scope again.
	id2 := e.scratch("ok", nil)
	e.waitState(id2, StatePassed)
	if countLines(calls) != 2 {
		t.Fatalf("systemd-run tried again: %d calls", countLines(calls))
	}
}

func TestScopeMemoryMaxFromLimits(t *testing.T) {
	var calls string
	e := newTestEnv(t, nil, func(e *testEnv) {
		calls = fakeSystemdRun(e, "while [ \"$1\" != -- ]; do shift; done; shift\nexec \"$@\"\n")
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo scoped"}})
	})
	a := e.scratch("ok", func(t *TaskSpec) { t.Limits = Limits{MemoryMax: "2G"} })
	e.waitState(a, StatePassed)
	b := e.scratch("ok", nil)
	e.waitState(b, StatePassed)
	raw, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "MemoryMax=2G") || !strings.Contains(lines[1], "MemoryMax=6G") ||
		!strings.Contains(lines[0], "--unit=cerberus-"+a) || !strings.Contains(lines[0], "--slice=test.slice") {
		t.Fatalf("systemd-run calls:\n%s", raw)
	}
	if e.m.noScope.Load() {
		t.Fatal("a working scope disabled scopes")
	}
}

func TestSpawnLimitsAndTree(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "pwd; read -r x"}})
		e.cfg.Sessions.IdleWaitSec = 0
		e.cfg.Guard.MaxSessions = 10
		spawnOn(e, 2, 2, 3, false)
	})
	root := e.scratch("wait", nil)
	e.waitState(root, StateRunning)
	child := func(parent string, ws string) (string, error) {
		infos, err := e.m.Create(context.Background(), TaskSpec{Workspace: ws, Task: "sub", Agent: "wait", Parent: parent})
		if err != nil {
			return "", err
		}
		return infos[0].ID, nil
	}
	c1, err := child(root, WorkspaceSame)
	if err != nil {
		t.Fatal(err)
	}
	ci, _ := e.m.Get(c1)
	ri, _ := e.m.Get(root)
	if ci.Parent != root || ci.Depth != 1 || ci.Workspace != WorkspaceFolder || ci.Worktree != ri.Worktree {
		t.Fatalf("same-workspace child %+v (parent worktree %s)", ci, ri.Worktree)
	}
	if !feedHas(e.m, c1, FeedSpawned, "by "+ri.Name+": ") {
		t.Fatal("spawn not in the feed")
	}
	c2, err := child(root, WorkspaceScratch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child(root, ""); err == nil || !strings.Contains(err.Error(), "children") {
		t.Fatalf("third child: %v", err)
	}
	g1, err := child(c1, "")
	if err != nil {
		t.Fatal(err)
	}
	if gi, _ := e.m.Get(g1); gi.Depth != 2 {
		t.Fatalf("grandchild depth %d", gi.Depth)
	}
	if err := e.m.CanSpawn(g1); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth 3: %v", err)
	}
	if err := e.m.CanSpawn(c2); err == nil || !strings.Contains(err.Error(), "tree") {
		t.Fatalf("tree total: %v", err)
	}
	var got []string
	for _, i := range e.m.Descendants(root) {
		got = append(got, i.ID)
	}
	if want := []string{c1, c2, g1}; !slices.Equal(got, want) {
		t.Fatalf("descendants %v, want %v", got, want)
	}
	if !e.m.IsDescendant(root, g1) || e.m.IsDescendant(c2, g1) {
		t.Fatal("IsDescendant wrong")
	}
	// Let every child finish launching before the config changes below:
	// launch reads cfg.Agent.Spawn, and the test must not race with it.
	for _, id := range []string{c1, c2, g1} {
		e.waitState(id, StateRunning)
	}
	// Discarding a child frees its slot.
	if err := e.m.Discard(c2); err != nil {
		t.Fatal(err)
	}
	if err := e.m.CanSpawn(root); err != nil {
		t.Fatalf("after discard: %v", err)
	}
	if _, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceSame, Task: "x", Agent: "wait"}); err == nil {
		t.Fatal(`"same" without a parent accepted`)
	}
	e.cfg.Agent.Spawn.Enabled = false
	if err := e.m.CanSpawn(root); err == nil {
		t.Fatal("spawn allowed while disabled")
	}
	e.m.Bulk(context.Background(), []string{root, c1, g1}, BulkOp{Kind: BulkStop})
}

func TestSpawnApproval(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo child; sleep 0.2"}},
			config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "read -r x"}})
		e.cfg.Guard.MaxSessions = 10
		spawnOn(e, 2, 4, 8, true)
	})
	root := e.scratch("wait", nil)
	e.waitState(root, StateRunning)
	mk := func() string {
		infos, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceScratch, Task: "sub", Agent: "ok", Parent: root})
		if err != nil {
			t.Fatal(err)
		}
		return infos[0].ID
	}
	a, b, c := mk(), mk(), mk()
	time.Sleep(300 * time.Millisecond) // the scheduler had its chance
	// A pending spawn survives a daemon shutdown and restart as pending.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m2, err := NewManager(e.cfg, nil, nil, notify.New(e.cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	if i, _ := m2.Get(c); i.State != StateQueued || i.WaitingReason != ReasonNeedsApproval {
		t.Fatalf("reloaded pending spawn %s %q", i.State, i.WaitingReason)
	}
	m2.Shutdown(context.Background())
	m3, err := NewManager(e.cfg, nil, nil, notify.New(e.cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	if i, _ := m3.Get(c); i.State != StateQueued || i.WaitingReason != ReasonNeedsApproval {
		t.Fatalf("pending spawn after shutdown %s %q", i.State, i.WaitingReason)
	}
	m3.Shutdown(context.Background())
	for _, id := range []string{a, b} {
		if i, _ := e.m.Get(id); i.State != StateQueued || i.WaitingReason != ReasonNeedsApproval {
			t.Fatalf("pending spawn %s %q", i.State, i.WaitingReason)
		}
	}
	if err := e.m.Approve(root, true); err == nil {
		t.Fatal("approved a session that was not pending")
	}
	if err := e.m.Approve(a, true); err != nil {
		t.Fatal(err)
	}
	e.waitState(a, StatePassed)
	if err := e.m.Approve(b, false); err != nil {
		t.Fatal(err)
	}
	if i, _ := e.m.Get(b); i.State != StateDiscarded {
		t.Fatalf("rejected spawn is %s", i.State)
	}
	if !feedHas(e.m, a, FeedState, "approved") || !feedHas(e.m, b, FeedState, "rejected") {
		t.Fatal("approval not in the feed")
	}
	e.m.Stop(root)
}

func TestAttachMany(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "echo", Interactive: true,
			Command: []string{"bash", "-c", `head -c 4000 /dev/zero | tr '\0' 'z'; echo; echo "hi $CERBERUS_SESSION"; read -r l; echo "re:$l"; read -r x`}})
		e.cfg.Sessions.IdleWaitSec = 0
	})
	a, b := e.scratch("echo", nil), e.scratch("echo", nil)
	for _, id := range []string{a, b} {
		waitFor(t, "greeting", func() bool { i, _ := e.m.Get(id); return strings.HasPrefix(i.Activity, "hi ") })
	}
	if _, _, _, err := e.m.AttachMany([]string{a, "nope"}, 8); err == nil {
		t.Fatal("unknown id accepted")
	}
	backlogs, ch, detach, err := e.m.AttachMany([]string{a, b, a}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	if len(backlogs) != 2 {
		t.Fatalf("%d backlogs", len(backlogs))
	}
	for id, bl := range backlogs {
		if len(bl) == 0 || len(bl) > 1024 || !strings.Contains(string(bl), "hi "+id) {
			t.Fatalf("backlog of %s: %d bytes %q", id, len(bl), bl)
		}
	}
	e.m.Bulk(context.Background(), []string{a, b}, BulkOp{Kind: BulkInput, Data: "ping\r"})
	seen := map[string]string{}
	deadline := time.After(waitLong)
	for !strings.Contains(seen[a], "re:ping") || !strings.Contains(seen[b], "re:ping") {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatal("wall stream closed")
			}
			seen[c.ID] += string(c.Data)
		case <-deadline:
			t.Fatalf("wall saw %q", seen)
		}
	}
	detach()
	for range ch { // closes after detach
	}
	e.m.Bulk(context.Background(), []string{a, b}, BulkOp{Kind: BulkStop})
}

func TestMCPConfigForClaude(t *testing.T) {
	var argsFile string
	e := newTestEnv(t, nil, func(e *testEnv) {
		dir := filepath.Join(e.home, "claudebin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		argsFile = filepath.Join(e.home, "claude.args")
		fake := "#!/bin/bash\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n"
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		e.profiles(config.AgentProfile{Name: "claude-headless", Command: []string{"claude", "-p", "{task}"}},
			config.AgentProfile{Name: "shell", Command: []string{"bash", "-c", "true"}})
		spawnOn(e, 2, 4, 8, false)
	})
	id := e.scratch("claude-headless", func(t *TaskSpec) { t.Task = "fix it" })
	e.waitState(id, StatePassed)
	raw, _ := os.ReadFile(argsFile)
	mcpPath := filepath.Join(e.m.sessDir, id, mcpConfigName)
	if want := "-p\nfix it\n--mcp-config\n" + mcpPath + "\n"; string(raw) != want {
		t.Fatalf("claude argv %q, want %q", raw, want)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
		} `json:"mcpServers"`
	}
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg.MCPServers["cerberus"]
	if !slices.Equal(srv.Args, []string{"mcp"}) || srv.Command == "" || srv.Env["CERBERUS_SESSION"] != id ||
		srv.Env["OPENAI_API_KEY"] != e.cfg.SessionAgentToken(id) || srv.Env["CERBERUS_URL"] == "" {
		t.Fatalf("mcp.json %s", data)
	}
	if strings.Contains(string(data), e.cfg.Server.Token) {
		t.Fatal("the admin token leaked into mcp.json")
	}
	if st, _ := os.Stat(mcpPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("mcp.json mode %v", st.Mode())
	}
	// Other profiles get nothing; with spawning off, claude gets nothing.
	other := e.scratch("shell", nil)
	e.waitState(other, StatePassed)
	if _, err := os.Stat(filepath.Join(e.m.sessDir, other, mcpConfigName)); !os.IsNotExist(err) {
		t.Fatal("mcp.json written for a non-claude profile")
	}
	e.cfg.Agent.Spawn.Enabled = false
	off := e.scratch("claude-headless", func(t *TaskSpec) { t.Task = "again" })
	e.waitState(off, StatePassed)
	if raw, _ := os.ReadFile(argsFile); strings.Contains(string(raw), "--mcp-config") {
		t.Fatalf("argv with spawning off: %q", raw)
	}
}

func TestFeedRecordsAndRedacts(t *testing.T) {
	const secret = "sk-feed-live-abcdef123"
	e := newTestEnv(t, nil, func(e *testEnv) {
		withFileVault(e, map[string]string{"api": secret})
		e.profiles(config.AgentProfile{Name: "echo", Interactive: true, Command: []string{"bash", "-c", "read -r a; read -r b"}})
		e.cfg.Sessions.IdleWaitSec = 0
	})
	id := e.scratch("echo", func(t *TaskSpec) { t.Name = "feeder" })
	e.waitState(id, StateRunning)
	if err := e.m.FollowUp(context.Background(), id, "use "+secret+" now"); err != nil {
		t.Fatal(err)
	}
	e.m.Record(id, FeedInput, "typed "+secret+strings.Repeat(" more", 40))
	if err := e.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StateStopped)
	items := e.m.Feed(time.Time{}, 0)
	kinds := map[string]bool{}
	for _, it := range items {
		if it.SessionID != id {
			continue
		}
		kinds[it.Kind] = true
		if it.Name != "feeder" {
			t.Fatalf("item name %q", it.Name)
		}
		if strings.Contains(it.Text, secret) {
			t.Fatalf("secret in feed: %q", it.Text)
		}
		if it.Kind == FeedInput && len([]rune(it.Text)) > inputClip {
			t.Fatalf("input not clipped: %q", it.Text)
		}
	}
	for _, k := range []string{FeedCreated, FeedState, FeedFollowUp, FeedInput} {
		if !kinds[k] {
			t.Fatalf("no %s item in %v", k, kinds)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(e.home, "data", "cerberus", feedName))
	if len(raw) == 0 || strings.Contains(string(raw), secret) {
		t.Fatalf("feed.jsonl %d bytes, secret present: %v", len(raw), strings.Contains(string(raw), secret))
	}
}
