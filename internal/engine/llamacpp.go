package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

// termGrace is how long a llama-server gets between SIGTERM and SIGKILL.
const termGrace = 10 * time.Second

// llamaSpec is everything needed to start one model.
type llamaSpec struct {
	info     ModelInfo
	path     string
	mmproj   string
	draft    string
	ctx      int
	parallel int
	ngl      int // -ngl when Cerberus picks a GPU split (0 = leave to llama-server, -1 = CPU only: -ngl 0)
	args     []string
}

// llamaProc is one running llama-server.
type llamaProc struct {
	cmd  *exec.Cmd
	port int
	done chan struct{} // closed when the process exits
}

func (p *llamaProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// llamaCpp spawns one llama-server per loaded model.
type llamaCpp struct {
	cfg     config.LlamaCppConfig
	models  []config.ModelConfig
	timeout time.Duration
	log     *slog.Logger

	// Injectable for tests.
	command  func(name string, args ...string) *exec.Cmd
	lookPath func(string) (string, error)
	logDir   func() (string, error)
	http     *http.Client

	loadMu sync.Mutex // serialises starts so two loads never race for a port

	mu     sync.Mutex
	specs  map[string]llamaSpec
	procs  map[string]*llamaProc
	exits  map[string]exitInfo         // how each model's last process ended
	onExit func(id string, e exitInfo) // hears exits Cerberus did not ask for (the manager)
}

// oomScoreAdj makes the kernel's OOM killer pick a llama-server before the
// daemon that started it (Linux; a no-op elsewhere).
const oomScoreAdj = 500

func (b *llamaCpp) setOnExit(fn func(id string, e exitInfo)) {
	b.mu.Lock()
	b.onExit = fn
	b.mu.Unlock()
}

// newLlamaCpp returns the llama.cpp backend for cfg.
func newLlamaCpp(cfg *config.Config, log *slog.Logger) *llamaCpp {
	lc := cfg.Engines.LlamaCpp
	if lc.Binary == "" {
		lc.Binary = "llama-server"
	}
	if lc.Ctx <= 0 {
		lc.Ctx = 32768
	}
	if lc.Parallel <= 0 {
		lc.Parallel = 4
	}
	if lc.PortBase <= 0 {
		lc.PortBase = 39200
	}
	timeout := time.Duration(cfg.Engines.LoadTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	var models []config.ModelConfig
	for _, m := range cfg.Models {
		if m.Path != "" && (m.Backend == "" || m.Backend == BackendLlamaCpp) {
			models = append(models, m)
		}
	}
	return &llamaCpp{
		cfg: lc, models: models, timeout: timeout, log: log,
		command:  exec.Command,
		lookPath: exec.LookPath,
		logDir: func() (string, error) {
			d, err := platform.DataDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(d, "logs"), nil
		},
		http:  defaultHTTPClient,
		specs: map[string]llamaSpec{},
		procs: map[string]*llamaProc{},
		exits: map[string]exitInfo{},
	}
}

func (b *llamaCpp) Name() string { return BackendLlamaCpp }

// Available is true when enabled and the server binary can be found.
func (b *llamaCpp) Available() bool {
	if !b.cfg.Enabled {
		return false
	}
	_, err := b.lookPath(b.cfg.Binary)
	return err == nil
}

// List returns configured models plus *.gguf files found in ModelDirs.
func (b *llamaCpp) List(ctx context.Context) ([]ModelInfo, error) {
	specs := b.buildSpecs()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.specs = specs
	out := make([]ModelInfo, 0, len(specs))
	for _, mc := range b.models {
		out = append(out, b.withState(specs[mc.ID].info))
	}
	for _, f := range scanOrder(specs, b.models) {
		out = append(out, b.withState(specs[f].info))
	}
	return out, nil
}

// scanOrder lists the scanned (non-configured) spec IDs in a stable order.
func scanOrder(specs map[string]llamaSpec, configured []config.ModelConfig) []string {
	isCfg := map[string]bool{}
	for _, mc := range configured {
		isCfg[mc.ID] = true
	}
	var ids []string
	for id := range specs {
		if !isCfg[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// withState marks a model loaded when its process is up. Caller holds b.mu.
func (b *llamaCpp) withState(m ModelInfo) ModelInfo {
	if p := b.procs[m.ID]; p != nil && p.alive() {
		m.Loaded = true
	}
	return m
}

// buildSpecs merges configured models with the disk scan; configured
// entries win by ID or path.
func (b *llamaCpp) buildSpecs() map[string]llamaSpec {
	specs := map[string]llamaSpec{}
	paths := map[string]bool{}
	for _, mc := range b.models {
		specs[mc.ID] = b.configuredSpec(mc)
		paths[filepath.Clean(mc.Path)] = true
	}
	for _, f := range scanGGUF(b.cfg.ModelDirs, b.log) {
		if _, dup := specs[f.ID]; dup || paths[filepath.Clean(f.Path)] {
			continue
		}
		specs[f.ID] = llamaSpec{
			info: ModelInfo{ID: f.ID, Backend: BackendLlamaCpp, Name: f.ID, Local: true,
				SizeMiB: f.SizeMiB, Ctx: b.cfg.Ctx, Vision: f.MMProj != "", Path: f.Path},
			path: f.Path, mmproj: f.MMProj, ctx: b.cfg.Ctx, parallel: b.cfg.Parallel,
		}
	}
	return specs
}

func (b *llamaCpp) configuredSpec(mc config.ModelConfig) llamaSpec {
	s := llamaSpec{path: mc.Path, draft: mc.Draft, ctx: mc.Ctx, parallel: mc.Parallel, args: mc.Args}
	if s.ctx <= 0 {
		s.ctx = b.cfg.Ctx
	}
	if s.parallel <= 0 {
		s.parallel = b.cfg.Parallel
	}
	size := mc.SizeMiB
	if size <= 0 {
		if st, err := os.Stat(mc.Path); err == nil {
			size = mib(st.Size())
		}
	}
	name := mc.Name
	if name == "" {
		name = mc.ID
	}
	s.info = ModelInfo{ID: mc.ID, Backend: BackendLlamaCpp, Name: name, Local: true,
		SizeMiB: size, Ctx: s.ctx, Vision: mc.Vision, Path: mc.Path}
	return s
}

// Load starts llama-server for m (if it isn't running) and waits for health.
func (b *llamaCpp) Load(ctx context.Context, m ModelInfo) (Endpoint, error) {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()

	b.mu.Lock()
	if p := b.procs[m.ID]; p != nil && p.alive() {
		b.mu.Unlock()
		return b.endpoint(m, p), nil
	}
	spec, ok := b.specs[m.ID]
	b.mu.Unlock()
	if !ok {
		b.mu.Lock()
		b.specs = b.buildSpecs()
		spec, ok = b.specs[m.ID]
		b.mu.Unlock()
		if !ok {
			return Endpoint{}, fmt.Errorf("llamacpp: %w: %s", ErrUnknownModel, m.ID)
		}
	}
	if m.Ctx > 0 {
		spec.ctx = m.Ctx // manager overrides win
	}
	if m.MaxParallel > 0 {
		spec.parallel = m.MaxParallel // slots = the manager's queue limit
	}
	if m.plan != nil {
		// Too big for the VRAM left: offload a share of the layers (when the
		// GGUF says how many there are) rather than let it run out of memory.
		user := append(slices.Clone(b.cfg.Args), spec.args...)
		need := m.plan.need(spec.info.SizeMiB, spec.ctx, 1)
		if _, ok := gpuRatio(need, m.plan.FreeMiB); ok && !hasNGL(user) {
			layers := 0
			if !cpuOnly(need, spec.info.SizeMiB, m.plan.FreeMiB) {
				layers = ggufLayers(spec.path)
			}
			spec.ngl = llamaNGL(need, spec.info.SizeMiB, layers, m.plan.FreeMiB, user)
		}
	}

	p, logPath, err := b.start(spec)
	if err != nil {
		return Endpoint{}, err
	}
	if err := b.waitHealthy(ctx, p); err != nil {
		b.stop(context.Background(), m.ID, p)
		return Endpoint{}, fmt.Errorf("llamacpp %s: %w%s", m.ID, err, logTail(logPath))
	}
	b.log.Info("model loaded", "backend", BackendLlamaCpp, "model", m.ID, "port", p.port)
	return b.endpoint(m, p), nil
}

// start spawns the process in its own group with output to logs/<id>.log.
func (b *llamaCpp) start(s llamaSpec) (*llamaProc, string, error) {
	port, err := b.freePort()
	if err != nil {
		return nil, "", err
	}
	dir, err := b.logDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	logPath := filepath.Join(dir, safeFileName(s.info.ID)+".log")
	// Keep the previous run's log (a crash's CUDA error or abort) as .log.1.
	if _, err := os.Stat(logPath); err == nil {
		_ = os.Rename(logPath, logPath+".1")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, "", err
	}
	cmd := b.command(b.cfg.Binary, b.args(s, port)...)
	cmd.Stdout, cmd.Stderr = f, f
	platform.NewGroup(cmd)
	fmt.Fprintf(f, "# cerberus %s: %s\n", time.Now().Format(time.RFC3339), strings.Join(cmd.Args, " "))
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, logPath, fmt.Errorf("llamacpp: start %s: %w", b.cfg.Binary, err)
	}
	_ = platform.SetOOMScoreAdj(cmd.Process.Pid, oomScoreAdj) // best effort
	p := &llamaProc{cmd: cmd, port: port, done: make(chan struct{})}
	b.mu.Lock()
	b.procs[s.info.ID] = p
	b.mu.Unlock()
	go b.reap(s.info.ID, p, f)
	return p, logPath, nil
}

// reap waits for the process to exit, records how it ended and forgets
// it. Cerberus' own stop removes the process from procs before it signals
// it, so a process still in procs here ended without Cerberus asking
// (railguard, the OOM killer, a user, a crash): that is logged at Warn and
// handed to the manager's hook.
func (b *llamaCpp) reap(id string, p *llamaProc, f *os.File) {
	err := p.cmd.Wait()
	e := exitInfo{Time: time.Now(), Code: -1}
	if p.cmd.Process != nil {
		e.PID = p.cmd.Process.Pid
	}
	if ps := p.cmd.ProcessState; ps != nil {
		e.Code = ps.ExitCode()
		e.Signal, e.SigNum = platform.ExitSignal(ps)
	}
	b.mu.Lock()
	outside := b.procs[id] == p
	if outside {
		delete(b.procs, id)
	}
	e.ByUs = !outside
	b.exits[id] = e
	hook := b.onExit
	b.mu.Unlock()
	close(p.done)
	f.Close()
	if !outside {
		b.log.Debug("llama-server exited", "model", id, "err", err)
		return
	}
	how := e.Signal
	if how == "" {
		how = fmt.Sprintf("exit code %d", e.Code)
	}
	if hook == nil {
		b.log.Warn("llama-server ended without Cerberus asking", "model", id, "pid", e.PID, "by", how)
		return
	}
	// The manager names the cause and logs the one Warn line.
	b.log.Info("llama-server ended without Cerberus asking", "model", id, "pid", e.PID, "by", how)
	hook(id, e)
}

// enginePID is the pid of the llama-server serving id, 0 when none runs
// (pidReporter).
func (b *llamaCpp) enginePID(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.procs[id]; p != nil && p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// exitSince returns how id's process ended when it ended after since,
// waiting up to wait for one that is still running (exitReporter).
func (b *llamaCpp) exitSince(id string, since time.Time, wait time.Duration) (exitInfo, bool) {
	b.mu.Lock()
	p := b.procs[id]
	b.mu.Unlock()
	if p != nil {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-p.done:
		case <-t.C:
			return exitInfo{}, false
		}
	}
	b.mu.Lock()
	e, ok := b.exits[id]
	b.mu.Unlock()
	if !ok || e.Time.Before(since) {
		return exitInfo{}, false
	}
	return e, true
}

// args builds the llama-server command line.
func (b *llamaCpp) args(s llamaSpec, port int) []string {
	a := []string{"-m", s.path, "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"-c", strconv.Itoa(s.ctx), "-np", strconv.Itoa(s.parallel),
		"--jinja", "--flash-attn", "on", "--metrics"}
	if s.mmproj != "" {
		a = append(a, "--mmproj", s.mmproj)
	}
	if s.draft != "" {
		a = append(a, "-md", s.draft)
	}
	if s.ngl > 0 {
		a = append(a, "-ngl", strconv.Itoa(s.ngl))
	} else if s.ngl < 0 {
		a = append(a, "-ngl", "0") // no room on the GPU (cpuOnly)
	}
	a = append(a, b.cfg.Args...)
	return append(a, s.args...)
}

// freePort finds a port from PortBase that we don't use and that binds.
func (b *llamaCpp) freePort() (int, error) {
	b.mu.Lock()
	used := map[int]bool{}
	for _, p := range b.procs {
		used[p.port] = true
	}
	b.mu.Unlock()
	for port := b.cfg.PortBase; port < b.cfg.PortBase+1000 && port < 65536; port++ {
		if used[port] {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		l.Close()
		return port, nil
	}
	return 0, fmt.Errorf("llamacpp: no free port from %d", b.cfg.PortBase)
}

// waitHealthy polls /health with exponential backoff until 200, the process
// exits, ctx ends or the load timeout passes.
func (b *llamaCpp) waitHealthy(ctx context.Context, p *llamaProc) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", p.port)
	delay := 50 * time.Millisecond
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return errors.New("llama-server exited during load")
		case <-ctx.Done():
			return fmt.Errorf("waiting for health: %w", ctx.Err())
		case <-t.C:
		}
		rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
		ok := getOK(rctx, b.http, url, nil)
		rcancel()
		if ok {
			return nil
		}
		delay = min(delay*2, 2*time.Second)
		t.Reset(delay)
	}
}

// Unload stops m's process (SIGTERM to the group, SIGKILL after 10 s).
func (b *llamaCpp) Unload(ctx context.Context, m ModelInfo) error {
	b.mu.Lock()
	p := b.procs[m.ID]
	b.mu.Unlock()
	if p == nil {
		return nil
	}
	b.stop(ctx, m.ID, p)
	b.log.Info("model unloaded", "backend", BackendLlamaCpp, "model", m.ID)
	return nil
}

// stop terminates p and waits for it to exit (or ctx to end).
func (b *llamaCpp) stop(ctx context.Context, id string, p *llamaProc) {
	b.mu.Lock()
	if b.procs[id] == p {
		delete(b.procs, id)
	}
	b.mu.Unlock()
	platform.Terminate(p.cmd.Process, termGrace, p.done)
	select {
	case <-p.done:
	case <-ctx.Done():
	}
}

// StopAll terminates every process this backend spawned.
func (b *llamaCpp) StopAll(ctx context.Context) {
	b.mu.Lock()
	procs := make(map[string]*llamaProc, len(b.procs))
	for id, p := range b.procs {
		procs[id] = p
	}
	b.mu.Unlock()
	var wg sync.WaitGroup
	for id, p := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.stop(ctx, id, p)
		}()
	}
	wg.Wait()
}

// Slots is the -np llama-server would start m with (models[].parallel,
// else engines.llamacpp.parallel).
func (b *llamaCpp) Slots(m ModelInfo) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.specs[m.ID]; ok && s.parallel > 0 {
		return s.parallel
	}
	return b.cfg.Parallel
}

// Endpoint points at m's running server (empty BaseURL when not running).
func (b *llamaCpp) Endpoint(m ModelInfo) Endpoint {
	b.mu.Lock()
	p := b.procs[m.ID]
	b.mu.Unlock()
	if p != nil && !p.alive() {
		p = nil
	}
	return b.endpoint(m, p)
}

// endpoint points at p (nil: not running, an empty BaseURL).
func (b *llamaCpp) endpoint(m ModelInfo, p *llamaProc) Endpoint {
	ep := Endpoint{Model: modelName(m), Local: true, Backend: BackendLlamaCpp}
	if p != nil && p.port > 0 {
		ep.BaseURL = fmt.Sprintf("http://127.0.0.1:%d/v1", p.port)
		if p.cmd != nil && p.cmd.Process != nil {
			ep.PID = p.cmd.Process.Pid
		}
	}
	return ep
}

// safeFileName keeps IDs usable as file names on every OS.
func safeFileName(id string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, id)
}

// logTail returns the last lines of a log file for error messages.
func logTail(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const n = 2048
	if st, err := f.Stat(); err == nil && st.Size() > n {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	raw, _ := io.ReadAll(io.LimitReader(f, n))
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	return " (log: " + path + "):\n" + string(raw)
}
