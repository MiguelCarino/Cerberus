package sysmon

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/audit"
)

// maxProcs is how many processes a sample sends (plus every Cerberus one).
const maxProcs = 150

// pmonEvery spaces the per-process GPU query (nvidia-smi pmon samples for ~1 s).
const pmonEvery = 4 * time.Second

// builtinProtected are never signalled or reniced from the UI. "cerberus"
// is not here: the daemon is protected by pid (with its ancestors), and a
// process named cerberus is protected unless it runs inside a session (the
// built-in agent is `cerberus agent`, which must stay stoppable).
var builtinProtected = []string{"systemd", "init", "Hyprland", "hyprland", "gnome-shell", "kwin_wayland", "kwin_x11",
	"sway", "river", "niri", "labwc", "wayfire", "mutter", "dbus-daemon", "dbus-broker", "dbus-broker-lau",
	"pipewire", "pipewire-pulse", "wireplumber", "Xwayland", "Xorg", "gdm", "sddm", "login",
	"uwsm", "polkitd", "systemd-logind",
	// Graphical session wrappers and leaders (comm is cut to 15 bytes).
	"gdm-wayland-ses", "gdm-x-session", "gnome-session-b", "gnome-session", "startplasma-way", "startplasma-x11",
	"plasma_session", "start-hyprland", "uwsm-session", "lightdm", "greetd"}

// daemonName is the comm of Cerberus itself (see builtinProtected).
const daemonName = "cerberus"

// compositors: their ancestors and their session leader are protected too.
var compositors = map[string]bool{"Hyprland": true, "hyprland": true, "gnome-shell": true, "kwin_wayland": true,
	"kwin_x11": true, "sway": true, "river": true, "niri": true, "labwc": true, "wayfire": true, "Xorg": true}

// modelServers are LLM engines; marked as Cerberus-related.
var modelServers = map[string]bool{"llama-server": true, "llmster": true, "ollama": true, "lms": true, "vllm": true}

type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(nvidiaSmi string) runner {
	if nvidiaSmi == "" {
		nvidiaSmi = "nvidia-smi"
	}
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			name = nvidiaSmi
		}
		return exec.CommandContext(ctx, name, args...).Output()
	}
}

// staticInfo is what does not change during a process's life (cached by pid+start).
type staticInfo struct {
	start     uint64
	uid       int
	user      string
	cmd       string
	group     string
	sessionID string
}

type procState struct {
	prev   map[int]uint64 // pid → utime+stime ticks
	prevAt time.Time
	info   map[int]staticInfo
	users  map[int]string

	gpuMu    sync.Mutex
	gpuAt    time.Time
	gpuMem   map[int]int
	gpuPct   map[int]float64
	gpuBusy  bool
	gpuNoCmd bool
}

func newProcState() procState {
	return procState{prev: map[int]uint64{}, info: map[int]staticInfo{}, users: map[int]string{}}
}

func (s *sampler) forgetProcesses() {
	if len(s.procs.prev) > 0 || len(s.procs.info) > 0 {
		s.procs.prev, s.procs.info = map[int]uint64{}, map[int]staticInfo{}
		s.procs.prevAt = time.Time{}
	}
}

// rawProc is one /proc/[pid]/stat read.
type rawProc struct {
	pid, ppid, nice int
	sid             int // session id (the session leader's pid)
	comm, state     string
	ticks, start    uint64
	rssPages        int
	kernel          bool
}

func (s *sampler) readRaw(pid int) (rawProc, bool) {
	b, err := os.ReadFile(s.procPath(strconv.Itoa(pid), "stat"))
	if err != nil {
		return rawProc{}, false
	}
	return parseStat(pid, b)
}

func parseStat(pid int, b []byte) (rawProc, bool) {
	l, r := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if l < 0 || r < l {
		return rawProc{}, false
	}
	p := rawProc{pid: pid, comm: string(b[l+1 : r])}
	f := strings.Fields(string(b[r+1:]))
	if len(f) < 20 {
		return rawProc{}, false
	}
	p.state = f[0]
	p.ppid, _ = strconv.Atoi(f[1])
	p.sid, _ = strconv.Atoi(f[3])
	ut, _ := strconv.ParseUint(f[11], 10, 64)
	st, _ := strconv.ParseUint(f[12], 10, 64)
	p.ticks = ut + st
	p.nice, _ = strconv.Atoi(f[16])
	p.start, _ = strconv.ParseUint(f[19], 10, 64)
	p.kernel = pid == 2 || p.ppid == 2
	return p, true
}

func (s *sampler) listPIDs() []int {
	ents, err := os.ReadDir(s.m.proc)
	if err != nil {
		return nil
	}
	pids := make([]int, 0, len(ents))
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil && n > 0 {
			pids = append(pids, n)
		}
	}
	return pids
}

// processes scans every process (active mode only).
func (s *sampler) processes(ctx context.Context, now time.Time) ([]Proc, []Group) {
	ps := &s.procs
	dt := now.Sub(ps.prevAt).Seconds()
	first := ps.prevAt.IsZero() || dt <= 0
	raws := map[int]rawProc{}
	for _, pid := range s.listPIDs() {
		if r, ok := s.readRaw(pid); ok {
			if !r.kernel {
				if b, err := os.ReadFile(s.procPath(strconv.Itoa(pid), "statm")); err == nil {
					if f := strings.Fields(string(b)); len(f) > 1 {
						r.rssPages, _ = strconv.Atoi(f[1])
					}
				}
			}
			raws[pid] = r
		}
	}
	s.maybeQueryGPU(ctx, now)
	gpuMem, gpuPct := s.gpuReadings()
	protected := s.protectedSet(raws)
	page := os.Getpagesize()

	cur := make(map[int]uint64, len(raws))
	var procs []Proc
	groups := map[string]*Group{}
	addGroup := func(p Proc) {
		key := p.Group
		g := groups[key]
		if g == nil {
			g = &Group{Name: p.Group, SessionID: p.SessionID}
			groups[key] = g
		}
		g.CPU += p.CPUPct
		g.RSSMiB += p.RSSMiB
		g.GPUMiB += p.GPUMiB
		g.Count++
		g.Cerberus = g.Cerberus || p.Cerberus
	}
	for pid, r := range raws {
		cur[pid] = r.ticks
		cpu := 0.0
		if prev, ok := ps.prev[pid]; ok && !first && r.ticks >= prev {
			cpu = float64(r.ticks-prev) * 100 / (dt * clkTck)
		}
		if r.kernel {
			addGroup(Proc{Group: "kernel", CPUPct: cpu})
			continue
		}
		info := s.static(pid, r)
		p := Proc{PID: pid, PPID: r.ppid, UID: info.uid, Name: r.comm, Cmd: info.cmd, User: info.user,
			Group: info.group, State: r.state, CPUPct: cpu, RSSMiB: r.rssPages * page >> 20, Nice: r.nice,
			GPUMiB: gpuMem[pid], GPUPct: gpuPct[pid], SessionID: info.sessionID, Protected: protected[pid]}
		if !s.btime.IsZero() {
			p.Started = s.btime.Add(time.Duration(r.start) * time.Second / clkTck)
		}
		p.Cerberus = s.isCerberus(raws, pid) || p.SessionID != ""
		if p.SessionID == "" && p.Cerberus {
			p.SessionID = s.ancestorSession(raws, pid)
		}
		if p.SessionID != "" {
			p.Group = "session " + p.SessionID
		}
		addGroup(p)
		procs = append(procs, p)
	}
	for pid := range ps.info {
		if _, ok := raws[pid]; !ok {
			delete(ps.info, pid)
		}
	}
	ps.prev, ps.prevAt = cur, now
	return topProcs(procs), sortedGroups(groups)
}

// static reads (once per process life) the owner, command line, group and session.
func (s *sampler) static(pid int, r rawProc) staticInfo {
	ps := &s.procs
	if in, ok := ps.info[pid]; ok && in.start == r.start {
		return in
	}
	in := staticInfo{start: r.start, uid: -1}
	dir := s.procPath(strconv.Itoa(pid))
	if f, err := os.Open(filepath.Join(dir, "status")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
				if fs := strings.Fields(v); len(fs) > 0 {
					in.uid, _ = strconv.Atoi(fs[0])
				}
				break
			}
		}
		f.Close()
	}
	in.user = s.userName(in.uid)
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		c := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
		if len(c) > 200 {
			c = c[:200]
		}
		in.cmd = c
	}
	cg := ""
	if b, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "0::"); ok {
				cg = v
			}
		}
	}
	in.group = groupName(cg, r.comm)
	if in.uid == s.m.uid {
		if b, err := os.ReadFile(filepath.Join(dir, "environ")); err == nil {
			for _, kv := range bytes.Split(b, []byte{0}) {
				if v, ok := bytes.CutPrefix(kv, []byte("CERBERUS_SESSION=")); ok {
					in.sessionID = string(v)
					break
				}
			}
		}
	}
	ps.info[pid] = in
	return in
}

func (s *sampler) userName(uid int) string {
	if uid < 0 {
		return ""
	}
	if n, ok := s.procs.users[uid]; ok {
		return n
	}
	n := strconv.Itoa(uid)
	if u, err := user.LookupId(n); err == nil {
		n = u.Username
	}
	s.procs.users[uid] = n
	return n
}

// groupName turns a cgroup path into an app name.
func groupName(cg, comm string) string {
	if cg == "" || cg == "/" {
		return comm
	}
	if strings.Contains(cg, "agents.slice") {
		return "agents"
	}
	last := cg[strings.LastIndexByte(cg, '/')+1:]
	last = strings.ReplaceAll(last, `\x2d`, "-")
	switch {
	case last == "init.scope":
		return "systemd"
	case strings.HasPrefix(last, "session-") && strings.HasSuffix(last, ".scope"):
		return comm
	case strings.HasPrefix(last, "app-"):
		n := strings.TrimPrefix(last, "app-")
		n = strings.TrimSuffix(strings.TrimSuffix(n, ".scope"), ".service")
		if i := strings.IndexByte(n, '@'); i > 0 {
			n = n[:i]
		}
		for _, pre := range []string{"Hyprland-", "hyprland-", "gnome-", "kde-", "flatpak-", "dbus-:", "niri-", "sway-"} {
			if rest, ok := strings.CutPrefix(n, pre); ok && rest != "" {
				n = rest
			}
		}
		if i := strings.LastIndexByte(n, '-'); i > 0 && isID(n[i+1:]) {
			n = n[:i]
		}
		if n == "" {
			return comm
		}
		return n
	case strings.HasSuffix(last, ".service"):
		n := strings.TrimSuffix(last, ".service")
		if i := strings.IndexByte(n, '@'); i > 0 {
			n = n[:i]
		}
		return n
	case strings.HasSuffix(last, ".scope"):
		return comm
	}
	return comm
}

func isID(s string) bool {
	if len(s) < 3 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isCerberus: the daemon, anything below it, model servers and their children.
func (s *sampler) isCerberus(raws map[int]rawProc, pid int) bool {
	for i, cur := 0, pid; i < 64 && cur > 1; i++ {
		r, ok := raws[cur]
		if !ok {
			return false
		}
		if cur == s.m.self || modelServers[r.comm] {
			return true
		}
		cur = r.ppid
	}
	return false
}

func (s *sampler) ancestorSession(raws map[int]rawProc, pid int) string {
	for i, cur := 0, pid; i < 64 && cur > 1; i++ {
		if in, ok := s.procs.info[cur]; ok && in.sessionID != "" {
			return in.sessionID
		}
		r, ok := raws[cur]
		if !ok {
			return ""
		}
		cur = r.ppid
	}
	return ""
}

// protectedSet: pid 1, the daemon and its ancestors, compositors with their
// ancestors and their session leader, kernel threads and every process named
// in the protected list. raws must be the whole process table: Signal and
// Renice build it fresh, never from the (truncated, idle-empty) last scan.
func (s *sampler) protectedSet(raws map[int]rawProc) map[int]bool {
	names := s.protectedNames()
	out := map[int]bool{1: true}
	mark := func(pid int) {
		for i, cur := 0, pid; i < 64 && cur > 0; i++ {
			out[cur] = true
			r, ok := raws[cur]
			if !ok {
				return
			}
			cur = r.ppid
		}
	}
	mark(s.m.self)
	for pid, r := range raws {
		if r.kernel || names[r.comm] {
			out[pid] = true
		}
		if r.comm == daemonName && s.envSession(pid) == "" {
			out[pid] = true
		}
		if compositors[r.comm] {
			mark(pid)
			// The session leader (a tty login shell that started the
			// compositor, or a display-manager wrapper): killing it hangs up
			// the whole graphical session, even when the compositor itself
			// runs under systemd --user and is not its descendant.
			if r.sid > 1 {
				if _, ok := raws[r.sid]; ok {
					mark(r.sid)
				}
			}
		}
	}
	return out
}

// envSession is pid's CERBERUS_SESSION, or "" (also when unreadable).
func (s *sampler) envSession(pid int) string {
	b, err := os.ReadFile(s.procPath(strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("CERBERUS_SESSION=")); ok {
			return string(v)
		}
	}
	return ""
}

// readAll reads every process's stat (the live table for protection checks).
func (s *sampler) readAll() map[int]rawProc {
	raws := map[int]rawProc{}
	for _, pid := range s.listPIDs() {
		if r, ok := s.readRaw(pid); ok {
			raws[pid] = r
		}
	}
	return raws
}

func (s *sampler) protectedNames() map[string]bool {
	names := map[string]bool{}
	for _, n := range builtinProtected {
		names[n] = true
	}
	for _, n := range s.m.protectedNames() {
		names[n] = true
	}
	return names
}

// ---------- GPU per process ----------

// maybeQueryGPU starts an nvidia-smi pmon in the background at most every
// pmonEvery; the sample uses whatever reading is current.
func (s *sampler) maybeQueryGPU(ctx context.Context, now time.Time) {
	ps := &s.procs
	ps.gpuMu.Lock()
	defer ps.gpuMu.Unlock()
	if ps.gpuBusy || ps.gpuNoCmd || now.Sub(ps.gpuAt) < pmonEvery {
		return
	}
	if s.m.gpu == nil || len(s.m.gpu.Snapshot().GPUs) == 0 {
		return
	}
	ps.gpuBusy = true
	go func() {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := s.m.run(qctx, "nvidia-smi", "pmon", "-c", "1", "-s", "um")
		mem, pct := parsePmon(out)
		ps.gpuMu.Lock()
		defer ps.gpuMu.Unlock()
		ps.gpuBusy = false
		ps.gpuAt = s.m.now()
		if err != nil && len(out) == 0 {
			if _, lookErr := exec.LookPath("nvidia-smi"); lookErr != nil {
				ps.gpuNoCmd = true
			}
			return
		}
		ps.gpuMem, ps.gpuPct = mem, pct
	}()
}

func (s *sampler) gpuReadings() (map[int]int, map[int]float64) {
	ps := &s.procs
	ps.gpuMu.Lock()
	defer ps.gpuMu.Unlock()
	return ps.gpuMem, ps.gpuPct
}

// parsePmon reads `nvidia-smi pmon -c 1 -s um` (columns found by header name;
// "-" means none). Memory is summed over GPUs per pid.
func parsePmon(out []byte) (map[int]int, map[int]float64) {
	mem, pct := map[int]int{}, map[int]float64{}
	var cols []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "#" {
			if len(f) > 1 && f[1] == "gpu" {
				cols = f[1:]
			}
			continue
		}
		if cols == nil || len(f) < len(cols)-1 {
			continue
		}
		get := func(name string) string {
			for i, c := range cols {
				if c == name && i < len(f) {
					return f[i]
				}
			}
			return "-"
		}
		pid, err := strconv.Atoi(get("pid"))
		if err != nil || pid <= 0 {
			continue
		}
		if v, err := strconv.ParseFloat(get("sm"), 64); err == nil {
			pct[pid] += v
		}
		if v, err := strconv.Atoi(get("fb")); err == nil {
			mem[pid] += v
		}
	}
	return mem, pct
}

// ---------- ranking ----------

func topProcs(all []Proc) []Proc {
	score := func(p Proc) float64 { return p.CPUPct + float64(p.RSSMiB)/64 + float64(p.GPUMiB)/64 + p.GPUPct }
	sort.Slice(all, func(i, j int) bool {
		si, sj := score(all[i]), score(all[j])
		if si != sj {
			return si > sj
		}
		return all[i].PID < all[j].PID
	})
	out := make([]Proc, 0, maxProcs)
	for _, p := range all {
		if len(out) < maxProcs || p.Cerberus {
			out = append(out, p)
		}
	}
	return out
}

func sortedGroups(m map[string]*Group) []Group {
	out := make([]Group, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPU != out[j].CPU {
			return out[i].CPU > out[j].CPU
		}
		if out[i].RSSMiB != out[j].RSSMiB {
			return out[i].RSSMiB > out[j].RSSMiB
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ---------- actions ----------

// checkTarget re-reads the process table now and refuses protected or
// foreign processes. It never trusts the last scan: that one is truncated to
// the top processes and empty while the monitor is idle.
func (m *Monitor) checkTarget(pid int) (rawProc, error) {
	s := &m.st
	if pid <= 1 || pid == m.self {
		if r, ok := s.readRaw(pid); ok || pid == m.self {
			return r, ErrProtected
		}
		return rawProc{}, ErrNoProcess
	}
	raws := s.readAll()
	r, ok := raws[pid]
	if !ok {
		// Not listed: gone, or a thread id. A signal to a thread id reaches
		// its whole process, so only a real process id is accepted.
		if r, ok = s.readRaw(pid); !ok || s.statusField(pid, "Tgid:") != pid {
			return rawProc{}, ErrNoProcess
		}
		raws[pid] = r
	}
	if r.kernel || s.protectedSet(raws)[pid] || compositors[r.comm] {
		return r, ErrProtected
	}
	if s.statusField(pid, "Uid:") != m.uid {
		return r, ErrNotOwned
	}
	return r, nil
}

// statusField is the first number of a /proc/<pid>/status line, or -1.
func (s *sampler) statusField(pid int, key string) int {
	f, err := os.Open(s.procPath(strconv.Itoa(pid), "status"))
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key); ok {
			if fs := strings.Fields(v); len(fs) > 0 {
				if n, err := strconv.Atoi(fs[0]); err == nil {
					return n
				}
			}
			return -1
		}
	}
	return -1
}

// Signal sends "term", "kill", "stop" or "cont" to pid (audited as "system").
func (m *Monitor) Signal(pid int, sig string) error {
	return m.SignalAs(audit.ActorSystem, pid, sig)
}

// SignalAs is Signal on behalf of actor (see audit.Record.Actor); every
// attempt on a host process, allowed or refused, is audited when an audit
// log is set (WithAudit).
func (m *Monitor) SignalAs(actor string, pid int, sig string) error {
	r, err := m.checkTarget(pid)
	if err == nil {
		err = sendSignal(pid, sig)
	}
	m.auditAction(actor, "process.signal", sig, pid, r.comm, err)
	return err
}

// Renice lowers pid's priority (nice 0..19, never below its current nice).
func (m *Monitor) Renice(pid, nice int) error {
	return m.ReniceAs(audit.ActorSystem, pid, nice)
}

// ReniceAs is Renice on behalf of actor, audited like SignalAs.
func (m *Monitor) ReniceAs(actor string, pid, nice int) error {
	comm, err := m.renice(pid, nice)
	m.auditAction(actor, "process.renice", "nice "+strconv.Itoa(nice), pid, comm, err)
	return err
}

func (m *Monitor) renice(pid, nice int) (string, error) {
	if nice < 0 || nice > 19 {
		return "", errNiceRange
	}
	r, err := m.checkTarget(pid)
	if err != nil {
		return r.comm, err
	}
	if nice < r.nice {
		return r.comm, errRaise
	}
	tasks, _ := os.ReadDir(m.st.procPath(strconv.Itoa(pid), "task"))
	if len(tasks) == 0 {
		return r.comm, setNice(pid, nice)
	}
	var first error
	for _, t := range tasks {
		if tid, err := strconv.Atoi(t.Name()); err == nil {
			if err := setNice(tid, nice); err != nil && first == nil {
				first = err
			}
		}
	}
	return r.comm, first
}

// auditAction records one kill/renice attempt: the targets are the pid and
// the process name, the detail is the action and its outcome.
func (m *Monitor) auditAction(actor, event, what string, pid int, comm string, err error) {
	if m.audit == nil {
		return
	}
	detail := what + ": ok"
	if err != nil {
		detail = what + ": " + err.Error()
	}
	targets := []string{strconv.Itoa(pid)}
	if comm != "" {
		targets = append(targets, comm)
	}
	if aerr := m.audit.Event(actor, event, detail, targets...); aerr != nil {
		m.log.Warn("audit write failed", "err", aerr)
	}
}
