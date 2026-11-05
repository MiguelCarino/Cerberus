package sysmon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/gpu"
)

type tree struct {
	t    *testing.T
	proc string
	sys  string
}

func newTree(t *testing.T) *tree {
	d := t.TempDir()
	tr := &tree{t: t, proc: filepath.Join(d, "proc"), sys: filepath.Join(d, "sys")}
	tr.base()
	return tr
}

func (tr *tree) write(p, content string) {
	tr.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) link(target, p string) {
	tr.t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.Symlink(target, p); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) stat(user, idle uint64) {
	tr.write(filepath.Join(tr.proc, "stat"), "cpu  "+u(user*2)+" 0 0 "+u(idle*2)+" 0 0 0 0 0 0\n"+
		"cpu0 "+u(user)+" 0 0 "+u(idle)+" 0 0 0 0 0 0\n"+
		"cpu1 "+u(user)+" 0 0 "+u(idle)+" 0 0 0 0 0 0\nbtime 1700000000\n")
}

func u(v uint64) string { return strconv.FormatUint(v, 10) }

func (tr *tree) base() {
	P, S := tr.proc, tr.sys
	tr.stat(100, 900)
	tr.write(filepath.Join(P, "meminfo"), "MemTotal:       32768000 kB\nMemAvailable:   16384000 kB\nCached:          4096000 kB\nBuffers:         1024000 kB\nSwapTotal:       8388608 kB\nSwapFree:        7340032 kB\n")
	tr.write(filepath.Join(P, "loadavg"), "1.50 1.00 0.50 2/900 1234\n")
	tr.write(filepath.Join(P, "cpuinfo"), "processor\t: 0\nmodel name\t: Test CPU 9000\n")
	tr.write(filepath.Join(P, "uptime"), "3600.50 7000.00\n")
	tr.write(filepath.Join(P, "net/dev"), "Inter-|   Receive\n face |bytes packets errs drop fifo frame compressed multicast|bytes\n    lo: 100 1 0 0 0 0 0 0 100 1 0 0 0 0 0 0\n  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n")
	tr.write(filepath.Join(P, "diskstats"), "259 2 nvme0n1p2 10 0 2048 0 10 0 4096 0 0 100 0 0 0 0 0\n259 0 nvme0n1 10 0 2048 0 10 0 4096 0 0 100 0 0 0 0 0\n8 16 sdb 5 0 100 0 5 0 100 0 0 10 0 0 0 0 0\n8 17 sdb1 5 0 100 0 5 0 100 0 0 10 0 0 0 0 0\n")
	tr.write(filepath.Join(P, "self/mountinfo"),
		"29 1 0:34 /root / rw,relatime shared:1 - btrfs /dev/nvme0n1p2 rw\n"+
			"30 29 0:35 /home /home rw,relatime shared:2 - btrfs /dev/nvme0n1p2 rw\n"+
			"31 29 0:5 / /proc rw - proc proc rw\n"+
			"32 29 8:17 / /mnt/My\\040Disk rw - ext4 /dev/sdb1 rw\n"+
			"33 29 0:40 / /tmp rw - tmpfs tmpfs rw\n")
	for i := 0; i < 2; i++ {
		c := filepath.Join(S, "devices/system/cpu", "cpu"+strconv.Itoa(i), "cpufreq")
		tr.write(filepath.Join(c, "scaling_cur_freq"), "3000000\n")
		tr.write(filepath.Join(c, "cpuinfo_max_freq"), "5000000\n")
	}
	tr.write(filepath.Join(S, "class/hwmon/hwmon0/name"), "k10temp\n")
	tr.write(filepath.Join(S, "class/hwmon/hwmon0/temp1_label"), "Tctl\n")
	tr.write(filepath.Join(S, "class/hwmon/hwmon0/temp1_input"), "55250\n")
	tr.write(filepath.Join(S, "devices/pci0000:00/nvme/nvme0/model"), "x")
	tr.write(filepath.Join(S, "class/hwmon/hwmon1/name"), "nvme\n")
	tr.write(filepath.Join(S, "class/hwmon/hwmon1/temp1_input"), "41850\n")
	tr.link(filepath.Join(S, "devices/pci0000:00/nvme/nvme0"), filepath.Join(S, "class/hwmon/hwmon1/device"))
	tr.write(filepath.Join(S, "block/nvme0n1/device/model"), "FAST NVME 2TB   \n")
	tr.write(filepath.Join(S, "block/nvme0n1/queue/rotational"), "0\n")
	tr.write(filepath.Join(S, "block/nvme0n1/nvme0n1p2/partition"), "2\n")
	tr.link(filepath.Join(S, "block/nvme0n1/nvme0n1p2"), filepath.Join(S, "class/block/nvme0n1p2"))
	tr.write(filepath.Join(S, "block/sdb/device/model"), "BIG HDD\n")
	tr.write(filepath.Join(S, "block/sdb/queue/rotational"), "1\n")
	tr.write(filepath.Join(S, "block/sdb/sdb1/partition"), "1\n")
	tr.link(filepath.Join(S, "block/sdb/sdb1"), filepath.Join(S, "class/block/sdb1"))
	tr.link(filepath.Join(S, "block/sdb/sdb1"), filepath.Join(S, "dev/block/8:17"))
	tr.write(filepath.Join(S, "block/zram0/mm_stat"), "1048576000 524288000 0 0 0 0 0\n")
}

func (tr *tree) pid(pid, ppid int, comm string, ticks uint64, uid int, cgroup, environ string) {
	d := filepath.Join(tr.proc, strconv.Itoa(pid))
	// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime prio nice threads itreal starttime
	tr.write(filepath.Join(d, "stat"), strconv.Itoa(pid)+" ("+comm+") S "+strconv.Itoa(ppid)+" 1 1 0 -1 0 0 0 0 0 "+u(ticks)+" 0 0 0 20 0 1 0 500 0 0\n")
	tr.write(filepath.Join(d, "statm"), "1000 2560 100 0 0 0 0\n") // 2560 pages = 10 MiB at 4 KiB
	tr.write(filepath.Join(d, "status"), "Name:\t"+comm+"\nUid:\t"+strconv.Itoa(uid)+"\t"+strconv.Itoa(uid)+"\n")
	tr.write(filepath.Join(d, "cmdline"), comm+"\x00--flag\x00")
	tr.write(filepath.Join(d, "cgroup"), "0::"+cgroup+"\n")
	tr.write(filepath.Join(d, "environ"), environ)
}

type fakeGPU struct{ snap gpu.Snapshot }

func (f *fakeGPU) Snapshot() gpu.Snapshot      { return f.snap }
func (f *fakeGPU) Refresh(ctx context.Context) {}

func newTestMonitor(t *testing.T, tr *tree, clock *time.Time) *Monitor {
	cfg := config.Defaults()
	m := New(cfg, &fakeGPU{}, nil, WithRoots(tr.proc, tr.sys),
		WithRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) { return nil, errors.New("none") }))
	m.now = func() time.Time { return *clock }
	m.st = newSampler(m)
	return m
}

func withStatfs(t *testing.T) {
	old := statfsFn
	statfsFn = func(p string) (uint64, uint64, uint64, bool) {
		switch p {
		case "/":
			return 1000 << 30, 600 << 30, 590 << 30, true
		case "/mnt/My Disk":
			return 8000 << 30, 100 << 30, 100 << 30, true
		}
		return 0, 0, 0, false
	}
	t.Cleanup(func() { statfsFn = old })
}

func TestCPUMemNet(t *testing.T) {
	tr := newTree(t)
	now := time.Unix(1700001000, 0)
	m := newTestMonitor(t, tr, &now)
	m.Sample(context.Background(), false)
	tr.stat(150, 950) // +50 busy, +50 idle per CPU → 50%
	tr.write(filepath.Join(tr.proc, "net/dev"), "  eth0: 3000 10 0 0 0 0 0 0 6000 20 0 0 0 0 0 0\n")
	now = now.Add(2 * time.Second)
	s := m.Sample(context.Background(), false)
	if s.CPU.Pct != 50 || len(s.CPU.Cores) != 2 || s.CPU.Cores[1] != 50 {
		t.Fatalf("cpu = %+v", s.CPU)
	}
	if s.CPU.MHz[0] != 3000 || s.CPU.MaxMHz != 5000 || s.CPU.TempC != 55.25 || s.CPU.Model != "Test CPU 9000" || s.CPU.Load[0] != 1.5 {
		t.Fatalf("cpu details = %+v", s.CPU)
	}
	if s.CPU.BusyTicks != 300 {
		t.Fatalf("busy ticks = %d", s.CPU.BusyTicks)
	}
	if s.Mem.TotalMiB != 32000 || s.Mem.AvailMiB != 16000 || s.Mem.CachedMiB != 5000 || s.Mem.SwapUsedMiB != 1024 || s.Mem.ZramOrigMiB != 1000 || s.Mem.ZramComprMiB != 500 {
		t.Fatalf("mem = %+v", s.Mem)
	}
	if len(s.Net) != 1 || s.Net[0].Name != "eth0" || s.Net[0].RxBps != 1000 || s.Net[0].TxBps != 2000 {
		t.Fatalf("net = %+v", s.Net)
	}
	if s.Uptime != 3600500*time.Millisecond {
		t.Fatalf("uptime = %v", s.Uptime)
	}
}

func TestDisks(t *testing.T) {
	withStatfs(t)
	tr := newTree(t)
	now := time.Unix(1700001000, 0)
	m := newTestMonitor(t, tr, &now)
	m.Sample(context.Background(), false)
	tr.write(filepath.Join(tr.proc, "diskstats"), "259 2 nvme0n1p2 10 0 4096 0 10 0 8192 0 0 600 0 0 0 0 0\n259 0 nvme0n1 10 0 4096 0 10 0 8192 0 0 600 0 0 0 0 0\n8 16 sdb 5 0 100 0 5 0 100 0 0 10 0 0 0 0 0\n8 17 sdb1 5 0 100 0 5 0 100 0 0 10 0 0 0 0 0\n")
	now = now.Add(2 * time.Second)
	s := m.Sample(context.Background(), false)
	if len(s.Disks) != 2 {
		t.Fatalf("disks = %+v", s.Disks)
	}
	root := s.Disks[0]
	if root.Device != "nvme0n1p2" || strings.Join(root.Mounts, ",") != "/,/home" || root.Model != "FAST NVME 2TB" || root.Rotational || root.TempC != 41.85 {
		t.Fatalf("root disk = %+v", root)
	}
	if root.SizeB != 1000<<30 || root.UsedB != 400<<30 || root.FreeB != 590<<30 || root.UsedPct < 40 || root.UsedPct > 41 {
		t.Fatalf("root sizes = %+v", root)
	}
	if root.ReadBps != 2048*512/2 || root.WriteBps != 4096*512/2 || root.BusyPct != 25 {
		t.Fatalf("root io = %+v", root)
	}
	hdd := s.Disks[1]
	if hdd.Device != "sdb1" || hdd.Mounts[0] != "/mnt/My Disk" || hdd.Model != "BIG HDD" || !hdd.Rotational || hdd.Idle {
		t.Fatalf("hdd = %+v", hdd)
	}
	now = now.Add(6 * time.Minute) // no I/O for over 5 minutes
	s = m.Sample(context.Background(), false)
	if !s.Disks[1].Idle || s.Disks[0].Idle {
		t.Fatalf("idle flags wrong: %+v", s.Disks)
	}
}

func TestGroupName(t *testing.T) {
	for _, c := range []struct{ cg, comm, want string }{
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/app-firefox-12345.scope", "firefox", "firefox"},
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/app-Hyprland-kitty@a1b2c3.service", "kitty", "kitty"},
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/app-flatpak-org.mozilla.firefox-4321.scope", "firefox", "org.mozilla.firefox"},
		{"/user.slice/user-1000.slice/user@1000.service/session.slice/pipewire.service", "pipewire", "pipewire"},
		{"/user.slice/user-1000.slice/user@1000.service/agents.slice/run-u12.scope", "bash", "agents"},
		{"/init.scope", "systemd", "systemd"},
		{"/user.slice/user-1000.slice/session-2.scope", "sshd", "sshd"},
		{"/system.slice/systemd-journald.service", "systemd-journal", "systemd-journald"},
		{"", "kworker", "kworker"},
	} {
		if got := groupName(c.cg, c.comm); got != c.want {
			t.Errorf("groupName(%q) = %q, want %q", c.cg, got, c.want)
		}
	}
}

func TestProcesses(t *testing.T) {
	tr := newTree(t)
	uid := os.Getuid()
	app := "/user.slice/user-1000.slice/user@1000.service/app.slice/"
	tr.pid(1, 0, "systemd", 0, 0, "/init.scope", "")
	tr.pid(2, 0, "kthreadd", 0, 0, "/", "")
	tr.pid(3, 2, "kworker/0:0", 0, 0, "/", "")
	tr.pid(100, 1, "Hyprland", 1000, uid, app+"wayland-wm@Hyprland.service", "")
	tr.pid(50, 1, "uwsm-session", 0, uid, "/user.slice", "") // compositor's parent
	tr.write(filepath.Join(tr.proc, "100/stat"), "100 (Hyprland) S 50 1 1 0 -1 0 0 0 0 0 1000 0 0 0 20 0 1 0 500 0 0\n")
	tr.pid(200, 1, "firefox", 500, uid, app+"app-firefox-777.scope", "")
	tr.pid(201, 200, "Isolated Web Co", 100, uid, app+"app-firefox-777.scope", "")
	tr.pid(300, 1, "cerberus", 10, uid, app+"cerberus.service", "")
	tr.pid(301, 300, "bash", 0, uid, app+"agents.slice/run-u5.scope", "CERBERUS_SESSION=s-1\x00PATH=/bin\x00")
	tr.pid(302, 301, "make", 0, uid, app+"agents.slice/run-u5.scope", "PATH=/bin\x00")
	tr.pid(400, 1, "llmster", 0, uid, app+"lmstudio.service", "")
	tr.pid(401, 400, "llama-server", 200, uid, app+"lmstudio.service", "")
	now := time.Unix(1700001000, 0)
	m := newTestMonitor(t, tr, &now)
	m.self = 300
	m.Sample(context.Background(), true)
	tr.pid(200, 1, "firefox", 700, uid, app+"app-firefox-777.scope", "") // +200 ticks over 2 s = 100%
	now = now.Add(2 * time.Second)
	m.Sample(context.Background(), true)
	ps := map[int]Proc{}
	for _, p := range m.Processes() {
		ps[p.PID] = p
	}
	if _, ok := ps[3]; ok {
		t.Fatal("kernel threads must not be listed")
	}
	ff := ps[200]
	if ff.CPUPct != 100 || ff.Group != "firefox" || ff.RSSMiB != 10 || ff.Cmd != "firefox --flag" || ff.Protected || ff.Cerberus {
		t.Fatalf("firefox = %+v", ff)
	}
	if ff.Started != time.Unix(1700000005, 0) {
		t.Fatalf("started = %v", ff.Started)
	}
	if !ps[100].Protected || !ps[50].Protected || !ps[1].Protected || !ps[300].Protected {
		t.Fatalf("protected flags: hypr=%v uwsm=%v init=%v self=%v", ps[100].Protected, ps[50].Protected, ps[1].Protected, ps[300].Protected)
	}
	if b := ps[301]; b.SessionID != "s-1" || !b.Cerberus || b.Group != "session s-1" {
		t.Fatalf("session shell = %+v", b)
	}
	if mk := ps[302]; mk.SessionID != "s-1" || !mk.Cerberus {
		t.Fatalf("session child must inherit the session: %+v", mk)
	}
	if !ps[401].Cerberus || !ps[400].Cerberus {
		t.Fatal("model servers must be marked")
	}
	groups := map[string]Group{}
	for _, g := range m.Groups() {
		groups[g.Name] = g
	}
	if g := groups["firefox"]; g.Count != 2 || g.CPU != 100 || g.RSSMiB != 20 {
		t.Fatalf("firefox group = %+v", g)
	}
	if g := groups["session s-1"]; g.Count != 2 || !g.Cerberus || g.SessionID != "s-1" {
		t.Fatalf("session group = %+v", g)
	}
	if g, ok := groups["kernel"]; !ok || g.Count != 2 {
		t.Fatalf("kernel group = %+v", g)
	}
	// Idle sampling forgets processes.
	m.Sample(context.Background(), false)
	if len(m.Processes()) != 0 || len(m.Groups()) != 0 {
		t.Fatal("idle sample must not keep processes")
	}
}

func TestTopProcsKeepsCerberus(t *testing.T) {
	var all []Proc
	for i := 0; i < 300; i++ {
		all = append(all, Proc{PID: i + 10, CPUPct: float64(300 - i)})
	}
	all = append(all, Proc{PID: 5, Cerberus: true})
	out := topProcs(all)
	if len(out) != maxProcs+1 {
		t.Fatalf("len = %d", len(out))
	}
	found := false
	for _, p := range out {
		found = found || p.PID == 5
	}
	if !found || out[0].PID != 10 {
		t.Fatal("ranking wrong")
	}
}

func TestParsePmon(t *testing.T) {
	out := []byte(`# gpu         pid   type     sm    mem    enc    dec    jpg    ofa     fb   command
# Idx           #    C/G      %      %      %      %      %      %     MB   name
    0       2684     G      3      1      -      -      -      -    210   Hyprland
    0      12345     C     87     40      -      -      -      -  18000   llama-server
    0      55555     G      -      -      -      -      -      -     40   firefox
`)
	mem, pct := parsePmon(out)
	if mem[12345] != 18000 || pct[12345] != 87 || mem[2684] != 210 || pct[2684] != 3 || mem[55555] != 40 || pct[55555] != 0 {
		t.Fatalf("mem=%v pct=%v", mem, pct)
	}
}

func TestWatchTouchLease(t *testing.T) {
	tr := newTree(t)
	now := time.Unix(1700001000, 0)
	m := newTestMonitor(t, tr, &now)
	if m.Active() {
		t.Fatal("starts idle")
	}
	rel := m.Watch()
	if !m.Active() {
		t.Fatal("watch activates")
	}
	rel()
	rel() // idempotent
	if !m.Active() {
		t.Fatal("lease keeps it active after release")
	}
	now = now.Add(61 * time.Second)
	if m.Active() {
		t.Fatal("lease expires after 60 s")
	}
	m.Touch()
	if !m.Active() {
		t.Fatal("touch activates")
	}
}

func TestRunUsesIntervalsAndWakes(t *testing.T) {
	tr := newTree(t)
	cfg := config.Defaults()
	cfg.Monitor.ActiveIntervalMs = 20
	cfg.Monitor.IdleIntervalSec = 3600
	m := New(cfg, nil, nil, WithRoots(tr.proc, tr.sys))
	samples := make(chan Snapshot, 64)
	m.AddHook(func(s Snapshot) { samples <- s })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	first := <-samples
	if first.Active {
		t.Fatal("first sample is idle")
	}
	select {
	case <-samples:
		t.Fatal("idle mode must not sample again soon")
	case <-time.After(100 * time.Millisecond):
	}
	rel := m.Watch()
	for i := 0; i < 3; i++ {
		select {
		case s := <-samples:
			if !s.Active {
				t.Fatal("watched samples are active")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("active sampling did not run")
		}
	}
	rel()
	cancel()
	<-done
}

func TestSignalAndRenice(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	m := New(config.Defaults(), nil, nil)
	if err := m.Signal(1, "term"); !errors.Is(err, ErrProtected) {
		t.Fatalf("pid 1: %v", err)
	}
	if err := m.Signal(os.Getpid(), "term"); !errors.Is(err, ErrProtected) {
		t.Fatalf("self: %v", err)
	}
	if err := m.Signal(os.Getppid(), "term"); !errors.Is(err, ErrProtected) {
		t.Fatalf("parent (an ancestor of the daemon): %v", err)
	}
	if err := m.Signal(999999999, "term"); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("missing: %v", err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid := cmd.Process.Pid
	if err := m.Renice(pid, 25); err == nil {
		t.Fatal("nice 25 must be refused")
	}
	if err := m.Renice(pid, 10); err != nil {
		t.Fatalf("renice: %v", err)
	}
	r, _ := m.st.readRaw(pid)
	if r.nice != 10 {
		t.Fatalf("nice = %d", r.nice)
	}
	if err := m.Renice(pid, 5); !errors.Is(err, errRaise) {
		t.Fatalf("raising priority must be refused: %v", err)
	}
	if err := m.Signal(pid, "bogus"); err == nil {
		t.Fatal("bad signal name accepted")
	}
	if err := m.Signal(pid, "term"); err != nil {
		t.Fatalf("term: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child did not exit")
	}
}

// TestCheckTargetLiveProtection: protection is computed from the live table,
// so the session leader is refused while the monitor is idle (no scan) and
// whatever its rank, and built-in agent sessions (`cerberus agent`) stay
// actionable while a bare cerberus process is not.
func TestCheckTargetLiveProtection(t *testing.T) {
	tr := newTree(t)
	uid := os.Getuid()
	app := "/user.slice/user-1000.slice/user@1000.service/app.slice/"
	stat := func(pid, ppid, sid int, comm string) {
		tr.write(filepath.Join(tr.proc, strconv.Itoa(pid), "stat"), strconv.Itoa(pid)+" ("+comm+") S "+strconv.Itoa(ppid)+" "+
			strconv.Itoa(sid)+" "+strconv.Itoa(sid)+" 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500 0 0\n")
	}
	tr.pid(1, 0, "systemd", 0, 0, "/init.scope", "")
	tr.pid(40, 1, "bash", 0, uid, "/user.slice/user-1000.slice/session-2.scope", "") // tty login shell
	stat(40, 1, 40, "bash")
	tr.pid(41, 40, "start-hyprlnd", 0, uid, "/user.slice/user-1000.slice/session-2.scope", "") // custom wrapper
	stat(41, 40, 40, "start-hyprlnd")
	tr.pid(90, 1, "systemd", 0, uid, app, "") // systemd --user
	tr.pid(100, 90, "Hyprland", 0, uid, app+"wayland-wm@Hyprland.service", "")
	stat(100, 90, 40, "Hyprland") // under systemd --user, same session as the login shell
	tr.pid(200, 1, "firefox", 0, uid, app+"app-firefox-1.scope", "")
	tr.pid(300, 1, "cerberus", 0, uid, app+"cerberus.service", "")
	tr.pid(310, 300, "cerberus", 0, uid, app+"agents.slice/run-u1.scope", "CERBERUS_SESSION=s-9\x00")
	tr.pid(320, 1, "cerberus", 0, uid, app+"other.service", "")
	tr.pid(330, 1, "bash", 0, 0, "/", "") // root's
	now := time.Unix(1700001000, 0)
	m := newTestMonitor(t, tr, &now)
	m.self = 300
	// No Sample at all: the monitor is idle and has no process list.
	for _, c := range []struct {
		pid  int
		want error
	}{
		{40, ErrProtected},  // session leader of the compositor
		{100, ErrProtected}, // compositor
		{90, ErrProtected},  // systemd --user (name)
		{300, ErrProtected}, // the daemon
		{320, ErrProtected}, // a cerberus outside any session
		{310, nil},          // built-in agent session: stoppable
		{200, nil},
		{330, ErrNotOwned},
		{999, ErrNoProcess},
	} {
		if _, err := m.checkTarget(c.pid); !errors.Is(err, c.want) {
			t.Errorf("checkTarget(%d) = %v, want %v", c.pid, err, c.want)
		}
	}
	// The scan marks the same rows.
	m.Sample(context.Background(), true)
	got := map[int]bool{}
	for _, p := range m.Processes() {
		got[p.PID] = p.Protected
	}
	if !got[40] || !got[100] || got[310] || !got[320] || got[200] {
		t.Fatalf("scan protected flags = %v", got)
	}
}
