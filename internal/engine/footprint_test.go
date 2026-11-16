package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func TestFootprintStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), footprintFile)
	file := func() string { return path }
	key := footprintKey(BackendLMStudio, "q", 32768, 1)

	f := newFootprints(file)
	if _, ok := f.need(key); ok {
		t.Fatal("learned with no file")
	}
	for _, n := range []int{9000, 8000, 8500, 9500} {
		f.add(key, n, time.Unix(100, 0))
	}
	// The last three are 8000, 8500, 9500: the median wins.
	if n, ok := f.need(key); !ok || n != 8500+headroomMiB {
		t.Fatalf("need %d %v", n, ok)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file not written private: %v", err)
	}

	again := newFootprints(file) // a restart
	if n, ok := again.need(key); !ok || n != 8500+headroomMiB {
		t.Fatalf("after restart need %d %v", n, ok)
	}
	// One sample poisoned low (memory freed outside Cerberus during the
	// load) does not plan the next loads short (contract-engine-7).
	again.add(key, 4000, time.Unix(200, 0)) // 8500, 9500, 4000
	if n, _ := again.need(key); n != 8500+headroomMiB {
		t.Fatalf("need %d: one low sample won", n)
	}
	two := newFootprints(nil)
	two.add(key, 9000, time.Unix(1, 0))
	two.add(key, 6000, time.Unix(2, 0))
	if n, _ := two.need(key); n != 9000+headroomMiB {
		t.Fatalf("two samples: need %d, want the larger", n)
	}

	for name, body := range map[string]string{"corrupt": "{nope", "other version": `{"v":2,"models":{"` + key + `":{"samples":[1000]}}}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := newFootprints(file).need(key); ok {
			t.Fatalf("%s file was trusted", name)
		}
	}
	mem := newFootprints(nil)
	mem.add(key, 5000, time.Unix(1, 0))
	if n, ok := mem.need(key); !ok || n != 5000+headroomMiB {
		t.Fatalf("memory store: %d %v", n, ok)
	}
}

func TestFootprintSample(t *testing.T) {
	cases := []struct {
		used     int
		share    float64
		size     int
		estimate int
		want     int
		ok       bool
	}{
		{20000, 1, 17000, 22000, 20000, true},
		{10000, 0.5, 17000, 22000, 20000, true}, // a split: scaled to the whole model
		{300, 1, 17000, 22000, 0, false},        // already loaded, or the desktop freed memory
		{16000, 1, 17000, 22000, 0, false},      // under the weights: something else freed VRAM meanwhile
		{8400, 0.5, 17000, 22000, 0, false},     // the same, scaled from a split
		{60000, 1, 17000, 22000, 0, false},      // something else loaded meanwhile
		{-5, 1, 17000, 22000, 0, false},
		{10000, 0, 17000, 22000, 0, false},
	}
	for _, tc := range cases {
		got, ok := footprintSample(tc.used, tc.share, tc.size, tc.estimate)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%+v: got %d %v", tc, got, ok)
		}
	}
}

// A load measures its footprint; the next load of the same shape plans
// with it instead of the estimate.
func TestFootprintLearnedAndUsed(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.VRAMReserveMiB = -1
	cfg.Models = []config.ModelConfig{{ID: "q", SizeMiB: 5000, Ctx: 8192}}
	f := &planFake{fakeBackend: newFake(BackendLMStudio, true, "q", "other"), slots: 1}
	m, v := planManager(cfg, 100000, 24564, nil, f)
	v.onRefresh = func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.loaded["q"] {
			v.free.Store(20000 - 7100)
		} else {
			v.free.Store(20000)
		}
	}
	if err := m.Load(ctx, "q"); err != nil {
		t.Fatal(err)
	}
	est := estimateNeedMiB(5000, 8192, 1)
	if got := f.last(t).plan.NeedMiB; got != est {
		t.Fatalf("first plan %d, want the estimate %d", got, est)
	}
	if err := m.Unload(ctx, "q"); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(ctx, "q"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(t).plan.NeedMiB; got != 7100+headroomMiB {
		t.Fatalf("second plan %d, want the learned %d", got, 7100+headroomMiB)
	}
}

// A sample is dropped when another model was unloaded during the load.
func TestFootprintSkippedWhenAnotherUnloadRan(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.VRAMReserveMiB = -1
	cfg.Models = []config.ModelConfig{{ID: "q", SizeMiB: 5000, Ctx: 8192}}
	f := &planFake{fakeBackend: newFake(BackendLMStudio, true, "q", "other"), slots: 1}
	m, v := planManager(cfg, 100000, 24564, nil, f)
	if err := m.Load(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	v.onRefresh = func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.loaded["q"] {
			v.free.Store(20000 - 7100)
		} else {
			v.free.Store(20000)
		}
	}
	f.onLoad = func(info ModelInfo) {
		if info.ID == "q" {
			if err := m.Unload(ctx, "other"); err != nil {
				t.Error(err)
			}
		}
	}
	if err := m.Load(ctx, "q"); err != nil {
		t.Fatal(err)
	}
	ctxLen, slots := planShape(f.last(t), 1)
	if _, ok := m.footprints.need(footprintKey(BackendLMStudio, "q", ctxLen, slots)); ok {
		t.Fatal("learned from a load that overlapped an unload")
	}
}

// A load that had to evict first is not sampled: the evicted engine may
// still be handing its memory back.
func TestFootprintSkippedAfterEviction(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Engines.VRAMReserveMiB = -1
	cfg.Models = []config.ModelConfig{{ID: "q", SizeMiB: 5000, Ctx: 8192}, {ID: "other", SizeMiB: 5000}}
	f := &planFake{fakeBackend: newFake(BackendLMStudio, true, "q", "other"), slots: 1}
	m, v := planManager(cfg, 100000, 24564, nil, f)
	if err := m.Load(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	v.onRefresh = func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.loaded["q"]:
			v.free.Store(1000)
		case f.loaded["other"]:
			v.free.Store(3000) // q does not fit until other goes
		default:
			v.free.Store(8100)
		}
	}
	if err := m.Load(ctx, "q"); err != nil {
		t.Fatal(err)
	}
	if got := f.unloaded(); len(got) != 1 || got[0] != "other" {
		t.Fatalf("unloaded %v, want [other]", got)
	}
	if _, ok := m.footprints.need(footprintKey(BackendLMStudio, "q", 8192, 1)); ok {
		t.Fatal("learned from a load that evicted first")
	}
}
