package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	activityEvery     = time.Second // Activity / LastOutput / OutputBytes reach readers at most this often
	activityRunes     = 160
	activityScan      = 4 << 10         // bytes of the last line masked and clipped for Activity
	scopeHeadBytes    = 4 << 10         // output of a scoped run kept to recognise a scope failure
	scopeFailWindow   = 5 * time.Second // a scope that fails does so at once
	scopeProbeTimeout = 10 * time.Second
)

// limitMinute is the unit of Limits.MaxMinutes (a variable so tests run fast).
var limitMinute = time.Minute

// busFailures are what systemd-run prints when it cannot reach the user
// manager (no user bus: a container, sudo, an SSH login without lingering).
var busFailures = []string{
	"failed to connect to bus",
	"failed to connect to user scope bus",
	"failed to create bus connection",
	"failed to start transient scope unit",
	"dbus_session_bus_address",
}

// looksLikeScopeFailure reports whether a scoped run's first output says
// systemd-run could not create the scope.
func looksLikeScopeFailure(head []byte) bool {
	h := strings.ToLower(string(head))
	for _, f := range busFailures {
		if strings.Contains(h, f) {
			return true
		}
	}
	return false
}

// mayBeScopeFailure reports whether a scoped run's output so far could
// still be systemd-run's own failure message: every complete line starts
// with "Failed to" (case-insensitive) and the unfinished last line could
// become one. Anything else is the agent's output and is shown at once.
func mayBeScopeFailure(out []byte) bool {
	const prefix = "failed to"
	lines := strings.Split(strings.ToLower(string(out)), "\n")
	for i, l := range lines {
		l = strings.TrimLeft(strings.TrimRight(l, "\r"), "\r")
		if i == len(lines)-1 {
			return strings.HasPrefix(l, prefix) || strings.HasPrefix(prefix, l)
		}
		if !strings.HasPrefix(l, prefix) {
			return false
		}
	}
	return true
}

// wrapScope runs argv inside a systemd scope with a memory cap when
// configured and available (Linux only), and returns the scope's unit name
// ("" when not wrapped). The session's Limits.MemoryMax overrides
// Guard.SessionMemoryMax. Once the user bus proved unreachable it never
// wraps again in this daemon's lifetime.
func (m *Manager) wrapScope(argv []string, info Info) ([]string, string) {
	g := m.conf().Guard
	if runtime.GOOS != "linux" || g.SystemdSlice == "" || m.noScope.Load() {
		return argv, ""
	}
	sr, err := exec.LookPath("systemd-run")
	if err != nil {
		return argv, ""
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	unit := "cerberus-" + info.ID + "-" + strconv.Itoa(info.Attempt+1) + "-" + hex.EncodeToString(b)
	w := []string{sr, "--user", "--scope", "--quiet", "--unit=" + unit, "--slice=" + g.SystemdSlice}
	if mem := orDefault(info.Limits.MemoryMax, g.SessionMemoryMax); mem != "" {
		w = append(w, "-p", "MemoryMax="+mem)
	}
	return append(append(w, "--"), argv...), unit
}

// scopeFallback handles a run whose systemd scope could not start (its
// output looks like a bus failure and a probe scope fails too): it
// remembers that scopes do not work here, logs it once, and relaunches the
// same attempt without the scope. It reports whether it took over.
func (m *Manager) scopeFallback(s *session, r *run, code int) bool {
	if r.unit == "" || code == 0 || r.ended.Sub(r.started) > scopeFailWindow || !looksLikeScopeFailure(r.head) {
		return false
	}
	// The run's output is mostly the agent's own, so it only raises the
	// question: systemd-run itself decides. If a bare probe scope starts,
	// scopes work and this was just the agent failing.
	if m.scopeProbe() {
		return false
	}
	m.mu.Lock()
	if s.run == r {
		s.run = nil
	}
	if m.haltedLocked(s) {
		m.mu.Unlock()
		return false
	}
	if m.noScope.CompareAndSwap(false, true) {
		m.log.Warn("systemd-run cannot reach the user bus; sessions run without a systemd scope (no memory cap) until restart",
			"output", firstLine(string(r.head)))
	}
	s.info.Attempt--
	s.agentAttempts--
	s.next, s.resume = r.task, r.resume
	m.mu.Unlock()
	fmt.Fprint(s.out, "\r\n\x1b[2m── cerberus: no systemd user bus; running without a scope ──\x1b[0m\r\n")
	if err := m.launch(s); err != nil {
		m.fail(s, err)
	}
	return true
}

// ScopesEnforced reports whether new runs get a systemd scope, and so
// their MemoryMax: Linux, a slice configured, systemd-run installed and the
// user bus not found unreachable since the daemon started.
func (m *Manager) ScopesEnforced() bool {
	if runtime.GOOS != "linux" || m.conf().Guard.SystemdSlice == "" || m.noScope.Load() {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// scopeProbe reports whether systemd-run can start a transient user scope
// now: it runs `true` in a throwaway scope, with no agent involved.
func (m *Manager) scopeProbe() bool {
	sr, err := exec.LookPath("systemd-run")
	if err != nil {
		return false
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	ctx, cancel := context.WithTimeout(m.ctx, scopeProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sr, "--user", "--scope", "--quiet",
		"--unit=cerberus-probe-"+hex.EncodeToString(b), "--slice="+m.conf().Guard.SystemdSlice, "--", "true")
	return cmd.Run() == nil
}

// applyLimitsLocked (re)arms r's time limit and sets its output limit from
// s.info.Limits. A time limit already passed stops the run now.
func (m *Manager) applyLimitsLocked(s *session, r *run) {
	l := s.info.Limits
	r.maxOut.Store(int64(l.MaxOutputMiB) << 20)
	if r.limit != nil {
		r.limit.Stop()
		r.limit = nil
	}
	if l.MaxMinutes <= 0 {
		return
	}
	left := time.Duration(l.MaxMinutes)*limitMinute - time.Since(r.started)
	if left <= 0 {
		m.limitStopLocked(s, r, "time limit", fmt.Sprintf("ran longer than %d min", l.MaxMinutes))
		return
	}
	r.limit = time.AfterFunc(left, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.limitStopLocked(s, r, "time limit", fmt.Sprintf("ran longer than %d min", l.MaxMinutes))
	})
}

// limitStopLocked stops r because it passed a limit. The session ends as
// stopped with reason as its WaitingReason.
func (m *Manager) limitStopLocked(s *session, r *run, reason, detail string) {
	if s.run != r || s.stopping {
		return
	}
	s.stopping = true
	s.stopReason = reason
	terminateRun(r)
	m.recordLocked(s, FeedLimit, reason+": "+detail)
}

// trackOutput is the pump's bookkeeping for chunk p (already redacted):
// the output limit, and Activity, LastOutput and OutputBytes, which reach
// the session at most once per activityEvery (a timer delivers the last
// change after output stops). Pump goroutine only.
func (m *Manager) trackOutput(s *session, r *run, p []byte) {
	r.outBytes += int64(len(p))
	if lim := r.maxOut.Load(); lim > 0 && r.outBytes > lim && !r.limitHit {
		r.limitHit = true
		m.mu.Lock()
		m.limitStopLocked(s, r, "output limit", fmt.Sprintf("more than %d MiB of output", lim>>20))
		m.mu.Unlock()
	}
	line := r.lastLine
	if lines := r.act.feed(p); len(lines) > 0 {
		if l := lastNonEmptyLine(lines); l != nil {
			line = activityText(l, &r.redact)
			r.lastLine = line
		}
	}
	if pend := bytes.TrimSpace(r.act.pending()); len(pend) > 0 {
		line = activityText(pend, &r.redact)
	}
	now := time.Now()
	r.actMu.Lock()
	r.actLine, r.actBytes, r.actTime, r.actDirty = line, r.actBytes+int64(len(p)), now, true
	due := now.Sub(r.actSent) >= activityEvery
	if !due && r.actTimer == nil {
		r.actTimer = time.AfterFunc(activityEvery-now.Sub(r.actSent), func() { m.flushActivity(s, r) })
	}
	r.actMu.Unlock()
	if due {
		m.flushActivity(s, r)
	}
}

// flushActivity hands the run's pending activity to the session and emits
// one state event (no meta.json write: the next state change persists it).
func (m *Manager) flushActivity(s *session, r *run) {
	r.actMu.Lock()
	if r.actTimer != nil {
		r.actTimer.Stop()
		r.actTimer = nil
	}
	if !r.actDirty {
		r.actMu.Unlock()
		return
	}
	line, n, at := r.actLine, r.actBytes, r.actTime
	r.actDirty, r.actBytes, r.actSent = false, 0, time.Now()
	r.actMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if line != "" {
		s.info.Activity = line
	}
	s.info.OutputBytes += n
	s.info.LastOutput = at
	m.emitLocked(EventState, m.snapshotLocked(s))
}

// lastNonEmptyLine is the last line of text (lines end in '\n') that holds
// more than white space, without its newline; nil when there is none.
func lastNonEmptyLine(text []byte) []byte {
	for len(text) > 0 {
		text = bytes.TrimRight(text, "\n")
		i := bytes.LastIndexByte(text, '\n')
		if l := bytes.TrimSpace(text[i+1:]); len(l) > 0 {
			return l
		}
		if i < 0 {
			break
		}
		text = text[:i]
	}
	return nil
}

// activityText masks secrets in the head of a plain-text line and clips it
// to activityRunes characters. The stripped text can join a value the raw
// stream kept apart, so it is masked again here.
func activityText(line []byte, r *redactor) string {
	if len(line) > activityScan {
		line = line[:activityScan]
	}
	if len(r.values) > 0 {
		line = r.mask(nil, line)
	}
	if !utf8.Valid(line) {
		line = bytes.ToValidUTF8(line, []byte("�"))
	}
	return clipRunes(strings.TrimSpace(string(line)), activityRunes)
}
