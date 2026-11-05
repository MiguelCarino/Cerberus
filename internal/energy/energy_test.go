package energy

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/sysmon"
	"carino.systems/cerberus/internal/usage"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6*math.Max(1, math.Abs(b)) }

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testCfg() *config.Config {
	c := config.Defaults()
	c.Monitor.CPUPower = "model"
	c.Monitor.BaselineW = 40
	c.Monitor.PSUEfficiency = 0.8
	c.Monitor.ExtraW = 0
	c.Monitor.IdleIntervalSec = 30
	return c
}

func snap(at time.Time, cpuPct, gpuW float64) *sysmon.Snapshot {
	return &sysmon.Snapshot{Time: at, CPU: sysmon.CPU{Pct: cpuPct, MHz: []int{5000}, MaxMHz: 5000},
		GPU: gpu.Snapshot{GPUs: []gpu.GPU{{PowerW: gpuW}}}}
}

func TestCPUModel(t *testing.T) {
	mc := testCfg().Monitor
	if w := cpuModel(mc, sysmon.CPU{Pct: 0}); !near(w, 30) {
		t.Fatalf("idle = %v", w)
	}
	if w := cpuModel(mc, sysmon.CPU{Pct: 100, MHz: []int{5000}, MaxMHz: 5000}); !near(w, 142) {
		t.Fatalf("full = %v", w)
	}
	half := cpuModel(mc, sysmon.CPU{Pct: 50, MHz: []int{2500}, MaxMHz: 5000})
	if want := 30 + 112*math.Pow(0.5, 1.3)*0.5; !near(half, want) {
		t.Fatalf("half = %v, want %v", half, want)
	}
}

func TestPowerAndWall(t *testing.T) {
	e := New(testCfg(), t.TempDir(), nil)
	s := snap(time.Unix(1700000000, 0), 0, 100)
	s.Disks = []sysmon.Disk{{Model: "HDD", Device: "sdb1", Rotational: true}, {Model: "HDD", Device: "sdb1", Rotational: true},
		{Model: "HDD2", Device: "sdc1", Rotational: true, Idle: true}, {Model: "SSD", Device: "nvme"}}
	p := e.Sample(s)
	if p.CPUSource != "model" || !p.Estimated || !near(p.CPUW, 30) || !near(p.GPUW, 100) || !near(p.RestW, 45) {
		t.Fatalf("power = %+v", p)
	}
	if !near(p.WallW, (30+100+45)/0.8) {
		t.Fatalf("wall = %v", p.WallW)
	}
}

func TestRAPL(t *testing.T) {
	sys := t.TempDir()
	dir := filepath.Join(sys, "class/powercap/intel-rapl:0")
	write(t, filepath.Join(dir, "max_energy_range_uj"), "1000000000\n")
	write(t, filepath.Join(dir, "energy_uj"), "999000000\n")
	c := testCfg()
	c.Monitor.CPUPower = "auto"
	e := New(c, t.TempDir(), nil, WithRoots(t.TempDir(), sys))
	t0 := time.Unix(1700000000, 0)
	p := e.Sample(snap(t0, 0, 0))
	if p.CPUSource != "model" || !p.Estimated {
		t.Fatalf("first RAPL sample has no delta yet: modelled watts must say so: %+v", p)
	}
	write(t, filepath.Join(dir, "energy_uj"), "1000000\n") // wrapped: 1,000,000 + 1,000,000 µJ = 2 J over 2 s
	p = e.Sample(snap(t0.Add(2*time.Second), 0, 0))
	if !near(p.CPUW, 1) || p.CPUSource != "rapl" {
		t.Fatalf("wrapped watts = %+v", p)
	}
	c2 := testCfg()
	c2.Monitor.CPUPower = "auto"
	e2 := New(c2, t.TempDir(), nil, WithRoots(t.TempDir(), t.TempDir()))
	if p := e2.Sample(snap(t0, 0, 0)); p.CPUSource != "model" || e2.RAPLReadable() {
		t.Fatalf("unreadable RAPL must fall back to the model: %+v", p)
	}
}

func TestIntegrationMinutesTotalsSeries(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	clock := base
	e := New(testCfg(), dir, nil, WithClock(func() time.Time { return clock }))
	// 0 W CPU load → 30 W CPU, 0 W GPU, 40 W rest → (70)/0.8 = 87.5 W wall.
	for i := 0; i <= 8; i++ { // every 30 s for 4 minutes
		clock = base.Add(time.Duration(i) * 30 * time.Second)
		e.Sample(snap(clock, 0, 0))
	}
	// A 10 minute gap (daemon off) is not counted.
	clock = base.Add(14 * time.Minute)
	e.Sample(snap(clock, 0, 0))
	clock = clock.Add(30 * time.Second)
	e.Sample(snap(clock, 0, 0))
	e.Flush()
	tot := e.Totals()
	wantKWh := 87.5 * (240 + 30) / 3.6e6
	if !near(tot.Today.Wall, wantKWh) || !near(tot.MonthToDate.Wall, wantKWh) || !near(tot.Today.CPU, 30*270/3.6e6) {
		t.Fatalf("today = %+v, want wall %v", tot.Today, wantKWh)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "2026-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "\n"); n != 5 { // minutes 0-3 and 14
		t.Fatalf("rows on disk = %d:\n%s", n, raw)
	}
	pts := e.Series("minute", base.Add(-time.Hour))
	if len(pts) != 5 || !near(pts[0].Wall, 87.5) {
		t.Fatalf("minute series = %+v", pts)
	}
	hours := e.Series("hour", base.Add(-time.Hour))
	// The running hour and day average over the time elapsed so far, gaps
	// included as off (10:14:30 now: 870 s into the hour, 36870 s into the day).
	if len(hours) != 1 || !near(hours[0].Wall, 87.5*270/870) {
		t.Fatalf("hour series = %+v", hours)
	}
	if days := e.Series("day", base.AddDate(0, 0, -2)); len(days) != 1 || !near(days[0].Wall, 87.5*270/36870) {
		t.Fatalf("day series = %+v", days)
	}
	// A new process reads the history back.
	clock = base.Add(time.Hour)
	e2 := New(testCfg(), dir, nil, WithClock(func() time.Time { return clock }))
	e2.load()
	if got := e2.Totals(); !got.Loaded || !near(got.Today.Wall, wantKWh) {
		t.Fatalf("reloaded totals = %+v", got.Today)
	}
	// A finished hour averages over all of it.
	if hours := e2.Series("hour", base.Add(-time.Hour)); len(hours) != 1 || !near(hours[0].Wall, 87.5*270/3600) {
		t.Fatalf("finished hour = %+v", hours)
	}
}

func TestMinuteSplit(t *testing.T) {
	base := time.Date(2026, 11, 1, 10, 0, 45, 0, time.Local)
	e := New(testCfg(), t.TempDir(), nil, WithClock(func() time.Time { return base }))
	e.Sample(snap(base, 0, 0))
	e.Sample(snap(base.Add(30*time.Second), 0, 0)) // 15 s in minute 0, 15 s in minute 1
	e.Flush()
	if len(e.minutes) != 2 || e.minutes[0].Sec != 15 || e.minutes[1].Sec != 15 {
		t.Fatalf("minutes = %+v", e.minutes)
	}
}

func TestLoadSkipsOldAndCurrentRun(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local)
	old := time.Date(2025, 1, 10, 12, 0, 0, 0, time.Local)
	write(t, filepath.Join(dir, old.Format("2006-01")+".jsonl"), `{"t":`+strconv.FormatInt(old.Unix(), 10)+`,"cpu":1,"gpu":1,"rest":1,"wall":3,"sec":60}`+"\n")
	prev := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	before := now.Add(-time.Hour)
	write(t, filepath.Join(dir, "2026-10.jsonl"), `{"t":`+strconv.FormatInt(prev.Unix(), 10)+`,"cpu":100,"gpu":0,"rest":0,"wall":100,"sec":3600}`+"\n")
	write(t, filepath.Join(dir, "2026-11.jsonl"),
		`{"t":`+strconv.FormatInt(before.Unix(), 10)+`,"cpu":0,"gpu":0,"rest":0,"wall":3600,"sec":500}`+"\n"+"garbage\n")
	e := New(testCfg(), dir, nil, WithClock(func() time.Time { return now }))
	// This run flushes a row of the same minute before history loads: it is
	// counted live and must not be counted again from disk.
	e.mu.Lock()
	own := row{T: before.Unix(), Wall: 3600, Sec: 500}
	e.addRowLocked(own, true)
	e.appendRow(own)
	e.mu.Unlock()
	e.load()
	if _, err := os.Stat(filepath.Join(dir, old.Format("2006-01")+".jsonl")); !os.IsNotExist(err) {
		t.Fatal("months past history_months must be deleted")
	}
	tot := e.Totals()
	if !near(tot.Today.Wall, 1) { // 3600 W × 500 s twice = 1 kWh; this run's row is not counted twice
		t.Fatalf("today = %+v", tot.Today)
	}
	// The previous run's row and this run's row for one minute are one point.
	if pts := e.Series("minute", before.Add(-time.Minute)); len(pts) != 1 || !near(pts[0].Wall, 3600) {
		t.Fatalf("minute series = %+v", pts)
	}
	if tot.MonthsOfHistory != 1 || !near(tot.MonthlyAvg12.Wall, 0.1) {
		t.Fatalf("monthly avg = %+v (%d months)", tot.MonthlyAvg12, tot.MonthsOfHistory)
	}
	// Projection: 1 kWh so far, daily average over the last 7 days (1 kWh / 7) × days left.
	left := time.Date(2026, 12, 1, 0, 0, 0, 0, time.Local).Sub(now).Hours() / 24
	if want := 1 + 1.0/7*left; !near(tot.ProjectedMonthKWh, want) {
		t.Fatalf("projected = %v, want %v", tot.ProjectedMonthKWh, want)
	}
}

func TestSessionAttribution(t *testing.T) {
	proc, sys := t.TempDir(), t.TempDir()
	self := os.Getpid()
	write(t, filepath.Join(proc, strconv.Itoa(self), "cgroup"), "0::/user.slice/cerberus.service\n")
	write(t, filepath.Join(proc, "4242", "cgroup"), "0::/user.slice/agents.slice/run-u9.scope\n")
	stat := filepath.Join(sys, "fs/cgroup/user.slice/agents.slice/run-u9.scope/cpu.stat")
	write(t, stat, "usage_usec 1000000\nuser_usec 1\n")
	c := testCfg()
	e := New(c, t.TempDir(), nil, WithRoots(proc, sys), WithSessions(func() map[string]int { return map[string]int{"s1": 4242} }))
	t0 := time.Unix(1700000000, 0)
	s := snap(t0, 100, 0) // 100% → 142 W modelled
	s.CPU.BusyTicks = 1000
	e.Sample(s)
	write(t, stat, "usage_usec 1500000\n") // +0.5 s of CPU
	s2 := snap(t0.Add(10*time.Second), 100, 0)
	s2.CPU.BusyTicks = 1200 // +200 ticks = 2 s busy → share 25%
	e.Sample(s2)
	cpu, gpuJ := e.SessionEnergy("s1")
	if want := 0.25 * 142 * 10; !near(cpu, want) || gpuJ != 0 {
		t.Fatalf("cpu J = %v, want %v", cpu, want)
	}
	e.OnUsage(usage.Record{SessionID: "s1", EnergyJ: 500})
	e.OnUsage(usage.Record{SessionID: "", EnergyJ: 500})
	if _, g := e.SessionEnergy("s1"); g != 500 {
		t.Fatalf("gpu J = %v", g)
	}
	wall, _ := e.SessionCost("s1")
	if !near(wall, (355+500)/0.8) {
		t.Fatalf("wall J = %v", wall)
	}
	e.Flush()
	e2 := New(c, e.dir, nil)
	got := e2.loadSessions()["s1"]
	if got == nil || !near(got.CPUJ, 355) || got.GPUJ != 500 {
		t.Fatalf("persisted = %+v", got)
	}
}

func TestSessionTreeFallback(t *testing.T) {
	proc := t.TempDir()
	self := os.Getpid()
	cg := "0::/user.slice/cerberus.service\n"
	write(t, filepath.Join(proc, strconv.Itoa(self), "cgroup"), cg)
	mk := func(pid, ppid int, ticks int) {
		write(t, filepath.Join(proc, strconv.Itoa(pid), "stat"), strconv.Itoa(pid)+" (x) S "+strconv.Itoa(ppid)+" 0 0 0 0 0 0 0 0 0 "+strconv.Itoa(ticks)+" 0 0 0 20 0 1 0 5\n")
		write(t, filepath.Join(proc, strconv.Itoa(pid), "cgroup"), cg)
	}
	mk(10, 1, 50)
	mk(11, 10, 25)
	mk(12, 1, 999)
	e := New(testCfg(), t.TempDir(), nil, WithRoots(proc, t.TempDir()))
	u, ok := e.sessionUsec("s", 10, e.procTable)
	if !ok || u != 750000 {
		t.Fatalf("tree usec = %d %v", u, ok)
	}
	// Reaped children's time (cutime/cstime) stays in the tree.
	write(t, filepath.Join(proc, "10", "stat"), "10 (x) S 1 0 0 0 0 0 0 0 0 0 50 0 40 0 20 0 1 0 5\n")
	if u, _ := treeUsec(e.procTable(), 10); u != 1150000 {
		t.Fatalf("with reaped children = %d", u)
	}
	// Two sessions without their own cgroup share one scan per sample.
	e2 := New(testCfg(), t.TempDir(), nil, WithRoots(proc, t.TempDir()),
		WithSessions(func() map[string]int { return map[string]int{"a": 10, "b": 12} }))
	t0 := time.Unix(1700000000, 0)
	s1 := snap(t0, 100, 0)
	s1.CPU.BusyTicks = 1000
	e2.Sample(s1)
	mk(12, 1, 1099) // +100 ticks
	s2 := snap(t0.Add(10*time.Second), 100, 0)
	s2.CPU.BusyTicks = 1400
	e2.Sample(s2)
	if e2.scans != 2 {
		t.Fatalf("scans = %d, want one per sample", e2.scans)
	}
	if b, _ := e2.SessionEnergy("b"); !near(b, 0.25*142*10) {
		t.Fatalf("session b cpu J = %v", b)
	}
}

func TestBillSummary(t *testing.T) {
	c := testCfg()
	c.Tariff.HouseholdKWhMonth = 200
	e := New(c, t.TempDir(), nil)
	b := e.Bill(Totals{Today: KWh{Wall: 1}, MonthToDate: KWh{Wall: 10}, ProjectedMonthKWh: 30})
	per := 4.054 * 1.16
	if !near(b.Marginal.PerKWh, per) || !near(b.TodayCost, per) || !near(b.MonthCost, 10*per) || !near(b.ProjectedMonthCost, 30*per) {
		t.Fatalf("bill = %+v", b)
	}
	if b.Currency != "MXN" || b.TariffName == "" || b.Verified {
		t.Fatalf("labels = %+v", b)
	}
}

func TestModelEnergy(t *testing.T) {
	e := New(testCfg(), t.TempDir(), nil, WithUsage(func(since time.Time) []usage.Agg {
		return []usage.Agg{{Model: "a", EnergyJ: 10}, {Model: "b"}}
	}))
	m := e.ModelEnergy(time.Now())
	if m["a"] != 10 || len(m) != 1 {
		t.Fatalf("model energy = %v", m)
	}
}

func TestProjectionEarlyInRun(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	clock := base
	e := New(testCfg(), t.TempDir(), nil, WithClock(func() time.Time { return clock }))
	for i := 1; i <= 4; i++ {
		e.Sample(snap(clock, 0, 0))
		clock = base.Add(time.Duration(i) * 30 * time.Second)
	}
	e.Sample(snap(clock, 0, 0))
	tot := e.Totals()
	// 87.5 W average → 2.1 kWh/day, projected over the rest of the month.
	left := time.Date(2026, 12, 1, 0, 0, 0, 0, time.Local).Sub(clock).Hours() / 24
	want := tot.MonthToDate.Wall + 87.5*24/1000*left
	if math.Abs(tot.ProjectedMonthKWh-want)/want > 0.01 {
		t.Fatalf("projected = %v, want ≈ %v", tot.ProjectedMonthKWh, want)
	}
}

func TestSpinningDiskCountedOncePerDrive(t *testing.T) {
	ds := []sysmon.Disk{{Model: "HDD", Device: "sdb1", Parent: "sdb", Rotational: true},
		{Model: "HDD", Device: "sdb2", Parent: "sdb", Rotational: true},
		{Model: "HDD", Device: "sdc1", Parent: "sdc", Rotational: true}}
	if n := spinningDisks(ds); n != 2 {
		t.Fatalf("spinning drives = %d, want 2 (two partitions of sdb are one drive)", n)
	}
}

func TestDaySeriesWholeFirstDay(t *testing.T) {
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
	clock := day.AddDate(0, 0, 3)
	e := New(testCfg(), "", nil, WithClock(func() time.Time { return clock }))
	e.mu.Lock()
	for h := 0; h < 24; h++ {
		e.addRowLocked(row{T: day.Add(time.Duration(h) * time.Hour).Unix(), Wall: 100, Sec: 3600}, false)
	}
	e.mu.Unlock()
	pts := e.Series("day", day.Add(18*time.Hour+22*time.Minute))
	if len(pts) != 1 || !pts[0].T.Equal(day) || !near(pts[0].Wall, 100*86400/day.AddDate(0, 0, 1).Sub(day).Seconds()) {
		t.Fatalf("first day = %+v, want its whole-day average", pts)
	}
}

func TestLocalHourHalfHourZone(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	at := time.Date(2026, 11, 1, 0, 10, 0, 0, ist)
	if got, want := localHour(at), time.Date(2026, 11, 1, 0, 0, 0, 0, ist).Unix(); got != want {
		t.Fatalf("localHour = %v, want %v (the local midnight hour)", time.Unix(got, 0).In(ist), time.Unix(want, 0).In(ist))
	}
	before := time.Date(2026, 10, 31, 23, 59, 0, 0, ist)
	if got, want := localHour(before), time.Date(2026, 10, 31, 23, 0, 0, 0, ist).Unix(); got != want {
		t.Fatalf("localHour before midnight = %v", time.Unix(got, 0).In(ist))
	}
}

func TestOldMonthsExpireWhileRunning(t *testing.T) {
	dir := t.TempDir()
	c := testCfg()
	c.Monitor.HistoryMonths = 2
	oct := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	write(t, filepath.Join(dir, "2026-10.jsonl"), `{"t":`+strconv.FormatInt(oct.Unix(), 10)+`,"cpu":0,"gpu":0,"rest":0,"wall":100,"sec":60}`+"\n")
	clock := time.Date(2026, 11, 30, 23, 0, 0, 0, time.Local)
	e := New(c, dir, nil, WithClock(func() time.Time { return clock }))
	e.load()
	if len(e.hours) != 1 {
		t.Fatalf("hours = %d", len(e.hours))
	}
	clock = time.Date(2026, 12, 1, 0, 5, 0, 0, time.Local)
	e.Sample(snap(clock, 0, 0))
	e.Sample(snap(clock.Add(30*time.Second), 0, 0))
	e.Sample(snap(clock.Add(70*time.Second), 0, 0)) // crosses a minute: a December row is written
	if _, err := os.Stat(filepath.Join(dir, "2026-10.jsonl")); !os.IsNotExist(err) {
		t.Fatal("October must expire once December starts (history_months = 2)")
	}
	for h := range e.hours {
		if time.Unix(h, 0).Before(time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)) {
			t.Fatal("expired hours stay in memory")
		}
	}
}

func TestForgetAndKnown(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sessions.json"), `{"gone":{"CPUJ":5,"GPUJ":0},"kept":{"CPUJ":7,"GPUJ":1}}`)
	e := New(testCfg(), dir, nil, WithKnown(func(id string) bool { return id == "kept" }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := e.SessionEnergy("kept"); c == 7 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c, _ := e.SessionEnergy("gone"); c != 0 {
		t.Fatal("a session that no longer exists keeps its record")
	}
	e.Forget("kept")
	cancel()
	<-done
	if got := e.loadSessions(); len(got) != 0 {
		t.Fatalf("persisted after forget = %v", got)
	}
}

func TestLeaderboardUsesAttribution(t *testing.T) {
	c := testCfg()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	e := New(c, t.TempDir(), nil, WithClock(func() time.Time { return now }))
	u, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	r := usage.Record{Time: now.Add(-time.Hour), SessionID: "s1", Model: "m", EnergyJ: 0.8 * 3.6e6}
	u.Add(r)
	e.OnUsage(r) // 0.8 MJ DC charged to s1 → 1 kWh at the wall (PSU 0.8)
	b, err := e.Leaderboard(u, []usage.SessionInfo{{ID: "s1", Start: now.Add(-2 * time.Hour), End: now}}, "today", "session", "energy", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rows) != 1 || !near(b.Rows[0].EnergyKWh, 1) || b.Currency != c.Tariff.Currency {
		t.Fatalf("board = %+v", b)
	}
	if want := b.PricePerKWh; !near(b.Rows[0].EnergyCost, want) {
		t.Fatalf("energy cost %v, want %v", b.Rows[0].EnergyCost, want)
	}
}
