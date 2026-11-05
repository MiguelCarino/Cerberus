package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeBackend is an in-memory Backend.
type fakeBackend struct {
	name      string
	local     bool
	loadDelay time.Duration
	failLoad  error

	mu       sync.Mutex
	models   []ModelInfo
	loaded   map[string]bool
	loads    map[string]int
	unloads  []string
	lists    int
	stopAlls int
}

func newFake(name string, local bool, ids ...string) *fakeBackend {
	f := &fakeBackend{name: name, local: local, loaded: map[string]bool{}, loads: map[string]int{}}
	for _, id := range ids {
		f.models = append(f.models, ModelInfo{ID: id, Backend: name, Name: id, Local: local, SizeMiB: 1000})
	}
	return f
}

func (f *fakeBackend) Name() string    { return f.name }
func (f *fakeBackend) Available() bool { return true }
func (f *fakeBackend) List(context.Context) ([]ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	out := slices.Clone(f.models)
	for i := range out {
		out[i].Loaded = f.loaded[out[i].ID]
	}
	return out, nil
}
func (f *fakeBackend) Load(ctx context.Context, m ModelInfo) (Endpoint, error) {
	if f.loadDelay > 0 {
		time.Sleep(f.loadDelay)
	}
	if f.failLoad != nil {
		return Endpoint{}, f.failLoad
	}
	f.mu.Lock()
	f.loads[m.ID]++
	f.loaded[m.ID] = true
	f.mu.Unlock()
	return f.Endpoint(m), nil
}
func (f *fakeBackend) Unload(ctx context.Context, m ModelInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unloads = append(f.unloads, m.ID)
	delete(f.loaded, m.ID)
	return nil
}
func (f *fakeBackend) Endpoint(m ModelInfo) Endpoint {
	return Endpoint{BaseURL: "http://" + f.name + "/v1", Model: modelName(m), Local: f.local, Backend: f.name}
}
func (f *fakeBackend) loadCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads[id]
}
func (f *fakeBackend) unloaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.unloads)
}

// fakeSpawner is a fakeBackend that owns processes.
type fakeSpawner struct{ *fakeBackend }

func (f fakeSpawner) StopAll(context.Context) {
	f.mu.Lock()
	f.stopAlls++
	f.loaded = map[string]bool{}
	f.mu.Unlock()
}

type fakeVRAM struct {
	free      atomic.Int64
	active    atomic.Bool
	refreshes atomic.Int32
	onRefresh func() // optional: what a fresh query sees
}

func (v *fakeVRAM) FreeVRAMMiB() int      { return int(v.free.Load()) }
func (v *fakeVRAM) SetActive(active bool) { v.active.Store(active) }
func (v *fakeVRAM) Refresh(context.Context) {
	v.refreshes.Add(1)
	if v.onRefresh != nil {
		v.onRefresh()
	}
}

func testManager(cfg *config.Config, free int, bs ...Backend) (*Manager, *fakeVRAM) {
	if cfg == nil {
		cfg = config.Defaults()
	}
	v := &fakeVRAM{}
	v.free.Store(int64(free))
	m := newManager(cfg, v, quietLog(), bs)
	m.memAvail = func() int { return 0 } // tests never depend on this machine's RAM
	return m, v
}

func TestResolve(t *testing.T) {
	cfg := config.Defaults()
	cfg.Engines.DefaultModel = "qwen3-8b"
	local := newFake("lmstudio", true, "qwen/qwen3-8b", "qwen/qwen3-32b", "gemma-3-4b")
	cloud := newFake("openai", false, "openrouter/claude-x", "openrouter/gpt-y")
	m, _ := testManager(cfg, -1, local, cloud)
	tests := []struct {
		in, want string
		err      bool
	}{
		{"qwen/qwen3-8b", "qwen/qwen3-8b", false},
		{"qwen3-32b", "qwen/qwen3-32b", false}, // unique suffix
		{"gemma", "gemma-3-4b", false},         // unique prefix
		{"qwen", "", true},                     // ambiguous prefix
		{"default", "qwen/qwen3-8b", false},    // alias → suffix match
		{"", "qwen/qwen3-8b", false},           // empty means default
		{"openrouter/gpt-y", "openrouter/gpt-y", false},
		{"nope", "", true},
	}
	for _, tc := range tests {
		got, err := m.Resolve(context.Background(), tc.in)
		if tc.err {
			if !errors.Is(err, ErrUnknownModel) {
				t.Errorf("Resolve(%q) err = %v, want ErrUnknownModel", tc.in, err)
			}
			continue
		}
		if err != nil || got.ID != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.in, got.ID, err, tc.want)
		}
	}
}

func TestModelsCacheAndOverrides(t *testing.T) {
	cfg := config.Defaults()
	cfg.Models = []config.ModelConfig{
		{ID: "a", SizeMiB: 4321, Ctx: 8192, Vision: true, Name: "native-a"},
		{ID: "b", Backend: "ollama", SizeMiB: 1}, // wrong backend: ignored
	}
	f := newFake("lmstudio", true, "a", "b")
	dup := newFake("ollama", true, "a") // duplicate ID: first backend wins
	m, _ := testManager(cfg, -1, f, dup)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }

	ms := m.Models(context.Background())
	if len(ms) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(ms), ms)
	}
	a := ms[0]
	if a.Backend != "lmstudio" || a.SizeMiB != 4321 || a.Ctx != 8192 || !a.Vision || a.Name != "native-a" {
		t.Errorf("override not applied: %+v", a)
	}
	if ms[1].SizeMiB != 1000 {
		t.Errorf("override with other backend applied: %+v", ms[1])
	}
	m.Models(context.Background())
	if f.lists != 1 {
		t.Errorf("lists = %d within cache TTL, want 1", f.lists)
	}
	now = now.Add(6 * time.Second)
	m.Models(context.Background())
	if f.lists != 2 {
		t.Errorf("lists = %d after TTL, want 2", f.lists)
	}
}

func TestAcquireConcurrentLoadsOnce(t *testing.T) {
	f := newFake("llamacpp", true, "m1")
	f.loadDelay = 50 * time.Millisecond
	m, v := testManager(nil, -1, f)
	var wg sync.WaitGroup
	releases := make([]func(), 8)
	for i := range releases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep, rel, err := m.Acquire(context.Background(), "m1")
			if err != nil || ep.BaseURL == "" {
				t.Errorf("Acquire: %v %+v", err, ep)
				return
			}
			releases[i] = rel
		}()
	}
	wg.Wait()
	if n := f.loadCount("m1"); n != 1 {
		t.Fatalf("loaded %d times, want 1", n)
	}
	if got := m.Models(context.Background())[0]; got.InFlight != 8 || !got.Loaded {
		t.Fatalf("after acquire: %+v", got)
	}
	if !v.active.Load() {
		t.Error("gpu not set active while loaded")
	}
	for _, rel := range releases {
		rel()
		rel() // idempotent
	}
	got := m.Models(context.Background())[0]
	if got.InFlight != 0 || got.LastUsed.IsZero() {
		t.Fatalf("after release: %+v", got)
	}
}

func TestAcquireLoadErrorReleasesInFlight(t *testing.T) {
	f := newFake("llamacpp", true, "m1")
	f.failLoad = errors.New("boom")
	m, v := testManager(nil, -1, f)
	if _, _, err := m.Acquire(context.Background(), "m1"); err == nil {
		t.Fatal("want error")
	}
	if got := m.Models(context.Background())[0]; got.InFlight != 0 || got.Loaded {
		t.Fatalf("state after failed load: %+v", got)
	}
	if v.active.Load() {
		t.Error("gpu left active")
	}
}

func TestCloudAcquireDoesNotLoad(t *testing.T) {
	c := newFake("openai", false, "prov/x")
	m, v := testManager(nil, 100, c)
	ep, rel, err := m.Acquire(context.Background(), "prov/x")
	if err != nil || ep.Local {
		t.Fatalf("Acquire: %v %+v", err, ep)
	}
	defer rel()
	if c.loadCount("prov/x") != 0 {
		t.Error("cloud model was loaded")
	}
	if v.active.Load() {
		t.Error("cloud request made the GPU active")
	}
}

func TestMakeRoom(t *testing.T) {
	ctx := context.Background()
	setup := func(overcommit bool) (*Manager, *fakeBackend, *fakeVRAM, *time.Time) {
		cfg := config.Defaults()
		cfg.Engines.AllowOverCommit = overcommit
		cfg.Engines.WaitForRoomSec = 0 // the no-wait rules; waiting is TestWaitForRoom
		cfg.Models = []config.ModelConfig{{ID: "new", SizeMiB: 5000}}
		f := newFake("llamacpp", true, "old", "mid", "busy", "new")
		m, v := testManager(cfg, 100000, f)
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
		return m, f, v, &now
	}

	t.Run("evicts LRU until it fits", func(t *testing.T) {
		m, f, v, _ := setup(false)
		// need = 5000*1.15+512 = 6262; free 5300 + old(1000) fits.
		v.free.Store(5300)
		if err := m.Load(ctx, "new"); err != nil {
			t.Fatal(err)
		}
		if got := f.unloaded(); !slices.Equal(got, []string{"old"}) {
			t.Fatalf("unloaded %v, want [old]", got)
		}
	})
	t.Run("short on VRAM but fits with RAM: loads (engine spills to RAM)", func(t *testing.T) {
		m, f, v, _ := setup(false)
		v.free.Store(4000)                       // need 6262; even evicting old+mid is not enough
		m.memAvail = func() int { return 16000 } // but system RAM covers the rest
		if err := m.Load(ctx, "new"); err != nil {
			t.Fatal(err)
		}
		if f.loadCount("new") != 1 {
			t.Fatal("not loaded")
		}
		if slices.Contains(f.unloaded(), "busy") {
			t.Fatal("unloaded an in-flight model")
		}
	})
	t.Run("never evicts in-flight; errors when VRAM+RAM cannot hold it", func(t *testing.T) {
		m, f, v, _ := setup(false)
		v.free.Store(0)
		err := m.Load(ctx, "new")
		if !errors.Is(err, ErrNoRoom) {
			t.Fatalf("err = %v, want ErrNoRoom", err)
		}
		if slices.Contains(f.unloaded(), "busy") {
			t.Fatal("unloaded an in-flight model")
		}
	})
	t.Run("overcommit tries anyway", func(t *testing.T) {
		m, f, v, _ := setup(true)
		v.free.Store(0)
		if err := m.Load(ctx, "new"); err != nil {
			t.Fatal(err)
		}
		if f.loadCount("new") != 1 {
			t.Fatal("not loaded")
		}
	})
	t.Run("queries VRAM afresh before deciding", func(t *testing.T) {
		m, f, v, _ := setup(false)
		v.free.Store(100000)                        // a reading taken before "mid" finished loading
		v.onRefresh = func() { v.free.Store(5300) } // what the GPU really has now
		if err := m.Load(ctx, "new"); err != nil {
			t.Fatal(err)
		}
		if v.refreshes.Load() == 0 {
			t.Fatal("makeRoom trusted the polled reading")
		}
		if got := f.unloaded(); !slices.Equal(got, []string{"old"}) {
			t.Fatalf("unloaded %v, want [old]", got)
		}
	})
	t.Run("unknown VRAM skips the check", func(t *testing.T) {
		m, f, v, _ := setup(false)
		v.free.Store(-1)
		if err := m.Load(ctx, "new"); err != nil || len(f.unloaded()) != 0 {
			t.Fatalf("err %v unloaded %v", err, f.unloaded())
		}
	})
}

func TestReaper(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.IdleTTLSec = 60
	cfg.Models = []config.ModelConfig{{ID: "short", TTLSec: 10}}
	f := newFake("lmstudio", true, "short", "long", "external", "busy")
	f.loaded["external"] = true // loaded by someone else, never used through us
	m, v := testManager(cfg, -1, f)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }

	for _, id := range []string{"short", "long"} {
		_, rel, err := m.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rel()
	}
	_, relBusy, err := m.Acquire(ctx, "busy")
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(30 * time.Second)
	m.reap(ctx)
	if got := f.unloaded(); !slices.Equal(got, []string{"short"}) {
		t.Fatalf("after 30s unloaded %v, want [short]", got)
	}
	now = now.Add(5 * time.Minute)
	m.reap(ctx)
	got := f.unloaded()
	if !slices.Contains(got, "long") || slices.Contains(got, "busy") || slices.Contains(got, "external") {
		t.Fatalf("after 5m unloaded %v", got)
	}
	relBusy()
	now = now.Add(5 * time.Minute)
	m.reap(ctx)
	if !slices.Contains(f.unloaded(), "busy") {
		t.Fatal("busy not reaped after release + TTL")
	}
	m.updateActive()
	if v.active.Load() {
		t.Error("an externally loaded model alone must not keep fast GPU polling")
	}
	if m.anyReapable() {
		t.Error("only the external model is loaded: nothing for the reaper to do")
	}
}

func TestActiveTail(t *testing.T) {
	ctx := context.Background()
	f := newFake("lmstudio", true, "m")
	m, v := testManager(nil, -1, f)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	_, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !v.active.Load() {
		t.Fatal("not active while a local request is in flight")
	}
	rel()
	if !v.active.Load() {
		t.Fatal("tail: should stay active right after the request")
	}
	now = now.Add(activeTail + time.Second)
	m.updateActive()
	if v.active.Load() {
		t.Fatal("still active after the tail with the model merely loaded")
	}
}

func TestStaleListingKeepsOurLoad(t *testing.T) {
	ctx := context.Background()
	f := newFake("lmstudio", true, "x", "y")
	f.loaded["y"] = true
	m, _ := testManager(nil, -1, f)
	m.Models(ctx)
	before := m.listAll(ctx) // x unloaded, y loaded
	gens := m.generations()
	if err := m.Load(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if err := m.Unload(ctx, "y"); err != nil {
		t.Fatal(err)
	}
	m.apply(before, gens)
	m.mu.Lock()
	xl, yl := m.states["x"].loaded, m.states["y"].loaded
	m.mu.Unlock()
	if !xl || yl {
		t.Fatalf("stale listing overwrote our state: x loaded=%v y loaded=%v", xl, yl)
	}
	m.apply(m.listAll(ctx), m.generations()) // a fresh listing applies as usual
	m.mu.Lock()
	xl, yl = m.states["x"].loaded, m.states["y"].loaded
	m.mu.Unlock()
	if !xl || yl {
		t.Fatalf("fresh listing: x loaded=%v y loaded=%v", xl, yl)
	}
}

func TestUnloadExplicit(t *testing.T) {
	f := newFake("ollama", true, "x")
	f.loaded["x"] = true
	m, v := testManager(nil, -1, f)
	if err := m.Unload(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if got := m.Models(context.Background())[0]; got.Loaded {
		t.Fatalf("still loaded: %+v", got)
	}
	if v.active.Load() {
		t.Error("gpu still active")
	}
	if err := m.Unload(context.Background(), "missing"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v", err)
	}
}

func TestShutdownOnlyStopsSpawned(t *testing.T) {
	ctx := context.Background()
	sp := fakeSpawner{newFake("llamacpp", true, "own")}
	ext := newFake("lmstudio", true, "ext")
	m, _ := testManager(nil, -1, sp, ext)
	for _, id := range []string{"own", "ext"} {
		if err := m.Load(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	m.Shutdown(ctx)
	m.Shutdown(ctx) // idempotent
	if sp.stopAlls != 1 {
		t.Fatalf("StopAll called %d times", sp.stopAlls)
	}
	if len(ext.unloaded()) != 0 {
		t.Fatal("external model unloaded on shutdown")
	}
	for _, mi := range m.Models(ctx) {
		if mi.ID == "own" && mi.Loaded {
			t.Fatal("spawned model still marked loaded")
		}
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	sp := fakeSpawner{newFake("llamacpp", true, "own")}
	m, _ := testManager(nil, -1, sp)
	m.reapEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	if err := m.Load(ctx, "own"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let a few reaper ticks run
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.stopAlls != 1 {
		t.Fatalf("StopAll = %d, want 1", sp.stopAlls)
	}
}
