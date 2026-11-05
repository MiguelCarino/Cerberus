package users

import (
	"sync"
	"time"
)

// Lockout defaults (config users.lockout_failures and users.lockout_minutes).
const (
	DefaultLockoutFailures = 5
	DefaultLockoutWindow   = 15 * time.Minute
	maxLockoutEntries      = 4096
)

// Lockout counts failed sign-ins per lowercased name, also for names that do
// not exist (so a lockout does not reveal which names are real). Failures
// failures within window lock the name for window. Memory only.
type Lockout struct {
	mu       sync.Mutex
	failures int
	window   time.Duration
	now      func() time.Time
	m        map[string]*lockEntry
}

type lockEntry struct {
	fails    []time.Time
	until    time.Time
	inflight int // attempts between Begin and End
}

// NewLockout makes a lockout counter.
func NewLockout(failures int, window time.Duration, now func() time.Time) *Lockout {
	if now == nil {
		now = time.Now
	}
	l := &Lockout{now: now, m: map[string]*lockEntry{}}
	l.Set(failures, window)
	return l
}

// Set changes the limits (zero keeps the defaults).
func (l *Lockout) Set(failures int, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if failures <= 0 {
		failures = DefaultLockoutFailures
	}
	if window <= 0 {
		window = DefaultLockoutWindow
	}
	l.failures, l.window = failures, window
}

// Locked reports whether name is locked, and for how long.
func (l *Lockout) Locked(name string) (bool, time.Duration) {
	name = NormName(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[name]
	if e == nil {
		return false, 0
	}
	if now := l.now(); now.Before(e.until) {
		return true, e.until.Sub(now)
	}
	return false, 0
}

// Begin reserves an attempt for name before its password is checked. It
// is false while the name is locked, or while the attempts already in
// flight could reach the limit together with the recent failures, so a
// burst of concurrent guesses checks no more passwords than the limit
// allows. Every true Begin must be followed by one End.
func (l *Lockout) Begin(name string) (ok bool, retry time.Duration) {
	name = NormName(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entryLocked(name, now)
	if now.Before(e.until) {
		return false, e.until.Sub(now)
	}
	cut := now.Add(-l.window)
	recent := 0
	for _, t := range e.fails {
		if t.After(cut) {
			recent++
		}
	}
	if recent+e.inflight >= l.failures {
		return false, time.Second
	}
	e.inflight++
	return true, 0
}

// End releases a Begin. failed records the attempt as a failure (as Fail);
// locked then says whether the name is now locked. After a success the
// caller checks Locked again before signing in: a concurrent failure may
// have locked the name meanwhile.
func (l *Lockout) End(name string, failed bool) (locked bool, retry time.Duration) {
	name = NormName(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[name]; e != nil && e.inflight > 0 {
		e.inflight--
	}
	if !failed {
		return false, 0
	}
	return l.failLocked(name)
}

func (l *Lockout) entryLocked(name string, now time.Time) *lockEntry {
	e := l.m[name]
	if e == nil {
		if len(l.m) >= maxLockoutEntries {
			l.pruneLocked(now)
		}
		e = &lockEntry{}
		l.m[name] = e
	}
	return e
}

// Fail records a failure for name. locked is true when the name is now
// locked (this failure locked it, or it already was).
func (l *Lockout) Fail(name string) (locked bool, retry time.Duration) {
	name = NormName(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failLocked(name)
}

func (l *Lockout) failLocked(name string) (locked bool, retry time.Duration) {
	now := l.now()
	e := l.entryLocked(name, now)
	if now.Before(e.until) {
		return true, e.until.Sub(now)
	}
	cut := now.Add(-l.window)
	kept := e.fails[:0]
	for _, t := range e.fails {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	e.fails = append(kept, now)
	if len(e.fails) >= l.failures {
		e.until = now.Add(l.window)
		e.fails = nil
		return true, l.window
	}
	return false, 0
}

// Clear forgets the failures and lock of name (a successful sign-in, an
// admin reset); attempts in flight stay counted.
func (l *Lockout) Clear(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name = NormName(name)
	if e := l.m[name]; e != nil && e.inflight > 0 {
		e.fails, e.until = nil, time.Time{}
		return
	}
	delete(l.m, name)
}

func (l *Lockout) pruneLocked(now time.Time) {
	cut := now.Add(-l.window)
	for k, e := range l.m {
		if now.Before(e.until) || e.inflight > 0 {
			continue
		}
		if len(e.fails) == 0 || !e.fails[len(e.fails)-1].After(cut) {
			delete(l.m, k)
		}
	}
	if len(l.m) >= maxLockoutEntries {
		for k, e := range l.m {
			if !now.Before(e.until) && e.inflight == 0 {
				delete(l.m, k)
			}
		}
	}
}
