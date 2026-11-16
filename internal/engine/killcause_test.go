package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/railguard"
)

// The real railguard line of 2026-11-03 00:07:12 (CST): LM Studio's
// llama-server, whose daemon then lived in an agent's command scope.
const killLine = `{"time": 1793686032, "kind": "vram", "why": "VRAM 98% full", "name": "llama-server", "pid": 2813161, "unit": "run-p109029-i105992.scope", "gb": 2.4, "vram_gb": 20.5}`

var killAt = time.Unix(1793686032, 0)

// historyDir writes a railguard history with lines and returns a reader
// over it, as NewManager's readHistory would be.
func historyDir(t *testing.T, lines ...string) func(from, to time.Time) []railguard.Entry {
	t.Helper()
	dir := t.TempDir()
	raw := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, railguard.HistoryFile), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(from, to time.Time) []railguard.Entry { return railguard.History(dir, from, to) }
}

// notes records engine notifications.
type notes struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (n *notes) send(m notify.Message) { n.mu.Lock(); n.msgs = append(n.msgs, m); n.mu.Unlock() }
func (n *notes) get() []notify.Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.msgs)
}

func line(at time.Time, kind, name, unit string, pid int) string {
	return `{"time": ` + strconv.FormatInt(at.Unix(), 10) + `, "kind": "` + kind + `", "why": "VRAM 97% full", "name": "` +
		name + `", "pid": ` + strconv.Itoa(pid) + `, "unit": "` + unit + `", "gb": 3.5, "vram_gb": 18.7}`
}

func TestAttribute(t *testing.T) {
	m, _ := testManager(nil, -1)
	m.history = historyDir(t,
		`{"time": 1793686000, "kind": "memory", "why": "3.7% RAM free", "name": "python3", "pid": 1, "unit": "run-p1-i1.scope", "gb": 9}`,
		killLine,
		line(killAt.Add(30*time.Second), "stuck", "llama-server", "cerberus.service", 5),
	)
	scope := "run-p109029-i105992.scope"

	// Its unit was neither managed nor ours: not blamed.
	if _, ok := m.attribute(killAt, 0, BackendLMStudio); ok {
		t.Fatal("blamed railguard for a kill in a unit that is not an engine's")
	}
	// LM Studio's daemon lived in that scope: blamed.
	m.engineUnits = func(b string) []string {
		if b == BackendLMStudio {
			return []string{scope}
		}
		return []string{"cerberus.service"}
	}
	e, ok := m.attribute(killAt.Add(-2*time.Second), 0, BackendLMStudio)
	if !ok || e.Kind != "vram" || e.Why != "VRAM 98% full" {
		t.Fatalf("attribute = %+v %v", e, ok)
	}
	if f := railguardFailure(e, e.Time); f.Cause != "railguard-vram" || f.Text != "ended by Railguard: VRAM 98% full" {
		t.Fatalf("failure = %+v", f)
	}
	// The window is -15 s .. +5 s around the death.
	// A llama.cpp model is matched by its pid only, never by unit.
	if _, ok := m.attribute(killAt.Add(-2*time.Second), 0, BackendLlamaCpp); ok {
		t.Fatal("a llama.cpp death with no pid matched by unit")
	}
	if _, ok := m.attribute(killAt.Add(-6*time.Second), 0, BackendLMStudio); ok {
		t.Fatal("a line 6 s after the death matched")
	}
	if _, ok := m.attribute(killAt.Add(16*time.Second), 0, BackendLMStudio); ok {
		t.Fatal("a line 16 s before the death matched")
	}
	// A pid Cerberus spawned matches whatever the unit.
	m.engineUnits = nil
	if _, ok := m.attribute(killAt, 2813161, BackendLlamaCpp); !ok {
		t.Fatal("the llama-server's own pid did not match")
	}
	// Other kinds (stuck) and other processes never match.
	if _, ok := m.attribute(killAt.Add(30*time.Second), 5, BackendLlamaCpp); ok {
		t.Fatal("a stuck line matched")
	}
	// No history file: nothing.
	m.history = func(from, to time.Time) []railguard.Entry { return railguard.History(t.TempDir(), from, to) }
	if _, ok := m.attribute(killAt, 2813161, BackendLlamaCpp); ok {
		t.Fatal("matched with no history")
	}
}

// A railguard VRAM kill of an LM Studio model: no slot cut, a 1024 MiB
// smaller GPU budget on the next load (a lower --gpu), one notification,
// and a second report of the same load changes nothing.
func TestRailguardKillCutsGPUShareNotSlots(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.VRAMReserveMiB = -1
	cfg.Models = []config.ModelConfig{{ID: "q27", SizeMiB: 16921, Ctx: 32768, MaxParallel: 2}}
	f := &planFake{fakeBackend: newFake(BackendLMStudio, true, "q27"), slots: 4}
	m, _ := planManager(cfg, 19890, 24564, nil, f)
	m.memAvail = func() int { return 64000 }
	now := time.Unix(1794745087, 0) // 2026-11-15 06:18:07, the cerberus.service kill
	m.now = func() time.Time { return now }
	m.history = historyDir(t, line(now, "vram", "llama-server", "cerberus.service", 141931))
	var n notes
	m.notify = n.send

	ep, rel, err := m.Acquire(ctx, "q27")
	if err != nil {
		t.Fatal(err)
	}
	before := gpuArg(t, lmsLoadArgs(f.last(t), 0, ""))
	limit := modelInfo(m, "q27").MaxParallel
	if !m.MarkCrashedGen("q27", ep.Gen) || !m.MarkCrashedGen("q27", ep.Gen) {
		t.Fatal("MarkCrashedGen = false")
	}
	rel()
	info := modelInfo(m, "q27")
	if info.Loaded || info.MaxParallel != limit {
		t.Fatalf("after the kill: loaded %v limit %d, want false %d (no slot cut)", info.Loaded, info.MaxParallel, limit)
	}
	if info.LastFailure == nil || info.LastFailure.Cause != "railguard-vram" || info.LastFailure.Text != "ended by Railguard: VRAM 97% full" {
		t.Fatalf("LastFailure = %+v", info.LastFailure)
	}
	if got := m.FailureText("q27"); got != "local engine ended by Railguard (VRAM 97% full); reloading with a smaller GPU share" {
		t.Fatalf("FailureText = %q", got)
	}
	if !IsTransientText(503, m.FailureText("q27")) {
		t.Fatal("the gateway's 503 text is not transient")
	}
	if msgs := n.get(); len(msgs) != 1 || msgs[0].Kind != "engine" || !strings.Contains(msgs[0].Body, "smaller GPU share") {
		t.Fatalf("notifications = %+v", msgs)
	}

	_, rel, err = m.Acquire(ctx, "q27")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	after := gpuArg(t, lmsLoadArgs(f.last(t), 0, ""))
	if p := f.last(t).plan; p.FreeMiB != 19890-1024 {
		t.Fatalf("budget %d, want %d", p.FreeMiB, 19890-1024)
	}
	if after >= before {
		t.Fatalf("--gpu %.2f after the kill, want lower than %.2f", after, before)
	}
}

func gpuArg(t *testing.T, args []string) float64 {
	t.Helper()
	i := slices.Index(args, "--gpu")
	if i < 0 {
		t.Fatalf("no --gpu in %v", args)
	}
	r, err := strconv.ParseFloat(args[i+1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVRAMCutStepsMaxAndDecay(t *testing.T) {
	m, _ := testManager(nil, -1)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	st := &modelState{}
	for range 6 {
		m.addVRAMCutLocked(st)
	}
	if got := st.cutLocked(now); got != vramCutMax {
		t.Fatalf("cut %d, want max %d", got, vramCutMax)
	}
	if got := st.cutLocked(now.Add(59 * time.Minute)); got != vramCutMax {
		t.Fatalf("cut %d before an hour, want %d", got, vramCutMax)
	}
	if got := st.cutLocked(now.Add(2*time.Hour + time.Minute)); got != vramCutMax-2*vramCutStep {
		t.Fatalf("cut %d after 2 h, want %d", got, vramCutMax-2*vramCutStep)
	}
	if got := st.cutLocked(now.Add(10 * time.Hour)); got != 0 {
		t.Fatalf("cut %d after 10 h, want 0", got)
	}
}

// A crash with no railguard line yet takes a slot off; when the line shows
// up a moment later the slot comes back and the GPU share is cut instead.
func TestReattributeLateHistoryLine(t *testing.T) {
	ctx := context.Background()
	f := newFake(BackendLMStudio, true, "m")
	m, _ := testManager(nil, -1, slotFake{f, 4})
	now := time.Unix(1794745087, 0)
	m.now = func() time.Time { return now }
	var lines []string
	dir := t.TempDir()
	m.history = func(from, to time.Time) []railguard.Entry {
		_ = os.WriteFile(filepath.Join(dir, railguard.HistoryFile), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
		return railguard.History(dir, from, to)
	}
	m.reattributeEvery = time.Hour // the test calls reattribute itself
	var n notes
	m.notify = n.send

	ep, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	m.MarkCrashedGen("m", ep.Gen)
	rel()
	if got := modelInfo(m, "m"); got.MaxParallel != 3 || got.LastFailure.Cause != "crash" {
		t.Fatalf("after the crash: limit %d failure %+v", got.MaxParallel, got.LastFailure)
	}
	lines = append(lines, line(now.Add(3*time.Second), "vram", "llmster", "lmstudio.service", 9))
	// The next load looks again before planning.
	_, rel, err = m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	got := modelInfo(m, "m")
	if got.MaxParallel != 4 || got.LastFailure.Cause != "railguard-vram" {
		t.Fatalf("after the late line: limit %d failure %+v, want 4 railguard-vram", got.MaxParallel, got.LastFailure)
	}
	m.mu.Lock()
	cut := m.states["m"].vramCut
	m.mu.Unlock()
	if cut != vramCutStep {
		t.Fatalf("vramCut %d, want %d", cut, vramCutStep)
	}
	if len(n.get()) != 2 {
		t.Fatalf("notifications = %+v, want the crash then the cause", n.get())
	}
}

// Cerberus' own unload during a request is not a crash: the request's
// failure is of an older load and changes nothing.
func TestUnloadDuringRequestIsNotACrash(t *testing.T) {
	ctx := context.Background()
	f := newFake(BackendLMStudio, true, "m")
	m, _ := testManager(nil, -1, slotFake{f, 4})
	var n notes
	m.notify = n.send
	ep, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Unload(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	m.MarkCrashedGen("m", ep.Gen)
	rel()
	if got := modelInfo(m, "m"); got.MaxParallel != 4 || got.LastFailure != nil {
		t.Fatalf("limit %d failure %+v, want 4 and none", got.MaxParallel, got.LastFailure)
	}
	if len(n.get()) != 0 {
		t.Fatalf("notified: %+v", n.get())
	}
}

// A model Cerberus loaded that a listing no longer shows (and whose TTL has
// not passed) is recorded once; one past its TTL is LM Studio's own unload.
func TestVanishedModel(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.IdleTTLSec = 600
	f := newFake(BackendLMStudio, true, "a", "b")
	m, _ := testManager(cfg, -1, f)
	now := time.Unix(5000, 0)
	m.now = func() time.Time { return now }
	var n notes
	m.notify = n.send
	for _, id := range []string{"a", "b"} {
		_, rel, err := m.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rel()
	}
	m.mu.Lock()
	m.states["b"].lastUsed = now.Add(-11 * time.Minute) // past its TTL
	m.mu.Unlock()
	f.mu.Lock()
	f.loaded = map[string]bool{}
	f.mu.Unlock()
	m.ensureFresh(ctx, true)
	m.ensureFresh(ctx, true)
	a, b := modelInfo(m, "a"), modelInfo(m, "b")
	if a.Loaded || a.LastFailure == nil || a.LastFailure.Cause != "vanished" ||
		!strings.Contains(a.LastFailure.Text, "LM Studio's TTL, Atrium game mode or another client") {
		t.Fatalf("a = loaded %v failure %+v", a.Loaded, a.LastFailure)
	}
	if b.Loaded || b.LastFailure != nil {
		t.Fatalf("b (past its TTL) = loaded %v failure %+v", b.Loaded, b.LastFailure)
	}
	if msgs := n.get(); len(msgs) != 1 || !strings.HasPrefix(msgs[0].Body, "a: unloaded outside Cerberus") {
		t.Fatalf("notifications = %+v, want one for a", msgs)
	}
}

// One railguard line is blamed on the engine it names only
// (contract-engine-8): a known pid must be that pid, a line in Cerberus'
// own unit is not LM Studio's when its daemon runs elsewhere, and a
// vanished model looks back at most a minute.
func TestAttributeOnlyTheRightEngine(t *testing.T) {
	m, _ := testManager(nil, -1)
	m.history = historyDir(t,
		line(killAt, "vram", "llama-server", "cerberus.service", 100),
		line(killAt.Add(-3*time.Minute), "vram", "llmster", "lmstudio.service", 200),
	)
	m.engineUnits = func(b string) []string {
		if b == BackendLMStudio {
			return []string{"lmstudio.service"}
		}
		return []string{"cerberus.service"}
	}
	// llama.cpp model B (pid 300, crashed on its own) within 15 s of
	// railguard ending model A (pid 100) in the same unit.
	if e, ok := m.attribute(killAt.Add(3*time.Second), 300, BackendLlamaCpp); ok {
		t.Fatalf("model B blamed for A's line: %+v", e)
	}
	if _, ok := m.attribute(killAt.Add(3*time.Second), 100, BackendLlamaCpp); !ok {
		t.Fatal("model A's own line did not match its pid")
	}
	// An LM Studio model that vanished in the same moment: its daemon is in
	// lmstudio.service, so a llama-server line in cerberus.service is not it.
	if e, ok := m.attributeSince(killAt.Add(-time.Minute), killAt.Add(time.Second), 0, BackendLMStudio); ok {
		t.Fatalf("an LM Studio model blamed for a cerberus.service line: %+v", e)
	}
	// ... unless the daemon runs in Cerberus' own unit (the old way).
	m.engineUnits = func(string) []string { return []string{"cerberus.service"} }
	if _, ok := m.attribute(killAt.Add(time.Second), 0, BackendLMStudio); !ok {
		t.Fatal("LM Studio's daemon in Cerberus' own unit: its line did not match")
	}
	// A vanished window from a listing 5 minutes back stops at a minute.
	m.engineUnits = func(b string) []string {
		if b == BackendLMStudio {
			return []string{"lmstudio.service"}
		}
		return []string{"cerberus.service"}
	}
	if e, ok := m.attributeSince(killAt.Add(-5*time.Minute), killAt, 0, BackendLMStudio); ok {
		t.Fatalf("a line 3 minutes back matched: %+v", e)
	}
	if _, ok := m.attributeSince(killAt.Add(-5*time.Minute), killAt.Add(-2*time.Minute-30*time.Second), 0, BackendLMStudio); !ok {
		t.Fatal("a line 30 s before the death did not match")
	}
}

// A crash report against a llama.cpp model whose process still runs
// carries that process' pid (pidReporter), so it is not matched by unit.
func TestLlamaCppEnginePID(t *testing.T) {
	b := &llamaCpp{procs: map[string]*llamaProc{}}
	if b.enginePID("x") != 0 {
		t.Fatal("pid with no process")
	}
	var _ pidReporter = b
}
