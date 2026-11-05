package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// slotFake is a local fake backend that reports parallel slots.
type slotFake struct {
	*fakeBackend
	slots int
}

func (s slotFake) Slots(ModelInfo) int { return s.slots }

// totalVRAM is a fakeVRAM that also knows the total.
type totalVRAM struct {
	*fakeVRAM
	total int
}

func (t totalVRAM) TotalVRAMMiB() int { return t.total }

// hookLog records wait-hook calls.
type hookLog struct {
	mu    sync.Mutex
	calls []string
}

func (h *hookLog) hook(r string) {
	h.mu.Lock()
	h.calls = append(h.calls, r)
	h.mu.Unlock()
}

func (h *hookLog) get() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

// waitFor polls cond (test helper only) until it holds or 5 s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func modelInfo(m *Manager, id string) ModelInfo {
	info, _ := m.Resolve(context.Background(), id)
	return info
}

func TestQueueLimitsConcurrencyFIFOAndCancel(t *testing.T) {
	cfg := config.Defaults()
	cfg.Models = []config.ModelConfig{{ID: "q", MaxParallel: 1}}
	f := newFake("llamacpp", true, "q")
	m, _ := testManager(cfg, -1, f)
	ctx := context.Background()

	_, rel0, err := m.Acquire(ctx, "q")
	if err != nil {
		t.Fatal(err)
	}
	if got := modelInfo(m, "q"); got.MaxParallel != 1 || got.InFlight != 1 {
		t.Fatalf("info %+v", got)
	}

	var mu sync.Mutex
	var order []int
	rels := make(chan func(), 3)
	hooks := make([]*hookLog, 3)
	for i := range 3 {
		hooks[i] = &hookLog{}
		hctx := WithWaitHook(ctx, hooks[i].hook)
		go func() {
			_, rel, err := m.Acquire(hctx, "q")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			rels <- rel
		}()
		waitFor(t, fmt.Sprintf("waiter %d queued", i), func() bool { return modelInfo(m, "q").Waiting == i+1 })
	}
	// A fourth caller gives up while queued: it leaves the queue.
	cctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { _, _, err := m.Acquire(cctx, "q"); errc <- err }()
	waitFor(t, "canceller queued", func() bool { return modelInfo(m, "q").Waiting == 4 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}
	if w := modelInfo(m, "q").Waiting; w != 3 {
		t.Fatalf("waiting %d after cancel, want 3", w)
	}
	if got := hooks[0].get(); len(got) != 1 || got[0] != "waiting for q: 1 of 1 slots busy" {
		t.Fatalf("hook while queued: %q", got)
	}

	rel0()
	for range 3 {
		select {
		case rel := <-rels:
			if n := modelInfo(m, "q").InFlight; n != 1 {
				t.Fatalf("in flight %d, want 1 (limit)", n)
			}
			rel()
		case <-time.After(5 * time.Second):
			t.Fatal("queue stalled")
		}
	}
	if !slices.Equal(order, []int{0, 1, 2}) {
		t.Fatalf("order %v, want FIFO", order)
	}
	for i, h := range hooks {
		if got := h.get(); len(got) != 2 || got[1] != "" {
			t.Errorf("hook %d calls %q, want reason then \"\"", i, got)
		}
	}
	if got := modelInfo(m, "q"); got.InFlight != 0 || got.Waiting != 0 {
		t.Fatalf("after: %+v", got)
	}
}

func TestDefaultLimit(t *testing.T) {
	cases := []struct {
		name  string
		total int
		size  int
		slots int // 0: backend reports none
		cfgN  int
		want  int
	}{
		{"big model: one at a time", 24000, 17000, 4, 0, 1},
		{"small model: engine slots", 24000, 8000, 4, 0, 4},
		{"unknown VRAM: engine slots", -1, 17000, 4, 0, 4},
		{"max_parallel wins", 24000, 17000, 4, 3, 3},
		{"no slots known: no limit", -1, 1000, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Models = []config.ModelConfig{{ID: "m", SizeMiB: tc.size, MaxParallel: tc.cfgN}}
			f := newFake("lmstudio", true, "m")
			var b Backend = f
			if tc.slots > 0 {
				b = slotFake{f, tc.slots}
			}
			v := &fakeVRAM{}
			v.free.Store(-1)
			m := newManager(cfg, totalVRAM{v, tc.total}, quietLog(), []Backend{b})
			if got := modelInfo(m, "m").MaxParallel; got != tc.want {
				t.Fatalf("limit %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("cloud: no limit", func(t *testing.T) {
		m, _ := testManager(nil, -1, newFake("openai", false, "c"))
		if got := modelInfo(m, "c").MaxParallel; got != 0 {
			t.Fatalf("cloud limit %d", got)
		}
	})
}

// roomSetup: "busy" (1000 MiB) is loaded and held; "new" needs 6262 MiB;
// 5300 MiB are free, so "new" fits only once "busy" is unloaded.
func roomSetup(t *testing.T, wait time.Duration) (*Manager, *fakeBackend, func()) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Models = []config.ModelConfig{{ID: "new", SizeMiB: 5000}}
	f := newFake("llamacpp", true, "busy", "new", "other", "z")
	m, v := testManager(cfg, 100000, f)
	m.roomWaitOverride = wait
	ctx := context.Background()
	_, relOther, err := m.Acquire(ctx, "other") // loaded and busy for the whole test
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relOther)
	_, rel, err := m.Acquire(ctx, "busy")
	if err != nil {
		t.Fatal(err)
	}
	v.free.Store(5300) // + busy's 1000 once evicted = 6300 >= 6262
	return m, f, rel
}

func TestWaitForRoom(t *testing.T) {
	m, f, relBusy := roomSetup(t, time.Minute)
	h := &hookLog{}
	ctx := WithWaitHook(context.Background(), h.hook)
	done := make(chan error, 1)
	go func() {
		_, rel, err := m.Acquire(ctx, "new")
		if err == nil {
			rel()
		}
		done <- err
	}()
	waitFor(t, "room wait reported", func() bool { return len(h.get()) > 0 })
	if got := h.get()[0]; got != "waiting for room: busy, other busy" {
		t.Fatalf("hook reason %q", got)
	}
	if f.loadCount("new") != 0 || len(f.unloaded()) != 0 {
		t.Fatal("loaded or evicted while the other models were busy")
	}
	relBusy()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("load did not proceed after busy released")
	}
	if got := f.unloaded(); !slices.Equal(got, []string{"busy"}) {
		t.Fatalf("unloaded %v, want [busy] (other is still busy)", got)
	}
	if f.loadCount("new") != 1 {
		t.Fatal("target not loaded")
	}
	if got := h.get(); got[len(got)-1] != "" {
		t.Fatalf("hook did not end with \"\": %q", got)
	}
}

func TestWaitForRoomTimeoutAndCancel(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		m, f, _ := roomSetup(t, 50*time.Millisecond)
		_, _, err := m.Acquire(context.Background(), "new")
		if !errors.Is(err, ErrRoomTimeout) || !errors.Is(err, ErrNoRoom) || !IsTransient(err) {
			t.Fatalf("err %v, want a transient ErrRoomTimeout", err)
		}
		if f.loadCount("new") != 0 {
			t.Fatal("loaded anyway")
		}
		if got := modelInfo(m, "new"); got.InFlight != 0 {
			t.Fatalf("slot leaked: %+v", got)
		}
	})
	t.Run("caller cancels", func(t *testing.T) {
		m, _, _ := roomSetup(t, time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, _, err := m.Acquire(ctx, "new")
		if !errors.Is(err, context.DeadlineExceeded) || IsTransient(err) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("waiting off: old spill rule", func(t *testing.T) {
		m, f, _ := roomSetup(t, -1)
		m.memAvail = func() int { return 16000 }
		if _, rel, err := m.Acquire(context.Background(), "new"); err != nil {
			t.Fatal(err)
		} else {
			rel()
		}
		if slices.Contains(f.unloaded(), "busy") {
			t.Fatal("evicted a busy model")
		}
	})
}

// A load waiting for room must not block requests to loaded models, and the
// busy model's queue must drain without needing the waiting load's locks.
func TestQueueAndRoomNoDeadlock(t *testing.T) {
	cfg := config.Defaults()
	cfg.Models = []config.ModelConfig{{ID: "a", MaxParallel: 1}, {ID: "b", SizeMiB: 5000}}
	f := newFake("llamacpp", true, "a", "b", "c")
	m, v := testManager(cfg, 100000, f)
	m.roomWaitOverride = time.Minute
	ctx := context.Background()
	if err := m.Load(ctx, "c"); err != nil { // loaded, idle
		t.Fatal(err)
	}
	_, relA1, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// b needs 6262: both c (evictable) and a (busy) must go. The fake GPU
	// reports what evictions freed, as a real one would.
	v.onRefresh = func() { v.free.Store(int64(4300 + 1000*len(f.unloaded()))) }

	a2 := make(chan func(), 1)
	go func() {
		_, rel, err := m.Acquire(ctx, "a") // queued behind a1
		if err != nil {
			t.Error(err)
		}
		a2 <- rel
	}()
	waitFor(t, "a2 queued", func() bool { return modelInfo(m, "a").Waiting == 1 })

	bDone := make(chan error, 1)
	go func() {
		_, rel, err := m.Acquire(ctx, "b")
		if err == nil {
			rel()
		}
		bDone <- err
	}()
	waitFor(t, "b waiting for room", func() bool { return slices.Contains(f.unloaded(), "c") })

	// b holds roomMu now; a loaded model's request still goes straight through.
	if err := m.Load(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	relA1() // hands a's slot to a2: a stays busy, b keeps waiting
	var relA2 func()
	select {
	case relA2 = <-a2:
	case <-time.After(5 * time.Second):
		t.Fatal("a's queue did not advance while b waited for room")
	}
	select {
	case err := <-bDone:
		t.Fatalf("b proceeded while a was still busy: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	relA2()
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: b never loaded")
	}
	if !slices.Contains(f.unloaded(), "a") || f.loadCount("b") != 1 {
		t.Fatalf("unloaded %v, b loads %d", f.unloaded(), f.loadCount("b"))
	}
}

func TestMarkCrashed(t *testing.T) {
	f := newFake("lmstudio", true, "m")
	cloud := newFake("openai", false, "c")
	m, _ := testManager(nil, -1, slotFake{f, 4}, cloud)
	ctx := context.Background()
	_, rel, err := m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !m.MarkCrashed("m") || !m.MarkCrashed("m") { // two requests failing from one crash
		t.Fatal("MarkCrashed(local) = false")
	}
	rel()
	if got := modelInfo(m, "m"); got.Loaded || got.MaxParallel != 3 {
		t.Fatalf("after crash: loaded %v limit %d, want false 3", got.Loaded, got.MaxParallel)
	}
	_, rel, err = m.Acquire(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if f.loadCount("m") != 2 || !slices.Equal(f.unloaded(), []string{"m"}) {
		t.Fatalf("loads %d unloads %v: want a clean reload", f.loadCount("m"), f.unloaded())
	}
	for range 5 {
		m.MarkCrashed("m")
		_, rel, _ := m.Acquire(ctx, "m")
		rel()
	}
	if got := modelInfo(m, "m").MaxParallel; got != 1 {
		t.Fatalf("limit %d, want floor 1", got)
	}
	if m.MarkCrashed("c") || m.MarkCrashed("nope") {
		t.Fatal("MarkCrashed accepted a cloud or unknown model")
	}
}

func TestLMSLoadArgs(t *testing.T) {
	base := ModelInfo{ID: "qwen/qwen3.8-27b", SizeMiB: 17000, Ctx: 32768, MaxParallel: 1}
	tail := []string{"-y", "--identifier", "qwen/qwen3.8-27b"}
	head := []string{"load", "qwen/qwen3.8-27b", "--context-length", "32768", "--ttl", "600", "--parallel", "1"}

	fits := base
	fits.plan = &loadPlan{FreeMiB: 23000} // need = 19550+512+2125 = 22187
	if got := lmsLoadArgs(fits, 600, ""); !slices.Equal(got, append(slices.Clone(head), tail...)) {
		t.Fatalf("fits: %v", got)
	}
	tight := base
	tight.plan = &loadPlan{FreeMiB: 11093} // half of 22187
	want := append(append(slices.Clone(head), "--gpu", "0.50"), tail...)
	if got := lmsLoadArgs(tight, 600, ""); !slices.Equal(got, want) {
		t.Fatalf("tight: %v", got)
	}
	tiny := base
	tiny.plan = &loadPlan{FreeMiB: 100}
	if got := lmsLoadArgs(tiny, 600, ""); !slices.Contains(got, "0.10") {
		t.Fatalf("clamped low: %v", got)
	}
	if got := lmsLoadArgs(tight, 600, "max"); !slices.Equal(got, append(append(slices.Clone(head), "--gpu", "max"), tail...)) {
		t.Fatalf("explicit gpu: %v", got)
	}
	unknown := base
	unknown.plan = &loadPlan{FreeMiB: -1}
	if got := lmsLoadArgs(unknown, 0, ""); slices.Contains(got, "--gpu") || slices.Contains(got, "--ttl") {
		t.Fatalf("unknown VRAM: %v", got)
	}
	four := base
	four.MaxParallel = 4
	four.plan = &loadPlan{FreeMiB: 23000} // KV for 4x32k = 8500: no longer fits
	if got := lmsLoadArgs(four, 600, ""); !slices.Contains(got, "--gpu") || !slices.Contains(got, "4") {
		t.Fatalf("four slots: %v", got)
	}
}

func TestLMStudioLoadPassesParallelAndGPU(t *testing.T) {
	cfg := config.Defaults()
	// The manager hands Load its queue limit and plan; lmsLoadArgs is what
	// Load runs (TestLMStudioLoad covers the plain path).
	m := ModelInfo{ID: "x", SizeMiB: 1000, Ctx: 4096, MaxParallel: 2, plan: &loadPlan{FreeMiB: 100000}}
	got := lmsLoadArgs(m, int(cfg.IdleTTL(nil)/time.Second), cfg.Engines.LMStudio.GPU)
	if !slices.Contains(got, "--parallel") || slices.Contains(got, "--gpu") {
		t.Fatalf("args %v", got)
	}
}

func TestLlamaNGLAndGGUF(t *testing.T) {
	if n := llamaNGL(17000, 32768, 64, 11093, nil); n != 31 { // 64 x 0.49998
		t.Fatalf("ngl %d, want 31", n)
	}
	if n := llamaNGL(17000, 32768, 64, 100000, nil); n != 0 {
		t.Fatalf("fits: ngl %d", n)
	}
	if n := llamaNGL(17000, 32768, 64, 1000, []string{"--n-gpu-layers=10"}); n != 0 {
		t.Fatalf("user set it: ngl %d", n)
	}
	if n := llamaNGL(17000, 32768, 0, 1000, nil); n != 0 {
		t.Fatalf("unknown layers: ngl %d", n)
	}

	var b bytes.Buffer
	le := binary.LittleEndian
	str := func(s string) { binary.Write(&b, le, uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	binary.Write(&b, le, uint32(3))
	binary.Write(&b, le, uint64(0))
	binary.Write(&b, le, uint64(4))
	str("general.name") // string
	binary.Write(&b, le, uint32(8))
	str("x")
	str("tokenizer.ggml.tokens") // array of strings
	binary.Write(&b, le, uint32(9))
	binary.Write(&b, le, uint32(8))
	binary.Write(&b, le, uint64(2))
	str("a")
	str("bb")
	str("general.architecture")
	binary.Write(&b, le, uint32(8))
	str("qwen3")
	str("qwen3.block_count")
	binary.Write(&b, le, uint32(4))
	binary.Write(&b, le, uint32(64))
	if n, err := readGGUFLayers(&b); n != 64 || err != nil {
		t.Fatalf("layers %d err %v", n, err)
	}
	if n, _ := readGGUFLayers(strings.NewReader("nope")); n != 0 {
		t.Fatal("garbage parsed")
	}
}

func TestIsTransient(t *testing.T) {
	text := []struct {
		status int
		msg    string
		want   bool
	}{
		{400, "terminated", true},
		{400, "Channel Error", true},
		{503, "local engine failed: Channel Error (it will be reloaded)", true},
		{502, "upstream: dial tcp 127.0.0.1:1234: connect: connection refused", true},
		{503, "model_load_failed: lms load: exit status 1", true},
		{503, "not enough free VRAM: x needs about 30000 MiB", false},
		{503, "not enough free VRAM: timed out waiting for busy models to finish: x", true},
		{400, "context length exceeded", false},
		{429, "over budget", false},
		{499, "client cancelled", false},
		{404, "unknown model: x", false},
		{0, "model call failed: read: connection reset by peer", true},
		{0, "unexpected EOF", true},
		{0, "invalid request", false},
	}
	for _, tc := range text {
		if got := IsTransientText(tc.status, tc.msg); got != tc.want {
			t.Errorf("IsTransientText(%d, %q) = %v, want %v", tc.status, tc.msg, got, tc.want)
		}
	}
	errs := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, false},
		{fmt.Errorf("call: %w", context.DeadlineExceeded), false},
		{fmt.Errorf("x: %w", ErrRoomTimeout), true},
		{fmt.Errorf("x: %w", ErrNoRoom), false},
		{errors.New("model call failed (400): terminated"), true},
		{errors.New("model call failed (502): bad gateway"), true},
		{errors.New("model call failed (400): invalid messages"), false},
		{errors.New("model call failed (429): rate limited"), false},
	}
	for _, tc := range errs {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
	// The hook is a no-op without WithWaitHook, and nil is ignored.
	waitHook(context.Background())("x")
	waitHook(WithWaitHook(context.Background(), nil))("x")
}
