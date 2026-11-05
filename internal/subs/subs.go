// Package subs observes consumer subscriptions (plans used through their
// official CLIs, never proxied) and estimates how much of each plan's usage
// windows has been consumed, for a gauge in the UI. Nothing is enforced.
//
// For kind "claude-code" it reads Claude Code's own transcripts: the *.jsonl
// files under DataDir (one folder per project, searched recursively so
// sub-agent transcripts count too). Each assistant line carries the API
// usage of one message; the CLI writes a message more than once, so lines
// are de-duplicated by message.id. Counted tokens are input + output +
// cache creation; cache reads are reported separately and not counted.
//
// Reading is incremental: per file the tracker remembers size, mtime and the
// offset of the last complete line, and on each scan only stats files and
// reads what was appended. It scans every minute while active (SetActive:
// the UI is watching or a Claude session is live) and every 15 minutes
// otherwise; between scans its goroutine sleeps on a timer.
package subs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

// KindClaudeCode is the only subscription kind implemented.
const KindClaudeCode = "claude-code"

const (
	activeInterval = time.Minute
	idleInterval   = 15 * time.Minute
	window         = 5 * time.Hour
	week           = 7 * 24 * time.Hour
	// keepFor bounds the message history: the weekly sum needs 7 days and
	// the 5 h block chain a little more to find where the current block began.
	keepFor = 8 * 24 * time.Hour
	maxLine = 16 << 20 // longer lines (huge tool results) are skipped
)

// PlanUsage is one subscription's estimated consumption. Limits are the
// user's estimates from config (0 = unknown). The 5 h figures cover the
// current block only and are zero when no block is open. Sessions counts
// the distinct CLI sessions active in the current block.
type PlanUsage struct {
	Name, Kind      string
	Window5hTokens  int
	WeeklyTokens    int
	Window5hLimit   int
	WeeklyLimit     int
	Window5hResetAt time.Time
	LastActivity    time.Time
	Sessions        int
	Err             string
	// Cache reads (cheap, and not counted toward the limits above), in the
	// current 5 h block and in the rolling week.
	CacheReadTokens       int
	WeeklyCacheReadTokens int
}

// Tracker scans the configured subscriptions' local records.
type Tracker struct {
	log  *slog.Logger
	now  func() time.Time
	wake chan struct{}

	// cerberusDirs are the folders whose sessions count when a plan has
	// AllSessions=false (worktrees and session dirs).
	cerberusDirs []string

	scanMu sync.Mutex // one scan at a time; guards each plan's scan state
	plans  []*plan

	mu     sync.Mutex // guards active and every plan's published fields
	active bool
}

// plan is one subscription. The scan state belongs to the scanner (under
// scanMu); the published fields are swapped under Tracker.mu and read by
// Snapshot.
type plan struct {
	cfg   config.SubscriptionConfig
	dir   string
	files map[string]*fileState
	seen  map[string]time.Time // de-duplication key → message time
	last  time.Time

	// published
	msgs    []msg // sorted by time, never mutated once published
	lastPub time.Time
	err     string
}

type fileState struct {
	info fs.FileInfo
	size int64
	mod  time.Time
	off  int64 // just past the last complete line read
}

type msg struct {
	at        time.Time
	tokens    int
	cacheRead int
	session   string
}

// New builds a tracker for cfg.Subscriptions. It reads nothing until Run.
func New(cfg *config.Config, log *slog.Logger) *Tracker {
	if log == nil {
		log = slog.Default()
	}
	t := &Tracker{log: log, now: time.Now, wake: make(chan struct{}, 1)}
	if d, err := platform.DataDir(); err == nil {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			d = r
		}
		t.cerberusDirs = []string{filepath.Join(d, "worktrees"), filepath.Join(d, "sessions")}
	}
	if cfg == nil {
		return t
	}
	home, _ := os.UserHomeDir()
	for _, sc := range cfg.Subscriptions {
		dir := sc.DataDir
		switch {
		case dir == "":
			dir = filepath.Join(home, ".claude", "projects")
		case dir == "~":
			dir = home
		case strings.HasPrefix(dir, "~/"):
			dir = filepath.Join(home, dir[2:])
		}
		t.plans = append(t.plans, &plan{cfg: sc, dir: dir, files: map[string]*fileState{}, seen: map[string]time.Time{}})
	}
	return t
}

// SetActive switches between the active (1 min) and idle (15 min) scan
// intervals. Turning active triggers a scan right away.
func (t *Tracker) SetActive(on bool) {
	t.mu.Lock()
	changed := t.active != on
	t.active = on
	t.mu.Unlock()
	if changed {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

func (t *Tracker) isActive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// Run scans once, then on the active or idle interval until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	if len(t.plans) == 0 {
		return
	}
	t.scan()
	timer := time.NewTimer(t.interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			t.scan()
		case <-t.wake:
			if t.isActive() {
				t.scan()
			}
		}
		timer.Reset(t.interval())
	}
}

func (t *Tracker) interval() time.Duration {
	if t.isActive() {
		return activeInterval
	}
	return idleInterval
}

// Snapshot returns every plan's usage as of now.
func (t *Tracker) Snapshot() []PlanUsage {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]PlanUsage, 0, len(t.plans))
	for _, p := range t.plans {
		u := PlanUsage{Name: p.cfg.Name, Kind: p.cfg.Kind, Window5hLimit: p.cfg.Window5hLimit,
			WeeklyLimit: p.cfg.WeeklyLimit, LastActivity: p.lastPub, Err: p.err}
		summarise(&u, p.msgs, now)
		out = append(out, u)
	}
	return out
}

// summarise fills the window and weekly figures from msgs (sorted by time).
// Blocks follow Claude's rule: a block opens at the first message once the
// previous block's 5 h have passed (so also after any gap of 5 h or more)
// and resets 5 h after it opened.
func summarise(u *PlanUsage, msgs []msg, now time.Time) {
	var start time.Time
	for _, m := range msgs {
		if start.IsZero() || !m.at.Before(start.Add(window)) {
			start = m.at
		}
	}
	open := !start.IsZero() && now.Before(start.Add(window))
	if open {
		u.Window5hResetAt = start.Add(window)
	}
	weekFrom := now.Add(-week)
	sessions := map[string]bool{}
	for _, m := range msgs {
		if m.at.After(weekFrom) {
			u.WeeklyTokens += m.tokens
			u.WeeklyCacheReadTokens += m.cacheRead
		}
		if open && !m.at.Before(start) {
			u.Window5hTokens += m.tokens
			u.CacheReadTokens += m.cacheRead
			sessions[m.session] = true
		}
	}
	u.Sessions = len(sessions)
}

// scan updates every plan from disk.
func (t *Tracker) scan() {
	t.scanMu.Lock()
	defer t.scanMu.Unlock()
	now := t.now()
	for _, p := range t.plans {
		var added []msg
		var errText string
		if p.cfg.Kind != KindClaudeCode {
			errText = "unsupported subscription kind " + strconv.Quote(p.cfg.Kind)
		} else {
			var err error
			added, err = t.scanClaude(p, now)
			if err != nil {
				errText = err.Error()
			}
		}
		t.publish(p, added, errText, now)
	}
}

// publish prunes old messages and, when anything changed, replaces the
// plan's published slice with a new sorted one.
func (t *Tracker) publish(p *plan, added []msg, errText string, now time.Time) {
	cut := now.Add(-keepFor)
	for k, at := range p.seen {
		if at.Before(cut) {
			delete(p.seen, k)
		}
	}
	t.mu.Lock()
	old := p.msgs
	t.mu.Unlock()
	i := 0
	for i < len(old) && old[i].at.Before(cut) {
		i++
	}
	msgs := old
	if len(added) > 0 || i > 0 {
		msgs = make([]msg, 0, len(old)-i+len(added))
		msgs = append(append(msgs, old[i:]...), added...)
		slices.SortStableFunc(msgs, func(a, b msg) int { return a.at.Compare(b.at) })
	}
	t.mu.Lock()
	p.msgs, p.err, p.lastPub = msgs, errText, p.last
	t.mu.Unlock()
}

// scanClaude walks the project folders and reads what each transcript
// gained since the last scan.
func (t *Tracker) scanClaude(p *plan, now time.Time) ([]msg, error) {
	projects, err := os.ReadDir(p.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("no Claude Code transcripts at " + p.dir)
		}
		return nil, err
	}
	var added []msg
	present := map[string]bool{}
	for _, d := range projects {
		if !d.IsDir() || (!p.cfg.AllSessions && !t.ownProject(d.Name())) {
			continue
		}
		_ = filepath.WalkDir(filepath.Join(p.dir, d.Name()), func(path string, e fs.DirEntry, err error) error {
			// Regular files only: a symlink may point anywhere, and opening a
			// FIFO would block the scan forever.
			if err != nil || !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return nil
			}
			present[path] = true
			added = p.readFile(path, info, now, added, t.log)
			return nil
		})
	}
	for path := range p.files {
		if !present[path] {
			delete(p.files, path)
		}
	}
	return added, nil
}

// encodeProject is how Claude Code names a project folder: the absolute
// path with every character other than ASCII letters and digits replaced
// by '-'. The mapping is lossy, so ownership is decided by comparing
// encodings rather than decoding folder names.
func encodeProject(path string) string {
	b := []byte(path)
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// ownProject reports whether a project folder's path is inside one of the
// Cerberus worktree or session directories.
func (t *Tracker) ownProject(name string) bool {
	for _, d := range t.cerberusDirs {
		enc := encodeProject(d)
		if name == enc || strings.HasPrefix(name, enc+"-") {
			return true
		}
	}
	return false
}

// readFile reads the new complete lines of one transcript and appends the
// messages it has not seen to added.
func (p *plan) readFile(path string, info fs.FileInfo, now time.Time, added []msg, log *slog.Logger) []msg {
	st := p.files[path]
	switch {
	case st == nil:
		st = &fileState{}
		p.files[path] = st
		if info.ModTime().Before(now.Add(-keepFor)) {
			// Untouched for longer than the history kept: nothing in it
			// can count, so start reading at its end.
			st.off = info.Size()
		}
	case !os.SameFile(st.info, info) || info.Size() < st.off:
		st.off = 0 // replaced or truncated: read again; de-duplication drops what was counted
	case info.Size() == st.size && info.ModTime().Equal(st.mod):
		return added
	}
	st.info, st.size, st.mod = info, info.Size(), info.ModTime()
	if st.off >= st.size {
		return added
	}
	f, err := openRegular(path)
	if err != nil {
		log.Debug("subs: open transcript", "err", err)
		return added
	}
	defer f.Close()
	if _, err := f.Seek(st.off, io.SeekStart); err != nil {
		return added
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	cut := now.Add(-keepFor)
	br := bufio.NewReaderSize(io.LimitReader(f, st.size-st.off), 64<<10)
	var line []byte
	var pending int64 // bytes of the current line consumed so far
	tooLong := false
	for {
		part, err := br.ReadSlice('\n')
		pending += int64(len(part))
		if !tooLong {
			if len(line)+len(part) > maxLine {
				tooLong, line = true, line[:0]
			} else {
				line = append(line, part...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return added // EOF in a partial line: it is read again once complete
		}
		st.off += pending
		if !tooLong {
			if m, key, ok := parseLine(line, session); ok && !m.at.Before(cut) {
				if key == "" {
					key = path + ":" + strconv.FormatInt(st.off, 10)
				}
				if _, dup := p.seen[key]; !dup {
					p.seen[key] = m.at
					added = append(added, m)
					if m.at.After(p.last) {
						p.last = m.at
					}
				}
			}
		}
		line, pending, tooLong = line[:0], 0, false
	}
}

// entry is the part of a transcript line the tracker reads.
type entry struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"sessionId"`
	UUID      string `json:"uuid"`
	Message   struct {
		ID    string `json:"id"`
		Usage *struct {
			Input         int `json:"input_tokens"`
			Output        int `json:"output_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var assistantMark = []byte(`"assistant"`)

// parseLine decodes an assistant line with usage. key de-duplicates it
// (message.id, else the line's uuid; "" when it has neither).
func parseLine(line []byte, fallbackSession string) (m msg, key string, ok bool) {
	if !bytes.Contains(line, assistantMark) {
		return msg{}, "", false // cheap skip for the many non-assistant lines
	}
	var e entry
	if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.Usage == nil {
		return msg{}, "", false
	}
	at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return msg{}, "", false
	}
	u := e.Message.Usage
	m = msg{at: at, tokens: u.Input + u.Output + u.CacheCreation, cacheRead: u.CacheRead, session: e.SessionID}
	if m.tokens == 0 && m.cacheRead == 0 {
		return msg{}, "", false
	}
	if m.session == "" {
		m.session = fallbackSession
	}
	key = e.Message.ID
	if key == "" {
		key = e.UUID
	}
	return m, key, true
}
