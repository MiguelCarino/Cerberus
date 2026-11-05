// Package sysmon samples the whole machine: CPU, memory, network, disks,
// GPU and (while someone is watching) every process, grouped by app.
//
// Sampling is energy-aware. While a Monitor panel, the stream or `cerberus
// top` is watching, it samples every ActiveIntervalMs and scans processes.
// Otherwise it takes one cheap sample every IdleIntervalSec for the energy
// history, with no process scan.
//
// Everything is read from /proc and /sys under injectable roots, so the
// readers are tested against fixture trees. On systems without them (macOS,
// Windows for now) the snapshot is simply empty.
package sysmon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/gpu"
)

// CPU is the processor's state.
type CPU struct {
	Pct       float64   // all cores, 0-100
	Cores     []float64 // per logical CPU, 0-100
	MHz       []int     // per logical CPU
	MaxMHz    int       // highest cpuinfo_max_freq (for the power model)
	Load      [3]float64
	TempC     float64
	Model     string
	Threads   int
	BusyTicks uint64 // cumulative busy jiffies, all CPUs (energy attribution)
}

// Mem is memory and swap.
type Mem struct {
	TotalMiB, AvailMiB, CachedMiB, SwapTotalMiB, SwapUsedMiB, ZramOrigMiB, ZramComprMiB int
}

// Net is one interface's throughput.
type Net struct {
	Name         string
	RxBps, TxBps float64
}

// Disk is one mounted filesystem and the drive under it.
type Disk struct {
	Device, Model     string
	Mounts            []string
	FSType            string
	SizeB, UsedB      uint64
	FreeB             uint64
	UsedPct           float64
	ReadBps, WriteBps float64
	BusyPct           float64
	Rotational        bool
	TempC             float64
	Idle              bool   // rotational and no I/O for 5 minutes (no SMART without root)
	Parent            string // the whole disk under the partition (sdb for sdb1); "" when unknown
}

// Power is the machine's draw, filled by the energy package.
type Power struct {
	CPUW, GPUW, RestW, WallW float64
	CPUSource                string // "rapl" | "model"
	Estimated                bool   // the CPU part is modelled, not measured
}

// Snapshot is one sample of the machine.
type Snapshot struct {
	Time   time.Time
	CPU    CPU
	Mem    Mem
	Net    []Net
	Disks  []Disk
	GPU    gpu.Snapshot
	Power  Power
	Uptime time.Duration
	Active bool // sampled in active mode (processes are current)
}

// Proc is one process.
type Proc struct {
	PID, PPID, UID  int
	Name, Cmd, User string
	Group, State    string
	CPUPct          float64 // of one core, like top
	RSSMiB, GPUMiB  int
	GPUPct          float64
	Nice            int
	Started         time.Time
	Cerberus        bool
	SessionID       string
	Protected       bool
}

// Group is the processes of one app, service or Cerberus session.
type Group struct {
	Name      string  `json:"name"`
	CPU       float64 `json:"cpu"`
	RSSMiB    int     `json:"rss_mib"`
	GPUMiB    int     `json:"gpu_mib"`
	Count     int     `json:"count"`
	Cerberus  bool    `json:"cerberus"`
	SessionID string  `json:"session_id"`
}

// GPUSource is what sysmon needs from the GPU monitor.
type GPUSource interface {
	Snapshot() gpu.Snapshot
	Refresh(ctx context.Context)
}

// Errors from Signal and Renice.
var (
	ErrProtected = errors.New("this process is protected")
	ErrNotOwned  = errors.New("not your process")
	ErrNoProcess = errors.New("no such process")
)

// leaseFor keeps the monitor active after the last HTTP poll or watcher.
const leaseFor = 60 * time.Second

// Monitor samples the machine.
type Monitor struct {
	cfg  config.MonitorConfig
	gpu  GPUSource
	log  *slog.Logger
	proc string // /proc root
	sys  string // /sys root
	self int
	uid  int
	now  func() time.Time
	run  runner

	powerFn func(*Snapshot) Power
	hooks   []func(Snapshot)
	audit   *audit.Logger // kill/renice records (nil: none)

	wake chan struct{}

	mu     sync.RWMutex
	snap   Snapshot
	procs  []Proc
	groups []Group
	refs   int
	lease  time.Time
	subs   map[chan struct{}]struct{}

	protected atomic.Pointer[[]string] // Reload's monitor.protected (nil: cfg.Protected)

	st sampler // touched only by the Run goroutine (and tests)
}

// Option configures New.
type Option func(*Monitor)

// WithRoots points the readers at fixture trees (tests).
func WithRoots(proc, sys string) Option { return func(m *Monitor) { m.proc, m.sys = proc, sys } }

// WithAudit records every kill and renice of a host process in l.
func WithAudit(l *audit.Logger) Option { return func(m *Monitor) { m.audit = l } }

// WithRunner replaces the command runner (nvidia-smi pmon) in tests.
func WithRunner(r runner) Option { return func(m *Monitor) { m.run = r } }

// New returns a monitor; call Run to start sampling.
func New(cfg *config.Config, g GPUSource, log *slog.Logger, opts ...Option) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	mc := cfg.Monitor
	if mc.ActiveIntervalMs <= 0 {
		mc.ActiveIntervalMs = 2000
	}
	if mc.IdleIntervalSec <= 0 {
		mc.IdleIntervalSec = 30
	}
	m := &Monitor{cfg: mc, gpu: g, log: log, proc: "/proc", sys: "/sys", self: os.Getpid(), uid: os.Getuid(),
		now: time.Now, run: execRunner(cfg.GPU.NvidiaSmi), wake: make(chan struct{}, 1), subs: map[chan struct{}]struct{}{}}
	for _, o := range opts {
		o(m)
	}
	m.st = newSampler(m)
	return m
}

// SetPowerFunc installs the power calculation (energy package); call before Run.
func (m *Monitor) SetPowerFunc(fn func(*Snapshot) Power) { m.powerFn = fn }

// AddHook calls fn after every sample (alerts); call before Run.
func (m *Monitor) AddHook(fn func(Snapshot)) { m.hooks = append(m.hooks, fn) }

// Watch marks the monitor active until release is called (refcounted).
func (m *Monitor) Watch() (release func()) {
	m.mu.Lock()
	m.refs++
	m.mu.Unlock()
	m.poke()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.refs--
			m.lease = m.now().Add(leaseFor)
			m.mu.Unlock()
		})
	}
}

// Touch keeps the monitor active for the next 60 s (a poll, `cerberus top`).
func (m *Monitor) Touch() {
	m.mu.Lock()
	was := m.activeLocked()
	m.lease = m.now().Add(leaseFor)
	m.mu.Unlock()
	if !was {
		m.poke()
	}
}

// Active reports whether someone is watching.
func (m *Monitor) Active() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeLocked()
}

func (m *Monitor) activeLocked() bool { return m.refs > 0 || m.now().Before(m.lease) }

func (m *Monitor) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Subscribe returns a channel that receives a value after each sample.
func (m *Monitor) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
	}
}

// Snapshot returns the last sample (never blocks on I/O).
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap
}

// Processes returns the last process sample (empty when idle).
func (m *Monitor) Processes() []Proc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Proc(nil), m.procs...)
}

// Groups returns the last per-app aggregation (empty when idle).
func (m *Monitor) Groups() []Group {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Group(nil), m.groups...)
}

// Run samples until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		return
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		active := m.Active()
		m.Sample(ctx, active)
		next := time.Duration(m.cfg.IdleIntervalSec) * time.Second
		if active {
			next = time.Duration(m.cfg.ActiveIntervalMs) * time.Millisecond
		}
		t.Reset(next)
	}
}

// Sample takes one sample now (Run calls it; exported for tests and tools).
func (m *Monitor) Sample(ctx context.Context, active bool) Snapshot {
	snap := m.st.sample(ctx, active)
	if m.powerFn != nil {
		snap.Power = m.powerFn(&snap)
	}
	var procs []Proc
	var groups []Group
	if active {
		procs, groups = m.st.processes(ctx, snap.Time)
	} else {
		m.st.forgetProcesses()
	}
	m.mu.Lock()
	m.snap, m.procs, m.groups = snap, procs, groups
	for ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	m.mu.Unlock()
	for _, h := range m.hooks {
		h(snap)
	}
	return snap
}
