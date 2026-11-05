package sysmon

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clkTck is USER_HZ, 100 on every Linux build that matters.
const clkTck = 100

// hddIdleAfter: a spinning drive with no I/O for this long counts as idle.
const hddIdleAfter = 5 * time.Minute

type cpuTimes struct{ busy, total uint64 }

type ioCount struct{ readB, writeB, ticksMs uint64 }

type netCount struct{ rx, tx uint64 }

// sampler holds the previous counters; used only by the sampling goroutine.
type sampler struct {
	m *Monitor

	prevAt   time.Time
	prevCPU  []cpuTimes // [0] = all CPUs, then each CPU
	prevIO   map[string]ioCount
	prevNet  map[string]netCount
	lastIO   map[string]time.Time // whole-disk device → last time it did I/O
	cpuModel string
	maxMHz   int
	tempPath string // hwmon temp input for the CPU ("" = none, "-" = not looked up yet)
	btime    time.Time

	procs procState
}

func newSampler(m *Monitor) sampler {
	return sampler{m: m, prevIO: map[string]ioCount{}, prevNet: map[string]netCount{}, lastIO: map[string]time.Time{},
		tempPath: "-", procs: newProcState()}
}

func (s *sampler) path(root string, parts ...string) string {
	return filepath.Join(append([]string{root}, parts...)...)
}

func (s *sampler) procPath(parts ...string) string { return s.path(s.m.proc, parts...) }
func (s *sampler) sysPath(parts ...string) string  { return s.path(s.m.sys, parts...) }

// sample reads everything except processes.
func (s *sampler) sample(ctx context.Context, active bool) Snapshot {
	now := s.m.now()
	dt := now.Sub(s.prevAt).Seconds()
	if s.prevAt.IsZero() || dt <= 0 {
		dt = 0
	}
	snap := Snapshot{Time: now, Active: active}
	snap.CPU = s.readCPU()
	snap.Mem = s.readMem()
	snap.Net = s.readNet(dt)
	snap.Disks = s.readDisks(now, dt)
	snap.Uptime = s.readUptime()
	if g := s.m.gpu; g != nil {
		if active {
			want := 3 * time.Duration(s.m.cfg.ActiveIntervalMs) * time.Millisecond / 2
			if gs := g.Snapshot(); now.Sub(gs.Time) > want {
				g.Refresh(ctx)
			}
		}
		snap.GPU = g.Snapshot()
	}
	s.prevAt = now
	return snap
}

// ---------- CPU ----------

func (s *sampler) readCPU() CPU {
	var c CPU
	raw, err := os.ReadFile(s.procPath("stat"))
	if err != nil {
		return c
	}
	var cur []cpuTimes
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "btime ") && s.btime.IsZero() {
			if v, err := strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64); err == nil {
				s.btime = time.Unix(v, 0)
			}
		}
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		var vals [10]uint64
		for i := 1; i < len(f) && i <= 10; i++ {
			vals[i-1], _ = strconv.ParseUint(f[i], 10, 64)
		}
		// user nice system idle iowait irq softirq steal guest guest_nice;
		// guest time is already inside user/nice.
		idle := vals[3] + vals[4]
		total := vals[0] + vals[1] + vals[2] + vals[3] + vals[4] + vals[5] + vals[6] + vals[7]
		cur = append(cur, cpuTimes{busy: total - idle, total: total})
	}
	if len(cur) == 0 {
		return c
	}
	c.BusyTicks = cur[0].busy
	c.Threads = len(cur) - 1
	pct := func(i int) float64 {
		if i >= len(s.prevCPU) {
			return 0
		}
		db, dtot := cur[i].busy-s.prevCPU[i].busy, cur[i].total-s.prevCPU[i].total
		if cur[i].total < s.prevCPU[i].total || dtot == 0 {
			return 0
		}
		return clampPct(float64(db) * 100 / float64(dtot))
	}
	c.Pct = pct(0)
	c.Cores = make([]float64, c.Threads)
	c.MHz = make([]int, c.Threads)
	for i := 0; i < c.Threads; i++ {
		c.Cores[i] = pct(i + 1)
		c.MHz[i] = readInt(s.sysPath("devices/system/cpu", "cpu"+strconv.Itoa(i), "cpufreq/scaling_cur_freq")) / 1000
	}
	s.prevCPU = cur
	if s.maxMHz == 0 {
		for i := 0; i < c.Threads; i++ {
			s.maxMHz = max(s.maxMHz, readInt(s.sysPath("devices/system/cpu", "cpu"+strconv.Itoa(i), "cpufreq/cpuinfo_max_freq"))/1000)
		}
	}
	c.MaxMHz = s.maxMHz
	if s.cpuModel == "" {
		s.cpuModel = s.readCPUModel()
	}
	c.Model = s.cpuModel
	if raw, err := os.ReadFile(s.procPath("loadavg")); err == nil {
		f := strings.Fields(string(raw))
		for i := 0; i < 3 && i < len(f); i++ {
			c.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	c.TempC = s.readCPUTemp()
	return c
}

func (s *sampler) readCPUModel() string {
	f, err := os.Open(s.procPath("cpuinfo"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// readCPUTemp finds the CPU sensor once (k10temp, zenpower, coretemp…),
// preferring the Tctl/Tdie/Package label, then reads it.
func (s *sampler) readCPUTemp() float64 {
	if s.tempPath == "-" {
		s.tempPath = s.findCPUTemp()
	}
	if s.tempPath == "" {
		return 0
	}
	return float64(readInt(s.tempPath)) / 1000
}

func (s *sampler) findCPUTemp() string {
	dirs, _ := filepath.Glob(s.sysPath("class/hwmon", "hwmon*"))
	sort.Strings(dirs)
	for _, d := range dirs {
		switch readTrim(filepath.Join(d, "name")) {
		case "k10temp", "zenpower", "coretemp", "cpu_thermal", "k8temp":
		default:
			continue
		}
		labels, _ := filepath.Glob(filepath.Join(d, "temp*_label"))
		sort.Strings(labels)
		for _, l := range labels {
			switch readTrim(l) {
			case "Tctl", "Tdie", "Package id 0", "CPU":
				return strings.TrimSuffix(l, "_label") + "_input"
			}
		}
		if p := filepath.Join(d, "temp1_input"); exists(p) {
			return p
		}
	}
	return ""
}

// ---------- memory ----------

func (s *sampler) readMem() Mem {
	var m Mem
	raw, err := os.ReadFile(s.procPath("meminfo"))
	if err != nil {
		return m
	}
	kv := map[string]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.Atoi(f[0])
		kv[k] = n / 1024
	}
	m.TotalMiB, m.AvailMiB = kv["MemTotal"], kv["MemAvailable"]
	m.CachedMiB = kv["Cached"] + kv["Buffers"]
	m.SwapTotalMiB = kv["SwapTotal"]
	m.SwapUsedMiB = kv["SwapTotal"] - kv["SwapFree"]
	zs, _ := filepath.Glob(s.sysPath("block", "zram*", "mm_stat"))
	for _, z := range zs {
		f := strings.Fields(readTrim(z))
		if len(f) >= 2 {
			o, _ := strconv.ParseUint(f[0], 10, 64)
			c, _ := strconv.ParseUint(f[1], 10, 64)
			m.ZramOrigMiB += int(o >> 20)
			m.ZramComprMiB += int(c >> 20)
		}
	}
	return m
}

// ---------- network ----------

func (s *sampler) readNet(dt float64) []Net {
	raw, err := os.ReadFile(s.procPath("net/dev"))
	if err != nil {
		return nil
	}
	cur := map[string]netCount{}
	var out []Net
	for _, line := range strings.Split(string(raw), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "lo" || len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		cur[name] = netCount{rx, tx}
		n := Net{Name: name}
		if p, ok := s.prevNet[name]; ok && dt > 0 && rx >= p.rx && tx >= p.tx {
			n.RxBps, n.TxBps = float64(rx-p.rx)/dt, float64(tx-p.tx)/dt
		}
		out = append(out, n)
	}
	s.prevNet = cur
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *sampler) readUptime() time.Duration {
	f := strings.Fields(readTrim(s.procPath("uptime")))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return time.Duration(v * float64(time.Second))
}

// ---------- disks ----------

// realFS are the filesystems worth showing (no proc, tmpfs, overlay…).
var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true, "zfs": true, "bcachefs": true,
	"jfs": true, "reiserfs": true, "nilfs2": true, "hfsplus": true, "apfs": true}

type mount struct {
	key, dev, source, fstype string
	points                   []string
}

func (s *sampler) readMounts() []*mount {
	raw, err := os.ReadFile(s.procPath("self/mountinfo"))
	if err != nil {
		return nil
	}
	byKey := map[string]*mount{}
	var order []*mount
	for _, line := range strings.Split(string(raw), "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		a, b := strings.Fields(pre), strings.Fields(post)
		if len(a) < 5 || len(b) < 2 || !realFS[b[0]] {
			continue
		}
		majmin, point, source := a[2], unescapeMount(a[4]), b[1]
		dev := s.devName(majmin, source)
		key := source
		if !strings.HasPrefix(source, "/dev/") {
			key = majmin
		}
		mt := byKey[key]
		if mt == nil {
			mt = &mount{key: key, dev: dev, source: source, fstype: b[0]}
			byKey[key] = mt
			order = append(order, mt)
		}
		mt.points = append(mt.points, point)
	}
	return order
}

// devName maps a mount to its block device name (nvme0n1p2, dm-0, sda1).
func (s *sampler) devName(majmin, source string) string {
	if !strings.HasPrefix(majmin, "0:") {
		if t, err := os.Readlink(s.sysPath("dev/block", majmin)); err == nil {
			return filepath.Base(t)
		}
	}
	if strings.HasPrefix(source, "/dev/") {
		if r, err := filepath.EvalSymlinks(source); err == nil {
			return filepath.Base(r)
		}
		return filepath.Base(source)
	}
	return ""
}

// parentDisk returns the whole-disk device of a partition or mapper device.
func (s *sampler) parentDisk(dev string) string {
	if dev == "" {
		return ""
	}
	if exists(s.sysPath("block", dev)) {
		if slaves, _ := os.ReadDir(s.sysPath("block", dev, "slaves")); len(slaves) > 0 {
			return s.parentDisk(slaves[0].Name()) // dm-crypt / LVM: follow the first backing device
		}
		return dev
	}
	if t, err := filepath.EvalSymlinks(s.sysPath("class/block", dev)); err == nil && exists(filepath.Join(t, "partition")) {
		return filepath.Base(filepath.Dir(t))
	}
	return dev
}

func (s *sampler) readDiskStats() map[string]ioCount {
	raw, err := os.ReadFile(s.procPath("diskstats"))
	if err != nil {
		return nil
	}
	out := map[string]ioCount{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		rs, _ := strconv.ParseUint(f[5], 10, 64)
		ws, _ := strconv.ParseUint(f[9], 10, 64)
		tk, _ := strconv.ParseUint(f[12], 10, 64)
		out[f[2]] = ioCount{readB: rs * 512, writeB: ws * 512, ticksMs: tk}
	}
	return out
}

func (s *sampler) readDisks(now time.Time, dt float64) []Disk {
	mounts := s.readMounts()
	stats := s.readDiskStats()
	temps := s.nvmeTemps()
	// Whole-disk activity, for HDD idleness.
	for dev, c := range stats {
		if p, ok := s.prevIO[dev]; ok && (c.readB != p.readB || c.writeB != p.writeB) {
			s.lastIO[dev] = now
		} else if _, seen := s.lastIO[dev]; !seen {
			s.lastIO[dev] = now // unknown history: assume recently active
		}
	}
	var out []Disk
	for _, mt := range mounts {
		d := Disk{Device: mt.dev, Mounts: mt.points, FSType: mt.fstype}
		if size, free, avail, ok := statfsFn(mt.points[0]); ok {
			d.SizeB, d.FreeB = size, avail
			if size >= free {
				d.UsedB = size - free
			}
			if d.UsedB+d.FreeB > 0 {
				d.UsedPct = float64(d.UsedB) * 100 / float64(d.UsedB+d.FreeB)
			}
		}
		parent := s.parentDisk(mt.dev)
		d.Parent = parent
		if parent != "" {
			d.Model = readTrim(s.sysPath("block", parent, "device/model"))
			d.Rotational = readTrim(s.sysPath("block", parent, "queue/rotational")) == "1"
			d.TempC = temps[parent]
		}
		if c, ok := stats[mt.dev]; ok {
			if p, ok := s.prevIO[mt.dev]; ok && dt > 0 && c.readB >= p.readB && c.writeB >= p.writeB && c.ticksMs >= p.ticksMs {
				d.ReadBps = float64(c.readB-p.readB) / dt
				d.WriteBps = float64(c.writeB-p.writeB) / dt
				d.BusyPct = clampPct(float64(c.ticksMs-p.ticksMs) / (dt * 10))
			}
		}
		if d.Rotational && parent != "" {
			if t, ok := s.lastIO[parent]; ok && now.Sub(t) >= hddIdleAfter {
				d.Idle = true
			}
		}
		out = append(out, d)
	}
	s.prevIO = stats
	return out
}

// statfsFn reports a mount's size, free and available bytes (tests replace it).
var statfsFn = statfs

// nvmeTemps maps a whole-disk device (nvme0n1) to its NVMe temperature.
func (s *sampler) nvmeTemps() map[string]float64 {
	out := map[string]float64{}
	dirs, _ := filepath.Glob(s.sysPath("class/hwmon", "hwmon*"))
	for _, d := range dirs {
		if readTrim(filepath.Join(d, "name")) != "nvme" {
			continue
		}
		t, err := filepath.EvalSymlinks(filepath.Join(d, "device"))
		if err != nil {
			continue
		}
		ctrl := filepath.Base(t) // nvme0
		c := float64(readInt(filepath.Join(d, "temp1_input"))) / 1000
		blocks, _ := filepath.Glob(s.sysPath("block", ctrl+"n*"))
		for _, b := range blocks {
			out[filepath.Base(b)] = c
		}
	}
	return out
}

// ---------- helpers ----------

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(b))
}

func readInt(p string) int {
	n, _ := strconv.Atoi(readTrim(p))
	return n
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func clampPct(v float64) float64 { return min(max(v, 0), 100) }

// unescapeMount undoes mountinfo's octal escapes (\040 = space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
