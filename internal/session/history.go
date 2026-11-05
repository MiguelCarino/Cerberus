package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	searchDefaultLimit = 50
	searchMaxLimit     = 500
	snippetRadius      = 80
	maxSnippets        = 3
	pruneEvery         = 24 * time.Hour
	conversationFile   = "conversation.json"
	resumeDefaultTask  = "Continue where you left off."
)

// SearchQuery selects sessions for Search. Empty fields match everything.
type SearchQuery struct {
	Text  string // words that must all appear in title, task or transcript (case-insensitive)
	Repo  string // the repo or folder path, or its base name
	Agent string
	State string
	Since time.Time // created at or after
	Until time.Time // created before
	Limit int       // default 50
	// Owners keeps only sessions whose stored Owner is one of these ids
	// (Phase 4a: a user's own id and the ids moved to them); nil = all.
	Owners []string
}

// SearchHit is one matching session.
type SearchHit struct {
	Session  Info
	Snippets []string // at most 3, about 80 characters either side of a match
	Score    int
}

// historyItem is what Search and Prune need of a session, copied under the lock.
type historyItem struct {
	info Info
	dir  string
	out  *output
}

func (m *Manager) history() []historyItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]historyItem, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- { // newest first
		s := m.sessions[m.order[i]]
		items = append(items, historyItem{info: m.snapshotLocked(s), dir: s.dir, out: s.out})
	}
	return items
}

// Search finds sessions whose title, task and transcript together contain
// every word of q.Text. It reads transcripts from disk on demand, newest
// session first, and stops after q.Limit hits; the hits are then ordered by
// score (title and task matches weigh more than transcript matches).
func (m *Manager) Search(q SearchQuery) []SearchHit {
	limit := q.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	limit = min(limit, searchMaxLimit)
	words := strings.Fields(strings.ToLower(q.Text))
	hits := []SearchHit{}
	for _, it := range m.history() {
		if len(hits) >= limit {
			break
		}
		if !q.matchesFilters(it.info) {
			continue
		}
		if len(words) == 0 {
			hits = append(hits, SearchHit{Session: it.info, Snippets: []string{}})
			continue
		}
		if h, ok := scoreSession(it, words); ok {
			hits = append(hits, h)
		}
	}
	slices.SortStableFunc(hits, func(a, b SearchHit) int { return b.Score - a.Score })
	return hits
}

func (q SearchQuery) matchesFilters(info Info) bool {
	if q.Owners != nil && !slices.Contains(q.Owners, info.Owner) {
		return false
	}
	if q.Agent != "" && info.Agent != q.Agent {
		return false
	}
	if q.State != "" && info.State != q.State {
		return false
	}
	if !q.Since.IsZero() && info.Created.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && !info.Created.Before(q.Until) {
		return false
	}
	if q.Repo != "" {
		r := filepath.Clean(q.Repo)
		match := false
		for _, p := range []string{info.Repo, info.Dir} {
			if p != "" && (p == r || filepath.Base(p) == q.Repo) {
				match = true
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// scoreSession checks that every word occurs somewhere and scores the hit:
// 10 per word in the title, 5 in the task, plus 1 per transcript occurrence
// (at most 10 per word).
func scoreSession(it historyItem, words []string) (SearchHit, bool) {
	title := strings.ToLower(it.info.Title)
	task := strings.ToLower(it.info.Task)
	text := it.out.transcript()
	lower := bytes.ToLower(text)
	if len(lower) != len(text) {
		text = lower // case folding changed byte lengths: show lower-cased snippets
	}
	score := 0
	var at []int // transcript match offsets, for snippets
	for _, w := range words {
		found := false
		if strings.Contains(title, w) {
			score += 10
			found = true
		}
		if strings.Contains(task, w) {
			score += 5
			found = true
		}
		wb := []byte(w)
		for off, n := 0, 0; n < 10; n++ {
			i := bytes.Index(lower[off:], wb)
			if i < 0 {
				break
			}
			at = append(at, off+i)
			off += i + len(wb)
			score++
			found = true
		}
		if !found {
			return SearchHit{}, false
		}
	}
	slices.Sort(at)
	return SearchHit{Session: it.info, Score: score, Snippets: snippets(text, at)}, true
}

// snippets cuts up to maxSnippets non-overlapping windows around offsets.
func snippets(text []byte, offsets []int) []string {
	out := []string{}
	end := -1
	for _, off := range offsets {
		if len(out) == maxSnippets {
			break
		}
		if off < end {
			continue // inside the previous snippet
		}
		lo, hi := max(off-snippetRadius, 0), min(off+snippetRadius, len(text))
		for lo > 0 && !utf8.RuneStart(text[lo]) {
			lo--
		}
		for hi < len(text) && !utf8.RuneStart(text[hi]) {
			hi++
		}
		s := strings.Join(strings.Fields(string(text[lo:hi])), " ")
		if lo > 0 {
			s = "…" + s
		}
		if hi < len(text) {
			s += "…"
		}
		out = append(out, s)
		end = hi
	}
	return out
}

// Transcript is the session's plain-text transcript (escape sequences
// removed, secrets masked).
func (m *Manager) Transcript(id string) ([]byte, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.out.transcript(), nil
}

// Resume continues a finished session (passed, failed, stopped or error) in
// its existing workspace: a new attempt with the profile's ResumeCommand when
// it has one, else its Command, with text as the task ("Continue where you
// left off." when empty). It queues like any start. Accepted and discarded
// sessions, and sessions whose workspace is gone, are refused.
func (m *Manager) Resume(ctx context.Context, id string, text string) error {
	text = strings.TrimSpace(text)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookup(id)
	if err != nil {
		return err
	}
	switch s.info.State {
	case StatePassed, StateFailed, StateStopped, StateError:
	case StateAccepted, StateDiscarded:
		return fmt.Errorf("session is %s and cannot be resumed", s.info.State)
	default:
		return fmt.Errorf("session is %s; only a finished session can be resumed", s.info.State)
	}
	if err := m.idleForWorkLocked(s); err != nil {
		if s.info.Worktree == "" {
			return errors.New("the session's workspace no longer exists")
		}
		return err
	}
	if st, err := os.Stat(s.info.Worktree); err != nil || !st.IsDir() {
		return errors.New("the session's workspace no longer exists")
	}
	if _, err := m.sessionProfile(s.info.Agent, s.dir); err != nil {
		return err
	}
	next := text
	if next == "" {
		next = resumeDefaultTask
	} else {
		s.task = text // retries after this run build on the new instruction
	}
	s.next = next
	s.resume = true
	s.agentAttempts = 0
	s.info.Error = ""
	m.setStateLocked(s, StateQueued)
	m.signal()
	return nil
}

// PruneReport is the outcome of a retention prune.
type PruneReport struct {
	Removed []string    // ids of deleted sessions
	Kept    []PruneKept // old sessions kept because their worktree holds work not on main
	Errors  []string
}

// PruneKept is an old session Prune left in place, and why.
type PruneKept struct {
	ID, Title, Worktree, Reason string
}

// prunable reports whether a session in state is finished for retention.
func prunable(state string) bool {
	switch state {
	case StateAccepted, StateDiscarded, StatePassed, StateFailed, StateStopped, StateError:
		return true
	}
	return false
}

// Prune deletes finished sessions last updated more than RetentionDays ago
// (0 keeps everything). A repo worktree with unaccepted work (uncommitted
// changes, or commits past BaseCommit) is kept with its session and listed.
// A folder session's folder is never touched: only Cerberus' own record of
// the session goes. Batches whose sessions are all gone are removed too.
func (m *Manager) Prune(ctx context.Context) (PruneReport, error) {
	m.pruneMu.Lock()
	defer m.pruneMu.Unlock()
	rep := PruneReport{Removed: []string{}, Kept: []PruneKept{}, Errors: []string{}}
	days := m.conf().Sessions.RetentionDays
	if days <= 0 {
		return rep, nil
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	m.mu.Lock()
	var old []*session
	for _, id := range m.order {
		s := m.sessions[id]
		if prunable(s.info.State) && s.info.Updated.Before(cutoff) &&
			!s.op && s.run == nil && s.checkCancel == nil {
			s.op = true // keeps Resume, Accept and Discard off it meanwhile
			old = append(old, s)
		}
	}
	m.mu.Unlock()

	for i, s := range old {
		if err := ctx.Err(); err != nil {
			// Only the sessions not reached yet are still ours: the others were
			// released (or deleted) already and may be under an Accept now.
			m.releaseOps(old[i:])
			return rep, err
		}
		m.mu.Lock()
		info := m.snapshotLocked(s)
		m.mu.Unlock()
		if reason := m.unacceptedWork(ctx, info); reason != "" {
			rep.Kept = append(rep.Kept, PruneKept{ID: info.ID, Title: info.Title, Worktree: info.Worktree, Reason: reason})
			m.releaseOps([]*session{s})
			continue
		}
		if err := m.deleteSession(s, info); err != nil {
			rep.Errors = append(rep.Errors, info.ID+": "+err.Error())
			m.releaseOps([]*session{s})
			continue
		}
		rep.Removed = append(rep.Removed, info.ID)
	}
	m.pruneBatches()
	if len(rep.Removed) > 0 || len(rep.Kept) > 0 {
		m.log.Info("pruned sessions", "removed", len(rep.Removed), "kept", len(rep.Kept), "errors", len(rep.Errors))
	}
	return rep, nil
}

func (m *Manager) releaseOps(ss []*session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range ss {
		if _, ok := m.sessions[s.info.ID]; ok {
			s.op = false
		}
	}
}

// unacceptedWork says why a repo session's worktree must be kept, or "".
// Errors keep it too: Prune only ever deletes what it has shown to be clean.
func (m *Manager) unacceptedWork(ctx context.Context, info Info) string {
	if info.Workspace != WorkspaceRepo || info.Worktree == "" {
		return ""
	}
	if _, err := os.Stat(info.Worktree); errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err := checkWorktree(ctx, info.Repo, info.Worktree); err != nil {
		return firstLine(err.Error())
	}
	st, err := runGit(ctx, info.Worktree, nil, "status", "--porcelain")
	if err != nil {
		return "cannot read the worktree's status: " + firstLine(err.Error())
	}
	if strings.TrimSpace(st) != "" {
		return "uncommitted changes in the worktree"
	}
	if info.BaseCommit != "" {
		n, err := gitLine(ctx, info.Worktree, "rev-list", "--count", info.BaseCommit+"..HEAD")
		if err != nil {
			return "cannot compare the worktree with its base: " + firstLine(err.Error())
		}
		if c, _ := strconv.Atoi(n); c > 0 {
			return fmt.Sprintf("%d commit(s) in the worktree not accepted", c)
		}
	}
	return ""
}

// deleteSession removes a session Prune has cleared: its clean worktree (repo
// sessions), then its folder under DATA/sessions (which holds a scratch
// session's workspace). A folder session's own folder is left alone.
func (m *Manager) deleteSession(s *session, info Info) error {
	m.endPreview(s, "session pruned")
	if info.Workspace == WorkspaceRepo && info.Worktree != "" && within(info.Worktree, m.wtDir) {
		if _, err := os.Stat(info.Worktree); err == nil {
			if err := removeWorktree(m.ctx, info.Repo, info.Worktree); err != nil {
				return err
			}
		}
	}
	s.out.closeSubs()
	s.out.release()
	if !within(s.dir, m.sessDir) {
		return fmt.Errorf("refusing to delete %s: not a session folder", s.dir)
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return err
	}
	_ = os.Remove(m.guardPath(info.ID))
	m.mu.Lock()
	delete(m.sessions, info.ID)
	m.order = slices.DeleteFunc(m.order, func(id string) bool { return id == info.ID })
	m.emitLocked(EventRemoved, info)
	m.mu.Unlock()
	if m.forget != nil {
		m.forget(info.ID)
	}
	return nil
}

// pruneLoop prunes once at start and then every 24 h on a single timer.
func (m *Manager) pruneLoop(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		if _, err := m.Prune(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("prune", "err", err)
		}
		t.Reset(pruneEvery)
	}
}
