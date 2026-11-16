package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/railguard"
)

// planFake is a fake local backend that records the ModelInfo (and so the
// plan) each Load got, reports slots, and can run a hook during Load.
type planFake struct {
	*fakeBackend
	slots  int
	onLoad func(ModelInfo) // optional: runs inside Load, before it returns

	pmu   sync.Mutex
	plans []ModelInfo
}

func (p *planFake) Slots(ModelInfo) int { return max(p.slots, 1) }

func (p *planFake) Load(ctx context.Context, m ModelInfo) (Endpoint, error) {
	p.pmu.Lock()
	p.plans = append(p.plans, m)
	p.pmu.Unlock()
	if p.onLoad != nil {
		p.onLoad(m)
	}
	return p.fakeBackend.Load(ctx, m)
}

func (p *planFake) last(t *testing.T) ModelInfo {
	t.Helper()
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if len(p.plans) == 0 || p.plans[len(p.plans)-1].plan == nil {
		t.Fatal("no planned load")
	}
	return p.plans[len(p.plans)-1]
}

// planManager is testManager with a VRAM source that knows the total and
// a railguard stub (present = rg != nil).
func planManager(cfg *config.Config, free, total int, rg *railguard.Settings, bs ...Backend) (*Manager, *fakeVRAM) {
	if cfg == nil {
		cfg = config.Defaults()
	}
	v := &fakeVRAM{}
	v.free.Store(int64(free))
	m := newManager(cfg, totalVRAM{v, total}, quietLog(), bs)
	m.memAvail = func() int { return 0 }
	m.cgroupRoom = func(ModelInfo) int { return -1 }
	m.railguard = func() (railguard.Settings, bool) {
		if rg == nil {
			return railguard.Defaults(), false
		}
		return *rg, true
	}
	return m, v
}

func TestVRAMReserve(t *testing.T) {
	rg := railguard.Defaults()
	off := rg
	off.VRAM = false
	at := func(pct float64) railguard.Settings { s := rg; s.VRAMPct = pct; return s }
	cases := []struct {
		name      string
		setting   int
		total     int
		rg        railguard.Settings
		present   bool
		want      int
		wantSrc   string
		wantLineP float64
	}{
		{"off", -1, 24564, rg, true, 0, "off", 0},
		{"fixed", 2048, 24564, rg, true, 2048, "fixed", 0},
		{"auto, total unknown", 0, -1, rg, true, 0, "unknown", 0},
		{"auto, railguard 97%", 0, 24564, rg, true, 737 + 768, "railguard", 97},
		{"auto, railguard 90%", 0, 24564, at(90), true, 2457 + 768, "railguard", 90},
		{"auto, railguard 99.5%", 0, 24564, at(99.5), true, 123 + 768, "railguard", 99.5},
		{"auto, railguard VRAM watch off", 0, 24564, off, true, 1024, "default", 0},
		{"auto, no railguard", 0, 24564, rg, false, 1024, "default", 0},
		{"auto, no railguard, big card: 4%", 0, 48000, rg, false, 1920, "default", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VRAMReserve(tc.setting, tc.total, tc.rg, tc.present)
			if got.MiB != tc.want || got.Source != tc.wantSrc || got.LinePct != tc.wantLineP {
				t.Fatalf("got %+v, want %d %s %v", got, tc.want, tc.wantSrc, tc.wantLineP)
			}
		})
	}
}

func TestReadRailguardFromTempState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if _, present := readRailguard(); present {
		t.Fatal("present with no files")
	}
	hypr := filepath.Join(dir, "hypr")
	if err := os.MkdirAll(hypr, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hypr, "railguard.json"), []byte(`{"vram_pct": 95}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, present := readRailguard()
	if !present || s.VRAMPct != 95 {
		t.Fatalf("got %+v %v", s, present)
	}
	if r := VRAMReserve(0, 24564, s, present); r.MiB != 1229+768 {
		t.Fatalf("reserve %+v", r)
	}
}

func TestPlanShapeAndNeed(t *testing.T) {
	cases := []struct {
		info        ModelInfo
		limit       int
		ctx, slots  int
		description string
	}{
		{ModelInfo{Backend: BackendLlamaCpp, Ctx: 32768}, 4, 32768, 1, "llama.cpp: -c shared by all slots"},
		{ModelInfo{Backend: BackendLMStudio, Ctx: 32768}, 4, 32768, 4, "LM Studio: ctx per --parallel slot"},
		{ModelInfo{Backend: BackendLMStudio, Ctx: 8192}, 0, 8192, lmsDefaultParallel, "LM Studio with no limit: its default"},
		{ModelInfo{Backend: BackendOllama}, 2, ollamaDefaultCtx, 1, "Ollama: its default context"},
	}
	for _, tc := range cases {
		if ctx, slots := planShape(tc.info, tc.limit); ctx != tc.ctx || slots != tc.slots {
			t.Errorf("%s: got %d x %d", tc.description, ctx, slots)
		}
	}
	m, _ := testManager(nil, -1)
	info := ModelInfo{ID: "x", Backend: BackendLMStudio, SizeMiB: 17000, Ctx: 32768}
	if got, want := m.planNeedMiB(info, 4), estimateNeedMiB(17000, 32768, 4); got != want {
		t.Fatalf("need %d, want the estimate %d", got, want)
	}
	m.footprints.add(footprintKey(BackendLMStudio, "x", 32768, 4), 20000, time.Unix(1, 0))
	if got := m.planNeedMiB(info, 4); got != 20000+headroomMiB {
		t.Fatalf("need %d, want the learned 20512", got)
	}
	if got := m.planNeedMiB(info, 2); got != estimateNeedMiB(17000, 32768, 2) {
		t.Fatalf("another shape used the learned need: %d", got)
	}
}

// engine-gateway-4: makeRoom plans with the KV cache, so an idle model is
// evicted when only the new model's KV fails to fit.
func TestMakeRoomCountsKV(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Manager, *planFake) {
		cfg := config.Defaults()
		cfg.Engines.VRAMReserveMiB = -1 // this test is about KV only
		cfg.Models = []config.ModelConfig{{ID: "A", SizeMiB: 6000}, {ID: "B", SizeMiB: 17000, Ctx: 131072}}
		f := &planFake{fakeBackend: newFake(BackendLlamaCpp, true, "A", "B"), slots: 4}
		m, v := testManager(cfg, 21000, f)
		m.memAvail = func() int { return 64000 } // B may spill the rest
		v.onRefresh = func() {                   // A's 6000 MiB come back once it is unloaded
			f.mu.Lock()
			defer f.mu.Unlock()
			if slices.Contains(f.unloads, "A") {
				v.free.Store(27000)
			}
		}
		return m, f
	}
	t.Run("idle model evicted", func(t *testing.T) {
		m, f := setup(t)
		if err := m.Load(ctx, "A"); err != nil {
			t.Fatal(err)
		}
		// Weights alone (20062) fit in 21000; with 128k of KV B needs 28562.
		if err := m.Load(ctx, "B"); err != nil {
			t.Fatal(err)
		}
		if got := f.unloaded(); !slices.Equal(got, []string{"A"}) {
			t.Fatalf("unloaded %v, want [A]", got)
		}
		b := f.last(t)
		if b.plan.FreeMiB != 27000 || b.plan.NeedMiB != 28562 {
			t.Fatalf("plan %+v, want free 27000 need 28562", *b.plan)
		}
	})
	t.Run("busy model waited for", func(t *testing.T) {
		m, f := setup(t)
		m.roomWaitOverride = 50 * time.Millisecond
		_, rel, err := m.Acquire(ctx, "A")
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
		err = m.Load(ctx, "B")
		if !errors.Is(err, ErrRoomTimeout) {
			t.Fatalf("err %v, want ErrRoomTimeout (B used to load next to busy A)", err)
		}
		if f.loadCount("B") != 0 || len(f.unloaded()) != 0 {
			t.Fatal("loaded B or evicted busy A")
		}
	})
}

// engine-gateway-5 and web-api-ops-4: the reserve is kept free. The
// journal case: an 18 GB model idle, a small model whose weights fit but
// whose weights + KV + reserve do not.
func TestMakeRoomKeepsReserve(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		setting int
		rg      *railguard.Settings
		evicts  bool
		budget  int
	}{
		{"auto, no railguard: 1024", 0, nil, true, 3000 + 18000 - 1024},
		{"auto, railguard 97%: 1505", 0, &railguard.Settings{VRAM: true, VRAMPct: 97}, true, 3000 + 18000 - 1505},
		{"off: today's rule", -1, nil, false, 3000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Engines.VRAMReserveMiB = tc.setting
			cfg.Models = []config.ModelConfig{{ID: "big", SizeMiB: 18000}, {ID: "small", SizeMiB: 1500, Ctx: 32768}}
			f := &planFake{fakeBackend: newFake(BackendLMStudio, true, "big", "small"), slots: 4}
			m, v := planManager(cfg, 30000, 24564, tc.rg, f)
			if err := m.Load(ctx, "big"); err != nil {
				t.Fatal(err)
			}
			v.free.Store(3000)
			// small: 1724 + 512 + KV 4 x 32k = 750 -> 2986 <= 3000, but not
			// under the reserve.
			if err := m.Load(ctx, "small"); err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(f.unloaded(), "big"); got != tc.evicts {
				t.Fatalf("big evicted %v, want %v", got, tc.evicts)
			}
			p := f.last(t).plan
			if p.FreeMiB != tc.budget || p.NeedMiB != 2986 {
				t.Fatalf("plan %+v, want budget %d need 2986", *p, tc.budget)
			}
		})
	}
	t.Run("budget never negative: CPU only, not no flag", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Engines.VRAMReserveMiB = 2000
		cfg.Engines.AllowOverCommit = true
		cfg.Models = []config.ModelConfig{{ID: "small", SizeMiB: 1500, Ctx: 32768}}
		f := &planFake{fakeBackend: newFake(BackendLMStudio, true, "small"), slots: 4}
		m, _ := planManager(cfg, 1000, 24564, nil, f)
		if err := m.Load(ctx, "small"); err != nil {
			t.Fatal(err)
		}
		got := f.last(t)
		if got.plan.FreeMiB != 0 {
			t.Fatalf("budget %d, want 0", got.plan.FreeMiB)
		}
		if args := lmsLoadArgs(got, 0, ""); !slices.Contains(args, "off") {
			t.Fatalf("args %v: want --gpu off (no room on the GPU)", args)
		}
	})
}

// The split uses the reduced budget and the shared need.
func TestSplitUsesReserve(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, setting int, rg *railguard.Settings, free int) []string {
		t.Helper()
		cfg := config.Defaults()
		cfg.Engines.VRAMReserveMiB = setting
		cfg.Models = []config.ModelConfig{{ID: "q27", SizeMiB: 16921, Ctx: 32768, MaxParallel: 1}}
		f := &planFake{fakeBackend: newFake(BackendLMStudio, true, "q27"), slots: 4}
		m, _ := planManager(cfg, free, 24564, rg, f)
		m.memAvail = func() int { return 64000 }
		if err := m.Load(ctx, "q27"); err != nil {
			t.Fatal(err)
		}
		return lmsLoadArgs(f.last(t), 0, "")
	}
	gpu := func(t *testing.T, args []string) float64 {
		t.Helper()
		i := slices.Index(args, "--gpu")
		if i < 0 {
			return 1
		}
		r, err := strconv.ParseFloat(args[i+1], 64)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	rg := &railguard.Settings{VRAM: true, VRAMPct: 97}
	// need = 19459 + 512 + KV 2115 = 22086
	if r := gpu(t, run(t, 0, rg, 19890)); r > 0.88 || r < 0.80 {
		t.Fatalf("with the railguard reserve --gpu %.2f, want about 0.83", r)
	}
	if r := gpu(t, run(t, -1, nil, 19890)); r != 0.90 {
		t.Fatalf("reserve off: --gpu %.2f, want today's 0.90", r)
	}
	if args := run(t, 0, rg, 24000); slices.Contains(args, "--gpu") {
		t.Fatalf("fits under the reserve: %v", args)
	}
}

// web-api-ops-4: after a load that pushed the card past the reserve, idle
// models are unloaded (LRU first) until it is free again; busy ones never.
func TestGuardAfterLoad(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.VRAMReserveMiB = 1000
	cfg.Models = []config.ModelConfig{{ID: "new", SizeMiB: 5000}}
	f := newFake(BackendLlamaCpp, true, "old", "mid", "busy", "new")
	m, v := planManager(cfg, 100000, 24564, nil, f)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	for _, id := range []string{"old", "mid", "busy"} {
		_, rel, err := m.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		if id != "busy" {
			rel()
		}
	}
	// 10000 free before "new"; it really takes 9500; each idle model gives
	// back 3500.
	v.onRefresh = func() {
		free := 10000
		f.mu.Lock()
		if f.loaded["new"] {
			free -= 9500
		}
		for _, id := range []string{"old", "mid", "busy"} {
			if !f.loaded[id] {
				free += 3500
			}
		}
		f.mu.Unlock()
		v.free.Store(int64(free))
	}
	if err := m.Load(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if got := f.unloaded(); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("unloaded %v, want [old] once", got)
	}

	t.Run("busy is never unloaded", func(t *testing.T) {
		m2, v2 := planManager(cfg, 100000, 24564, nil, f)
		f.mu.Lock()
		f.loaded, f.unloads = map[string]bool{}, nil
		f.mu.Unlock()
		_, rel, err := m2.Acquire(ctx, "busy")
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
		v2.onRefresh = func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.loaded["new"] {
				v2.free.Store(200)
			} else {
				v2.free.Store(10000)
			}
		}
		if err := m2.Load(ctx, "new"); err != nil {
			t.Fatal(err)
		}
		if got := f.unloaded(); len(got) != 0 {
			t.Fatalf("unloaded %v, want nothing (only busy left)", got)
		}
	})
}

// engine-gateway-7: LM Studio's cgroup limit caps the RAM side.
func TestOvercommitCgroupRoom(t *testing.T) {
	ctx := context.Background()
	setup := func(room int) (*Manager, *fakeBackend) {
		cfg := config.Defaults()
		cfg.Engines.WaitForRoomSec = 0
		cfg.Engines.VRAMReserveMiB = -1
		cfg.Models = []config.ModelConfig{{ID: "new", SizeMiB: 5000}}
		f := newFake(BackendLMStudio, true, "new")
		m, _ := testManager(cfg, 2000, f)
		m.memAvail = func() int { return 16000 }
		m.cgroupRoom = func(info ModelInfo) int {
			if info.Backend == BackendLMStudio {
				return room
			}
			return -1
		}
		return m, f
	}
	m, f := setup(1000)
	err := m.Load(ctx, "new")
	if !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), "cgroup limit leaves 1000 MiB") {
		t.Fatalf("err %v", err)
	}
	if f.loadCount("new") != 0 {
		t.Fatal("loaded past the cgroup limit")
	}
	m, f = setup(8000)
	if err := m.Load(ctx, "new"); err != nil || f.loadCount("new") != 1 {
		t.Fatalf("room enough: err %v", err)
	}
	m, f = setup(-1)
	if err := m.Load(ctx, "new"); err != nil || f.loadCount("new") != 1 {
		t.Fatalf("no cgroup limit: err %v", err)
	}
}

func TestLMSGPUOverBudget(t *testing.T) {
	m := ModelInfo{SizeMiB: 16921, Ctx: 32768, MaxParallel: 1, plan: &loadPlan{FreeMiB: 18385, NeedMiB: 22086}}
	for _, tc := range []struct {
		gpu  string
		over bool
	}{{"", false}, {"off", false}, {"max", true}, {"0.95", true}, {"0.5", false}, {"nope", false}} {
		if over, _ := lmsGPUOverBudget(m, tc.gpu); over != tc.over {
			t.Errorf("gpu %q: over %v, want %v", tc.gpu, over, tc.over)
		}
	}
	unknown := m
	unknown.plan = &loadPlan{FreeMiB: -1}
	if over, _ := lmsGPUOverBudget(unknown, "max"); over {
		t.Error("unknown VRAM warned")
	}
}

// A stale llmster-pid.lock whose pid now belongs to another process is not
// LM Studio's daemon (contract-engine-3): daemonUp, cgroupRoom and
// ownEngineUnits must not read that process.
func TestLLMsterPIDChecksTheProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lmstudio", ".internal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "llmster-pid.lock"), []byte("2622\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := procName
	t.Cleanup(func() { procName = old })
	cases := []struct {
		comm, exe string
		ok        bool
		want      int
	}{
		{"llmster", "/home/u/.lmstudio/llmster/0.0.20-1/llmster", true, 2622},
		{"llmster", "", true, 2622}, // exe unreadable
		{"", "/home/u/.lmstudio/llmster/0.0.20-1/llmster", true, 2622},
		{"bash", "/usr/bin/bash", true, 0}, // the pid was reused
		{"llama-server", "/usr/bin/llama-server", true, 0},
		{"", "", false, 0},   // no such process
		{"", "", true, 2622}, // off Linux: unknown, trusted
	}
	for _, tc := range cases {
		procName = func(int) (string, string, bool) { return tc.comm, tc.exe, tc.ok }
		if got := llmsterPID(); got != tc.want {
			t.Errorf("comm %q exe %q ok %v: pid %d, want %d", tc.comm, tc.exe, tc.ok, got, tc.want)
		}
	}
}
