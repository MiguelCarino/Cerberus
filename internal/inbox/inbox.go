// Package inbox is the in-app notification centre: every notification
// Cerberus sends (sessions, chats, budgets, alerts, doctor, downloads) is
// also kept here so the web UI can list it, badge the unread count and route
// a tap to the right view.
//
// Items live in memory as a ring of the newest Keep (default 1000) and on
// disk in DATA/inbox.jsonl (mode 0600). Add appends one line; read-state
// changes and deletions rewrite the file atomically (it is small), and the
// file is rotated (compacted to the ring) once appends have doubled it, so
// it never grows past about twice the ring. Subscribers get every change on
// a buffered channel; one that falls behind is dropped (its channel closed)
// so it refetches instead of silently missing an item.
package inbox

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// DefaultKeep is the ring size when Open is given keep <= 0.
const DefaultKeep = 1000

// Levels an item can have.
const (
	LevelInfo = "info"
	LevelWarn = "warn"
	LevelCrit = "crit"
)

const (
	subBuffer = 32
	maxLine   = 64 << 10
	maxTitle  = 300
	maxBody   = 2000
)

// Item is one notification. Link is an in-app route (e.g. "#/s/<id>").
type Item struct {
	ID    string
	Time  time.Time
	Kind  string
	Level string
	Title string
	Body  string
	Link  string
	Read  bool
	// Owner is the user the item is for (Phase 4a). "" is a system item
	// (downloads, alerts, serve), shown to admins.
	Owner string `json:",omitempty"`
}

// Event is one change, for live UIs: Type is "added" (Item set), "read"
// (IDs set, or All) or "deleted" (IDs set). Unread is the count after it.
type Event struct {
	Type   string
	Item   *Item    `json:",omitempty"`
	IDs    []string `json:",omitempty"`
	All    bool     `json:",omitempty"`
	Unread int
	// Owner is the owner of the items the event is about (Phase 4a); the
	// event stream sends it only to that user (or admins, for ""). Unread
	// is then recomputed per subscriber.
	Owner string `json:",omitempty"`
}

// Store holds the inbox.
type Store struct {
	path string
	keep int
	now  func() time.Time

	mu     sync.Mutex
	items  []Item // oldest first, at most keep
	f      *os.File
	lines  int // lines in the file
	subs   map[chan Event]struct{}
	closed bool // Close was called: memory only from here on
}

// Open loads path (missing is fine; malformed lines are skipped) and opens
// it for appending. keep <= 0 uses DefaultKeep.
func Open(path string, keep int) (*Store, error) {
	if keep <= 0 {
		keep = DefaultKeep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: path, keep: keep, now: time.Now, subs: map[chan Event]struct{}{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lines > len(s.items) {
		// Duplicates or trimmed lines: compact now so the file matches the ring.
		if err := s.rewriteLocked(); err != nil {
			return nil, err
		}
	}
	if s.f == nil {
		if err := s.openAppendLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	byID := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		s.lines++
		var it Item
		if json.Unmarshal(line, &it) != nil || it.ID == "" {
			continue
		}
		if i, ok := byID[it.ID]; ok { // a later line wins
			s.items[i] = it
			continue
		}
		byID[it.ID] = len(s.items)
		s.items = append(s.items, it)
	}
	if n := len(s.items) - s.keep; n > 0 {
		s.items = append([]Item(nil), s.items[n:]...)
	}
	return nil
}

func (s *Store) openAppendLocked() error {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_ = f.Chmod(0o600) // an older file may have looser bits
	s.f = f
	return nil
}

// rewriteLocked replaces the file with the ring (atomic rename), reopening
// it for appends.
func (s *Store) rewriteLocked() error {
	if s.closed {
		return nil
	}
	var buf bytes.Buffer
	for _, it := range s.items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	s.lines = len(s.items)
	return s.openAppendLocked()
}

// newID is sortable by time with a random suffix.
func newID(t time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strconv.FormatInt(t.UnixNano(), 36) + "-" + hex.EncodeToString(b)
}

// NormLevel maps anything but warn/crit (and a few synonyms) to info.
func NormLevel(l string) string {
	switch l {
	case LevelWarn, "warning":
		return LevelWarn
	case LevelCrit, "critical", "error":
		return LevelCrit
	}
	return LevelInfo
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Add stores it and returns it with ID, Time and Level filled. Write errors
// are not reported: the inbox must never fail a notification.
func (s *Store) Add(it Item) Item {
	if s == nil {
		return it
	}
	s.mu.Lock()
	if it.Time.IsZero() {
		it.Time = s.now()
	}
	if it.ID == "" {
		it.ID = newID(it.Time)
	}
	it.Level = NormLevel(it.Level)
	if it.Kind == "" {
		it.Kind = "system"
	}
	it.Title, it.Body = clip(it.Title, maxTitle), clip(it.Body, maxBody)
	it.Read = false

	s.items = append(s.items, it)
	if n := len(s.items) - s.keep; n > 0 {
		copy(s.items, s.items[n:])
		s.items = s.items[:s.keep]
	}
	if s.lines+1 >= 2*s.keep {
		_ = s.rewriteLocked() // rotation: compact to the ring
	} else if b, err := json.Marshal(it); err == nil && s.f != nil {
		if _, err := s.f.Write(append(b, '\n')); err == nil {
			s.lines++
		}
	}
	cp := it
	s.emitLocked(Event{Type: "added", Item: &cp, Unread: s.unreadLocked(), Owner: it.Owner})
	s.mu.Unlock()
	return it
}

// List returns items newest first; unreadOnly skips read ones and limit
// <= 0 returns all.
func (s *Store) List(unreadOnly bool, limit int) []Item { return s.ListWhere(nil, unreadOnly, limit) }

// ListWhere is List over the items keep accepts (nil: all).
func (s *Store) ListWhere(keep func(Item) bool, unreadOnly bool, limit int) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Item, 0, min(len(s.items), max(limit, 0)+1))
	for i := len(s.items) - 1; i >= 0; i-- {
		if unreadOnly && s.items[i].Read {
			continue
		}
		if keep != nil && !keep(s.items[i]) {
			continue
		}
		out = append(out, s.items[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Get returns one item.
func (s *Store) Get(id string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// Unread is the number of unread items.
func (s *Store) Unread() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unreadLocked()
}

func (s *Store) unreadLocked() int { return s.unreadWhereLocked(nil) }

// UnreadWhere counts the unread items keep accepts (nil: all).
func (s *Store) UnreadWhere(keep func(Item) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unreadWhereLocked(keep)
}

func (s *Store) unreadWhereLocked(keep func(Item) bool) int {
	n := 0
	for _, it := range s.items {
		if !it.Read && (keep == nil || keep(it)) {
			n++
		}
	}
	return n
}

// MarkRead marks the given items (or every item when all) read and returns
// how many changed. Unknown ids are ignored.
func (s *Store) MarkRead(ids []string, all bool) (int, error) { return s.MarkReadWhere(ids, all, nil) }

// MarkReadWhere is MarkRead over the items keep accepts (nil: all). It
// sends one "read" event per owner of the changed items.
func (s *Store) MarkReadWhere(ids []string, all bool, keep func(Item) bool) (int, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var changed []string
	byOwner := map[string][]string{}
	var owners []string
	for i := range s.items {
		if !s.items[i].Read && (all || want[s.items[i].ID]) && (keep == nil || keep(s.items[i])) {
			s.items[i].Read = true
			changed = append(changed, s.items[i].ID)
			o := s.items[i].Owner
			if _, ok := byOwner[o]; !ok {
				owners = append(owners, o)
			}
			byOwner[o] = append(byOwner[o], s.items[i].ID)
		}
	}
	if len(changed) == 0 {
		return 0, nil
	}
	err := s.rewriteLocked()
	for _, o := range owners {
		ev := Event{Type: "read", All: all, Unread: s.unreadLocked(), Owner: o}
		if !all {
			ev.IDs = byOwner[o]
		}
		s.emitLocked(ev)
	}
	return len(changed), err
}

// Delete removes one item; false when it does not exist.
func (s *Store) Delete(id string) (bool, error) { return s.DeleteWhere(id, nil) }

// DeleteWhere is Delete of an item keep accepts (nil: any).
func (s *Store) DeleteWhere(id string, keep func(Item) bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.items {
		if it.ID != id || (keep != nil && !keep(it)) {
			continue
		}
		s.items = append(s.items[:i], s.items[i+1:]...)
		err := s.rewriteLocked()
		s.emitLocked(Event{Type: "deleted", IDs: []string{id}, Unread: s.unreadLocked(), Owner: it.Owner})
		return true, err
	}
	return false, nil
}

// Subscribe streams changes until cancel is called. A subscriber more than
// its buffer behind is dropped: its channel is closed.
func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subBuffer)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
}

func (s *Store) emitLocked(ev Event) {
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// Close closes the file and every subscriber. Later Adds still update memory.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
