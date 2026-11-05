package users

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// LoginsFile is the browser sign-ins file inside the data folder.
const LoginsFile = "logins.json"

// LoginPrefix starts a sign-in cookie token.
const LoginPrefix = "cls-"

// Defaults for sign-in expiry (config users.session_idle_hours and
// users.session_max_days).
const (
	DefaultIdle   = 336 * time.Hour
	DefaultMaxAge = 30 * 24 * time.Hour
	touchEvery    = time.Minute
	maxAgent      = 80
)

// Login is one browser sign-in. Only the token's SHA-256 is stored.
type Login struct {
	ID       string    `json:"id"` // "l-" + 12 hex
	Hash     string    `json:"hash"`
	User     string    `json:"user"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	Expires  time.Time `json:"expires"`
	Addr     string    `json:"addr,omitempty"`
	Agent    string    `json:"agent,omitempty"`
}

// Logins is the sign-in list. It is safe for concurrent use and re-reads
// the file when another process changed it.
type Logins struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	list    []Login
	stamp   fileStamp
	idle    time.Duration
	maxAge  time.Duration
	written map[string]time.Time
}

func openLogins(dir string, now func() time.Time) (*Logins, error) {
	l := &Logins{path: filepath.Join(dir, LoginsFile), now: now, idle: DefaultIdle, maxAge: DefaultMaxAge,
		written: map[string]time.Time{}}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

// Path is logins.json.
func (l *Logins) Path() string { return l.path }

// SetLimits sets the idle and absolute expiry (zero keeps the default).
func (l *Logins) SetLimits(idle, maxAge time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if idle > 0 {
		l.idle = idle
	} else {
		l.idle = DefaultIdle
	}
	if maxAge > 0 {
		l.maxAge = maxAge
	} else {
		l.maxAge = DefaultMaxAge
	}
}

func (l *Logins) loadLocked() error {
	st := stampOf(l.path)
	raw, err := os.ReadFile(l.path)
	var f struct {
		Logins []Login `json:"logins"`
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("logins %s: %w", l.path, err)
		}
	}
	// Keep last-seen times this process has not written yet.
	old := map[string]time.Time{}
	for _, x := range l.list {
		old[x.ID] = x.LastSeen
	}
	for i := range f.Logins {
		if t, ok := old[f.Logins[i].ID]; ok && t.After(f.Logins[i].LastSeen) {
			f.Logins[i].LastSeen = t
		}
	}
	l.list = f.Logins
	l.stamp = st
	return nil
}

func (l *Logins) refreshLocked() {
	if st := stampOf(l.path); st != l.stamp {
		_ = l.loadLocked()
	}
}

// liveLocked reports whether a sign-in is still valid at now.
func (l *Logins) liveLocked(x Login, now time.Time) bool {
	if !now.Before(x.Expires) {
		return false
	}
	last := x.LastSeen
	if last.IsZero() {
		last = x.Created
	}
	return now.Sub(last) < l.idle
}

// saveLocked prunes expired sign-ins and writes the file.
func (l *Logins) saveLocked() error {
	now := l.now()
	kept := l.list[:0:0]
	for _, x := range l.list {
		if l.liveLocked(x, now) {
			kept = append(kept, x)
		}
	}
	l.list = kept
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(struct {
		Logins []Login `json:"logins"`
	}{l.list}, "", "  ")
	if err != nil {
		return err
	}
	if err := platform.WritePrivate(l.path, append(raw, '\n')); err != nil {
		return err
	}
	l.stamp = stampOf(l.path)
	return nil
}

func clipAgent(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxAgent {
		s = string(r[:maxAgent])
	}
	return s
}

// Create starts a sign-in for user and returns its token (shown once, set
// as the cookie).
func (l *Logins) Create(userID, addr, agent string) (string, Login, error) {
	tok, err := newToken(LoginPrefix)
	if err != nil {
		return "", Login{}, err
	}
	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return "", Login{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
	now := l.now().UTC()
	x := Login{ID: "l-" + hex.EncodeToString(idb), Hash: tokenHash(tok), User: userID, Created: now,
		LastSeen: now, Expires: now.Add(l.maxAge), Addr: addr, Agent: clipAgent(agent)}
	l.list = append(l.list, x)
	if err := l.saveLocked(); err != nil {
		l.list = l.list[:len(l.list)-1]
		return "", Login{}, err
	}
	l.written[x.ID] = now
	return tok, x, nil
}

// Verify returns the live sign-in of tok. Expired and idle ones fail (and
// are dropped at the next write).
func (l *Logins) Verify(tok string) (Login, bool) {
	if l == nil || !strings.HasPrefix(tok, LoginPrefix) {
		return Login{}, false
	}
	h := tokenHash(tok)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
	now := l.now()
	for _, x := range l.list {
		if x.Hash == h {
			if !l.liveLocked(x, now) {
				return Login{}, false
			}
			return x, true
		}
	}
	return Login{}, false
}

// Touch records use of a sign-in; the file is written at most once a
// minute per sign-in.
func (l *Logins) Touch(id, addr string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().UTC()
	for i := range l.list {
		if l.list[i].ID != id {
			continue
		}
		l.list[i].LastSeen = now
		if addr != "" {
			l.list[i].Addr = addr
		}
		if now.Sub(l.written[id]) >= touchEvery {
			l.written[id] = now
			_ = l.saveLocked()
		}
		return
	}
}

// List returns the user's live sign-ins, newest first.
func (l *Logins) List(userID string) []Login {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
	now := l.now()
	out := []Login{}
	for _, x := range l.list {
		if x.User == userID && l.liveLocked(x, now) {
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Get returns one sign-in by id.
func (l *Logins) Get(id string) (Login, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
	for _, x := range l.list {
		if x.ID == id {
			return x, true
		}
	}
	return Login{}, false
}

// Revoke ends one sign-in; false when it does not exist.
func (l *Logins) Revoke(id string) bool {
	return l.revokeWhere(func(x Login) bool { return x.ID == id }) > 0
}

// RevokeUser ends every sign-in of the user except the one with id except,
// and returns how many ended.
func (l *Logins) RevokeUser(userID, except string) int {
	return l.revokeWhere(func(x Login) bool { return x.User == userID && x.ID != except })
}

// RevokeUserIDs is RevokeUser returning the ended sign-in ids.
func (l *Logins) RevokeUserIDs(userID, except string) []string {
	var ids []string
	l.revokeWhere(func(x Login) bool {
		if x.User == userID && x.ID != except {
			ids = append(ids, x.ID)
			return true
		}
		return false
	})
	return ids
}

func (l *Logins) revokeWhere(match func(Login) bool) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
	kept := l.list[:0:0]
	n := 0
	for _, x := range l.list {
		if match(x) {
			n++
			delete(l.written, x.ID)
			continue
		}
		kept = append(kept, x)
	}
	if n == 0 {
		return 0
	}
	old := l.list
	l.list = kept
	if err := l.saveLocked(); err != nil {
		l.list = old
		return 0
	}
	return n
}
