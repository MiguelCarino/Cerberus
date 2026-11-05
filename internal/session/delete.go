package session

import (
	"context"
	"errors"
	"fmt"
)

// UnacceptedError refuses to delete a repo session whose worktree holds
// work that was never accepted (uncommitted changes, or commits past its
// base). Delete with force removes it anyway.
type UnacceptedError struct{ Reason string }

func (e *UnacceptedError) Error() string {
	return "the worktree has work that was not accepted (" + e.Reason + "); accept it, or delete with force"
}

// IsUnaccepted reports whether err is an *UnacceptedError.
func IsUnaccepted(err error) bool {
	var ue *UnacceptedError
	return errors.As(err, &ue)
}

// Delete removes a session for good: its record, transcript, recordings and
// Cerberus' own workspace (a repo worktree or the scratch folder). A folder
// session's folder is never touched. A running session is stopped first.
// Without force a repo worktree with unaccepted work is kept and Delete
// returns an *UnacceptedError.
func (m *Manager) Delete(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err == nil && s.op {
		err = ErrBusy
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	s.op = true // keeps Resume, Accept, Discard and the pruner off it meanwhile
	live := !prunable(s.info.State)
	if live {
		m.stopLocked(s)
	}
	m.mu.Unlock()

	m.endPreview(s, "session deleted")
	if live && !waitTimeout(&s.work, discardTimeout) {
		m.releaseOps([]*session{s})
		return fmt.Errorf("the session did not stop within %s; try again", discardTimeout)
	}
	m.mu.Lock()
	checking := s.checkCancel != nil || s.run != nil
	info := m.snapshotLocked(s)
	m.mu.Unlock()
	if checking {
		// Checks (or a run that would not stop) still use the folder.
		m.releaseOps([]*session{s})
		return ErrBusy
	}

	if !force {
		if reason := m.unacceptedWork(ctx, info); reason != "" {
			m.releaseOps([]*session{s})
			return &UnacceptedError{Reason: reason}
		}
	}
	if err := m.deleteSession(s, info); err != nil {
		m.releaseOps([]*session{s})
		return err
	}
	m.log.Info("session deleted", "id", id, "forced", force)
	return nil
}
