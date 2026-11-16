package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/railguard"
)

// shedSetup loads old, mid and new (idle, LRU in that order) and busy (a
// request in flight) on a 24564 MiB card where the desktop takes 14000
// MiB and each model 2000 MiB.
func shedSetup(t *testing.T, rg *railguard.Settings) (*Manager, *fakeBackend, *fakeVRAM, *time.Time, func()) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Defaults()
	f := newFake(BackendLMStudio, true, "old", "mid", "new", "busy")
	for i := range f.models {
		f.models[i].SizeMiB = 2000
	}
	m, v := planManager(cfg, 100000, 24564, rg, f)
	m.memAvail = func() int { return 64000 }
	now := time.Unix(1794745087, 0)
	m.now = func() time.Time { return now }
	var busyRel func()
	for _, id := range []string{"old", "mid", "new", "busy"} {
		_, rel, err := m.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		if id == "busy" {
			busyRel = rel
		} else {
			rel()
		}
	}
	v.onRefresh = func() {
		used := 14000
		f.mu.Lock()
		for _, loaded := range f.loaded {
			if loaded {
				used += 2000
			}
		}
		f.mu.Unlock()
		v.free.Store(int64(24564 - used))
	}
	v.onRefresh()
	return m, f, v, &now, busyRel
}

func TestPressureAskShedsIdleLRU(t *testing.T) {
	ctx := context.Background()
	m, f, _, now, rel := shedSetup(t, nil)
	defer rel()
	var n notes
	m.notify = n.send
	ask := railguard.Pressure{V: 1, Resource: "vram", UsedMiB: 22000, TotalMiB: 24564, TargetMiB: 19000}
	m.pressure = func(at time.Time) (railguard.Pressure, bool) {
		ask.Time, ask.ActAt = float64(now.Unix()), float64(now.Unix()+20)
		return ask, true
	}
	m.pressureTick(ctx)
	if got := f.unloaded(); !slices.Equal(got, []string{"old", "mid"}) {
		t.Fatalf("unloaded %v, want [old mid]: least recently used first, down to the target", got)
	}
	if msgs := n.get(); len(msgs) != 1 || msgs[0].Title != "Railguard asked for VRAM" ||
		msgs[0].Body != "Unloaded old, mid (4000 MiB)." {
		t.Fatalf("notifications = %+v", msgs)
	}

	// A target only the busy model could meet: idle ones go, busy stays.
	ask.TargetMiB = 10000
	m.pressureTick(ctx)
	if got := f.unloaded(); !slices.Equal(got, []string{"old", "mid", "new"}) || !modelInfo(m, "busy").Loaded {
		t.Fatalf("unloaded %v, busy loaded %v: busy must stay", got, modelInfo(m, "busy").Loaded)
	}

	// No ask (stale, malformed, other version: ReadPressure says no):
	// nothing more happens.
	m.pressure = func(time.Time) (railguard.Pressure, bool) { return railguard.Pressure{}, false }
	m.pressureTick(ctx)
	if got := f.unloaded(); len(got) != 3 {
		t.Fatalf("unloaded %v without an ask", got)
	}
}

// The real file reader: stale and wrong-version files are ignored.
func TestPressureFileReader(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	now := time.Unix(1794745087, 0)
	write := func(body string) {
		t.Helper()
		writeFile(t, filepath.Join(dir, "hypr", railguard.PressureFile), 0)
		if err := os.WriteFile(filepath.Join(dir, "hypr", railguard.PressureFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"v":1,"time":1794745080,"resource":"vram","used_mib":23900,"total_mib":24564,"target_mib":22000,"act_at":1794745100}`)
	if p, ok := readPressure(now); !ok || p.TargetMiB != 22000 {
		t.Fatalf("fresh ask = %+v %v", p, ok)
	}
	write(`{"v":1,"time":1794745000,"resource":"vram","used_mib":23900,"total_mib":24564,"target_mib":22000}`)
	if _, ok := readPressure(now); ok {
		t.Fatal("an ask 87 s old was used")
	}
	write(`{"v":2,"time":1794745080,"resource":"vram","target_mib":22000}`)
	if _, ok := readPressure(now); ok {
		t.Fatal("a v2 ask was used")
	}
	write(`{"v":1,"time":`)
	if _, ok := readPressure(now); ok {
		t.Fatal("a malformed ask was used")
	}
	// NewManager looks for the file on Linux only.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.Enabled, cfg.Engines.LMStudio.Enabled, cfg.Engines.Ollama.Enabled = false, false, false
	cfg.Providers = nil
	if got := NewManager(cfg, nil, quietLog()).pressure != nil; got != (runtime.GOOS == "linux") {
		t.Fatalf("pressure reader set = %v on %s", got, runtime.GOOS)
	}
}

// The loop never looks while no local model is loaded, and looks every
// interval once one is.
func TestPressureLoopIdleNoPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFake(BackendLMStudio, true, "m")
	m, _ := testManager(nil, -1, f)
	m.pressureEvery = 5 * time.Millisecond
	var probes atomic.Int32
	m.pressure = func(time.Time) (railguard.Pressure, bool) {
		probes.Add(1)
		return railguard.Pressure{}, false
	}
	m.Models(ctx)
	done := make(chan struct{})
	go func() { m.pressureLoop(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	if n := probes.Load(); n != 0 {
		t.Fatalf("%d probes with nothing loaded, want 0", n)
	}
	if err := m.Load(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "probes once a model is loaded", func() bool { return probes.Load() >= 3 })
	if err := m.Unload(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	m.wakeReaper()
	time.Sleep(30 * time.Millisecond)
	before := probes.Load()
	time.Sleep(80 * time.Millisecond)
	if after := probes.Load(); after != before {
		t.Fatalf("probes went on after the unload: %d -> %d", before, after)
	}
	cancel()
	<-done
}

// Proactive shed: 6 s at railguard's line minus 1% while requests run
// sheds idle models down to the reserve; a 2 s spike does not.
func TestProactiveShed(t *testing.T) {
	ctx := context.Background()
	rg := railguard.Defaults() // present, VRAM watch on, line 97%
	m, f, v, now, rel := shedSetup(t, &rg)
	defer rel()
	m.active.Store(true)
	hot := int64(24564 - 23900) // 97.3% used
	setFree := func(free int64) { v.onRefresh = nil; v.free.Store(free) }

	// A 2 s spike.
	setFree(hot)
	m.pressureTick(ctx)
	*now = now.Add(2 * time.Second)
	m.pressureTick(ctx)
	setFree(5000)
	*now = now.Add(2 * time.Second)
	m.pressureTick(ctx)
	setFree(hot)
	*now = now.Add(2 * time.Second)
	m.pressureTick(ctx)
	if got := f.unloaded(); len(got) != 0 {
		t.Fatalf("a 2 s spike unloaded %v", got)
	}

	// Not while idle (the monitor polls slowly then).
	m.active.Store(false)
	for range 5 {
		*now = now.Add(2 * time.Second)
		m.pressureTick(ctx)
	}
	if got := f.unloaded(); len(got) != 0 {
		t.Fatalf("shed while idle: %v", got)
	}

	// 6 s at the line: shed, LRU first, down to total - reserve.
	m.active.Store(true)
	var n notes
	m.notify = n.send
	for i := range 4 {
		if i == 3 {
			// From here the reading follows what is loaded: 22000 used.
			v.onRefresh = func() {
				used := 14000
				f.mu.Lock()
				for _, loaded := range f.loaded {
					if loaded {
						used += 2000
					}
				}
				f.mu.Unlock()
				v.free.Store(int64(24564 - used))
			}
		}
		m.pressureTick(ctx)
		*now = now.Add(2 * time.Second)
	}
	// reserve = ceil(24564 × 3%) + 768 = 1505, so the target is 23059.
	// The shed starts from the 23900 reading; after the oldest model goes
	// the fresh reading is 20000, under the target, so only it goes.
	if got := f.unloaded(); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("unloaded %v, want [old]", got)
	}
	if msgs := n.get(); len(msgs) != 1 || !strings.Contains(msgs[0].Body, "before railguard had to act") {
		t.Fatalf("notifications = %+v", msgs)
	}

	// Off when the reserve is off.
	m.cfg.Engines.VRAMReserveMiB = -1
	setFree(hot)
	for range 5 {
		*now = now.Add(2 * time.Second)
		m.pressureTick(ctx)
	}
	if got := f.unloaded(); len(got) != 1 {
		t.Fatalf("shed with the reserve off: %v", got)
	}
}

// A model another client of LM Studio loaded (llm-chat, the app) is never
// shed: Cerberus cannot see its requests (contract-engine-9).
func TestShedLeavesOtherClientsModels(t *testing.T) {
	ctx := context.Background()
	m, f, v, now, rel := shedSetup(t, nil)
	defer rel()
	// "chat" was loaded behind Cerberus' back: the listing shows it loaded,
	// and it was never used through Cerberus (LRU first).
	f.mu.Lock()
	f.models = append(f.models, ModelInfo{ID: "chat", Backend: BackendLMStudio, Local: true, SizeMiB: 2000})
	f.loaded["chat"] = true
	f.mu.Unlock()
	m.ensureFresh(ctx, true)
	v.onRefresh()
	ask := railguard.Pressure{V: 1, Resource: "vram", UsedMiB: 24000, TotalMiB: 24564, TargetMiB: 19000}
	m.pressure = func(at time.Time) (railguard.Pressure, bool) {
		ask.Time, ask.ActAt = float64(now.Unix()), float64(now.Unix()+20)
		return ask, true
	}
	m.pressureTick(ctx)
	got := f.unloaded()
	if slices.Contains(got, "chat") {
		t.Fatalf("unloaded %v: another client's model was shed", got)
	}
	if !slices.Contains(got, "old") {
		t.Fatalf("unloaded %v: Cerberus' own idle models were not shed", got)
	}
}
