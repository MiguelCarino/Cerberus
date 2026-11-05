// Package audit is Cerberus' append-only audit log: DATA/audit.jsonl, one
// JSON record per line, mode 0600, rotated at 8 MiB to audit.jsonl.1 …
// audit.jsonl.5 (the oldest is dropped).
//
// It records every state-changing API call and CLI action, plus auth
// failures, token rotations, device pairing and revocation, sandbox
// fallbacks and kill/renice of host processes. A record holds who (Actor),
// from where (Remote), what (Method + Route pattern, never the raw URL),
// which ids (Targets), the result (Status) and a short Detail. Request
// bodies are never stored: Detail and every target are masked through the
// vault values and then clipped to 80 characters, so a typed input or a
// follow-up can be logged without leaking a secret.
//
// A nil *Logger is valid and records nothing, so packages can hold an
// optional logger without checking it. Logging never blocks on anything but
// the file write and never fails the action being recorded: errors are
// returned for the caller to log, not to act on.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// FileName is the log's name inside the data folder.
const FileName = "audit.jsonl"

const (
	defaultMaxBytes = 8 << 20 // rotate when the live file would pass this
	defaultKeep     = 5       // rotated files kept
	clipRunes       = 80      // Detail and each target are clipped to this
	minSecretLen    = 6       // shorter vault values are not masked (same rule as session output)
	mask            = "••••••"
	maxTargets      = 16
	maxLineBytes    = 64 << 10 // a longer line in the file is skipped by Query

	// DefaultLimit and MaxLimit bound Query.
	DefaultLimit = 200
	MaxLimit     = 5000
)

// Actor kinds. An actor is "owner:<device>", "agent:<session>", "cli" or
// "system".
const (
	ActorCLI    = "cli"
	ActorSystem = "system"
)

// Owner is the actor for a person using a paired device ("admin" for the
// admin token).
func Owner(device string) string { return "owner:" + orDefault(device, "admin") }

// Agent is the actor for a session's agent token.
func Agent(session string) string { return "agent:" + orDefault(session, "?") }

// User is the actor of a password sign-in (Phase 4a).
func User(name string) string { return "user:" + orDefault(name, "?") }

// Client is the actor of an MCP client token (an outside agent such as
// Claude Code, paired with scope "mcp"), by its device name.
func Client(name string) string { return "client:" + orDefault(name, "?") }

// Record is one audit entry.
type Record struct {
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor"`            // owner:<device> | user:<name> | agent:<session> | cli | system
	User    string    `json:"user,omitempty"`   // the acting user's name ("" for the admin token, system, CLI)
	Remote  string    `json:"remote,omitempty"` // remote address (API) or "" (CLI, system)
	Method  string    `json:"method,omitempty"` // HTTP method, or "" for non-HTTP events
	Route   string    `json:"route,omitempty"`  // route pattern ("POST /api/sessions/{id}/stop") or event name ("sandbox.fallback")
	Targets []string  `json:"targets,omitempty"`
	Status  int       `json:"status,omitempty"` // HTTP status, or 0
	Detail  string    `json:"detail,omitempty"` // short, masked and clipped
}

// Options tune a Logger; the zero value is the specification's defaults.
type Options struct {
	MaxBytes int64           // rotate at this size (default 8 MiB)
	Keep     int             // rotated files kept (default 5)
	Secrets  func() []string // vault values to mask (nil = none); may be slow, never called with the lock held
	Now      func() time.Time
}

// Logger appends records to DATA/audit.jsonl. It is safe for concurrent use.
type Logger struct {
	path    string
	max     int64
	keep    int
	secrets func() []string
	now     func() time.Time

	mu sync.Mutex
	f  *os.File
}

// Open opens (creating if needed) dir/audit.jsonl for appending. The folder
// is created 0700 and the file is forced to 0600.
func Open(dir string, o Options) (*Logger, error) {
	if dir == "" {
		return nil, errors.New("audit: no data folder")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Logger{path: filepath.Join(dir, FileName), max: o.MaxBytes, keep: o.Keep, secrets: o.Secrets, now: o.Now}
	if l.max <= 0 {
		l.max = defaultMaxBytes
	}
	if l.keep <= 0 {
		l.keep = defaultKeep
	}
	if l.now == nil {
		l.now = time.Now
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.openLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

// Path is the live log file.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *Logger) openLocked() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_ = f.Chmod(0o600) // an older file with a wider mode is narrowed
	l.f = f
	return nil
}

// Log appends r. Time defaults to now, Actor to "system"; Detail and the
// targets are masked through the vault values and clipped to 80 characters.
func (l *Logger) Log(r Record) error {
	if l == nil {
		return nil
	}
	var secrets []string
	if l.secrets != nil {
		secrets = l.secrets() // before the lock: the vault may run secret-tool
	}
	if r.Time.IsZero() {
		r.Time = l.now()
	}
	r.Time = r.Time.UTC()
	r.Actor = Clip(orDefault(r.Actor, ActorSystem))
	r.User = Clip(r.User)
	r.Remote = Clip(r.Remote)
	r.Method = Clip(r.Method)
	r.Route = Clip(stripQuery(r.Route))
	r.Detail = Clip(Redact(r.Detail, secrets))
	if len(r.Targets) > maxTargets {
		r.Targets = append(r.Targets[:maxTargets:maxTargets], fmt.Sprintf("… %d more", len(r.Targets)-maxTargets))
	}
	targets := make([]string, 0, len(r.Targets))
	for _, t := range r.Targets {
		targets = append(targets, Clip(Redact(t, secrets)))
	}
	r.Targets = targets
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		if err := l.openLocked(); err != nil {
			return err
		}
	}
	// Stat every time: the CLI may append to the same file from another process.
	if st, err := l.f.Stat(); err == nil && st.Size() > 0 && st.Size()+int64(len(line)) > l.max {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	_, err = l.f.Write(line)
	return err
}

// Event is a shorthand for a non-HTTP record (sandbox fallback, process
// kill, token rotation …).
func (l *Logger) Event(actor, event, detail string, targets ...string) error {
	return l.Log(Record{Actor: actor, Route: event, Targets: targets, Detail: detail})
}

// rotateLocked shifts audit.jsonl.N up by one (dropping the oldest) and
// starts a fresh live file.
func (l *Logger) rotateLocked() error {
	_ = l.f.Close()
	l.f = nil
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return l.openLocked()
}

// Close closes the file; later Logs reopen it.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Query returns up to limit records at or after since (zero = any time)
// whose actor matches (see MatchActor), newest first. limit <= 0 means
// DefaultLimit; it is capped at MaxLimit. Rotated files are read too.
func (l *Logger) Query(since time.Time, actor string, limit int) ([]Record, error) {
	return l.QueryFilter(Filter{Since: since, Actor: actor, Limit: limit})
}

// Filter selects records for QueryFilter. User "" matches every record,
// NoUser ("-") the records without a user (admin token, system), anything
// else the exact name.
type Filter struct {
	Since time.Time
	Actor string
	User  string
	Limit int
}

// NoUser is Filter.User for records with no user.
const NoUser = "-"

// MatchUser reports whether a record's user matches a Filter.User.
func MatchUser(user, filter string) bool {
	switch filter {
	case "":
		return true
	case NoUser:
		return user == ""
	}
	return user == filter
}

// QueryFilter is Query with a user filter.
func (l *Logger) QueryFilter(fl Filter) ([]Record, error) {
	if l == nil {
		return nil, nil
	}
	since, actor, limit := fl.Since, fl.Actor, fl.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	files := []string{l.path}
	for i := 1; i <= l.keep; i++ {
		files = append(files, fmt.Sprintf("%s.%d", l.path, i))
	}
	var out []Record // newest first
	for _, p := range files {
		recs, oldest, err := readFile(p, since, actor, fl.User)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		for i := len(recs) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, recs[i])
		}
		if len(out) >= limit || (!since.IsZero() && !oldest.IsZero() && oldest.Before(since)) {
			break // older files hold only older records
		}
	}
	return out, nil
}

// readFile returns p's matching records in file order and the time of its
// first record.
func readFile(p string, since time.Time, actor, user string) ([]Record, time.Time, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	var (
		out    []Record
		oldest time.Time
	)
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) { // an over-long line: skip it whole
				_, err = br.ReadSlice('\n')
			}
			continue
		}
		if len(bytes.TrimSpace(line)) > 0 && len(line) <= maxLineBytes {
			var r Record
			if json.Unmarshal(line, &r) == nil {
				if oldest.IsZero() {
					oldest = r.Time
				}
				if (since.IsZero() || !r.Time.Before(since)) && MatchActor(r.Actor, actor) && MatchUser(r.User, user) {
					out = append(out, r)
				}
			}
		}
		if err == io.EOF {
			return out, oldest, nil
		}
		if err != nil {
			return out, oldest, err
		}
	}
}

// MatchActor reports whether a record's actor matches a filter: "" matches
// all, a filter without ":" matches that kind ("agent" matches
// "agent:abc"), anything else must match exactly.
func MatchActor(actor, filter string) bool {
	switch {
	case filter == "":
		return true
	case actor == filter:
		return true
	case !strings.Contains(filter, ":"):
		return strings.HasPrefix(actor, filter+":")
	}
	return false
}

// Redact replaces every secret (values of at least 6 bytes, longest
// first; a multi-line value also by each of its lines) in s with a mask.
func Redact(s string, secrets []string) string {
	if s == "" || len(secrets) == 0 {
		return s
	}
	seen := map[string]bool{}
	var vals []string
	add := func(v string) {
		if len(v) >= minSecretLen && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	for _, v := range secrets {
		add(v)
		if strings.ContainsAny(v, "\r\n") {
			for _, line := range strings.FieldsFunc(v, func(c rune) bool { return c == '\n' || c == '\r' }) {
				add(strings.TrimSpace(line))
			}
		}
	}
	slices.SortStableFunc(vals, func(a, b string) int { return len(b) - len(a) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, mask)
	}
	return s
}

// Clip makes s one valid UTF-8 line of at most 80 characters (control
// characters become spaces; a cut ends in "…"). Mask before clipping, or a
// secret cut at the edge escapes the mask.
func Clip(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= clipRunes {
		return s
	}
	r := []rune(s)
	return string(r[:clipRunes-1]) + "…"
}

// stripQuery drops a query string from a route, in case a caller passed a
// raw URL.
func stripQuery(route string) string {
	if i := strings.IndexByte(route, '?'); i >= 0 {
		return route[:i]
	}
	return route
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
