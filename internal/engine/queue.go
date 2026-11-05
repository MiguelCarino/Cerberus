package engine

import (
	"context"
	"fmt"
	"time"

	"carino.systems/cerberus/internal/gpu"
)

// Per-model request queue (SCOPE "As built: model queue and room").
//
// A local model admits at most limitLocked(st) requests at once; further
// callers wait in FIFO order in Acquire instead of being sent to an engine
// that would run out of memory serving them all together. A slot is held
// from admission (before loading) until the release func runs, and is handed
// straight to the oldest waiter on release, so the queue stays FIFO and the
// model never looks idle (evictable) between two queued requests.
//
// Lock ordering: the queue lives under Manager.mu only. A waiter blocks on
// its own channel with no lock held, so it never holds loadMu or roomMu while
// queued, and releasing a slot never needs loadMu or roomMu.

// waiter is one request queued for a slot. granted is set (under m.mu) when
// a slot was handed to it; ch is closed at the same moment.
type waiter struct {
	ch      chan struct{}
	granted bool
}

// slotter is implemented by local backends that know how many requests one
// loaded model serves in parallel (llama.cpp -np, LM Studio --parallel).
type slotter interface {
	Slots(m ModelInfo) int
}

// vramTotaler is implemented by VRAM sources that know the total VRAM.
type vramTotaler interface {
	TotalVRAMMiB() int
}

// gpuVRAM adapts *gpu.Monitor, adding the total from its snapshot.
type gpuVRAM struct{ *gpu.Monitor }

func (g gpuVRAM) TotalVRAMMiB() int {
	total := 0
	for _, d := range g.Snapshot().GPUs {
		if d.MemTotalMiB <= 0 {
			return -1
		}
		total += d.MemTotalMiB
	}
	if total == 0 {
		return -1
	}
	return total
}

// totalRetry is how often an unknown total VRAM is asked again.
const totalRetry = 30 * time.Second

// primeTotal learns the total VRAM once (it does not change) so that
// limitLocked can read it under m.mu without any I/O. Call it without m.mu.
func (m *Manager) primeTotal() {
	if m.totalMiB.Load() > 0 {
		return
	}
	tv, ok := m.vram.(vramTotaler)
	if !ok {
		return
	}
	now := time.Now().UnixNano()
	if last := m.totalTried.Load(); last != 0 && time.Duration(now-last) < totalRetry {
		return
	}
	m.totalTried.Store(now)
	if t := tv.TotalVRAMMiB(); t > 0 {
		m.totalMiB.Store(int64(t))
	}
}

// limitLocked is how many requests st admits at once (0 = no limit, for
// cloud models and engines that do not report slots). It is
// models[].max_parallel when set, else 1 for a model larger than half the
// total VRAM (two requests' KV caches on top of it are what ran the GPU out
// of memory), else the engine's slot count; minus one per crash (min 1).
// Caller holds m.mu.
func (m *Manager) limitLocked(st *modelState) int {
	if !st.info.Local {
		return 0
	}
	base := 0
	if mc := modelConfig(m.conf(), st.info.ID); mc != nil && mc.MaxParallel > 0 {
		base = mc.MaxParallel
	} else {
		total := int(m.totalMiB.Load())
		switch s, ok := st.backend.(slotter); {
		case total > 0 && st.info.SizeMiB*2 > total:
			base = 1
		case ok:
			base = max(s.Slots(st.info), 1)
		}
	}
	if base == 0 {
		return 0
	}
	return max(base-st.crashCut, 1)
}

// admit takes a request slot of st, waiting in FIFO order while all slots
// are busy. It gives up when ctx ends. The wait hook hears the reason when
// waiting starts and "" when it ends.
func (m *Manager) admit(ctx context.Context, st *modelState) error {
	m.mu.Lock()
	lim := m.limitLocked(st)
	if lim == 0 || (st.active < lim && len(st.waiters) == 0) {
		st.active++
		m.mu.Unlock()
		return nil
	}
	w := &waiter{ch: make(chan struct{})}
	st.waiters = append(st.waiters, w)
	reason := fmt.Sprintf("waiting for %s: %d of %d slots busy", st.info.ID, st.active, lim)
	m.mu.Unlock()

	hook := waitHook(ctx)
	hook(reason)
	defer hook("")
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.granted {
		// The slot arrived as we gave up: pass it on.
		m.releaseSlotLocked(st)
	} else {
		for i, x := range st.waiters {
			if x == w {
				st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
				break
			}
		}
	}
	return ctx.Err()
}

// releaseSlotLocked frees one slot of st and hands free slots to waiters,
// oldest first. Caller holds m.mu.
func (m *Manager) releaseSlotLocked(st *modelState) {
	if st.active > 0 {
		st.active--
	}
	m.dispatchLocked(st)
}

// dispatchLocked grants slots to waiters while the limit allows.
func (m *Manager) dispatchLocked(st *modelState) {
	lim := m.limitLocked(st)
	for len(st.waiters) > 0 && (lim == 0 || st.active < lim) {
		w := st.waiters[0]
		st.waiters[0] = nil
		st.waiters = st.waiters[1:]
		w.granted = true
		st.active++
		close(w.ch)
	}
}

// broadcastLocked wakes everything waiting for room (a local model became
// idle, crashed or was unloaded). Caller holds m.mu.
func (m *Manager) broadcastLocked() {
	if m.relCh != nil {
		close(m.relCh)
	}
	m.relCh = make(chan struct{})
}

// busyLocal lists the loaded local models (other than except) holding
// request slots, and the channel that closes on the next release. Both are
// taken under one lock, so a release after this call is never missed.
func (m *Manager) busyLocal(except *modelState) ([]string, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, id := range m.order {
		st := m.states[id]
		if st != except && st.info.Local && st.loaded && (st.active > 0 || st.inFlight > 0) {
			ids = append(ids, id)
		}
	}
	if m.relCh == nil {
		m.relCh = make(chan struct{})
	}
	return ids, m.relCh
}

// MarkCrashed records that a local model's engine failed while serving (a
// crash, "Channel Error", a dropped connection): the model counts as not
// loaded, so the next Acquire unloads what is left and loads it afresh, and
// its request limit drops by one (min 1) for the rest of the daemon's life.
// Several requests failing from one crash lower it once. It reports whether
// id named a local model.
func (m *Manager) MarkCrashed(id string) bool { return m.markCrashed(id, 0, false) }

// MarkCrashedGen is MarkCrashed for a request that was served by the load
// gen (its Endpoint.Gen). A failure reported against an older load (a
// request of the crashed engine that only came back after the model was
// reloaded) changes nothing: the fresh engine is fine and that crash was
// already counted. It reports whether id named a local model.
func (m *Manager) MarkCrashedGen(id string, gen uint64) bool { return m.markCrashed(id, gen, true) }

func (m *Manager) markCrashed(id string, gen uint64, checkGen bool) bool {
	st := m.match(id)
	if st == nil {
		return false
	}
	m.mu.Lock()
	if !st.info.Local {
		m.mu.Unlock()
		return false
	}
	if checkGen && gen != st.loadSeq {
		m.mu.Unlock()
		m.log.Debug("crash report of an earlier load ignored", "model", st.info.ID, "gen", gen)
		return true
	}
	before := m.limitLocked(st)
	first := !st.crashed
	if first && before > 1 {
		st.crashCut++
	}
	after := m.limitLocked(st)
	st.crashed = true
	st.setLoadedLocked(false)
	m.broadcastLocked()
	m.mu.Unlock()
	if first {
		m.log.Warn("local engine failed; the model will be reloaded",
			"model", st.info.ID, "max_parallel_before", before, "max_parallel", after)
	}
	return true
}

// roomWait is how long a load waits for busy models (0: never).
func (m *Manager) roomWait() time.Duration {
	if m.roomWaitOverride != 0 {
		return max(m.roomWaitOverride, 0)
	}
	return time.Duration(max(m.conf().Engines.WaitForRoomSec, 0)) * time.Second
}
