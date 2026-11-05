package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

// Work previews (docs/SCOPE.md "As built: work previews"): what a session
// can show of the site it is building.
//
//   - Static: a folder of the worktree holding an index.html, from the config
//     (RepoConfig.Preview.Dir or .cerberus.json preview.dir) or detected
//     (staticCandidates, root first). It is served while the worktree exists.
//   - Live: TCP servers listening on loopback or every interface that are
//     owned by the session's own processes (Linux: /proc). Ownership is the
//     only thing that lets a port be proxied, so a session can never point a
//     preview at another local service.
//   - Managed ("Run preview"): a preview server Cerberus starts in the
//     worktree, in the session's sandbox, with a port it picked.

// Managed preview server states.
const (
	PreviewStarting = "starting"
	PreviewRunning  = "running"
	PreviewExited   = "exited"
	PreviewFailed   = "failed"
	PreviewStopped  = "stopped"
)

const (
	previewLogMax    = 16 << 10
	previewScanTTL   = time.Second
	previewWatch     = 5 * time.Second
	previewStopGrace = 3 * time.Second
	previewReadyMax  = 2 * time.Minute
	previewTagEnv    = "CERBERUS_PREVIEW_TAG"
)

// ErrNoPreviewCommand: Run preview found nothing to run.
var ErrNoPreviewCommand = errors.New("no preview command: add {\"preview\":{\"command\":\"…{port}…\"}} to .cerberus.json, or a dev, start or preview script to package.json")

// staticCandidates are the folders searched for an index.html when nothing
// is configured, in order.
var staticCandidates = []string{".", "public", "dist", "build", "out", "site", "_site", "docs"}

// PreviewInfo is the preview summary on Info (lists show a chip from it).
type PreviewInfo struct {
	Static  bool   // a static site is served (PreviewURL)
	Live    []int  `json:",omitempty"` // ports of live servers the session owns
	Managed string `json:",omitempty"` // the Run preview server's state ("" = none)
}

// LiveServer is one live server a session owns.
type LiveServer struct {
	Port   int    `json:"port"`
	V6     bool   `json:"v6,omitempty"` // reachable on [::1] only
	Source string `json:"source"`       // "detected" | "managed"
}

// ManagedPreview is the Run preview server of a session, or what it would
// run.
type ManagedPreview struct {
	State   string    `json:"state"`   // "" = not started
	Command string    `json:"command"` // with {port}; "" = nothing to run
	Source  string    `json:"source"`  // where Command comes from
	Port    int       `json:"port,omitempty"`
	Error   string    `json:"error,omitempty"`
	Started time.Time `json:"started,omitzero"`
	LogTail string    `json:"log_tail"`
}

// managed is a running (or ended) Run preview server.
type managed struct {
	cmd      *exec.Cmd
	port     int
	command  string
	source   string
	state    string
	err      string
	started  time.Time
	log      *syncTail
	done     chan struct{} // closed when the process has been reaped
	stopping bool
}

// syncTail is a tailBuffer safe for a process's stdout and stderr.
type syncTail struct {
	mu sync.Mutex
	t  tailBuffer
}

func (s *syncTail) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.Write(p)
}

func (s *syncTail) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.String()
}

// ---------- static ----------

// staticDir is the folder served as the session's static preview ("" =
// none): the configured folder, else the first candidate with an
// index.html.
func (m *Manager) staticDir(repo, wt string) string {
	if wt == "" {
		return ""
	}
	if p := m.settings(repo, wt).Preview; p != nil && p.Dir != "" {
		return previewDir(wt, p)
	}
	return detectStatic(wt)
}

// detectStatic returns the first of staticCandidates inside wt that holds
// a regular index.html ("" = none).
func detectStatic(wt string) string {
	for _, c := range staticCandidates {
		d := previewDir(wt, &config.PreviewConfig{Dir: c})
		if d == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(d, "index.html")); err == nil && st.Mode().IsRegular() {
			return d
		}
	}
	return ""
}

// ---------- ownership ----------

// previewTag is the value of CERBERUS_PREVIEW_TAG in a session's agent
// environment: unguessable by other sessions, so a process carrying it was
// started (directly or not) by this session, even after it left the
// agent's process tree (a daemonised dev server).
func (m *Manager) previewTag(id string) string {
	m.pvKeyOnce.Do(func() {
		m.pvKey = make([]byte, 32)
		_, _ = rand.Read(m.pvKey)
	})
	mac := hmac.New(sha256.New, m.pvKey)
	mac.Write([]byte("cerberus preview tag\x00" + id))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// owners are the roots of one session's processes.
type owners struct {
	sids    []int  // the agent's PTY session and the managed server's session
	managed int    // the managed server's pid (0 = none)
	unit    string // the agent run's systemd scope
	tag     string
	port    int // the managed server's chosen port
}

func (m *Manager) ownersLocked(s *session) owners {
	o := owners{tag: m.previewTag(s.info.ID)}
	if r := s.run; r != nil && r.cmd != nil && r.cmd.Process != nil {
		select {
		case <-r.waitDone:
		default:
			o.sids = append(o.sids, r.cmd.Process.Pid)
			o.unit = r.unit
		}
	}
	if pv := s.pv; pv != nil && pv.cmd != nil && pv.cmd.Process != nil {
		select {
		case <-pv.done:
		default:
			o.sids = append(o.sids, pv.cmd.Process.Pid)
			o.managed = pv.cmd.Process.Pid
			o.port = pv.port
		}
	}
	return o
}

// procIndex is one pass over /proc, shared by every session scanned.
type procIndex struct {
	procs    []platform.Proc
	children map[int][]int
	tags     map[string][]int // CERBERUS_PREVIEW_TAG value -> pids
	units    map[string][]int // cgroup segment -> pids (only when asked)
}

func newProcIndex(wantUnits bool) *procIndex {
	ix := &procIndex{procs: platform.Procs(), children: map[int][]int{}, tags: map[string][]int{}}
	if wantUnits {
		ix.units = map[string][]int{}
	}
	self := os.Getpid()
	for _, p := range ix.procs {
		ix.children[p.PPID] = append(ix.children[p.PPID], p.PID)
		if p.PID == self {
			continue
		}
		if t := platform.ProcEnv(p.PID, previewTagEnv); t != "" {
			ix.tags[t] = append(ix.tags[t], p.PID)
		}
		if wantUnits {
			for _, u := range platform.ProcUnits(p.PID) {
				if strings.HasPrefix(u, "cerberus-") {
					ix.units[u] = append(ix.units[u], p.PID)
				}
			}
		}
	}
	return ix
}

// owned returns the pids that belong to o, split into the managed
// server's tree and the rest.
func (ix *procIndex) owned(o owners) (rest, managed []int) {
	seen := map[int]bool{}
	var walk func(pid int, into *[]int)
	walk = func(pid int, into *[]int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		*into = append(*into, pid)
		for _, c := range ix.children[pid] {
			walk(c, into)
		}
	}
	if o.managed > 0 {
		walk(o.managed, &managed)
		for _, p := range ix.procs {
			if p.SID == o.managed {
				walk(p.PID, &managed)
			}
		}
	}
	for _, sid := range o.sids {
		if sid == o.managed {
			continue
		}
		walk(sid, &rest)
		for _, p := range ix.procs {
			if p.SID == sid {
				walk(p.PID, &rest)
			}
		}
	}
	for _, pid := range ix.tags[o.tag] {
		walk(pid, &rest)
	}
	if o.unit != "" && ix.units != nil {
		for _, pid := range ix.units[o.unit+".scope"] {
			walk(pid, &rest)
		}
	}
	return rest, managed
}

// liveOf lists the servers o owns.
func (ix *procIndex) liveOf(o owners) []LiveServer {
	rest, mg := ix.owned(o)
	var out []LiveServer
	have := map[int]bool{}
	for _, l := range platform.ListenSockets(mg) {
		have[l.Port] = true
		out = append(out, LiveServer{Port: l.Port, V6: l.V6 && l.Loopback, Source: "managed"})
	}
	for _, l := range platform.ListenSockets(rest) {
		if !have[l.Port] {
			have[l.Port] = true
			out = append(out, LiveServer{Port: l.Port, V6: l.V6 && l.Loopback, Source: "detected"})
		}
	}
	slices.SortFunc(out, func(a, b LiveServer) int { return a.Port - b.Port })
	return out
}

// scanLive finds the live servers of the given sessions in one /proc pass
// (Linux). Elsewhere only a managed server counts, on the port it was given.
func (m *Manager) scanLive(ids []string) map[string][]LiveServer {
	m.mu.Lock()
	roots := map[string]owners{}
	wantUnits := false
	for _, id := range ids {
		if s, ok := m.sessions[id]; ok && !final(s.info.State) {
			o := m.ownersLocked(s)
			roots[id] = o
			wantUnits = wantUnits || o.unit != ""
		}
	}
	m.mu.Unlock()
	out := map[string][]LiveServer{}
	if !platform.SocketScan {
		for id, o := range roots {
			if o.managed > 0 && o.port > 0 {
				out[id] = []LiveServer{{Port: o.port, Source: "managed"}}
			}
		}
		return out
	}
	ix := newProcIndex(wantUnits)
	for id, o := range roots {
		if l := ix.liveOf(o); len(l) > 0 {
			out[id] = l
		}
	}
	return out
}

// PreviewDetect reports whether live servers are detected on this OS.
func (m *Manager) PreviewDetect() bool { return platform.SocketScan }

// PreviewLive returns the live servers session id owns now (at most
// previewScanTTL old).
func (m *Manager) PreviewLive(id string) []LiveServer {
	m.pvScanMu.Lock()
	defer m.pvScanMu.Unlock()
	if c, ok := m.pvScan[id]; ok && time.Since(c.at) < previewScanTTL {
		return slices.Clone(c.live)
	}
	live := m.scanLive([]string{id})[id]
	if m.pvScan == nil {
		m.pvScan = map[string]liveCache{}
	}
	m.pvScan[id] = liveCache{at: time.Now(), live: live}
	m.noteLive(id, live)
	return slices.Clone(live)
}

// liveCache is one session's last scan.
type liveCache struct {
	at   time.Time
	live []LiveServer
}

// OwnsPort reports whether session id owns a live server on port, and
// which.
func (m *Manager) OwnsPort(id string, port int) (LiveServer, bool) {
	for _, l := range m.PreviewLive(id) {
		if l.Port == port {
			return l, true
		}
	}
	return LiveServer{}, false
}

// noteLive records a scan on the session and emits a change when the set
// of ports moved.
func (m *Manager) noteLive(id string, live []LiveServer) {
	ports := make([]int, 0, len(live))
	for _, l := range live {
		ports = append(ports, l.Port)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || slices.Equal(s.pvLive, ports) {
		return
	}
	s.pvLive = ports
	m.emitLocked(EventState, m.snapshotLocked(s))
}

// previewInfoLocked is the summary on Info (nil when there is nothing).
func (s *session) previewInfoLocked() *PreviewInfo {
	pi := PreviewInfo{Static: s.info.PreviewURL != "", Live: slices.Clone(s.pvLive)}
	if s.pv != nil && s.pv.state != PreviewStopped {
		pi.Managed = s.pv.state
	}
	if !pi.Static && len(pi.Live) == 0 && pi.Managed == "" {
		return nil
	}
	return &pi
}

// TouchPreview notes that someone looked at session id's preview (idle
// stop of the managed server).
func (m *Manager) TouchPreview(id string) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok {
		s.pvUsed = time.Now()
	}
	m.mu.Unlock()
}

// previewLoop rescans the sessions that may have live servers, refreshes
// their static preview, and stops idle managed servers.
func (m *Manager) previewLoop(ctx context.Context) {
	t := time.NewTicker(previewWatch)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.previewTick()
	}
}

func (m *Manager) previewTick() {
	idle := time.Duration(max(m.conf().Sessions.PreviewIdleMin, 1)) * time.Minute
	if m.conf().Sessions.PreviewIdleMin <= 0 {
		idle = 30 * time.Minute
	}
	now := time.Now()
	var ids []string
	var stale []*session
	m.mu.Lock()
	for id, s := range m.sessions {
		if final(s.info.State) {
			continue
		}
		if live(s.info.State) {
			// The agent may have written an index.html since the last
			// state change.
			if u := m.previewURL(s.info.ID, s.info.Repo, s.info.Worktree); u != s.info.PreviewURL {
				s.info.PreviewURL = u
				m.emitLocked(EventState, m.snapshotLocked(s))
			}
		}
		if live(s.info.State) || s.pv != nil || len(s.pvLive) > 0 {
			ids = append(ids, id)
		}
		if pv := s.pv; pv != nil && !pv.stopping && (pv.state == PreviewRunning || pv.state == PreviewStarting) &&
			now.Sub(s.pvUsed) > idle {
			stale = append(stale, s)
		}
	}
	m.mu.Unlock()
	for _, s := range stale {
		m.log.Info("preview server idle, stopping", "id", s.info.ID)
		m.stopPreview(s, "stopped after "+idle.String()+" without visits")
	}
	if len(ids) == 0 {
		return
	}
	found := m.scanLive(ids)
	m.pvScanMu.Lock()
	if m.pvScan == nil {
		m.pvScan = map[string]liveCache{}
	}
	for _, id := range ids {
		m.pvScan[id] = liveCache{at: time.Now(), live: found[id]}
	}
	m.pvScanMu.Unlock()
	for _, id := range ids {
		m.noteLive(id, found[id])
	}
}

// ---------- managed preview server ----------

// previewCommand is the command Run preview starts and where it comes
// from: the repo config or .cerberus.json preview.command, else a
// package.json script (dev, start, preview) run with the lockfile's tool.
func (m *Manager) previewCommand(repo, wt string) (string, string) {
	if wt == "" {
		return "", ""
	}
	if p := m.settings(repo, wt).Preview; p != nil && strings.TrimSpace(p.Command) != "" {
		src := ".cerberus.json"
		if _, ok := m.repoConfig(repo); ok && repo != "" {
			src = "config"
		}
		return strings.TrimSpace(p.Command), src
	}
	return packageCommand(wt)
}

// packageCommand derives a preview command from wt/package.json.
func packageCommand(wt string) (string, string) {
	raw, err := os.ReadFile(filepath.Join(wt, "package.json"))
	if err != nil {
		return "", ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return "", ""
	}
	tool := "npm"
	for _, lf := range []struct{ file, tool string }{
		{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}, {"bun.lockb", "bun"}, {"bun.lock", "bun"},
	} {
		if _, err := os.Stat(filepath.Join(wt, lf.file)); err == nil {
			tool = lf.tool
			break
		}
	}
	for _, name := range []string{"dev", "start", "preview"} {
		script, ok := pkg.Scripts[name]
		if !ok || strings.TrimSpace(script) == "" {
			continue
		}
		var args string
		switch {
		case strings.Contains(script, "vite"):
			args = "--port {port} --strictPort --host 127.0.0.1"
		case strings.Contains(script, "astro"):
			args = "--port {port} --host 127.0.0.1"
		case strings.Contains(script, "next"):
			args = "-p {port} -H 127.0.0.1"
		}
		cmd := tool + " run " + name
		if args != "" {
			if tool == "npm" {
				cmd += " --"
			}
			cmd += " " + args
		}
		return cmd, "package.json \"" + name + "\""
	}
	return "", ""
}

// PreviewManaged returns session id's Run preview server, or what Run
// preview would start.
func (m *Manager) PreviewManaged(id string) (ManagedPreview, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return ManagedPreview{}, err
	}
	repo, wt := s.info.Repo, s.info.Worktree
	if pv := s.pv; pv != nil {
		mp := managedInfo(pv)
		m.mu.Unlock()
		return mp, nil
	}
	m.mu.Unlock()
	cmd, src := m.previewCommand(repo, wt)
	return ManagedPreview{Command: cmd, Source: src}, nil
}

func managedInfo(pv *managed) ManagedPreview {
	return ManagedPreview{State: pv.state, Command: pv.command, Source: pv.source, Port: pv.port, Error: pv.err,
		Started: pv.started, LogTail: pv.log.String()}
}

// StartPreview starts session id's Run preview server (a no-op when one
// runs). It runs in the worktree, in the session's sandbox (always bwrap
// for a confined owner) and memory scope, with PORT and {port} set to a
// free loopback port.
func (m *Manager) StartPreview(ctx context.Context, id string) (ManagedPreview, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return ManagedPreview{}, err
	}
	switch {
	case final(s.info.State):
		err = fmt.Errorf("session is %s", s.info.State)
	case s.info.Worktree == "":
		err = errors.New("session has no worktree")
	case s.op:
		err = ErrBusy
	case m.closing:
		err = errors.New("shutting down")
	}
	if err != nil {
		m.mu.Unlock()
		return ManagedPreview{}, err
	}
	if pv := s.pv; pv != nil && (pv.state == PreviewStarting || pv.state == PreviewRunning) {
		mp := managedInfo(pv)
		m.mu.Unlock()
		return mp, nil
	}
	info := s.info
	m.mu.Unlock()

	command, source := m.previewCommand(info.Repo, info.Worktree)
	if command == "" {
		return ManagedPreview{}, ErrNoPreviewCommand
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return ManagedPreview{}, fmt.Errorf("no free port: %w", err)
	}
	p, _ := m.conf().AgentProfile(info.Agent)
	plan, err := m.planSandbox(ctx, info, p, s.dir, nil)
	if err != nil {
		m.sandboxRefused(info.ID, err)
		return ManagedPreview{}, err
	}
	if plan.bin != "" && !plan.spec.Network {
		return ManagedPreview{}, errors.New("the sandbox has no network (sandbox.network is off), so a preview server inside it could not be reached")
	}
	if plan.fallback != nil {
		m.sandboxWarn(s, "running the preview server", plan.fallback)
	}
	text := strings.ReplaceAll(command, "{port}", strconv.Itoa(port))
	argv := []string{"bash", "-c", text}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/C", text}
	}
	argv, _ = m.wrapScope(plan.wrap(argv), info)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = info.Worktree
	cmd.Env = previewEnv(port, plan.bin != "")
	log := &syncTail{t: tailBuffer{max: previewLogMax}}
	fmt.Fprintf(log, "$ %s\n", text)
	cmd.Stdout, cmd.Stderr = log, log
	platform.NewSession(cmd)
	pv := &managed{cmd: cmd, port: port, command: command, source: source, state: PreviewStarting,
		started: time.Now(), log: log, done: make(chan struct{})}

	m.mu.Lock()
	if final(s.info.State) || s.op || m.closing {
		m.mu.Unlock()
		return ManagedPreview{}, ErrBusy
	}
	if cur := s.pv; cur != nil && (cur.state == PreviewStarting || cur.state == PreviewRunning) {
		mp := managedInfo(cur)
		m.mu.Unlock()
		return mp, nil
	}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return ManagedPreview{}, fmt.Errorf("start the preview server: %w", err)
	}
	s.pv = pv
	s.pvUsed = time.Now()
	m.emitLocked(EventState, m.snapshotLocked(s))
	mp := managedInfo(pv)
	m.wg.Add(2)
	m.mu.Unlock()
	m.log.Info("preview server started", "id", id, "port", port, "command", text)
	go m.waitPreview(s, pv)
	go m.readyPreview(s, pv)
	return mp, nil
}

// previewEnv is the managed server's environment: ours without Cerberus'
// own variables and credentials, plus PORT.
func previewEnv(port int, sandboxed bool) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "OPENAI_API_KEY" || k == "PORT" || k == "BROWSER" || strings.HasPrefix(k, "CERBERUS_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PORT="+strconv.Itoa(port), "BROWSER=none")
	if sandboxed {
		env = append(env, "TMPDIR=/tmp")
	}
	return env
}

// freeLoopbackPort asks the kernel for a free loopback port.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitPreview reaps the managed server and settles its state.
func (m *Manager) waitPreview(s *session, pv *managed) {
	defer m.wg.Done()
	err := pv.cmd.Wait()
	platform.KillSession(pv.cmd.Process.Pid) // whatever it left in its session
	m.mu.Lock()
	switch {
	case pv.stopping:
		pv.state = PreviewStopped
	case err != nil:
		pv.state, pv.err = PreviewFailed, err.Error()
	default:
		pv.state = PreviewExited
	}
	close(pv.done)
	if s.pv == pv {
		m.emitLocked(EventState, m.snapshotLocked(s))
	}
	m.mu.Unlock()
	fmt.Fprintf(pv.log, "\n── preview server %s ──\n", pv.state)
	m.pvScanMu.Lock()
	delete(m.pvScan, s.info.ID)
	m.pvScanMu.Unlock()
}

// readyPreview marks the managed server running once it listens.
func (m *Manager) readyPreview(s *session, pv *managed) {
	defer m.wg.Done()
	deadline := time.Now().Add(previewReadyMax)
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-pv.done:
			return
		case <-t.C:
		}
		ready := false
		if platform.SocketScan {
			m.pvScanMu.Lock()
			delete(m.pvScan, s.info.ID)
			m.pvScanMu.Unlock()
			for _, l := range m.PreviewLive(s.info.ID) {
				if l.Source == "managed" {
					ready = true
				}
			}
		} else if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(pv.port)), time.Second); err == nil {
			c.Close()
			ready = true
		}
		if ready {
			m.mu.Lock()
			if pv.state == PreviewStarting {
				pv.state = PreviewRunning
				if s.pv == pv {
					m.emitLocked(EventState, m.snapshotLocked(s))
				}
			}
			m.mu.Unlock()
			return
		}
	}
}

// StopPreview stops session id's Run preview server.
func (m *Manager) StopPreview(id string) error {
	m.mu.Lock()
	s, err := m.lookup(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if !m.stopPreview(s, "") {
		return errors.New("no preview server is running")
	}
	return nil
}

// stopPreview ends s's managed server (SIGTERM to its session, SIGKILL
// after previewStopGrace) and waits for it, up to twice the grace. It
// reports whether one was running.
func (m *Manager) stopPreview(s *session, note string) bool {
	m.mu.Lock()
	pv := s.pv
	if pv == nil || pv.state == PreviewStopped || pv.state == PreviewExited || pv.state == PreviewFailed {
		m.mu.Unlock()
		return false
	}
	pv.stopping = true
	m.mu.Unlock()
	if note != "" {
		fmt.Fprintf(pv.log, "\n── %s ──\n", note)
	}
	platform.Terminate(pv.cmd.Process, previewStopGrace, pv.done)
	select {
	case <-pv.done:
	case <-time.After(2*previewStopGrace + time.Second):
		platform.KillSession(pv.cmd.Process.Pid)
		m.log.Warn("preview server did not stop in time", "id", s.info.ID)
	}
	return true
}

// endPreview stops s's managed server before its worktree goes away.
func (m *Manager) endPreview(s *session, why string) { m.stopPreview(s, why) }
