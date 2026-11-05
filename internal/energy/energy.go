// Package energy turns sysmon samples into watts, kWh and money: CPU watts
// from RAPL (or a load model when RAPL is root-only), GPU watts from the GPU
// monitor, an estimate for the rest of the machine, wall watts through the
// PSU efficiency. It keeps a per-minute history on disk
// (DATA/energy/YYYY-MM.jsonl), hourly rollups in memory, and attributes
// energy to Cerberus sessions (CPU share of their cgroup, plus the GPU energy
// the gateway already charges to their requests).
package energy

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/sysmon"
	"carino.systems/cerberus/internal/usage"
)

// Power is the machine's draw (defined in sysmon to avoid an import cycle).
type Power = sysmon.Power

// spinningHDDW is added per spinning hard disk that is not idle.
const spinningHDDW = 5

// minuteKeep is how much per-minute detail stays in memory (minute charts).
const minuteKeep = 48 * time.Hour

// maxPoints caps a chart series.
const maxPoints = 2000

// KWh is energy by component.
type KWh struct{ CPU, GPU, Rest, Wall float64 }

func (k *KWh) add(j [4]float64) {
	k.CPU += j[0] / 3.6e6
	k.GPU += j[1] / 3.6e6
	k.Rest += j[2] / 3.6e6
	k.Wall += j[3] / 3.6e6
}

// Totals are the energy figures the Monitor shows.
type Totals struct {
	Today, Yesterday, Last7Days, MonthToDate, Last30Days KWh
	MonthlyAvg12                                         KWh // average per month over the last 12 months with data
	MonthsOfHistory                                      int
	ProjectedMonthKWh                                    float64 // wall
	ProjectionDays                                       float64 // how much data the projection rests on (days, ≤ 7)
	HistorySince                                         time.Time
	Loaded                                               bool // history read from disk (false right after start)
}

// Point is average power over one chart step.
type Point struct {
	T                    time.Time
	CPU, GPU, Rest, Wall float64 // watts
}

// row is one minute on disk: average watts over the seconds covered.
type row struct {
	T    int64   `json:"t"`
	CPU  float64 `json:"cpu"`
	GPU  float64 `json:"gpu"`
	Rest float64 `json:"rest"`
	Wall float64 `json:"wall"`
	Sec  float64 `json:"sec"`
}

func (r row) joules() [4]float64 {
	return [4]float64{r.CPU * r.Sec, r.GPU * r.Sec, r.Rest * r.Sec, r.Wall * r.Sec}
}

type bucket struct {
	j   [4]float64
	sec float64
}

// SessionJ is the energy charged to one session.
type SessionJ struct{ CPUJ, GPUJ float64 }

// Energy is the accountant.
type Energy struct {
	cfg  *config.Config
	lcfg atomic.Pointer[config.Config] // set by Reload; live settings read through conf()
	dir  string
	log  *slog.Logger
	now  func() time.Time
	proc string
	sys  string

	live  func() map[string]int // session id → leader pid
	known func(id string) bool  // the session still exists (nil: keep every record)
	aggFn func(since time.Time) []usage.Agg

	mu        sync.Mutex
	lastAt    time.Time
	lastPower Power
	curMin    int64 // unix seconds of the minute being filled (0 = none)
	cur       bucket
	minutes   []row             // last minuteKeep, ascending
	hours     map[int64]*bucket // unix hour start → totals
	loaded    bool
	first     time.Time
	ownRows   int       // rows this run appended before load (already counted live)
	firstLive time.Time // first sample of this run

	rapl *raplState

	sessions  map[string]*SessionJ
	sessDirty bool
	cg        map[string]string // session id → cpu.stat path ("" = use the process tree)
	prevUsec  map[string]uint64
	prevBusy  uint64
	scans     int // /proc scans made for attribution (tests)

	priceMu sync.Mutex // may be held while taking mu, never taken inside it
	priceAt time.Time
	price   float64
}

// Option configures New.
type Option func(*Energy)

// WithRoots points /proc and /sys reads at fixture trees (tests).
func WithRoots(proc, sys string) Option { return func(e *Energy) { e.proc, e.sys = proc, sys } }

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(e *Energy) { e.now = now } }

// WithSessions supplies the live sessions' leader pids for attribution.
func WithSessions(fn func() map[string]int) Option { return func(e *Energy) { e.live = fn } }

// WithKnown drops, when the history loads, energy records of sessions that
// no longer exist (deleted before Forget was wired, or while stopped).
func WithKnown(fn func(id string) bool) Option { return func(e *Energy) { e.known = fn } }

// WithUsage supplies per-model usage aggregates (model energy).
func WithUsage(fn func(since time.Time) []usage.Agg) Option { return func(e *Energy) { e.aggFn = fn } }

// New returns an accountant writing under dir (DATA/energy).
func New(cfg *config.Config, dir string, log *slog.Logger, opts ...Option) *Energy {
	if log == nil {
		log = slog.Default()
	}
	e := &Energy{cfg: cfg, dir: dir, log: log, now: time.Now, proc: "/proc", sys: "/sys",
		hours: map[int64]*bucket{}, sessions: map[string]*SessionJ{}, cg: map[string]string{}, prevUsec: map[string]uint64{}}
	for _, o := range opts {
		o(e)
	}
	e.rapl = newRAPL(e.sys, cfg.Monitor.CPUPower)
	return e
}

// RAPLReadable says whether the CPU package energy counter can be read.
func (e *Energy) RAPLReadable() bool { return e.rapl.readable() }

// Run loads the history, then flushes on shutdown.
func (e *Energy) Run(ctx context.Context) {
	e.load()
	sessions := e.loadSessions()
	// known runs before e.mu: the session manager calls into us under its lock.
	dropped := false
	if e.known != nil {
		for id := range sessions {
			if !e.known(id) {
				delete(sessions, id)
				dropped = true
			}
		}
	}
	e.mu.Lock()
	e.sessDirty = e.sessDirty || dropped // rewritten without them at the next flush
	for id, s := range sessions {
		if cur, ok := e.sessions[id]; ok {
			cur.CPUJ += s.CPUJ
			cur.GPUJ += s.GPUJ
		} else {
			e.sessions[id] = s
		}
	}
	e.mu.Unlock()
	<-ctx.Done()
	e.Flush()
}

// Flush writes the minute being filled and the session totals.
func (e *Energy) Flush() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.flushMinuteLocked()
	e.saveSessionsLocked()
}

// gapLimit: a pause between samples longer than this counts as "off".
func (e *Energy) gapLimit() time.Duration {
	idle := time.Duration(max(e.cfg.Monitor.IdleIntervalSec, 1)) * time.Second
	return 2*idle + 15*time.Second
}

// Sample computes the power for one sysmon snapshot and accounts for the
// time since the previous one (sysmon calls it from its sampling goroutine).
func (e *Energy) Sample(s *sysmon.Snapshot) Power {
	mc := e.conf().Monitor
	now := s.Time
	p := Power{CPUSource: "model", Estimated: true}
	if w, ok := e.rapl.watts(now); ok {
		p.CPUW, p.CPUSource, p.Estimated = w, "rapl", false
	} else {
		// No RAPL delta (not readable, or its first reading): the load model,
		// labelled as such even when RAPL is about to take over.
		p.CPUW = cpuModel(mc, s.CPU)
	}
	for _, g := range s.GPU.GPUs {
		if !math.IsNaN(g.PowerW) {
			p.GPUW += g.PowerW
		}
	}
	p.RestW = mc.BaselineW + spinningHDDW*float64(spinningDisks(s.Disks))
	eff := mc.PSUEfficiency
	if eff <= 0 || eff > 1 {
		eff = 1
	}
	p.WallW = (p.CPUW+p.GPUW+p.RestW)/eff + mc.ExtraW

	var live map[string]int
	if e.live != nil {
		live = e.live() // before e.mu: the session manager may call into us while holding its own lock
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.firstLive.IsZero() {
		e.firstLive = now
	}
	if !e.lastAt.IsZero() {
		gap := now.Sub(e.lastAt)
		if gap > 0 && gap <= e.gapLimit() {
			avg := [4]float64{(p.CPUW + e.lastPower.CPUW) / 2, (p.GPUW + e.lastPower.GPUW) / 2,
				(p.RestW + e.lastPower.RestW) / 2, (p.WallW + e.lastPower.WallW) / 2}
			e.addSpanLocked(e.lastAt, now, avg)
			e.attributeLocked(live, s, p.CPUW, gap.Seconds())
		}
	} else {
		e.attributeLocked(live, s, p.CPUW, 0) // prime the counters
	}
	e.lastAt, e.lastPower = now, p
	return p
}

// cpuModel estimates package watts from load and clock when RAPL is not readable.
func cpuModel(mc config.MonitorConfig, c sysmon.CPU) float64 {
	util := math.Min(math.Max(c.Pct/100, 0), 1)
	scale := 1.0
	if c.MaxMHz > 0 && len(c.MHz) > 0 {
		sum := 0
		for _, f := range c.MHz {
			sum += f
		}
		scale = math.Min(math.Max(float64(sum)/float64(len(c.MHz))/float64(c.MaxMHz), 0.3), 1)
	}
	return mc.CPUIdleW + (mc.CPUMaxW-mc.CPUIdleW)*math.Pow(util, 1.3)*scale
}

func spinningDisks(ds []sysmon.Disk) int {
	seen := map[string]bool{}
	n := 0
	for _, d := range ds {
		// One drive, however many of its partitions are mounted.
		key := d.Parent
		if key == "" {
			key = d.Device
		}
		key = d.Model + "|" + key
		if d.Rotational && !d.Idle && !seen[key] {
			seen[key] = true
			n++
		}
	}
	return n
}

// addSpanLocked spreads avg watts over [from, to), split at minute boundaries.
func (e *Energy) addSpanLocked(from, to time.Time, avg [4]float64) {
	for from.Before(to) {
		minStart := from.Truncate(time.Minute)
		end := minStart.Add(time.Minute)
		if end.After(to) {
			end = to
		}
		sec := end.Sub(from).Seconds()
		m := minStart.Unix()
		if e.curMin != 0 && m != e.curMin {
			e.flushMinuteLocked()
		}
		if e.curMin == 0 {
			e.curMin = m
		}
		for i := range avg {
			e.cur.j[i] += avg[i] * sec
		}
		e.cur.sec += sec
		from = end
	}
}

// flushMinuteLocked moves the minute being filled to disk and the rollups.
func (e *Energy) flushMinuteLocked() {
	if e.curMin == 0 || e.cur.sec <= 0 {
		e.curMin, e.cur = 0, bucket{}
		return
	}
	r := row{T: e.curMin, Sec: math.Round(e.cur.sec*10) / 10}
	r.CPU, r.GPU, r.Rest, r.Wall = e.cur.j[0]/e.cur.sec, e.cur.j[1]/e.cur.sec, e.cur.j[2]/e.cur.sec, e.cur.j[3]/e.cur.sec
	e.addRowLocked(r, true)
	e.appendRow(r)
	e.curMin, e.cur = 0, bucket{}
	if e.sessDirty {
		e.saveSessionsLocked()
	}
}

func (e *Energy) addRowLocked(r row, keepMinute bool) {
	t := time.Unix(r.T, 0)
	if e.first.IsZero() || t.Before(e.first) {
		e.first = t
	}
	h := localHour(t)
	b := e.hours[h]
	if b == nil {
		b = &bucket{}
		e.hours[h] = b
	}
	j := r.joules()
	for i := range j {
		b.j[i] += j[i]
	}
	b.sec += r.Sec
	if keepMinute && e.now().Sub(t) <= minuteKeep {
		if n := len(e.minutes); n > 0 && e.minutes[n-1].T == r.T {
			// A restart within a minute leaves two rows for it: one minute.
			e.minutes[n-1] = mergeRows(e.minutes[n-1], r)
		} else {
			e.minutes = append(e.minutes, r)
		}
		cut := e.now().Add(-minuteKeep).Unix()
		i := 0
		for i < len(e.minutes) && e.minutes[i].T < cut {
			i++
		}
		e.minutes = e.minutes[i:]
	}
}

// localHour is the unix start of t's hour on the local clock. Truncate works
// on absolute time (UTC hours), which in a UTC+5:30 zone would put half an
// hour of every day into the day before. The zone offset in force at t is
// used, so the two 01:00 hours of a DST fall-back stay apart.
func localHour(t time.Time) int64 {
	_, off := t.Zone()
	u := t.Unix() + int64(off)
	return u - ((u%3600)+3600)%3600 - int64(off)
}

func (e *Energy) monthFile(t time.Time) string {
	return filepath.Join(e.dir, t.Format("2006-01")+".jsonl")
}

func (e *Energy) appendRow(r row) {
	if e.dir == "" {
		return
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return
	}
	path := e.monthFile(time.Unix(r.T, 0))
	if _, err := os.Stat(path); os.IsNotExist(err) && e.loaded {
		// A new month: expire the months past HistoryMonths now, not only at
		// the next start (a daemon can run for months).
		e.expireLocked()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		e.log.Debug("energy history write failed", "err", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	if _, err := f.Write(append(b, '\n')); err == nil && !e.loaded {
		e.ownRows++
	}
}

// mergeRows combines two partial rows of the same minute, weighted by the
// seconds each measured.
func mergeRows(a, b row) row {
	sec := a.Sec + b.Sec
	if sec <= 0 {
		return a
	}
	avg := func(x, y float64) float64 { return (x*a.Sec + y*b.Sec) / sec }
	return row{T: a.T, CPU: avg(a.CPU, b.CPU), GPU: avg(a.GPU, b.GPU), Rest: avg(a.Rest, b.Rest), Wall: avg(a.Wall, b.Wall), Sec: sec}
}

// mergeMinutes merges neighbouring rows of the same minute (rows sorted by T).
func mergeMinutes(rows []row) []row {
	out := rows[:0]
	for _, r := range rows {
		if n := len(out); n > 0 && out[n-1].T == r.T {
			out[n-1] = mergeRows(out[n-1], r)
			continue
		}
		out = append(out, r)
	}
	return out
}

// oldestKept is the first day of the oldest month HistoryMonths keeps.
func (e *Energy) oldestKept() time.Time {
	months := e.cfg.Monitor.HistoryMonths
	if months <= 0 {
		months = 13
	}
	now := e.now()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local).AddDate(0, -(months - 1), 0)
}

// expireLocked deletes the month files and hourly rollups older than
// HistoryMonths (load does the same at start).
func (e *Energy) expireLocked() {
	oldest := e.oldestKept()
	files, _ := filepath.Glob(filepath.Join(e.dir, "????-??.jsonl"))
	for _, f := range files {
		mt, err := time.ParseInLocation("2006-01", strings.TrimSuffix(filepath.Base(f), ".jsonl"), time.Local)
		if err == nil && mt.Before(oldest) {
			_ = os.Remove(f)
		}
	}
	cut := oldest.Unix()
	for h := range e.hours {
		if h < cut {
			delete(e.hours, h)
		}
	}
	if !e.first.IsZero() && e.first.Before(oldest) {
		e.first = time.Time{}
		for h := range e.hours {
			if t := time.Unix(h, 0); e.first.IsZero() || t.Before(e.first) {
				e.first = t
			}
		}
	}
}

// load reads the month files kept, rebuilding hours and recent minutes, and
// deletes months older than HistoryMonths.
func (e *Energy) load() {
	oldest := e.oldestKept()
	files, _ := filepath.Glob(filepath.Join(e.dir, "????-??.jsonl"))
	sort.Strings(files)
	// Read under the lock so that no row of this run is appended meanwhile:
	// the rows it did append before (ownRows) end the newest file and are
	// already counted live.
	e.mu.Lock()
	defer e.mu.Unlock()
	var rows []row
	for _, f := range files {
		mt, err := time.ParseInLocation("2006-01", strings.TrimSuffix(filepath.Base(f), ".jsonl"), time.Local)
		if err != nil {
			continue
		}
		if mt.Before(oldest) {
			_ = os.Remove(f)
			continue
		}
		rows = append(rows, readRows(f)...)
	}
	rows = rows[:max(len(rows)-e.ownRows, 0)]
	for _, r := range rows {
		e.addRowLocked(r, true)
	}
	sort.SliceStable(e.minutes, func(i, j int) bool { return e.minutes[i].T < e.minutes[j].T })
	e.minutes = mergeMinutes(e.minutes)
	e.loaded = true
}

func readRows(path string) []row {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		var r row
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.T > 0 && r.Sec > 0 {
			out = append(out, r)
		}
	}
	return out
}

// ---------- totals and series ----------

// Totals sums the history (plus the minute being filled).
func (e *Energy) Totals() Totals {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	yesterday := today.AddDate(0, 0, -1)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	last7, last30 := now.Add(-7*24*time.Hour), now.Add(-30*24*time.Hour)
	yearAgo := month.AddDate(0, -12, 0)
	t := Totals{Loaded: e.loaded, HistorySince: e.first}
	monthsSeen := map[string]bool{}
	var year KWh
	add := func(at time.Time, j [4]float64) {
		switch {
		case !at.Before(today):
			t.Today.add(j)
		case !at.Before(yesterday):
			t.Yesterday.add(j)
		}
		if !at.Before(last7) {
			t.Last7Days.add(j)
		}
		if !at.Before(last30) {
			t.Last30Days.add(j)
		}
		if !at.Before(month) {
			t.MonthToDate.add(j)
		} else if !at.Before(yearAgo) {
			year.add(j)
			monthsSeen[at.Format("2006-01")] = true
		}
	}
	for h, b := range e.hours {
		add(time.Unix(h, 0), b.j)
	}
	if e.curMin != 0 {
		add(time.Unix(e.curMin, 0), e.cur.j)
	}
	t.MonthsOfHistory = len(monthsSeen)
	if n := float64(len(monthsSeen)); n > 0 {
		t.MonthlyAvg12 = KWh{year.CPU / n, year.GPU / n, year.Rest / n, year.Wall / n}
	}
	// Projection: month so far + the recent daily average × days left.
	// The span the 7-day figure really covers: from the oldest data (disk
	// history or this run's first sample), at least 10 s, at most 7 days.
	span := 0.0
	oldest := e.first
	if oldest.IsZero() || (!e.firstLive.IsZero() && e.firstLive.Before(oldest)) {
		oldest = e.firstLive
	}
	if !oldest.IsZero() {
		span = math.Min(7, math.Max(now.Sub(oldest).Hours()/24, 10.0/86400))
	}
	t.ProjectionDays = span
	daily := 0.0
	if span > 0 {
		daily = t.Last7Days.Wall / span
	}
	left := month.AddDate(0, 1, 0).Sub(now).Hours() / 24
	t.ProjectedMonthKWh = t.MonthToDate.Wall + daily*math.Max(left, 0)
	return t
}

// Series returns average power per step: "minute" (last 48 h), "hour" or "day".
func (e *Energy) Series(res string, since time.Time) []Point {
	e.mu.Lock()
	defer e.mu.Unlock()
	var pts []Point
	switch res {
	case "minute":
		rows := e.minutes
		if e.curMin != 0 {
			cur := row{T: e.curMin, Sec: e.cur.sec}
			if cur.Sec > 0 {
				cur.CPU, cur.GPU, cur.Rest, cur.Wall = e.cur.j[0]/cur.Sec, e.cur.j[1]/cur.Sec, e.cur.j[2]/cur.Sec, e.cur.j[3]/cur.Sec
			}
			if n := len(rows); n > 0 && rows[n-1].T == cur.T {
				rows = append(rows[:n-1:n-1], mergeRows(rows[n-1], cur))
			} else {
				rows = append(rows[:len(rows):len(rows)], cur)
			}
		}
		for _, r := range rows {
			if r.T >= since.Unix() {
				// A minute shows the average while measured (a partly covered
				// minute is not diluted); hours and days count gaps as off.
				pts = append(pts, point(time.Unix(r.T, 0), r.joules(), math.Max(r.Sec, 1)))
			}
		}
	case "day":
		// Whole local days only: a since in mid-day would sum part of its
		// first day and divide it by a full day.
		sl := since.In(time.Local) // days are bucketed on the local clock
		sinceDay := time.Date(sl.Year(), sl.Month(), sl.Day(), 0, 0, 0, 0, time.Local)
		days := map[int64]*bucket{}
		for h, b := range e.hours {
			t := time.Unix(h, 0)
			if t.Before(sinceDay) {
				continue
			}
			d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Unix()
			acc := days[d]
			if acc == nil {
				acc = &bucket{}
				days[d] = acc
			}
			for i := range b.j {
				acc.j[i] += b.j[i]
			}
		}
		for d, b := range days {
			start := time.Unix(d, 0)
			full := start.AddDate(0, 0, 1).Sub(start).Seconds() // 23 or 25 h on DST days
			pts = append(pts, point(start, b.j, e.stepSec(d, full)))
		}
	default: // hour
		for h, b := range e.hours {
			if h >= localHour(since) {
				pts = append(pts, point(time.Unix(h, 0), b.j, e.stepSec(h, 3600)))
			}
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T.Before(pts[j].T) })
	return downsample(pts)
}

func point(t time.Time, j [4]float64, sec float64) Point {
	return Point{T: t, CPU: j[0] / sec, GPU: j[1] / sec, Rest: j[2] / sec, Wall: j[3] / sec}
}

func downsample(pts []Point) []Point {
	if len(pts) <= maxPoints {
		return pts
	}
	step := (len(pts) + maxPoints - 1) / maxPoints
	out := make([]Point, 0, maxPoints)
	for i := 0; i < len(pts); i += step {
		end := min(i+step, len(pts))
		p := Point{T: pts[i].T}
		for _, q := range pts[i:end] {
			p.CPU += q.CPU
			p.GPU += q.GPU
			p.Rest += q.Rest
			p.Wall += q.Wall
		}
		n := float64(end - i)
		p.CPU, p.GPU, p.Rest, p.Wall = p.CPU/n, p.GPU/n, p.Rest/n, p.Wall/n
		out = append(out, p)
	}
	return out
}

// ---------- session attribution ----------

// attributeLocked charges each live session its share of CPU watts: its
// cgroup's CPU time over the machine's busy time in the same interval.
func (e *Energy) attributeLocked(live map[string]int, s *sysmon.Snapshot, cpuW, sec float64) {
	if e.live == nil {
		return
	}
	busy := s.CPU.BusyTicks
	dBusy := busy - e.prevBusy
	valid := e.prevBusy != 0 && busy >= e.prevBusy && dBusy > 0 && sec > 0
	e.prevBusy = busy
	for id := range e.prevUsec {
		if _, ok := live[id]; !ok {
			delete(e.prevUsec, id)
			delete(e.cg, id)
		}
	}
	// Sessions without their own cgroup share one /proc scan per sample
	// (made only when one of them needs it), never one scan per session.
	var tbl map[int]procTick
	table := func() map[int]procTick {
		if tbl == nil {
			tbl = e.procTable()
		}
		return tbl
	}
	for id, pid := range live {
		u, ok := e.sessionUsec(id, pid, table)
		if !ok {
			continue
		}
		prev, had := e.prevUsec[id]
		e.prevUsec[id] = u
		if !had || !valid || u < prev {
			continue
		}
		share := math.Min(float64(u-prev)/(float64(dBusy)*1e6/100), 1)
		sj := e.sessions[id]
		if sj == nil {
			sj = &SessionJ{}
			e.sessions[id] = sj
		}
		sj.CPUJ += share * cpuW * sec
		e.sessDirty = true
	}
}

// sessionUsec is a session's cumulative CPU time in µs: its cgroup's
// cpu.stat when it has its own (systemd-run scope), else its process tree
// from table (one shared scan of /proc).
func (e *Energy) sessionUsec(id string, pid int, table func() map[int]procTick) (uint64, bool) {
	path, known := e.cg[id]
	if !known {
		path = e.cgroupStat(pid)
		e.cg[id] = path
	}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
					n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
					return n, err == nil
				}
			}
		}
		delete(e.cg, id) // the scope is gone; look again next time
	}
	return treeUsec(table(), pid)
}

// cgroupStat returns the cpu.stat of pid's cgroup when it differs from ours.
func (e *Energy) cgroupStat(pid int) string {
	mine, theirs := e.cgroupOf(os.Getpid()), e.cgroupOf(pid)
	if theirs == "" || theirs == mine || theirs == "/" {
		return ""
	}
	p := filepath.Join(e.sys, "fs/cgroup", theirs, "cpu.stat")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func (e *Energy) cgroupOf(pid int) string {
	b, err := os.ReadFile(filepath.Join(e.proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "0::"); ok {
			return v
		}
	}
	return ""
}

// procTick is one process's parent and CPU ticks: its own plus its reaped
// children's (cutime+cstime), so a short command that ran and exited between
// two samples is still counted, at its parent.
type procTick struct {
	ppid  int
	ticks uint64
}

// procTable reads every process's stat once.
func (e *Energy) procTable() map[int]procTick {
	e.scans++
	ents, err := os.ReadDir(e.proc)
	if err != nil {
		return map[int]procTick{}
	}
	all := make(map[int]procTick, len(ents))
	for _, en := range ents {
		pid, err := strconv.Atoi(en.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(e.proc, en.Name(), "stat"))
		if err != nil {
			continue
		}
		r := strings.LastIndexByte(string(b), ')')
		if r < 0 {
			continue
		}
		f := strings.Fields(string(b[r+1:]))
		if len(f) < 15 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		var ticks uint64
		for _, i := range []int{11, 12, 13, 14} { // utime stime cutime cstime
			v, _ := strconv.ParseUint(f[i], 10, 64)
			ticks += v
		}
		all[pid] = procTick{ppid, ticks}
	}
	return all
}

// treeUsec sums the ticks of root and its descendants (µs).
func treeUsec(all map[int]procTick, root int) (uint64, bool) {
	if _, ok := all[root]; !ok {
		return 0, false
	}
	var total uint64
	for pid, p := range all {
		for i, cur := 0, pid; i < 64 && cur > 0; i++ {
			if cur == root {
				total += p.ticks
				break
			}
			cur = all[cur].ppid
		}
	}
	return total * 1e6 / 100, true
}

// OnUsage adds the gateway's GPU energy for a session's request.
func (e *Energy) OnUsage(r usage.Record) {
	// Chats label their requests with their chat id ("chat-…"); they are
	// not sessions, and nothing would ever Forget them.
	if r.SessionID == "" || r.EnergyJ <= 0 || strings.HasPrefix(r.SessionID, "chat-") {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sj := e.sessions[r.SessionID]
	if sj == nil {
		sj = &SessionJ{}
		e.sessions[r.SessionID] = sj
	}
	sj.GPUJ += r.EnergyJ
	e.sessDirty = true
}

// SessionEnergy is the CPU and GPU energy charged to a session (joules, DC side).
func (e *Energy) SessionEnergy(id string) (cpuJ, gpuJ float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if sj := e.sessions[id]; sj != nil {
		return sj.CPUJ, sj.GPUJ
	}
	return 0, 0
}

// ModelEnergy is the gateway energy per model since a time (joules).
func (e *Energy) ModelEnergy(since time.Time) map[string]float64 {
	out := map[string]float64{}
	if e.aggFn == nil {
		return out
	}
	for _, a := range e.aggFn(since) {
		if a.EnergyJ > 0 {
			out[a.Model] += a.EnergyJ
		}
	}
	return out
}

func (e *Energy) sessionsFile() string { return filepath.Join(e.dir, "sessions.json") }

func (e *Energy) loadSessions() map[string]*SessionJ {
	out := map[string]*SessionJ{}
	b, err := os.ReadFile(e.sessionsFile())
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (e *Energy) saveSessionsLocked() {
	if e.dir == "" || !e.sessDirty {
		return
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return
	}
	b, err := json.Marshal(e.sessions)
	if err != nil {
		return
	}
	if platform.WritePrivate(e.sessionsFile(), b) == nil {
		e.sessDirty = false
	}
}

// Forget drops a session's energy record (sessions pruned or discarded).
func (e *Energy) Forget(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.sessions[id]; ok {
		delete(e.sessions, id)
		e.sessDirty = true
	}
}

// stepSec is how many seconds a chart step starting at start (unix) spans:
// full steps count gaps as off, but the step still running only counts the
// time elapsed so far (the rest of today has not happened yet).
func (e *Energy) stepSec(start int64, full float64) float64 {
	if el := float64(e.now().Unix() - start); el > 0 && el < full {
		return math.Max(el, 60)
	}
	return full
}
