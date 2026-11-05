// Package gpu monitors the host's GPUs (NVIDIA through one-shot nvidia-smi
// queries, AMD through sysfs) and integrates their power draw into energy.
//
// The poll loop is cheap by design: one query per tick, and a timer only
// while models are active. When idle nothing polls; a reading older than the
// idle interval is refreshed on demand by whoever asks for it, so a fully idle
// daemon never wakes the GPU. A missing tool is never an error; the snapshot
// is simply empty.
package gpu

import (
	"context"
	"log/slog"
	"math"
	"os/exec"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
)

// GPU is one device's reading at a point in time.
type GPU struct {
	Index       int
	Name        string
	Vendor      string
	MemTotalMiB int
	MemUsedMiB  int
	PowerW      float64
	PowerLimitW float64
	UtilPct     int
	TempC       int
	PState      string
}

// Snapshot is the latest reading of all GPUs.
type Snapshot struct {
	Time    time.Time
	GPUs    []GPU
	EnergyJ float64 // cumulative since start, all GPUs
}

// queryFunc reads all GPUs once. A GPU whose power is unknown carries
// PowerW = NaN; the monitor turns it into 0 before publishing.
type queryFunc func(ctx context.Context) ([]GPU, error)

// Monitor polls GPUs and keeps the latest snapshot plus cumulative energy.
type Monitor struct {
	cfg config.GPUConfig
	log *slog.Logger

	queryMu sync.Mutex // serialises queries; guards query
	query   queryFunc  // nil until resolved by detect (tests set it directly)
	wake    chan struct{}

	mu        sync.RWMutex
	active    bool
	snap      Snapshot
	energyJ   float64
	lastPower float64 // summed known power at lastTime; NaN when unknown
	lastTime  time.Time
}

// New returns a monitor; call Run to start polling.
func New(cfg config.GPUConfig, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	if cfg.ActiveIntervalMs <= 0 {
		cfg.ActiveIntervalMs = 1000
	}
	if cfg.IdleIntervalMs <= 0 {
		cfg.IdleIntervalMs = 15000
	}
	if cfg.NvidiaSmi == "" {
		cfg.NvidiaSmi = "nvidia-smi"
	}
	if cfg.Backend == "" {
		cfg.Backend = "auto"
	}
	return &Monitor{cfg: cfg, log: log, wake: make(chan struct{}, 1), lastPower: math.NaN()}
}

// onDemandTimeout bounds a query made on behalf of a reader.
const onDemandTimeout = 5 * time.Second

// Run polls until ctx is cancelled. While active it ticks on a timer; while
// idle it blocks until SetActive wakes it (readers refresh on demand).
func (m *Monitor) Run(ctx context.Context) {
	m.queryMu.Lock()
	if m.query == nil {
		m.query = m.detect(ctx)
	}
	m.queryMu.Unlock()
	m.tick(ctx)
	t := time.NewTimer(time.Hour)
	t.Stop()
	defer t.Stop()
	for {
		var tc <-chan time.Time
		if m.isActive() {
			t.Reset(m.activeInterval())
			tc = t.C
		}
		select {
		case <-ctx.Done():
			return
		case <-tc:
		case <-m.wake:
		}
		t.Stop()
		m.tick(ctx)
	}
}

// Refresh queries the GPUs now, so the next reading is current (used before
// decisions that must not trust a reading taken before a model loaded).
func (m *Monitor) Refresh(ctx context.Context) {
	m.tick(ctx)
}

// refreshIfStale queries on demand when idle and the reading is older than
// the idle interval. While active the poll loop keeps the reading fresh.
func (m *Monitor) refreshIfStale() {
	m.mu.RLock()
	stale := !m.active && time.Since(m.snap.Time) > time.Duration(m.cfg.IdleIntervalMs)*time.Millisecond
	m.mu.RUnlock()
	if !stale {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), onDemandTimeout)
	defer cancel()
	m.tickIf(ctx, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return time.Since(m.snap.Time) > time.Duration(m.cfg.IdleIntervalMs)*time.Millisecond
	})
}

// SetActive selects the fast (true) or slow (false) poll interval.
func (m *Monitor) SetActive(active bool) {
	m.mu.Lock()
	changed := m.active != active
	m.active = active
	m.mu.Unlock()
	if changed {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

// Snapshot returns a copy of the latest reading (refreshed first when idle
// and stale).
func (m *Monitor) Snapshot() Snapshot {
	m.refreshIfStale()
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.snap
	s.GPUs = append([]GPU(nil), m.snap.GPUs...)
	s.EnergyJ = m.energyJ
	return s
}

// EnergyJ is the cumulative energy in joules since the monitor started. It
// extrapolates from the last sample at its power (for up to two active
// intervals), so a short request that ends between two ticks is charged for
// its real duration instead of zero or a whole tick.
func (m *Monitor) EnergyJ() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.energyJ
	if math.IsNaN(m.lastPower) || m.lastTime.IsZero() {
		return e
	}
	horizon := 2 * time.Duration(m.cfg.ActiveIntervalMs) * time.Millisecond
	if dt := time.Since(m.lastTime); dt > 0 && dt <= horizon {
		e += m.lastPower * dt.Seconds()
	}
	return e
}

// FreeVRAMMiB is the free VRAM summed over all GPUs, or -1 when unknown
// (refreshed first when idle and stale).
func (m *Monitor) FreeVRAMMiB() int {
	m.refreshIfStale()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.snap.GPUs) == 0 {
		return -1
	}
	free := 0
	for _, g := range m.snap.GPUs {
		if g.MemTotalMiB <= 0 {
			return -1
		}
		free += max(g.MemTotalMiB-g.MemUsedMiB, 0)
	}
	return free
}

func (m *Monitor) isActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *Monitor) activeInterval() time.Duration {
	return time.Duration(m.cfg.ActiveIntervalMs) * time.Millisecond
}

// tick runs one query and folds it into the snapshot and energy total.
func (m *Monitor) tick(ctx context.Context) {
	m.tickIf(ctx, nil)
}

// tickIf is tick, skipped when need (checked once queries are serialised)
// reports false. Nothing happens before Run has resolved the backend.
func (m *Monitor) tickIf(ctx context.Context, need func() bool) {
	m.queryMu.Lock()
	defer m.queryMu.Unlock()
	if m.query == nil || (need != nil && !need()) {
		return
	}
	gpus, err := m.query(ctx)
	now := time.Now()
	if err != nil {
		m.log.Debug("gpu query failed", "err", err)
		gpus = nil
	}
	m.record(now, gpus)
}

// record integrates power with the trapezoid rule. An interval is only
// integrated when both ends have a known power, so a failed or partial
// query leaves a gap instead of inventing energy.
func (m *Monitor) record(now time.Time, gpus []GPU) {
	power := math.NaN()
	for i := range gpus {
		if p := gpus[i].PowerW; !math.IsNaN(p) {
			if math.IsNaN(power) {
				power = 0
			}
			power += p
		} else {
			gpus[i].PowerW = 0
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !math.IsNaN(power) && !math.IsNaN(m.lastPower) && !m.lastTime.IsZero() {
		if dt := now.Sub(m.lastTime).Seconds(); dt > 0 {
			m.energyJ += (m.lastPower + power) / 2 * dt
		}
	}
	m.lastPower, m.lastTime = power, now
	m.snap = Snapshot{Time: now, GPUs: gpus, EnergyJ: m.energyJ}
}

// detect resolves the configured backend to a query function.
func (m *Monitor) detect(ctx context.Context) queryFunc {
	none := func(context.Context) ([]GPU, error) { return nil, nil }
	nv := func(ctx context.Context) ([]GPU, error) { return queryNvidia(ctx, m.cfg.NvidiaSmi) }
	amd := func(context.Context) ([]GPU, error) { return queryAMD(sysfsDRM) }
	switch m.cfg.Backend {
	case "nvidia":
		return nv
	case "amd":
		return amd
	case "none":
		return none
	}
	if _, err := exec.LookPath(m.cfg.NvidiaSmi); err == nil {
		if g, err := nv(ctx); err == nil && len(g) > 0 {
			m.log.Info("gpu backend", "backend", "nvidia")
			return nv
		}
	}
	if g, err := amd(ctx); err == nil && len(g) > 0 {
		m.log.Info("gpu backend", "backend", "amd")
		return amd
	}
	m.log.Info("gpu backend", "backend", "none")
	return none
}
