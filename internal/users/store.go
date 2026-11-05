package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// FileName is the accounts file inside the data folder.
const FileName = "users.json"

// MarkerName is written next to users.json once an account exists. With the
// marker present, a missing users.json is an error, never "no users": in
// single-user mode every paired device and agent acts as the admin, so
// losing (or moving away) the accounts file must fail closed.
const MarkerName = "users.enabled"

// ErrAccountsMissing: users.json is gone although accounts existed.
var ErrAccountsMissing = errors.New("users.json is missing but accounts existed here (" + MarkerName + "): restore users.json from a backup. " +
	"Only to go back to single-user mode on purpose (every paired device and agent then acts as the admin), delete " + MarkerName)

// Store is the account list. It is safe for concurrent use, and it re-reads
// users.json when another process (the CLI) changed it.
type Store struct {
	dir  string
	path string
	now  func() time.Time

	mu    sync.Mutex
	f     File
	stamp fileStamp

	// Logins are the browser sign-ins (logins.json).
	Logins *Logins
	// Lockout counts failed sign-ins per name (memory only).
	Lockout *Lockout
}

type fileStamp struct {
	size int64
	mod  time.Time
	ok   bool
}

func stampOf(p string) fileStamp {
	st, err := os.Stat(p)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: st.Size(), mod: st.ModTime(), ok: true}
}

// Open loads DATA/users.json (missing: no users) and DATA/logins.json.
func Open(dataDir string) (*Store, error) { return OpenWith(dataDir, time.Now) }

// OpenWith is Open with a clock (tests).
func OpenWith(dataDir string, now func() time.Time) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("users: no data folder")
	}
	if now == nil {
		now = time.Now
	}
	s := &Store{dir: dataDir, path: filepath.Join(dataDir, FileName), now: now}
	s.mu.Lock()
	err := s.loadLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	lg, err := openLogins(dataDir, now)
	if err != nil {
		return nil, err
	}
	s.Logins = lg
	s.Lockout = NewLockout(DefaultLockoutFailures, DefaultLockoutWindow, now)
	return s, nil
}

// Path is users.json.
func (s *Store) Path() string { return s.path }

// Dir is the data folder.
func (s *Store) Dir() string { return s.dir }

func (s *Store) loadLocked() error {
	st := stampOf(s.path)
	raw, err := os.ReadFile(s.path)
	var f File
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, merr := os.Stat(filepath.Join(s.dir, MarkerName)); merr == nil {
			return fmt.Errorf("users %s: %w", s.path, ErrAccountsMissing)
		}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("users %s: %w", s.path, err)
		}
	}
	if f.Version == 0 {
		f.Version = 1
	}
	s.f = f
	s.stamp = st
	return nil
}

// refreshLocked reloads the file when another process changed it. A broken
// or (with the marker) missing file keeps the last good list.
func (s *Store) refreshLocked() {
	if st := stampOf(s.path); st != s.stamp {
		_ = s.loadLocked()
	}
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	if err := platform.WritePrivate(s.path, append(raw, '\n')); err != nil {
		return err
	}
	s.stamp = stampOf(s.path)
	if len(s.f.Users) > 0 {
		marker := filepath.Join(s.dir, MarkerName)
		if _, err := os.Stat(marker); errors.Is(err, fs.ErrNotExist) {
			if err := platform.WritePrivate(marker, []byte("accounts exist: see "+FileName+"\n")); err != nil {
				return err
			}
		}
	}
	return nil
}

// mutate runs fn on a copy of the file and installs it only once saved.
func (s *Store) mutate(fn func(f *File) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	old := cloneFile(s.f)
	if err := fn(&s.f); err != nil {
		s.f = old
		return err
	}
	if err := s.saveLocked(); err != nil {
		s.f = old
		return err
	}
	return nil
}

func cloneFile(f File) File {
	out := File{Version: f.Version, Users: make([]User, len(f.Users))}
	for i, u := range f.Users {
		u.Repos = slices.Clone(u.Repos)
		u.Agents = slices.Clone(u.Agents)
		out.Users[i] = u
	}
	if f.Moved != nil {
		out.Moved = make(map[string]string, len(f.Moved))
		for k, v := range f.Moved {
			out.Moved[k] = v
		}
	}
	return out
}

func cloneUser(u User) User {
	u.Repos = slices.Clone(u.Repos)
	u.Agents = slices.Clone(u.Agents)
	return u
}

// HasUsers reports whether any account exists.
func (s *Store) HasUsers() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return len(s.f.Users) > 0
}

// List returns every account in creation order.
func (s *Store) List() []User {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := make([]User, 0, len(s.f.Users))
	for _, u := range s.f.Users {
		out = append(out, cloneUser(u))
	}
	return out
}

// Get returns the account with id.
func (s *Store) Get(id string) (User, bool) {
	if s == nil || id == "" {
		return User{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if i := s.indexLocked(id); i >= 0 {
		return cloneUser(s.f.Users[i]), true
	}
	return User{}, false
}

// ByName returns the account named name (case-insensitive).
func (s *Store) ByName(name string) (User, bool) {
	if s == nil {
		return User{}, false
	}
	name = NormName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for _, u := range s.f.Users {
		if u.Name == name {
			return cloneUser(u), true
		}
	}
	return User{}, false
}

func (s *Store) indexLocked(id string) int {
	for i, u := range s.f.Users {
		if u.ID == id {
			return i
		}
	}
	return -1
}

func newUserID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "u-" + hex.EncodeToString(b), nil
}

// Add creates an account. The first account must be an admin; it becomes
// the legacy owner (Moved[""]).
func (s *Store) Add(name, display, role, password string) (User, error) {
	return s.AddContext(context.Background(), name, display, role, password, false)
}

// AddContext is Add with a context for the hashing queue and the
// must-change flag (a generated password).
func (s *Store) AddContext(ctx context.Context, name, display, role, password string, mustChange bool) (User, error) {
	name = NormName(name)
	if !ValidName(name) {
		return User{}, ErrName
	}
	if !ValidRole(role) {
		return User{}, ErrRole
	}
	disp, err := cleanDisplay(display)
	if err != nil {
		return User{}, err
	}
	if err := CheckPassword(name, password); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(ctx, password)
	if err != nil {
		return User{}, err
	}
	id, err := newUserID()
	if err != nil {
		return User{}, err
	}
	now := s.now().UTC()
	u := User{ID: id, Name: name, Display: disp, Role: role, Password: hash, MustChange: mustChange,
		Created: now, Updated: now, PasswordSet: now}
	err = s.mutate(func(f *File) error {
		if len(f.Users) == 0 && role != RoleAdmin {
			return ErrFirstAdmin
		}
		for _, x := range f.Users {
			if x.Name == name {
				return ErrNameTaken
			}
			if x.ID == id {
				return errors.New("id collision, try again")
			}
		}
		if f.Moved == nil {
			f.Moved = map[string]string{}
		}
		if len(f.Users) == 0 {
			if _, ok := f.Moved[""]; !ok {
				f.Moved[""] = id
			}
		}
		f.Users = append(f.Users, u)
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return cloneUser(u), nil
}

// enabledAdminsLocked counts enabled admins, leaving out skip.
func enabledAdmins(f *File, skip string) int {
	n := 0
	for _, u := range f.Users {
		if u.ID != skip && u.Role == RoleAdmin && !u.Disabled {
			n++
		}
	}
	return n
}

// update changes one account under the lock.
func (s *Store) update(id string, fn func(f *File, u *User) error) (User, error) {
	var out User
	err := s.mutate(func(f *File) error {
		i := -1
		for j, u := range f.Users {
			if u.ID == id {
				i = j
			}
		}
		if i < 0 {
			return ErrNotFound
		}
		if err := fn(f, &f.Users[i]); err != nil {
			return err
		}
		f.Users[i].Updated = s.now().UTC()
		out = cloneUser(f.Users[i])
		return nil
	})
	return out, err
}

// SetRole changes a role. The last enabled admin cannot be demoted.
func (s *Store) SetRole(id, role string) (User, error) {
	if !ValidRole(role) {
		return User{}, ErrRole
	}
	return s.update(id, func(f *File, u *User) error {
		if u.Role == RoleAdmin && role != RoleAdmin && !u.Disabled && enabledAdmins(f, u.ID) == 0 {
			return ErrLastAdmin
		}
		u.Role = role
		return nil
	})
}

// SetDisabled disables or enables an account. The last enabled admin
// cannot be disabled.
func (s *Store) SetDisabled(id string, off bool) (User, error) {
	return s.update(id, func(f *File, u *User) error {
		if off && u.Role == RoleAdmin && !u.Disabled && enabledAdmins(f, u.ID) == 0 {
			return ErrLastAdmin
		}
		u.Disabled = off
		return nil
	})
}

// SetPassword sets a new password (checked against the rules).
func (s *Store) SetPassword(id, pw string, mustChange bool) (User, error) {
	return s.SetPasswordContext(context.Background(), id, pw, mustChange)
}

// SetPasswordContext is SetPassword with a context for the hashing queue.
func (s *Store) SetPasswordContext(ctx context.Context, id, pw string, mustChange bool) (User, error) {
	u, ok := s.Get(id)
	if !ok {
		return User{}, ErrNotFound
	}
	if err := CheckPassword(u.Name, pw); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(ctx, pw)
	if err != nil {
		return User{}, err
	}
	return s.update(id, func(_ *File, u *User) error {
		u.Password, u.MustChange, u.PasswordSet = hash, mustChange, s.now().UTC()
		return nil
	})
}

// SetBudget sets the monthly caps (negative values are refused).
func (s *Store) SetBudget(id string, b Budget) (User, error) {
	if b.MonthlyUSD < 0 || b.MonthlyTokens < 0 {
		return User{}, errors.New("a budget cannot be negative")
	}
	return s.update(id, func(_ *File, u *User) error { u.Budget = b; return nil })
}

// SetRepos sets the shared repos (absolute paths, cleaned, no duplicates).
func (s *Store) SetRepos(id string, repos []RepoShare) (User, error) {
	var clean []RepoShare
	seen := map[string]bool{}
	for _, r := range repos {
		p := filepath.Clean(strings.TrimSpace(r.Path))
		if !filepath.IsAbs(p) {
			return User{}, fmt.Errorf("repo %q: an absolute path is needed", r.Path)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		clean = append(clean, RepoShare{Path: p, Accept: r.Accept})
	}
	return s.update(id, func(_ *File, u *User) error { u.Repos = clean; return nil })
}

// SetAgents sets the allowed agent profiles.
func (s *Store) SetAgents(id string, agents []string) (User, error) {
	var clean []string
	for _, a := range agents {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(clean, a) {
			clean = append(clean, a)
		}
	}
	return s.update(id, func(_ *File, u *User) error { u.Agents = clean; return nil })
}

// SetDisplay sets the display name.
func (s *Store) SetDisplay(id, display string) (User, error) {
	d, err := cleanDisplay(display)
	if err != nil {
		return User{}, err
	}
	return s.update(id, func(_ *File, u *User) error { u.Display = d; return nil })
}

// NoteLogin records a successful sign-in.
func (s *Store) NoteLogin(id, addr string) {
	_, _ = s.update(id, func(_ *File, u *User) error {
		u.LastLogin, u.LastAddr = s.now().UTC(), addr
		return nil
	})
}

// Remove deletes an account. Its data moves to the enabled admin to (lazily,
// through Owner). The last enabled admin cannot be removed.
func (s *Store) Remove(id, to string) (User, error) {
	var gone User
	err := s.mutate(func(f *File) error {
		i, j := -1, -1
		for k, u := range f.Users {
			if u.ID == id {
				i = k
			}
			if u.ID == to {
				j = k
			}
		}
		if i < 0 {
			return ErrNotFound
		}
		u := f.Users[i]
		if u.Role == RoleAdmin && !u.Disabled && enabledAdmins(f, u.ID) == 0 {
			return ErrLastAdmin
		}
		if j < 0 || to == id || f.Users[j].Role != RoleAdmin || f.Users[j].Disabled {
			return errors.New("the data must go to another enabled admin")
		}
		if f.Moved == nil {
			f.Moved = map[string]string{}
		}
		f.Moved[id] = to
		gone = u
		f.Users = append(f.Users[:i], f.Users[i+1:]...)
		return nil
	})
	return gone, err
}

// maxChain bounds Owner's walk through Moved (a loop guard).
const maxChain = 32

// Owner resolves an owner id through Moved to the current owner. Owner("")
// is the legacy owner (the first admin), or "" when there are no users.
func (s *Store) Owner(id string) string {
	if s == nil {
		return id
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return ownerLocked(&s.f, id)
}

func ownerLocked(f *File, id string) string {
	if len(f.Users) == 0 && len(f.Moved) == 0 {
		return id
	}
	cur := id
	for range maxChain {
		next, ok := f.Moved[cur]
		if !ok {
			break
		}
		cur = next
	}
	if cur == "" {
		// Users exist but the file lost Moved[""]: the first admin.
		for _, u := range f.Users {
			if u.Role == RoleAdmin {
				return u.ID
			}
		}
	}
	return cur
}

// LegacyOwner is Owner(""): who owns the data from before users.
func (s *Store) LegacyOwner() string { return s.Owner("") }

// MovedFrom lists the ids whose data resolves to owner (owner itself
// included), for counting a removed user's reassigned objects.
func (s *Store) MovedFrom(owner string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := []string{owner}
	for k := range s.f.Moved {
		if k != owner && ownerLocked(&s.f, k) == owner {
			out = append(out, k)
		}
	}
	return out
}

// ---------- personal /v1 API keys ----------

// APIKeyPrefix starts a personal /v1 key.
const APIKeyPrefix = "cuk-"

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// NewAPIKey makes (or replaces) the user's personal /v1 key and returns it
// (shown once).
func (s *Store) NewAPIKey(id string) (string, error) {
	tok, err := newToken(APIKeyPrefix)
	if err != nil {
		return "", err
	}
	_, err = s.update(id, func(_ *File, u *User) error {
		u.APIKeyHash, u.APIKeyHint, u.APIKeyMade = tokenHash(tok), tok[len(tok)-4:], s.now().UTC()
		return nil
	})
	if err != nil {
		return "", err
	}
	return tok, nil
}

// RevokeAPIKey removes the user's personal key.
func (s *Store) RevokeAPIKey(id string) error {
	_, err := s.update(id, func(_ *File, u *User) error {
		u.APIKeyHash, u.APIKeyHint, u.APIKeyMade = "", "", time.Time{}
		return nil
	})
	return err
}

// VerifyAPIKey returns the enabled user whose personal key tok is.
func (s *Store) VerifyAPIKey(tok string) (User, bool) {
	if s == nil || !strings.HasPrefix(tok, APIKeyPrefix) {
		return User{}, false
	}
	h := tokenHash(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for _, u := range s.f.Users {
		if u.APIKeyHash != "" && u.APIKeyHash == h && !u.Disabled {
			return cloneUser(u), true
		}
	}
	return User{}, false
}

// ---------- sign-in ----------

// Verify checks a name and password. Unknown names, wrong passwords and
// disabled users all give ErrBadLogin; an unknown name still runs one
// PBKDF2 (against a dummy hash). A hash with an older iteration count is
// replaced.
func (s *Store) Verify(name, password string) (User, error) {
	return s.VerifyContext(context.Background(), name, password)
}

// VerifyContext is Verify; waiting for the hashing queue honours ctx.
func (s *Store) VerifyContext(ctx context.Context, name, password string) (User, error) {
	u, ok := s.ByName(name)
	if !ok || len(password) > MaxPassword {
		checkDummy(ctx, password)
		if err := ctx.Err(); err != nil {
			return User{}, err
		}
		return User{}, ErrBadLogin
	}
	good, rehash, err := CheckPasswordHash(ctx, u.Password, password)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return User{}, cerr
		}
		return User{}, ErrBadLogin
	}
	if !good || u.Disabled {
		return User{}, ErrBadLogin
	}
	if rehash {
		if h, err := HashPassword(ctx, password); err == nil {
			if nu, err := s.update(u.ID, func(_ *File, x *User) error {
				if x.Password == u.Password {
					x.Password = h
				}
				return nil
			}); err == nil {
				u = nu
			}
		}
	}
	return u, nil
}

// DummyChecks is how many unknown-name comparisons ran (tests).
func DummyChecks() int64 { return dummyChecks.Load() }

// EnsureFilesDir makes a user's own folder (0700) and returns it.
func (s *Store) EnsureFilesDir(id string) (string, error) {
	d := FilesDir(s.dir, id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}
