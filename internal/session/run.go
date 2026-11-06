package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

const (
	readBufSize = 32 << 10
	stopGrace   = 10 * time.Second
	drainWait   = 2 * time.Second // after exit, how long a grandchild may hold the PTY open
)

// run is one agent process attached to a PTY.
type run struct {
	cmd      *exec.Cmd
	pty      *os.File
	parser   markerParser // used only by the pump goroutine
	redact   redactor     // used only by the pump goroutine
	pinned   []string     // the secret values injected into this run, masked for its whole life
	pumpDone chan struct{}
	waitDone chan struct{} // closed when the process has been reaped

	interactive bool
	idle        *time.Timer // interactive only: fires after IdleWaitSec without output
	idleDur     time.Duration
	idleWaiting atomic.Bool    // waiting because of the idle timer (output resumes it)
	ask         string         // the last "ask" marker, for the next approval wait (guarded by Manager.mu)
	askAlways   []AlwaysOption // the "askrules" marker that followed it (guarded by Manager.mu)
	permits     string         // the agent's copy of the saved rules ("" = not a built-in agent)

	task    string    // the instruction this run was started with (a scope fallback relaunches it)
	resume  bool      // started with the profile's ResumeCommand
	started time.Time // launch time (time limit)
	ended   time.Time // when the process was reaped
	unit    string    // systemd scope unit name ("" = not in a scope)
	head    []byte    // first output of a scoped run (pump only until pumpDone)
	// held is a scoped run's first output while it may still be
	// systemd-run's own "Failed to connect to … bus" complaint: it reaches
	// the terminal only once it turns out to be the agent's (pump only until
	// pumpDone, then wait). A scope fallback drops it, so the transcript keeps
	// only Cerberus' one-line note.
	held    []byte
	holding bool

	limit    *time.Timer  // Limits.MaxMinutes (guarded by Manager.mu)
	maxOut   atomic.Int64 // Limits.MaxOutputMiB in bytes (0 = none)
	outBytes int64        // output of this run (pump only)
	limitHit bool         // the output limit fired (pump only)

	act      textStripper // plain text for Activity (pump only)
	lastLine string       // last complete non-empty line (pump only)

	actMu    sync.Mutex // the activity hand-off below, shared by the pump and its timer
	actLine  string
	actBytes int64
	actTime  time.Time
	actDirty bool
	actSent  time.Time
	actTimer *time.Timer
}

// start creates the worktree of a repo session if needed, then launches the
// agent. Folder and scratch sessions already know their folder.
func (m *Manager) start(s *session) {
	m.mu.Lock()
	id, repo, wt, ws, pinned := s.info.ID, s.info.Repo, s.info.Worktree, s.info.Workspace, s.info.BaseCommit
	m.mu.Unlock()
	if ws == WorkspaceScratch && wt != "" {
		// A scratch folder deleted by hand (or before a restart) comes back empty.
		if err := os.MkdirAll(wt, 0o755); err != nil {
			m.fail(s, fmt.Errorf("create scratch folder: %w", err))
			return
		}
	}
	if wt == "" && ws == WorkspaceRepo {
		// A pinned base (TaskSpec.Base, checked at Create) wins over main.
		base, err := pinned, error(nil)
		if base == "" {
			base, err = mainHead(m.ctx, repo)
		}
		if err == nil {
			wt = filepath.Join(m.wtDir, id)
			err = addWorktree(m.ctx, repo, wt, base)
		}
		if err != nil {
			m.fail(s, fmt.Errorf("create worktree: %w", err))
			return
		}
		m.mu.Lock()
		if final(s.info.State) {
			// Discarded while the worktree was being created (Discard gave
			// up waiting): nothing will clean this one up but us.
			m.mu.Unlock()
			if err := removeWorktree(m.ctx, repo, wt); err != nil {
				m.log.Warn("remove worktree of discarded session", "id", id, "err", err)
			}
			return
		}
		s.info.Worktree, s.info.BaseCommit = wt, base
		m.mu.Unlock()
	}
	if wt != "" {
		// Attachments that waited for the worktree (repo sessions), or
		// whose earlier delivery failed, move into the inbox now.
		rels, moved, err := m.deliverAttachments(m.ctx, s.dir, ws, wt)
		m.mu.Lock()
		m.onAttachmentsLocked(s, rels, moved)
		if len(moved) > 0 {
			m.persistLocked(s)
		}
		m.mu.Unlock()
		if err != nil {
			m.fail(s, err)
			return
		}
	}
	if err := m.launch(s); err != nil {
		m.fail(s, err)
	}
}

// fail puts s in the error state.
func (m *Manager) fail(s *session, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.stopping = false
	s.info.Error = err.Error()
	m.setStateLocked(s, StateError)
}

// launch starts the next run of s (task s.next, agent s.info.Agent) in its
// worktree and moves it to running.
func (m *Manager) launch(s *session) error {
	m.mu.Lock()
	if m.haltedLocked(s) {
		m.stoppedLocked(s) // applies a pending reason such as "killed"
		m.mu.Unlock()
		return nil
	}
	info, task, resume := s.info, s.next, s.resume
	s.resume = false
	m.mu.Unlock()

	p, err := m.sessionProfile(info.Agent, s.dir)
	if err != nil {
		return err
	}
	secrets, err := m.secretEnv(p, info.Repo)
	if err != nil {
		return err
	}
	taskFile := filepath.Join(s.dir, "task.md")
	if err := safeWrite(taskFile, []byte(task+"\n"), 0o600); err != nil {
		return err
	}
	var agentCfg, permitsCopy string
	if isBuiltinAgent(p) {
		if agentCfg, err = m.writeAgentConfig(s.dir); err != nil {
			return fmt.Errorf("write agent config: %w", err)
		}
		if permitsCopy, err = m.writePermits(s.dir, info.Owner); err != nil {
			return fmt.Errorf("write saved permissions: %w", err)
		}
	}
	ph := placeholders{Task: task, TaskFile: taskFile, Model: info.Model, BaseURL: baseURL(m.conf().Server.Listen),
		ServerURL: serverURL(m.conf().Server.Listen),
		Cwd:       info.Worktree, Session: info.ID, Self: selfPath(), ResumeFile: filepath.Join(s.dir, conversationFile)}
	command := p.Command
	if resume && len(p.ResumeCommand) > 0 {
		command = p.ResumeCommand
	}
	argv := ph.expand(command)
	extra, err := m.mcpArgs(p, argv, s.dir, info.ID)
	if err != nil {
		return fmt.Errorf("write MCP config: %w", err)
	}
	var protect []string
	if agentCfg != "" {
		protect = append(protect, agentCfg)
	}
	if permitsCopy != "" {
		protect = append(protect, filepath.Dir(permitsCopy))
	}
	plan, err := m.planSandbox(m.ctx, info, p, s.dir, protect)
	if err != nil {
		m.sandboxRefused(info.ID, err)
		return err
	}
	if plan.fallback != nil {
		m.sandboxWarn(s, "running the agent", plan.fallback)
	}
	setupHash := m.writeSetup(s.dir, setupInput{info: info, profile: p, command: command, resume: resume,
		argv: argv, mcp: extra, sandbox: plan.bin != "", secrets: secrets})
	// The scope goes outside, bwrap inside, the agent's command last.
	argv, unit := m.wrapScope(plan.wrap(append(argv, extra...)), info)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = info.Worktree
	cmd.Env = m.env(info, p, ph, s.dir, secrets)
	if agentCfg != "" {
		cmd.Env = append(cmd.Env, "CERBERUS_AGENT_CONFIG="+agentCfg)
	}
	if permitsCopy != "" {
		cmd.Env = append(cmd.Env, "CERBERUS_AGENT_PERMITS="+permitsCopy)
	}
	if plan.bin != "" {
		cmd.Env = append(cmd.Env, "TMPDIR=/tmp") // the sandbox's /tmp is the only temp folder it has
	}
	// StartPTY makes the child a session and process-group leader, so
	// platform.Terminate reaches the whole tree. Do not also NewGroup it.
	f, err := platform.StartPTY(cmd, max(info.Cols, 20), max(info.Rows, 5))
	if err != nil {
		return fmt.Errorf("start %s: %w", p.Name, err)
	}
	r := &run{cmd: cmd, pty: f, pumpDone: make(chan struct{}), waitDone: make(chan struct{}), interactive: p.Interactive,
		task: task, resume: resume, started: time.Now(), unit: unit, holding: unit != "", permits: permitsCopy}
	for _, v := range secrets {
		r.pinned = append(r.pinned, v)
	}
	m.refreshRedact(s, r, time.Now())
	m.startRecording(s, r, info)
	if p.Interactive && m.conf().Sessions.IdleWaitSec > 0 {
		r.idleDur = time.Duration(m.conf().Sessions.IdleWaitSec) * time.Second
		r.idle = time.AfterFunc(r.idleDur, func() { m.onIdle(s, r) })
	}

	m.mu.Lock()
	s.run = r
	if s.info.RunStarted.IsZero() {
		s.info.RunStarted = r.started
	}
	s.info.AttemptStarted = r.started
	s.next = ""
	if setupHash != "" {
		s.info.SetupHash = setupHash
	}
	s.info.Attempt++
	s.agentAttempts++
	s.info.ExitCode = nil
	s.info.Error = ""
	if !s.stopping {
		s.stopReason = "" // else a Kill or limit came between runs: keep its reason
	}
	m.setStateLocked(s, StateRunning)
	m.applyLimitsLocked(s, r)
	if m.haltedLocked(s) {
		terminateRun(r)
	}
	s.work.Add(1)
	m.wg.Add(1)
	m.mu.Unlock()

	go m.pump(s, r)
	go m.wait(s, r)
	return nil
}

// env is the agent's environment: ours, minus anything we set, plus the
// Cerberus variables, the profile's own (placeholders expanded) and the
// session's secrets.
func (m *Manager) env(info Info, p config.AgentProfile, ph placeholders, sessDir string, secrets map[string]string) []string {
	set := map[string]string{
		"CERBERUS_SESSION":     info.ID,
		"CERBERUS_SESSION_DIR": sessDir, // the built-in agent keeps conversation.json here
		"CERBERUS_URL":         serverURL(m.conf().Server.Listen),
		"OPENAI_BASE_URL":      ph.BaseURL,
		"OPENAI_API_KEY":       m.agentKey(info.ID), // /v1 + this session's /api/agent scope, never the admin token
		"CERBERUS_REPO":        info.Repo,           // deny_paths also apply to the repo the worktree came from
		"TERM":                 "xterm-256color",
		previewTagEnv:          m.previewTag(info.ID), // live preview ownership of servers that leave the agent's tree
	}
	for k, v := range p.Env {
		if v == apiKeyPlaceholder {
			set[k] = set["OPENAI_API_KEY"]
			continue
		}
		set[k] = ph.expand([]string{v})[0]
	}
	for k, v := range secrets {
		set[k] = v
	}
	env := make([]string, 0, len(os.Environ())+len(set))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ours := set[k]; !ours {
			env = append(env, kv)
		}
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return env
}

// runSecrets are the values to mask for r: the vault's current values plus
// the ones injected into r at launch, which stay masked even after the
// secret is rotated or deleted in the vault (the run still holds them).
// It may run secret-tool, so it is never called with m.mu held.
func (m *Manager) runSecrets(r *run) []string {
	vals := m.secretValues()
	if r == nil || len(r.pinned) == 0 {
		return vals
	}
	return append(append([]string(nil), r.pinned...), vals...)
}

// refreshRedact reloads r's redaction values when they are older than
// redactRefresh, and hands them to the transcript pass. Pump goroutine (or
// launch, before the pump starts) only.
func (m *Manager) refreshRedact(s *session, r *run, now time.Time) {
	if !r.redact.fetched.IsZero() && now.Sub(r.redact.fetched) < redactRefresh {
		return
	}
	r.redact.fetched = now
	vals := m.runSecrets(r)
	r.redact.set(vals)
	s.out.setSecrets(vals)
}

// pump reads the PTY until it closes: markers out, secrets masked, output to
// the scrollback, transcript and subscribers, state changes from markers.
//
// Markers are parsed first, on the raw stream, so a secret can never hide
// one; the redactor then sees only text that reaches people. Each stage holds
// back its own incomplete tail, and at the end the parser's tail goes through
// the redactor before both are flushed.
func (m *Manager) pump(s *session, r *run) {
	defer close(r.pumpDone)
	buf := make([]byte, readBufSize)
	for {
		n, err := r.pty.Read(buf)
		if n > 0 {
			if r.unit != "" && len(r.head) < scopeHeadBytes {
				r.head = append(r.head, buf[:min(n, scopeHeadBytes-len(r.head))]...)
			}
			chunk := buf[:n]
			if r.holding {
				r.held = append(r.held, chunk...)
				if time.Since(r.started) <= scopeFailWindow && len(r.held) <= scopeHeadBytes && mayBeScopeFailure(r.held) {
					chunk = nil
				} else {
					r.holding = false
					chunk, r.held = r.held, nil
				}
			}
			if len(chunk) > 0 {
				m.process(s, r, chunk)
			}
		}
		if err != nil {
			break
		}
	}
	if r.holding && len(r.held) > 0 && !looksLikeScopeFailure(r.held) {
		m.process(s, r, r.held) // the run ended on a partial "Failed to…": it was the agent's
		r.held = nil
	}
	r.holding = false
	if rest := r.redact.feed(r.parser.flush()); len(rest) > 0 {
		s.out.Write(rest)
	}
	if rest := r.redact.flush(); len(rest) > 0 {
		s.out.Write(rest)
	}
	s.out.stopCast()
	m.flushActivity(s, r)
}

// process sends one chunk of agent output through the marker parser and the
// redactor to the scrollback, transcript and subscribers. Pump goroutine only.
func (m *Manager) process(s *session, r *run, chunk []byte) {
	m.refreshRedact(s, r, time.Now())
	out, marks := r.parser.feed(chunk)
	if out = r.redact.feed(out); len(out) > 0 {
		s.out.Write(out)
		m.trackOutput(s, r, out)
		m.activity(s, r)
	}
	for _, mk := range marks {
		mk.text = string(r.redact.mask(nil, []byte(mk.text)))
		m.onMarker(s, r, mk)
	}
}

// activity restarts the idle timer and ends an idle wait.
func (m *Manager) activity(s *session, r *run) {
	if r.idle == nil {
		return
	}
	r.idle.Reset(r.idleDur)
	if r.idleWaiting.Load() {
		m.resume(s, r, true)
	}
}

func (m *Manager) onIdle(s *session, r *run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.run != r || s.info.State != StateRunning {
		return
	}
	r.idleWaiting.Store(true)
	s.info.WaitingReason = fmt.Sprintf("no output for %ds", int(r.idleDur/time.Second))
	m.setStateLocked(s, StateWaiting)
}

// resume moves a waiting run back to running (idleOnly: only an idle wait).
func (m *Manager) resume(s *session, r *run, idleOnly bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.run != r || s.info.State != StateWaiting || (idleOnly && !r.idleWaiting.Load()) {
		return
	}
	r.idleWaiting.Store(false)
	m.setStateLocked(s, StateRunning)
}

func (m *Manager) onMarker(s *session, r *run, mk marker) {
	switch mk.kind {
	case "waiting":
		m.markWaiting(s, r, orDefault(mk.text, "input needed"))
	case "ask":
		m.mu.Lock()
		if s.run == r {
			r.ask, r.askAlways = mk.text, nil
		}
		m.mu.Unlock()
	case "askrules":
		// Taken only right after an "ask" (and only once per ask): output
		// that is not the agent's cannot attach options to nothing. A save
		// validates the option again anyway.
		opts := parseAlways(mk.text, func(t string) string { return string(r.redact.mask(nil, []byte(t))) })
		m.mu.Lock()
		if s.run == r && r.ask != "" && r.askAlways == nil {
			r.askAlways = opts
		}
		m.mu.Unlock()
	case "permit":
		// A saved rule answered a call: count the use.
		m.mu.Lock()
		owner, ok := s.info.Owner, s.run == r && r.permits != ""
		m.mu.Unlock()
		if ok {
			m.permits.Bump(owner, strings.TrimSpace(mk.text))
		}
	case "resumed":
		m.resume(s, r, false)
	case "done":
		if r.interactive {
			m.markWaiting(s, r, "agent finished")
		}
	default:
		m.log.Debug("unknown cerberus marker", "kind", mk.kind)
	}
}

func (m *Manager) markWaiting(s *session, r *run, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.run != r || (s.info.State != StateRunning && s.info.State != StateWaiting) {
		return
	}
	r.idleWaiting.Store(false)
	s.info.WaitingReason = reason
	if reason == "approval" {
		s.info.AskSeq++
		s.info.WaitingAsk, r.ask = r.ask, ""
		s.info.WaitingAlways, r.askAlways = r.askAlways, nil
	} else {
		s.info.WaitingAsk = ""
		s.info.WaitingAlways = nil
	}
	m.setStateLocked(s, StateWaiting)
}

// wait reaps the process, drains the PTY and decides what happens next.
func (m *Manager) wait(s *session, r *run) {
	defer m.wg.Done()
	defer s.work.Done()
	err := r.cmd.Wait()
	// Nothing the agent started may outlive it: background servers, nohup'd
	// jobs and children in their own process groups would keep ports, memory
	// and the worktree busy. Kill before closing waitDone, so terminateRun
	// can rely on the session being gone once waitDone is closed.
	platform.KillSession(r.cmd.Process.Pid)
	r.ended = time.Now()
	close(r.waitDone)
	code := -1
	if r.cmd.ProcessState != nil {
		code = r.cmd.ProcessState.ExitCode()
	} else if err != nil {
		m.log.Warn("agent wait", "id", s.info.ID, "err", err)
	}
	t := time.NewTimer(drainWait)
	select {
	case <-r.pumpDone:
	case <-t.C:
	}
	t.Stop()
	r.pty.Close() // unblocks the pump if a grandchild still holds the terminal
	<-r.pumpDone
	if r.idle != nil {
		r.idle.Stop()
	}
	m.mu.Lock()
	if r.limit != nil {
		r.limit.Stop()
		r.limit = nil
	}
	s.info.RunEnded = r.ended // persisted with the state change onExit makes
	m.mu.Unlock()
	m.flushActivity(s, r) // the pump has ended: nothing re-arms the timer
	if m.scopeFallback(s, r, code) {
		return // the held systemd-run complaint is dropped with the attempt
	}
	if len(r.held) > 0 {
		// It read like systemd-run's complaint but scopes work: it was the
		// agent's own output after all.
		s.out.Write(r.redact.mask(nil, r.held))
		r.held = nil
	}
	m.onExit(s, r, code)
}

// onExit handles the end of a run: stopped, or checks and a verdict.
func (m *Manager) onExit(s *session, r *run, code int) {
	m.mu.Lock()
	if s.run == r {
		s.run = nil
	}
	s.info.ExitCode = &code
	if m.haltedLocked(s) {
		m.stoppedLocked(s)
		m.mu.Unlock()
		s.out.release()
		return
	}
	repo, wt := s.info.Repo, s.info.Worktree
	m.mu.Unlock()

	res := m.autoChecks(s, repo, wt)
	if res.ran {
		// Before taking m.mu: reading the vault may run secret-tool.
		res.masked = redactString(res.output, m.runSecrets(r))
	}
	m.mu.Lock()
	relaunch := m.verdictLocked(s, code, res)
	m.mu.Unlock()
	if relaunch {
		if err := m.launch(s); err != nil {
			m.fail(s, err)
		}
	}
	m.mu.Lock()
	idle := s.run == nil
	m.mu.Unlock()
	if idle {
		s.out.release()
	}
}

// haltedLocked reports whether s must not go on: a stop was requested, the
// daemon is closing, or the session was discarded.
func (m *Manager) haltedLocked(s *session) bool {
	return s.stopping || m.closing || final(s.info.State)
}

// stoppedLocked settles s as stopped after a requested stop.
func (m *Manager) stoppedLocked(s *session) {
	s.stopping = false
	if final(s.info.State) { // discarded meanwhile: nothing to settle
		s.stopReason = ""
		return
	}
	switch {
	case s.stopReason != "":
		s.info.WaitingReason = s.stopReason
		s.stopReason = ""
	case m.closing:
		s.info.WaitingReason = "daemon stopped"
	}
	m.setStateLocked(s, StateStopped)
}

// checkResult is the outcome of a checks pass.
type checkResult struct {
	ran, ok, cancelled bool
	output             string // raw, for the agent's next task
	masked             string // output with secrets masked, for people
}

// autoChecks runs the repo's checks after a run, if it has any.
func (m *Manager) autoChecks(s *session, repo, wt string) checkResult {
	checks := m.settings(repo, wt).Checks
	if len(checks) == 0 {
		return checkResult{}
	}
	m.mu.Lock()
	if m.haltedLocked(s) {
		m.mu.Unlock()
		return checkResult{cancelled: true}
	}
	ctx := m.beginChecksLocked(s)
	m.mu.Unlock()
	return m.execChecks(ctx, s, wt, checks)
}

// beginChecksLocked moves s to checking and returns the cancellable context.
func (m *Manager) beginChecksLocked(s *session) context.Context {
	ctx, cancel := context.WithCancel(m.ctx)
	s.checkCancel = cancel
	m.setStateLocked(s, StateChecking)
	return ctx
}

// execChecks runs checks in wt and echoes progress into the terminal stream.
func (m *Manager) execChecks(ctx context.Context, s *session, wt string, checks []string) checkResult {
	fmt.Fprintf(s.out, "\r\n\x1b[2m── cerberus: running %d check(s) ──\x1b[0m\r\n", len(checks))
	timeout := time.Duration(max(m.conf().Sessions.ChecksTimeoutSec, 1)) * time.Second
	// Checks may come from .cerberus.json, which the agent can write, so
	// they run in the session's sandbox too.
	m.mu.Lock()
	info := s.info
	m.mu.Unlock()
	p, _ := m.conf().AgentProfile(info.Agent)
	var ok bool
	var out string
	plan, err := m.planSandbox(ctx, info, p, s.dir, nil)
	switch {
	case err != nil:
		m.sandboxRefused(info.ID, err)
		out = fmt.Sprintf("checks not run: %v\n", err)
	default:
		if plan.fallback != nil {
			m.sandboxWarn(s, "running the checks", plan.fallback)
		}
		ok, out = runChecks(ctx, wt, checks, timeout, []string{"CERBERUS_SESSION=" + s.info.ID, "GIT_TERMINAL_PROMPT=0"}, plan.wrap)
	}
	res := checkResult{ran: true, ok: ok, output: out, cancelled: ctx.Err() != nil}
	m.mu.Lock()
	if s.checkCancel != nil {
		s.checkCancel()
		s.checkCancel = nil
	}
	m.mu.Unlock()
	verdict := "\x1b[32mchecks passed\x1b[0m"
	if !ok {
		verdict = "\x1b[31mchecks failed\x1b[0m"
	}
	fmt.Fprintf(s.out, "\x1b[2m── cerberus:\x1b[0m %s \x1b[2m──\x1b[0m\r\n", verdict)
	return res
}

// verdictLocked sets the state after a run and its checks. It reports
// whether another run (retry or escalation) should be launched.
func (m *Manager) verdictLocked(s *session, code int, res checkResult) bool {
	if res.cancelled || m.haltedLocked(s) {
		m.stoppedLocked(s)
		return false
	}
	if !res.ran {
		if code == 0 {
			m.setStateLocked(s, StatePassed)
		} else {
			s.info.Error = fmt.Sprintf("agent exited with code %d", code)
			m.setStateLocked(s, StateFailed)
		}
		return false
	}
	s.info.ChecksOutput = res.masked
	if res.ok {
		m.setStateLocked(s, StatePassed)
		return false
	}
	if s.agentAttempts < s.maxAttempts {
		s.next = retryTask(s.task, res.output)
		m.setStateLocked(s, StateRetrying)
		return true
	}
	if !s.escalated && s.info.EscalateTo != "" {
		s.escalated = true
		s.agentAttempts = 0
		s.info.Agent = s.info.EscalateTo
		if p, ok := m.conf().AgentProfile(s.info.Agent); ok && p.NeedsModel && s.info.Model == "" {
			s.info.Model = m.conf().Engines.DefaultModel
			if s.info.Model == "" {
				// A fresh install: the gateway's "default" alias stands for
				// the only (loaded) model. Not resolved here, under m.mu.
				s.info.Model = "default"
			}
		}
		s.next = escalateTask(s.task, res.output)
		m.setStateLocked(s, StateEscalated)
		return true
	}
	s.info.Error = "checks failed"
	m.setStateLocked(s, StateFailed)
	return false
}

func retryTask(task, checks string) string {
	return "The checks failed. Fix the problem.\n\nOriginal task:\n" + task + "\n\nCheck output:\n" + checks
}

func escalateTask(task, checks string) string {
	return task + "\n\nA previous agent left work in this tree; review it. " +
		"Its work did not pass the checks.\n\nCheck output:\n" + checks
}

// terminateRun hangs up the run's PTY process group, then terminates it
// (SIGTERM, then SIGKILL to the whole session after stopGrace). Interactive
// shells and CLIs ignore SIGTERM but exit on SIGHUP, so without the hangup
// they would always wait out the full grace period.
//
// StartPTY makes the agent a session leader, so its pgid and sid equal its
// PID; the group is signalled by that id, never looked up. Once waitDone is
// closed the leader is reaped and wait() has already killed the session, so
// there is nothing left to signal (and the PID may belong to someone else).
func terminateRun(r *run) {
	select {
	case <-r.waitDone:
		return
	default:
	}
	pid := r.cmd.Process.Pid
	if runtime.GOOS == "windows" {
		_ = r.cmd.Process.Kill()
		return
	}
	platform.HangupGroup(pid)
	platform.TerminateGroup(pid)
	go func() {
		t := time.NewTimer(stopGrace)
		defer t.Stop()
		select {
		case <-r.waitDone: // wait() killed the session after reaping the leader
		case <-t.C:
			platform.KillSession(pid)
		}
	}()
}
