package permits

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// FileName is the saved rules' file in the data folder.
const FileName = "permits.json"

// Sources of a saved rule.
const (
	SourceChat     = "chat"     // "Always allow" on a chat card
	SourceSession  = "session"  // "Every time" on an agent session's approval
	SourceSettings = "settings" // added by hand in Settings → Permissions
)

// maxPerOwner bounds one person's saved rules.
const maxPerOwner = 500

// bumpDelay is how long use counts wait before they are written.
const bumpDelay = 5 * time.Second

// Saved is one saved rule.
type Saved struct {
	ID       string    `json:"id"`
	Owner    string    `json:"owner"` // the user id ("" = the single owner before users)
	Tool     string    `json:"tool"`
	Content  string    `json:"content,omitempty"`
	Text     string    `json:"text"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
	Uses     int       `json:"uses"`
	Source   string    `json:"source"`
}

// Rule is the saved rule's rule.
func (s Saved) Rule() Rule { return Rule{Tool: s.Tool, Content: s.Content, Text: s.Text} }

// ErrNotFound is a rule id the owner does not have.
var ErrNotFound = errors.New("no such saved permission")

type file struct {
	Version int     `json:"version"`
	Rules   []Saved `json:"rules"`
}

// Store keeps the saved rules of every owner in DATA/permits.json (0600,
// written atomically). A nil *Store has no rules and saves none.
type Store struct {
	path string

	mu      sync.Mutex
	rules   []Saved
	resolve func(string) string
	subs    map[int]func()
	nextSub int
	dirty   bool // use counts not yet written
	timer   *time.Timer
	now     func() time.Time
}

// Open reads the saved rules in dir (none yet is fine). A file that cannot
// be read is an error: the caller runs without saved rules rather than
// overwrite it.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName), subs: map[int]func(){}, now: time.Now}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	s.rules = f.Rules
	return s, nil
}

// Path is the store's file.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// SetResolver sets how a stored owner id maps to the current owner (users
// moved or merged, the legacy "" owner): two ids name the same person when
// they resolve alike. Nil: ids compare as they are.
func (s *Store) SetResolver(f func(string) string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.resolve = f
	s.mu.Unlock()
}

func (s *Store) ownerLocked(id string) string {
	if s.resolve == nil {
		return id
	}
	return s.resolve(id)
}

// List is owner's saved rules, oldest first.
func (s *Store) List(owner string) []Saved {
	out := []Saved{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := s.ownerLocked(owner)
	for _, r := range s.rules {
		if s.ownerLocked(r.Owner) == want {
			out = append(out, r)
		}
	}
	return out
}

// Add saves r for owner (after Validate). A rule the owner already has is
// returned as it is (added false).
func (s *Store) Add(owner string, r Rule, source string) (Saved, bool, error) {
	if s == nil {
		return Saved{}, false, errors.New("saved permissions are unavailable on this server")
	}
	r.Content = strings.TrimSpace(r.Content)
	if err := Validate(r); err != nil {
		return Saved{}, false, err
	}
	r = NewRule(r.Tool, r.Content)
	s.mu.Lock()
	want := s.ownerLocked(owner)
	n := 0
	for _, x := range s.rules {
		if s.ownerLocked(x.Owner) != want {
			continue
		}
		n++
		if x.Tool == r.Tool && x.Content == r.Content {
			s.mu.Unlock()
			return x, false, nil
		}
	}
	if n >= maxPerOwner {
		s.mu.Unlock()
		return Saved{}, false, fmt.Errorf("at most %d saved permissions: remove some first", maxPerOwner)
	}
	sv := Saved{ID: newID(), Owner: want, Tool: r.Tool, Content: r.Content, Text: r.Text,
		Created: s.now().UTC().Truncate(time.Second), Source: source}
	s.rules = append(s.rules, sv)
	if err := s.writeLocked(); err != nil {
		s.rules = s.rules[:len(s.rules)-1]
		s.mu.Unlock()
		return Saved{}, false, err
	}
	subs := s.subsLocked()
	s.mu.Unlock()
	for _, f := range subs {
		f()
	}
	return sv, true, nil
}

// Remove deletes owner's rule id.
func (s *Store) Remove(owner, id string) (Saved, error) {
	if s == nil {
		return Saved{}, ErrNotFound
	}
	s.mu.Lock()
	want := s.ownerLocked(owner)
	i := slices.IndexFunc(s.rules, func(x Saved) bool { return x.ID == id && s.ownerLocked(x.Owner) == want })
	if i < 0 {
		s.mu.Unlock()
		return Saved{}, ErrNotFound
	}
	gone := s.rules[i]
	before := s.rules
	s.rules = slices.Delete(slices.Clone(s.rules), i, i+1)
	if err := s.writeLocked(); err != nil {
		s.rules = before
		s.mu.Unlock()
		return Saved{}, err
	}
	subs := s.subsLocked()
	s.mu.Unlock()
	for _, f := range subs {
		f()
	}
	return gone, nil
}

// Match finds owner's saved rule covering a call of tool with input
// (relative paths are cwd's) and counts the use.
func (s *Store) Match(owner, tool string, input map[string]any, cwd string) (Saved, bool) {
	if s == nil {
		return Saved{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := s.ownerLocked(owner)
	for i, r := range s.rules {
		if s.ownerLocked(r.Owner) == want && Matches(r.Rule(), tool, input, cwd) {
			s.bumpLocked(i)
			return s.rules[i], true
		}
	}
	return Saved{}, false
}

// Bump counts a use of owner's rule id (an agent session reports the saved
// rules that answered its calls).
func (s *Store) Bump(owner, id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := s.ownerLocked(owner)
	for i, r := range s.rules {
		if r.ID == id && s.ownerLocked(r.Owner) == want {
			s.bumpLocked(i)
			return
		}
	}
}

func (s *Store) bumpLocked(i int) {
	s.rules[i].Uses++
	s.rules[i].LastUsed = s.now().UTC().Truncate(time.Second)
	s.dirty = true
	if s.timer == nil {
		s.timer = time.AfterFunc(bumpDelay, func() { _ = s.Flush() })
	}
}

// Flush writes use counts that are still waiting.
func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if !s.dirty {
		return nil
	}
	return s.writeLocked()
}

// Subscribe calls f after every Add and Remove (not after uses). It
// returns the cancel.
func (s *Store) Subscribe(f func()) func() {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = f
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

func (s *Store) subsLocked() []func() {
	out := make([]func(), 0, len(s.subs))
	for _, f := range s.subs {
		out = append(out, f)
	}
	return out
}

func (s *Store) writeLocked() error {
	raw, err := json.MarshalIndent(file{Version: 1, Rules: s.rules}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := platform.WritePrivate(s.path, append(raw, '\n')); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- the agent's copy

// AgentFile is the copy of one owner's rules a built-in agent session
// reads ($CERBERUS_AGENT_PERMITS): ids, tools and contents only.
type AgentFile struct {
	Rules []AgentRule `json:"rules"`
}

// AgentRule is one rule in an AgentFile.
type AgentRule struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Content string `json:"content,omitempty"`
	Text    string `json:"text"`
}

// WriteAgentFile writes owner's rules to path (0600, atomically).
func (s *Store) WriteAgentFile(owner, path string) error {
	f := AgentFile{Rules: []AgentRule{}}
	for _, r := range s.List(owner) {
		f.Rules = append(f.Rules, AgentRule{ID: r.ID, Tool: r.Tool, Content: r.Content, Text: r.Text})
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return platform.WritePrivate(path, raw)
}

// AgentReader reads an AgentFile again whenever it changes (by modification
// time and size). A file that is missing or does not parse leaves the
// rules it last read.
type AgentReader struct {
	Path string

	mu    sync.Mutex
	mod   time.Time
	size  int64
	rules []AgentRule
}

// Match finds a rule covering a call of tool with input.
func (a *AgentReader) Match(tool string, input map[string]any, cwd string) (AgentRule, bool) {
	if a == nil || a.Path == "" {
		return AgentRule{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, err := os.Stat(a.Path); err == nil && (!st.ModTime().Equal(a.mod) || st.Size() != a.size) {
		if raw, err := os.ReadFile(a.Path); err == nil {
			var f AgentFile
			if json.Unmarshal(raw, &f) == nil {
				a.rules, a.mod, a.size = f.Rules, st.ModTime(), st.Size()
			}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		a.rules, a.mod, a.size = nil, time.Time{}, 0
	}
	for _, r := range a.rules {
		rule := Rule{Tool: r.Tool, Content: r.Content}
		if Validate(rule) == nil && Matches(rule, tool, input, cwd) {
			return r, true
		}
	}
	return AgentRule{}, false
}
