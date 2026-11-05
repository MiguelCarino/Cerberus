package session

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"carino.systems/cerberus/internal/permits"
)

// Saved permissions in built-in agent sessions (docs/SCOPE.md "As built:
// saved permissions"). At every launch the owner's saved rules are copied
// to permitsDir/permitsFile in the session folder, a folder bound read-only
// under bwrap (a folder, not the file: a bind of the file would keep the
// old copy after the atomic rewrite). The agent reads it through
// $CERBERUS_AGENT_PERMITS and again whenever it changes; the copies of
// running sessions are rewritten whenever the store changes.
const (
	permitsDir  = "permits"
	permitsFile = "permits.json"
)

// AlwaysOption is one rule "Every time" may save for the approval a session
// waits at (the agent's "askrules" marker).
type AlwaysOption struct {
	Text    string
	Tool    string
	Content string
}

// ErrBadOption refuses a save of an option the approval does not offer, or
// one permits.Validate refuses.
var ErrBadOption = errors.New("that option is not offered for this approval")

// maxAlways bounds the options taken from one marker.
const maxAlways = 8

// WithPermits gives the manager the saved rules: built-in agents get their
// owner's copy, and answers can save new ones (AnswerSave).
func WithPermits(st *permits.Store) Option {
	return func(m *Manager) {
		m.permits = st
		if st != nil {
			st.Subscribe(m.rewritePermits)
		}
	}
}

// permitsPath is the agent's copy of the saved rules for session dir.
func permitsPath(sessDir string) string { return filepath.Join(sessDir, permitsDir, permitsFile) }

// writePermits writes owner's rules for a launch and returns the file.
func (m *Manager) writePermits(sessDir, owner string) (string, error) {
	p := permitsPath(sessDir)
	return p, m.permits.WriteAgentFile(owner, p)
}

// rewritePermits refreshes the copy of every running built-in session.
func (m *Manager) rewritePermits() {
	type job struct{ path, owner string }
	var jobs []job
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.run != nil && s.run.permits != "" {
			jobs = append(jobs, job{s.run.permits, s.info.Owner})
		}
	}
	m.mu.Unlock()
	for _, j := range jobs {
		if err := m.permits.WriteAgentFile(j.owner, j.path); err != nil {
			m.log.Warn("saved permissions: rewrite failed", "path", j.path, "err", err)
		}
	}
}

// parseAlways decodes an "askrules" marker: base64url JSON of
// [{text, tool, content}]. Options permits.Validate refuses are dropped;
// mask hides vault values in the texts.
func parseAlways(text string, mask func(string) string) []AlwaysOption {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) > 8000 {
		return nil
	}
	var in []permits.Rule
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	var out []AlwaysOption
	for _, r := range in {
		if len(out) == maxAlways {
			break
		}
		r = permits.NewRule(r.Tool, strings.TrimSpace(r.Content))
		if permits.Validate(r) != nil {
			continue
		}
		o := AlwaysOption{Text: mask(r.Text), Tool: r.Tool, Content: mask(r.Content)}
		if o.Content != r.Content || o.Text != r.Text || !utf8.ValidString(o.Content) {
			continue // it held a secret: never offer to save it
		}
		out = append(out, o)
	}
	return out
}

// AnswerSave answers approval number seq "Every time": it saves
// Info.WaitingAlways[option] for the session's owner (validated again),
// then types y, so this call runs and the rewritten copy covers the next
// ones. It returns the session before the answer and the saved rule.
func (m *Manager) AnswerSave(id string, seq, option int) (Info, permits.Saved, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return Info{}, permits.Saved{}, err
	}
	waiting := func() bool {
		return s.run != nil && s.info.State == StateWaiting && s.info.WaitingReason == "approval" && s.info.AskSeq == seq
	}
	if !waiting() {
		m.mu.Unlock()
		return Info{}, permits.Saved{}, ErrNoApproval
	}
	if option < 0 || option >= len(s.info.WaitingAlways) {
		m.mu.Unlock()
		return Info{}, permits.Saved{}, ErrBadOption
	}
	o := s.info.WaitingAlways[option]
	rule := permits.NewRule(o.Tool, o.Content)
	if err := permits.Validate(rule); err != nil {
		m.mu.Unlock()
		return Info{}, permits.Saved{}, fmt.Errorf("%w: %v", ErrBadOption, err)
	}
	if m.permits == nil {
		m.mu.Unlock()
		return Info{}, permits.Saved{}, errors.New("saved permissions are unavailable on this server")
	}
	owner := s.info.Owner
	m.mu.Unlock()
	// Saved outside the lock: the store's subscribers rewrite the running
	// sessions' copies (rewritePermits takes m.mu), before y goes in.
	sv, _, err := m.permits.Add(owner, rule, permits.SourceSession)
	if err != nil {
		return Info{}, permits.Saved{}, err
	}
	m.mu.Lock()
	if !waiting() { // answered meanwhile: the rule stays saved
		m.mu.Unlock()
		return Info{}, sv, ErrNoApproval
	}
	r := s.run
	before := m.snapshotLocked(s)
	r.idleWaiting.Store(false)
	m.setStateLocked(s, StateRunning)
	m.mu.Unlock()
	_ = r.pty.SetWriteDeadline(time.Now().Add(inputTimeout))
	_, err = r.pty.Write([]byte(ApproveOnce + "\r"))
	return before, sv, err
}
