package engine

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/platform"
)

const (
	listCacheTTL   = 5 * time.Second
	reapInterval   = 15 * time.Second
	refreshEvery   = time.Minute // background re-list while something is loaded
	listTimeout    = 20 * time.Second
	shutdownGrace  = 30 * time.Second
	headroomMiB    = 512
	headroomFactor = 1.15
	activeTail     = 30 * time.Second // fast GPU polling after the last local request
)

// vramMonitor is the slice of *gpu.Monitor the manager uses (fakeable).
type vramMonitor interface {
	FreeVRAMMiB() int
	SetActive(active bool)
	Refresh(ctx context.Context)
}

// noVRAM is used when no GPU monitor is given: VRAM is unknown.
type noVRAM struct{}

func (noVRAM) FreeVRAMMiB() int { return -1 }
func (noVRAM) SetActive(bool)   {}

func (noVRAM) Refresh(context.Context) {}

// modelState is the manager's view of one model. Fields other than loadMu
// are guarded by Manager.mu.
type modelState struct {
	loadMu   ctxMutex // held while loading or unloading this model
	backend  Backend
	info     ModelInfo
	loaded   bool
	gen      uint64 // bumped whenever we change loaded, so a stale listing can't undo it
	inFlight int
	lastUsed time.Time // last use through Cerberus; zero = never touched by us

	// Request queue (queue.go). active counts the slots held, from
	// admission to release; waiters are queued for one, oldest first.
	active   int
	waiters  []*waiter
	crashCut int    // slots taken off the limit after crashes
	crashed  bool   // failed while serving: unload what is left before the next load
	loadSeq  uint64 // bumped on every load Cerberus makes (Endpoint.Gen)
}

// setLoadedLocked records a load or unload we made. Caller holds Manager.mu.
func (st *modelState) setLoadedLocked(loaded bool) {
	st.loaded = loaded
	st.info.Loaded = loaded
	st.gen++
}

// Manager merges all backends and owns load, unload and idle policy.
type Manager struct {
	cfg      *config.Config
	live     atomic.Pointer[config.Config] // set by Reload; settings read through conf()
	vram     vramMonitor
	log      *slog.Logger
	backends []Backend

	now       func() time.Time
	reapEvery time.Duration
	memAvail  func() int // MiB of free system RAM (-1 unknown); platform.MemAvailableMiB

	refreshMu sync.Mutex // one listing at a time
	roomMu    ctxMutex   // local loads (and the room made for them) one at a time
	wake      chan struct{}
	stopOnce  sync.Once

	// Room waiting and queue limits (queue.go).
	roomWaitOverride time.Duration // tests: >0 replaces engines.wait_for_room_sec, <0 disables
	totalMiB         atomic.Int64  // total VRAM once known
	totalTried       atomic.Int64  // unix ns of the last attempt to learn it

	mu           sync.Mutex
	relCh        chan struct{} // closed (and replaced) when a local model frees a slot or memory
	states       map[string]*modelState
	order        []string // model IDs in listing order
	listed       time.Time
	lastLocalEnd time.Time   // when the last local request through us ended
	tailTimer    *time.Timer // ends the fast-poll tail after lastLocalEnd
}

// Option configures NewManager.
type Option func(*managerOptions)

type managerOptions struct {
	secrets func(name string) string
}

// WithSecrets gives providers a key look-up (the vault). A provider's key is
// taken from KeyEnv, then secrets(KeySecret or Name), then config.Secret.
func WithSecrets(lookup func(name string) string) Option {
	return func(o *managerOptions) { o.secrets = lookup }
}

// NewManager builds one backend per enabled engine and provider.
func NewManager(cfg *config.Config, mon *gpu.Monitor, log *slog.Logger, opts ...Option) *Manager {
	if log == nil {
		log = slog.Default()
	}
	var o managerOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	var bs []Backend
	if cfg.Engines.LlamaCpp.Enabled {
		bs = append(bs, newLlamaCpp(cfg, log))
	}
	if cfg.Engines.LMStudio.Enabled {
		bs = append(bs, newLMStudio(cfg, log))
	}
	if cfg.Engines.Ollama.Enabled {
		bs = append(bs, newOllama(cfg, log))
	}
	for _, p := range cfg.Providers {
		if p.Enabled {
			b := newOpenAI(cfg, p, log)
			b.secrets = o.secrets
			bs = append(bs, b)
		}
	}
	var v vramMonitor = noVRAM{}
	if mon != nil {
		v = gpuVRAM{mon}
	}
	return newManager(cfg, v, log, bs)
}

func newManager(cfg *config.Config, v vramMonitor, log *slog.Logger, bs []Backend) *Manager {
	return &Manager{cfg: cfg, vram: v, log: log, backends: bs,
		now: time.Now, reapEvery: reapInterval, memAvail: platform.MemAvailableMiB,
		wake: make(chan struct{}, 1), states: map[string]*modelState{}}
}

// Models returns every model of every available backend (cached ≤5 s).
func (m *Manager) Models(ctx context.Context) []ModelInfo {
	m.ensureFresh(ctx, false)
	m.primeTotal()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelInfo, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.view(m.states[id]))
	}
	return out
}

// view is the public picture of st. Caller holds m.mu.
func (m *Manager) view(st *modelState) ModelInfo {
	info := st.info
	info.Loaded = st.loaded
	info.InFlight = st.inFlight
	info.MaxParallel = m.limitLocked(st)
	info.Waiting = len(st.waiters)
	if len(st.waiters) > 0 && st.waiters[0] != nil && !st.waiters[0].since.IsZero() {
		info.LongestWaitS = time.Since(st.waiters[0].since).Seconds()
	}
	if st.lastUsed.After(info.LastUsed) {
		info.LastUsed = st.lastUsed
	}
	return info
}

// Resolve finds a model by ID: exact, unique suffix or prefix, then the
// alias "default" (Engines.DefaultModel).
func (m *Manager) Resolve(ctx context.Context, id string) (ModelInfo, error) {
	st, err := m.resolve(ctx, id)
	if err != nil {
		return ModelInfo{}, err
	}
	m.primeTotal()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.view(st), nil
}

func (m *Manager) resolve(ctx context.Context, id string) (*modelState, error) {
	if id == "" {
		id = "default"
	}
	m.ensureFresh(ctx, false)
	if st := m.match(id); st != nil {
		return st, nil
	}
	m.ensureFresh(ctx, true) // maybe a model appeared since the last listing
	if st := m.match(id); st != nil {
		return st, nil
	}
	if id == "default" {
		return nil, fmt.Errorf("%w: default (set engines.default_model, or pick a model)", ErrUnknownModel)
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownModel, id)
}

func (m *Manager) match(id string) *modelState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.matchLocked(id); st != nil {
		return st
	}
	if id != "default" {
		return nil
	}
	if def := m.conf().Engines.DefaultModel; def != "" && def != "default" {
		return m.matchLocked(def)
	}
	return m.implicitDefaultLocked()
}

// implicitDefaultLocked stands in for an unset engines.default_model (a
// fresh install): the only loaded model, else the only model there is.
// With more than one candidate there is no guess and "default" stays
// unknown.
func (m *Manager) implicitDefaultLocked() *modelState {
	var loaded, only *modelState
	nLoaded := 0
	for _, id := range m.order {
		st := m.states[id]
		if st == nil {
			continue
		}
		if st.loaded {
			loaded = st
			nLoaded++
		}
		only = st
	}
	switch {
	case nLoaded == 1:
		return loaded
	case len(m.order) == 1:
		return only
	}
	return nil
}

// matchLocked: exact ID, else the single ID with id as suffix or prefix.
func (m *Manager) matchLocked(id string) *modelState {
	if st, ok := m.states[id]; ok && slices.Contains(m.order, id) {
		return st
	}
	var found *modelState
	for _, cand := range m.order {
		if strings.HasSuffix(cand, id) || strings.HasPrefix(cand, id) {
			if found != nil {
				return nil // ambiguous
			}
			found = m.states[cand]
		}
	}
	return found
}

// Acquire loads the model if needed and counts a request in flight. A
// local model's request first waits for a slot in the model's queue (see
// queue.go); the wait hook in ctx (WithWaitHook) hears why it waits. The
// release func is idempotent; it ends the request, frees the slot and
// stamps LastUsed.
func (m *Manager) Acquire(ctx context.Context, id string) (Endpoint, func(), error) {
	st, err := m.resolve(ctx, id)
	if err != nil {
		return Endpoint{}, nil, err
	}
	m.primeTotal()
	m.mu.Lock()
	local := st.info.Local
	m.mu.Unlock()
	if local {
		if err := m.admit(ctx, st); err != nil {
			return Endpoint{}, nil, err
		}
	}
	m.mu.Lock()
	st.inFlight++ // before loading, so the reaper and make-room leave it alone
	m.mu.Unlock()
	m.updateActive()

	ep, err := m.ensureLoaded(ctx, st)
	if err != nil {
		m.mu.Lock()
		st.inFlight--
		if local {
			m.releaseSlotLocked(st)
			m.broadcastLocked()
		}
		m.mu.Unlock()
		m.updateActive()
		return Endpoint{}, nil, err
	}
	m.mu.Lock()
	ep.Gen = st.loadSeq
	m.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			st.inFlight--
			st.lastUsed = m.now()
			if local {
				m.lastLocalEnd = st.lastUsed
				m.armTailLocked()
				m.releaseSlotLocked(st)
				m.broadcastLocked()
			}
			m.mu.Unlock()
			m.updateActive()
		})
	}
	return ep, release, nil
}

// Load loads a model explicitly (no request is counted).
func (m *Manager) Load(ctx context.Context, id string) error {
	st, err := m.resolve(ctx, id)
	if err != nil {
		return err
	}
	_, err = m.ensureLoaded(ctx, st)
	m.updateActive()
	return err
}

// Unload unloads a local model explicitly, even one Cerberus didn't load.
func (m *Manager) Unload(ctx context.Context, id string) error {
	st, err := m.resolve(ctx, id)
	if err != nil {
		return err
	}
	st.loadMu.Lock()
	defer st.loadMu.Unlock()
	m.mu.Lock()
	info, b := st.info, st.backend
	m.mu.Unlock()
	if !info.Local {
		return nil
	}
	if err := b.Unload(ctx, info); err != nil {
		return err
	}
	m.markUnloaded(st)
	m.updateActive()
	return nil
}

// loadPlan is what the manager learned while making room, handed to the
// backend's Load through ModelInfo.plan.
type loadPlan struct {
	FreeMiB int // free VRAM after evictions, -1 unknown
}

// ensureLoaded returns a ready endpoint, loading the model (once, even with
// concurrent callers) after making room for it.
func (m *Manager) ensureLoaded(ctx context.Context, st *modelState) (Endpoint, error) {
	m.mu.Lock()
	info, b := st.info, st.backend
	m.mu.Unlock()
	if !info.Local {
		return b.Endpoint(info), nil
	}
	// Both locks can be held for as long as a load waits for room, so a
	// request queued on them leaves when its caller does, and its wait hook
	// says it is waiting (not running).
	if err := st.loadMu.LockCtx(ctx, "waiting for "+info.ID+" to load"); err != nil {
		return Endpoint{}, err
	}
	defer st.loadMu.Unlock()
	m.mu.Lock()
	info, loaded := st.info, st.loaded
	m.mu.Unlock()
	if loaded {
		if ep := b.Endpoint(info); ep.BaseURL != "" {
			return ep, nil
		}
		// The engine lost it (a llama-server that exited). Count it as not
		// loaded before queueing for roomMu: a load waiting for room only
		// waits on loaded models, so it can never wait on us.
		m.markUnloaded(st)
	}

	if err := m.roomMu.LockCtx(ctx, "waiting for room: another model is loading"); err != nil {
		return Endpoint{}, err
	}
	defer m.roomMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Endpoint{}, err // gave up while queued: evict and load nothing
	}
	m.mu.Lock()
	crashed := st.crashed
	m.mu.Unlock()
	if crashed {
		// Whatever the crash left (LM Studio may still list the model, in a
		// bad state) goes first, so the load below applies the lower limit.
		if err := b.Unload(ctx, info); err != nil {
			m.log.Debug("unload after crash", "model", info.ID, "err", err)
		}
	}
	free, err := m.makeRoom(ctx, st, info)
	if err != nil {
		return Endpoint{}, err
	}
	m.mu.Lock()
	info.MaxParallel = m.limitLocked(st)
	m.mu.Unlock()
	info.plan = &loadPlan{FreeMiB: free}
	ep, err := b.Load(ctx, info)
	if err != nil {
		m.markUnloaded(st)
		return Endpoint{}, err
	}
	m.mu.Lock()
	st.setLoadedLocked(true)
	st.crashed = false
	st.loadSeq++
	st.lastUsed = m.now() // the idle TTL counts from load until first use
	m.mu.Unlock()
	m.wakeReaper()
	return ep, nil
}

// makeRoom unloads idle local models, least recently used first, until the
// target fits in free VRAM, and returns the free VRAM it expects after that
// (-1 unknown). Caller holds target.loadMu and m.roomMu. The GPU is queried
// afresh: a polled reading may predate a load that just finished.
//
// When the target still does not fit and another loaded local model is busy
// (holds request slots), it waits for that model to finish instead of
// overcommitting: three requests on a model squeezed in next to a busy one
// is how the GPU ran out of memory and the engine crashed. The wait blocks
// on the release broadcast (no polling), and is bounded by ctx and
// engines.wait_for_room_sec (then ErrRoomTimeout). Holding roomMu while
// waiting only holds up other loads; requests to loaded models never take
// roomMu, and a busy model's release needs only m.mu. Only when nothing
// busy holds memory (or waiting is off, or allow_overcommit is on) does the
// old rule apply: load if VRAM and RAM together can hold it.
func (m *Manager) makeRoom(ctx context.Context, target *modelState, info ModelInfo) (int, error) {
	need := int(float64(info.SizeMiB)*headroomFactor) + headroomMiB
	wait := m.roomWait()
	var deadline <-chan time.Time
	reason := ""
	defer func() {
		if reason != "" {
			waitHook(ctx)("")
		}
	}()
	for {
		// Taken before the idle list: a model that goes idle after the list
		// was made closes it, and the loop looks again instead of missing
		// that model (waiting on a release that already happened, or
		// spilling into RAM while it could have been evicted).
		_, since := m.busyLocal(target)
		m.vram.Refresh(ctx)
		free := m.vram.FreeVRAMMiB()
		if free < 0 {
			return -1, nil
		}
		for _, v := range m.idleLocal(target) {
			if free >= need {
				break
			}
			if size, ok := m.tryUnload(ctx, v, false); ok {
				free += size
			}
		}
		if free >= need {
			return free, nil
		}
		busy, _ := m.busyLocal(target)
		if len(busy) == 0 || wait <= 0 || m.conf().Engines.AllowOverCommit {
			select {
			case <-since:
				if wait > 0 && !m.conf().Engines.AllowOverCommit {
					continue // something was freed meanwhile: evict it first
				}
			default:
			}
			return free, m.overcommit(info, need, free)
		}
		if deadline == nil {
			t := time.NewTimer(wait)
			defer t.Stop()
			deadline = t.C
		}
		if r := "waiting for room: " + strings.Join(busy, ", ") + " busy"; r != reason {
			if reason == "" {
				m.log.Info("waiting for busy models to finish before loading",
					"model", info.ID, "busy", busy, "need_mib", need, "free_vram_mib", free)
			}
			reason = r
			waitHook(ctx)(r)
		}
		select {
		case <-since:
		case <-ctx.Done():
			return free, ctx.Err()
		case <-deadline:
			return free, fmt.Errorf("%w: %s needs about %d MiB (%d free); %s still busy after %s",
				ErrRoomTimeout, info.ID, need, free, strings.Join(busy, ", "), wait)
		}
	}
}

// overcommit decides a load that does not fit in free VRAM. The engines (LM
// Studio, Ollama, llama.cpp) keep what does not fit in VRAM in system RAM,
// slower but working; mixture-of-experts models do this well. So a VRAM
// shortfall is only a reason to evict idle models (and wait for busy ones),
// not to refuse: refuse only when VRAM and RAM together cannot hold it.
func (m *Manager) overcommit(info ModelInfo, need, free int) error {
	ram := -1
	if m.memAvail != nil {
		ram = m.memAvail()
	}
	if m.conf().Engines.AllowOverCommit || ram < 0 || free+ram-headroomMiB >= need {
		m.log.Info("model larger than free VRAM; the engine will keep part of it in RAM",
			"model", info.ID, "need_mib", need, "free_vram_mib", free, "free_ram_mib", ram)
		return nil
	}
	return fmt.Errorf("%w: %s needs about %d MiB, %d MiB VRAM + %d MiB RAM free", ErrNoRoom, info.ID, need, free, ram)
}

// idleLocal lists loaded local models with nothing in flight, LRU first.
func (m *Manager) idleLocal(except *modelState) []*modelState {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*modelState
	for _, id := range m.order {
		st := m.states[id]
		if st != except && st.info.Local && st.loaded && st.inFlight == 0 && st.active == 0 {
			out = append(out, st)
		}
	}
	slices.SortStableFunc(out, func(a, b *modelState) int {
		return m.view(a).LastUsed.Compare(m.view(b).LastUsed)
	})
	return out
}

// tryUnload unloads st if it is idle and nobody is loading it. With
// onlyExpired it also requires the idle TTL to have passed. It returns the
// model's size and whether it was unloaded.
func (m *Manager) tryUnload(ctx context.Context, st *modelState, onlyExpired bool) (int, bool) {
	if !st.loadMu.TryLock() {
		return 0, false // busy loading/unloading; never wait (avoids lock cycles)
	}
	defer st.loadMu.Unlock()
	m.mu.Lock()
	ok := st.loaded && st.inFlight == 0 && st.active == 0 && (!onlyExpired || m.expiredLocked(st))
	info, b := st.info, st.backend
	m.mu.Unlock()
	if !ok {
		return 0, false
	}
	if err := b.Unload(ctx, info); err != nil {
		m.log.Warn("unload failed", "model", info.ID, "err", err)
		return 0, false
	}
	m.markUnloaded(st)
	return info.SizeMiB, true
}

// expiredLocked: used by Cerberus and idle past its TTL. Models loaded by
// someone else and never used through Cerberus are left to their owner.
func (m *Manager) expiredLocked(st *modelState) bool {
	if st.lastUsed.IsZero() {
		return false
	}
	c := m.conf()
	ttl := c.IdleTTL(modelConfig(c, st.info.ID))
	return ttl > 0 && m.now().Sub(st.lastUsed) > ttl
}

func (m *Manager) markUnloaded(st *modelState) {
	m.mu.Lock()
	st.setLoadedLocked(false)
	m.broadcastLocked() // its memory is free: a load waiting for room re-checks
	m.mu.Unlock()
}

// Forget marks a local model as not loaded after its engine said so (it was
// unloaded behind Cerberus' back: the engine's own idle timer, its window, or
// a CLI), so the next Acquire loads it again instead of trusting the cache.
// It reports whether id named a local model.
func (m *Manager) Forget(id string) bool {
	st := m.match(id)
	if st == nil {
		return false
	}
	m.mu.Lock()
	local := st.info.Local
	if local {
		st.setLoadedLocked(false)
		m.broadcastLocked()
	}
	m.mu.Unlock()
	return local
}

// Run reaps idle models every 15 s while a model Cerberus used is loaded (only
// those can expire), and blocks without a timer otherwise. On ctx done it stops spawned processes.
func (m *Manager) Run(ctx context.Context) {
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		m.Shutdown(sctx)
	}()
	m.Models(ctx) // learn what is already loaded
	m.updateActive()
	var tick *time.Ticker
	var tc <-chan time.Time
	defer func() {
		if tick != nil {
			tick.Stop()
		}
	}()
	for {
		switch active := m.anyReapable(); {
		case active && tick == nil:
			tick = time.NewTicker(m.reapEvery)
			tc = tick.C
		case !active && tick != nil:
			tick.Stop()
			tick, tc = nil, nil
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-tc:
			m.reap(ctx)
		}
	}
}

// reap unloads expired models and occasionally re-lists to notice models
// that were unloaded behind our back (e.g. by LM Studio's own TTL).
func (m *Manager) reap(ctx context.Context) {
	m.mu.Lock()
	stale := m.now().Sub(m.listed) >= refreshEvery
	m.mu.Unlock()
	if stale {
		m.ensureFresh(ctx, true)
	}
	m.mu.Lock()
	var expired []*modelState
	for _, id := range m.order {
		st := m.states[id]
		if st.info.Local && st.loaded && st.inFlight == 0 && st.active == 0 && m.expiredLocked(st) {
			expired = append(expired, st)
		}
	}
	m.mu.Unlock()
	for _, st := range expired {
		if _, ok := m.tryUnload(ctx, st, true); ok {
			m.log.Info("idle model unloaded", "model", st.info.ID)
		}
	}
	m.updateActive()
}

// anyReapable reports whether a loaded local model has been used through
// Cerberus. Models someone else loaded never expire here, so they need no
// reaper ticks.
func (m *Manager) anyReapable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.states {
		if st.info.Local && st.loaded && !st.lastUsed.IsZero() {
			return true
		}
	}
	return false
}

// updateActive puts the GPU monitor in fast mode while a local request is in
// flight (loading included) and for activeTail after the last one, which is
// all per-request energy needs. A model merely sitting loaded (perhaps by
// another client) gets slow polling. Cloud requests don't touch the GPU.
func (m *Manager) updateActive() {
	m.mu.Lock()
	active := !m.lastLocalEnd.IsZero() && m.now().Sub(m.lastLocalEnd) < activeTail
	for _, st := range m.states {
		if st.info.Local && st.inFlight > 0 {
			active = true
			break
		}
	}
	m.mu.Unlock()
	m.vram.SetActive(active)
}

// armTailLocked (re)starts the timer that ends the fast-poll tail.
func (m *Manager) armTailLocked() {
	if m.tailTimer == nil {
		m.tailTimer = time.AfterFunc(activeTail, m.updateActive)
		return
	}
	m.tailTimer.Reset(activeTail)
}

func (m *Manager) wakeReaper() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Shutdown stops every process this manager spawned (llama.cpp). Models
// held by LM Studio or Ollama are left to them; their TTLs apply.
func (m *Manager) Shutdown(ctx context.Context) {
	m.stopOnce.Do(func() {
		var wg sync.WaitGroup
		for _, b := range m.backends {
			if s, ok := b.(spawner); ok {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.StopAll(ctx)
				}()
			}
		}
		wg.Wait()
		m.mu.Lock()
		for _, st := range m.states {
			if _, ok := st.backend.(spawner); ok {
				st.setLoadedLocked(false)
			}
		}
		m.mu.Unlock()
		m.updateActive()
	})
}
