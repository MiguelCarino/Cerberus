package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"carino.systems/cerberus/internal/platform"
)

const (
	inputTimeout   = 5 * time.Second
	discardTimeout = 15 * time.Second
)

// Attach returns the recent output and a live stream of new output. The
// channel is closed if the reader falls too far behind (reattach for a fresh
// backlog) or when the session is accepted or discarded. Call detach when done.
func (m *Manager) Attach(id string) ([]byte, <-chan []byte, func(), error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	m.mu.Unlock()
	if err != nil {
		return nil, nil, nil, err
	}
	backlog, ch, detach := s.out.attach()
	return backlog, ch, detach, nil
}

// Input writes data to the agent's terminal. Answering a waiting agent
// moves it back to running.
func (m *Manager) Input(id string, data []byte) error {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	r := s.run
	if r == nil {
		m.mu.Unlock()
		return ErrNotRunning
	}
	if s.info.State == StateWaiting {
		r.idleWaiting.Store(false)
		m.setStateLocked(s, StateRunning)
	}
	m.mu.Unlock()
	_ = r.pty.SetWriteDeadline(time.Now().Add(inputTimeout)) // a stuck child must not hang the caller
	_, err = r.pty.Write(data)
	return err
}

// Approval answers for AnswerApproval: the built-in agent's y / a / n.
const (
	ApproveOnce   = "y"
	ApproveAlways = "a"
	ApproveNo     = "n"
)

// AnswerApproval answers an approval wait from outside the terminal: it
// types answer and Enter only while the session still waits for approval
// number seq (Info.AskSeq), so an answer from a stale page never lands on
// a later prompt. answer is y, a or n, or any other one-line text, which
// the agent takes as a refusal with that reason. The returned Info is the
// session before the answer (for the audit detail).
func (m *Manager) AnswerApproval(id string, seq int, answer string) (Info, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" || strings.ContainsFunc(answer, func(r rune) bool { return r < 0x20 || r == 0x7f }) || utf8.RuneCountInString(answer) > 500 {
		return Info{}, ErrBadAnswer
	}
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return Info{}, err
	}
	r := s.run
	if r == nil || s.info.State != StateWaiting || s.info.WaitingReason != "approval" || s.info.AskSeq != seq {
		m.mu.Unlock()
		return Info{}, ErrNoApproval
	}
	before := m.snapshotLocked(s)
	// Running from here on, so a second answer for the same seq is refused.
	r.idleWaiting.Store(false)
	m.setStateLocked(s, StateRunning)
	m.mu.Unlock()
	_ = r.pty.SetWriteDeadline(time.Now().Add(inputTimeout))
	_, err = r.pty.Write([]byte(answer + "\r"))
	return before, err
}

// Resize sets the terminal size (also used for later runs).
func (m *Manager) Resize(id string, cols, rows int) error {
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return fmt.Errorf("invalid terminal size %dx%d", cols, rows)
	}
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	s.info.Cols, s.info.Rows = cols, rows
	r := s.run
	m.mu.Unlock()
	if r == nil {
		return nil
	}
	return platform.ResizePTY(r.pty, cols, rows)
}

// FollowUp sends text to a running agent, or starts a new run of the same
// agent in the same worktree with text as its task.
func (m *Manager) FollowUp(ctx context.Context, id, text string) error {
	return m.followUp(id, text, true)
}

// FollowUpIdle is FollowUp for the agent scope (cerberus_send): it only
// starts a new run of a finished or stopped session and never types into a
// running one, so a parent agent cannot answer its child's approval prompts
// or drive a child shell line by line. A session with a run (or about to
// get one) is ErrBusy.
func (m *Manager) FollowUpIdle(ctx context.Context, id, text string) error {
	if err := agentArgErr("follow-up", text); err != nil {
		return err // it becomes the next run's {task}
	}
	return m.followUp(id, text, false)
}

func (m *Manager) followUp(id, text string, typeIntoRun bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("follow-up text is empty")
	}
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if s.run != nil && !typeIntoRun {
		m.mu.Unlock()
		return busyError("the session is " + s.info.State + "; send works once it has finished or stopped")
	}
	mention := m.followUpMentionLocked(s)
	if s.run != nil {
		pending := s.pendingFiles
		m.mu.Unlock()
		typed := text
		if mention != "" {
			typed += " (" + mention + ")" // one line: a newline would send the text early
		}
		if err := m.Input(id, []byte(typed+"\r")); err != nil {
			return err
		}
		if mention != "" {
			m.clearPendingFiles(id, pending)
		}
		m.Record(id, FeedFollowUp, text)
		return nil
	}
	defer m.mu.Unlock()
	if err := m.idleForWorkLocked(s); err != nil {
		return err
	}
	if internalAgent(s.info.Agent) {
		// An internal run (a fine-tuning trainer) ignores {task} and writes
		// into a fixed folder: a follow-up would quietly run it again over
		// the first run's output. A new run starts from Settings.
		return ErrNoFollowUp
	}
	m.recordLocked(s, FeedFollowUp, text)
	if mention != "" {
		text += "\n\n" + mention
	}
	s.pendingFiles = nil
	s.task, s.next = text, text
	s.resume = false // a follow-up starts the profile's normal command
	s.agentAttempts = 0
	s.info.Error = ""
	m.setStateLocked(s, StateQueued)
	m.signal()
	return nil
}

// followUpMentionLocked is the "Files added: …" sentence for the next
// follow-up of s: the inbox paths added since the last one, for agents
// other than the built-in one (which lists new inbox files itself).
func (m *Manager) followUpMentionLocked(s *session) string {
	if len(s.pendingFiles) == 0 {
		return ""
	}
	if p, ok := m.conf().AgentProfile(s.info.Agent); ok && isBuiltinAgent(p) {
		return ""
	}
	return filesMention(s.pendingFiles)
}

// clearPendingFiles forgets the mentioned paths (and only those: a file
// added meanwhile waits for the next follow-up).
func (m *Manager) clearPendingFiles(id string, sent []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || len(sent) == 0 {
		return
	}
	s.pendingFiles = slices.DeleteFunc(s.pendingFiles, func(p string) bool { return slices.Contains(sent, p) })
	m.persistLocked(s)
}

// ErrNoFollowUp is returned for a follow-up that would start an internal
// session (a fine-tuning run) again.
var ErrNoFollowUp = errors.New("a fine-tuning run takes no follow-ups: start a new run from Settings › Fine-tuning")

// busyError is an ErrBusy with its own message.
type busyError string

func (e busyError) Error() string        { return string(e) }
func (e busyError) Is(target error) bool { return target == ErrBusy }

// idleForWorkLocked returns an error unless s is finished, keeps its
// worktree and has nothing in flight.
func (m *Manager) idleForWorkLocked(s *session) error {
	switch {
	case final(s.info.State):
		return fmt.Errorf("session is %s", s.info.State)
	case s.needsApproval:
		return errors.New("this spawned session was never approved: discard it")
	case s.op || s.run != nil || s.checkCancel != nil || live(s.info.State) || s.info.State == StateQueued:
		return ErrBusy
	case s.info.Worktree == "":
		return errors.New("session has no worktree")
	}
	return nil
}

// RunChecks runs the repo's checks again in the background (state checking,
// then passed or failed; no retry).
func (m *Manager) RunChecks(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookup(id)
	if err != nil {
		return err
	}
	if err := m.idleForWorkLocked(s); err != nil {
		return err
	}
	wt := s.info.Worktree
	checks := m.settings(s.info.Repo, wt).Checks
	if len(checks) == 0 {
		return errors.New("this repo has no checks configured")
	}
	cctx := m.beginChecksLocked(s)
	s.work.Add(1)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer s.work.Done()
		res := m.execChecks(cctx, s, wt, checks)
		masked := redactString(res.output, m.secretValues()) // before m.mu: may run secret-tool
		m.mu.Lock()
		switch {
		case res.cancelled || s.stopping || m.closing:
			m.stoppedLocked(s)
		case res.ok:
			s.info.ChecksOutput, s.info.Error = masked, ""
			m.setStateLocked(s, StatePassed)
		default:
			s.info.ChecksOutput, s.info.Error = masked, "checks failed"
			m.setStateLocked(s, StateFailed)
		}
		m.mu.Unlock()
		s.out.release()
	}()
	return nil
}

// Stop ends the agent (SIGTERM to its process group, SIGKILL after 10 s) or
// the running checks, or takes a session out of the queue.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookup(id)
	if err != nil {
		return err
	}
	if !m.stopLocked(s) {
		return ErrNotRunning
	}
	return nil
}

// stopLocked requests a stop and reports whether there was anything to stop.
// The waiter settles the state once the process is gone.
func (m *Manager) stopLocked(s *session) bool {
	switch {
	case s.run != nil:
		s.stopping = true
		terminateRun(s.run)
	case s.checkCancel != nil:
		s.stopping = true
		s.checkCancel()
	case s.info.State == StateQueued:
		// A pending spawn keeps needsApproval: it was never approved, so it
		// can be discarded but never run (idleForWorkLocked refuses it).
		s.info.WaitingReason = pendingStopReason(s, "")
		m.setStateLocked(s, StateStopped)
	case live(s.info.State): // starting, retrying or escalated: between runs
		s.stopping = true
	default:
		return false
	}
	return true
}

// pendingStopReason is the WaitingReason of a queued session taken out of
// the queue: reason ("" or "killed") for an approved one; for a spawn that
// never got approval, "rejected" stays and anything else says so.
func pendingStopReason(s *session, reason string) string {
	switch {
	case !s.needsApproval:
		return reason
	case s.info.WaitingReason == "rejected":
		return "rejected"
	}
	return "stopped before approval"
}

// Diff is the unified diff of the worktree against BaseCommit, including
// untracked files.
func (m *Manager) Diff(id string) (string, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	var wt, base, ws string
	if err == nil {
		wt, base, ws = s.info.Worktree, s.info.BaseCommit, s.info.Workspace
	}
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	if ws != WorkspaceRepo {
		return "", fmt.Errorf("a %s session has no diff: only repo sessions are tracked by git", ws)
	}
	if wt == "" || base == "" {
		return "", errors.New("session has no worktree")
	}
	if err := checkWorktree(m.ctx, s.info.Repo, wt); err != nil {
		return "", err
	}
	return diffWorktree(m.ctx, wt, base, s.dir)
}

// Accept commits the worktree's changes, squashes them onto main as one
// commit, fast-forwards main in the main checkout, optionally pushes, and
// removes the worktree. On failure the worktree is kept and the error
// returned. If only the push fails, the session is still accepted and both
// the sha and the push error are returned.
func (m *Manager) Accept(ctx context.Context, id string, o AcceptOpts) (string, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err == nil {
		err = m.idleForWorkLocked(s)
	}
	if err == nil && s.info.Workspace != WorkspaceRepo {
		err = fmt.Errorf("a %s session has nothing to accept: its files are already where they belong", s.info.Workspace)
	}
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	s.op = true
	repo, wt := s.info.Repo, s.info.Worktree
	msg := strings.TrimSpace(o.Message)
	if msg == "" {
		msg = orDefault(s.info.Title, titleFrom(s.info.Task))
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		s.op = false
		m.mu.Unlock()
	}()

	// Git runs on the manager's context: a dropped HTTP request must not
	// interrupt a rebase or merge halfway.
	gctx := m.ctx
	if err := checkWorktree(gctx, repo, wt); err != nil {
		return "", err
	}
	branch, err := mainBranch(gctx, repo)
	if err != nil {
		return "", err
	}
	sha, base, err := m.squashOntoMain(gctx, repo, wt, branch, msg)
	if base != "" {
		// The worktree now sits on base: diffs must be taken against it, or
		// main's newer commits would show up as the session's work.
		m.mu.Lock()
		s.info.BaseCommit = base
		m.persistLocked(s)
		m.mu.Unlock()
	}
	if err != nil {
		return "", err
	}
	if _, err := runGit(gctx, repo, nil, "merge", "--ff-only", "-q", sha); err != nil {
		return "", fmt.Errorf("fast-forward %s (the worktree is kept): %w", branch, err)
	}
	var pushErr error
	if o.Push {
		pushErr = pushOnly(gctx, repo, branch, sha)
	}
	m.endPreview(s, "session accepted")
	if err := removeWorktree(gctx, repo, wt); err != nil {
		m.log.Warn("remove worktree after accept", "id", id, "err", err)
	}
	m.mu.Lock()
	s.info.Worktree = ""
	s.info.Error = ""
	m.setStateLocked(s, StateAccepted)
	m.mu.Unlock()
	s.out.closeSubs()
	if pushErr != nil {
		return sha, fmt.Errorf("accepted locally as %.12s, but push failed: %w", sha, pushErr)
	}
	return sha, nil
}

// Discard stops the session and marks it discarded. A repo session's
// worktree and a scratch session's folder are deleted (Cerberus owns them);
// a folder session's folder is never touched.
func (m *Manager) Discard(id string) error {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err == nil && final(s.info.State) {
		err = fmt.Errorf("session is %s", s.info.State)
	} else if err == nil && s.op {
		err = ErrBusy
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	s.op = true
	m.stopLocked(s)
	m.mu.Unlock()

	if !waitTimeout(&s.work, discardTimeout) {
		m.log.Warn("discard: session did not stop in time", "id", id)
	}
	m.endPreview(s, "session discarded")
	m.mu.Lock()
	repo, wt, ws := s.info.Repo, s.info.Worktree, s.info.Workspace
	m.mu.Unlock()
	if err := m.removeWorkspace(ws, repo, wt, s.dir); err != nil {
		m.mu.Lock()
		s.op = false
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	s.op = false
	s.stopping = false
	s.info.Worktree = ""
	m.setStateLocked(s, StateDiscarded)
	m.mu.Unlock()
	s.out.closeSubs()
	s.out.release()
	return nil
}

// removeWorkspace deletes what Cerberus owns of a session's workspace.
func (m *Manager) removeWorkspace(ws, repo, wt, sessDir string) error {
	switch ws {
	case WorkspaceFolder:
		return nil // the user's own folder: never deleted or modified
	case WorkspaceScratch:
		if wt == "" || !within(wt, sessDir) {
			return nil // only ever the session's own workspace/ folder
		}
		return os.RemoveAll(wt)
	default:
		if wt != "" && !within(wt, m.wtDir) {
			// Repo worktrees live only in DATA/worktrees; anything else
			// came from an edited meta.json and is never deleted.
			m.log.Warn("not removing a worktree outside the worktrees folder", "path", wt)
			return nil
		}
		return removeWorktree(m.ctx, repo, wt)
	}
}

// waitTimeout waits for wg up to d and reports whether it finished.
func waitTimeout(wg interface{ Wait() }, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
