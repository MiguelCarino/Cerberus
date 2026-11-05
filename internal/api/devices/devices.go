// Package devices keeps the paired devices of a Cerberus daemon in
// devices.json (mode 0600, next to config.json). A device token is 32
// random bytes, handed out once at pairing ("cdv-" + hex) and stored only as
// its SHA-256, so the file never holds a usable credential. Each device has
// an id (for revocation), a name (the audit actor is "owner:<name>"), when
// it was created and when and from where it was last seen.
//
// The daemon and the CLI may both change the file (`cerberus pair` works
// with the daemon stopped). The store notices a file changed by another
// process (size or modification time) on its next lookup and reloads it,
// keeping its own unsaved last-seen times. Last-seen updates are written at
// most once a minute per device, so authenticating a request is a map
// lookup and a stat, never a write.
package devices

import (
	"crypto/rand"
	"crypto/sha256"
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
	"unicode"
)

// FileName is the store's name inside the config folder.
const FileName = "devices.json"

// TokenPrefix starts every device token.
const TokenPrefix = "cdv-"

// ClientPrefix starts the token of an MCP client (ScopeMCP). A prefix of its
// own means a daemon that predates scopes (and would ignore the field)
// never takes one for a full device token.
const ClientPrefix = "ccl-"

// ScopeMCP marks an MCP client: an outside agent (Claude Code, say) that may
// use only the agent scope (/api/agent/*): delegate to local models and
// start and steer the sessions it started. It never acts as the owner.
const ScopeMCP = "mcp"

// touchEvery bounds how often last-seen is written per device.
const touchEvery = time.Minute

// maxName bounds a device name.
const maxName = 40

// ErrNotFound is returned when no device has the id.
var ErrNotFound = errors.New("no such device")

// Device is one paired device. Hash is the hex SHA-256 of its token.
type Device struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	LastAddr string    `json:"last_addr,omitempty"`
	Scope    string    `json:"scope,omitempty"` // "" = a full device; ScopeMCP = an MCP client
	User     string    `json:"user,omitempty"`  // owner user id (Phase 4a); "" = the legacy owner
}

// Public is a device without its hash (what the API lists).
type Public struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Scope    string    `json:"scope,omitempty"`
	User     string    `json:"user,omitempty"`
	UserName string    `json:"user_name,omitempty"` // filled by the API
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	LastAddr string    `json:"last_addr,omitempty"`
}

// Store is the device list. It is safe for concurrent use.
type Store struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	devs    []Device
	byHash  map[string]int
	stamp   fileStamp // of the file as last read or written
	written map[string]time.Time
}

type fileStamp struct {
	size int64
	mod  time.Time
	ok   bool
}

// Open loads dir/devices.json (a missing file is an empty store).
func Open(dir string) (*Store, error) {
	return OpenWith(dir, time.Now)
}

// OpenWith is Open with a clock (tests).
func OpenWith(dir string, now func() time.Time) (*Store, error) {
	if dir == "" {
		return nil, errors.New("devices: no config folder")
	}
	s := &Store{path: filepath.Join(dir, FileName), now: now, written: map[string]time.Time{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path is the store's file.
func (s *Store) Path() string { return s.path }

// Hash is the stored form of a token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func stampOf(p string) fileStamp {
	st, err := os.Stat(p)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: st.Size(), mod: st.ModTime(), ok: true}
}

func (s *Store) loadLocked() error {
	st := stampOf(s.path)
	raw, err := os.ReadFile(s.path)
	var devs []Device
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		var f struct {
			Devices []Device `json:"devices"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("devices %s: %w", s.path, err)
		}
		devs = f.Devices
	}
	// Keep last-seen times this process knows and has not written yet.
	old := map[string]Device{}
	for _, d := range s.devs {
		old[d.ID] = d
	}
	for i := range devs {
		if o, ok := old[devs[i].ID]; ok && o.LastSeen.After(devs[i].LastSeen) {
			devs[i].LastSeen, devs[i].LastAddr = o.LastSeen, o.LastAddr
		}
	}
	s.devs = devs
	s.byHash = make(map[string]int, len(devs))
	for i, d := range devs {
		s.byHash[d.Hash] = i
	}
	s.stamp = st
	return nil
}

// refreshLocked reloads the file when another process changed it.
func (s *Store) refreshLocked() {
	if st := stampOf(s.path); st != s.stamp {
		_ = s.loadLocked() // a broken file keeps the last good list
	}
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(struct {
		Devices []Device `json:"devices"`
	}{s.devs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600) // WriteFile keeps the mode of an existing tmp
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.stamp = stampOf(s.path)
	return nil
}

// CleanName makes a device name safe for the audit actor and the UI. It
// keeps letters and digits of any script (accented and non-Latin names
// stay readable), combining marks, '-', '_', '.' and the apostrophe; a
// space becomes '-' and everything else is dropped. This matches the web
// form's DEVICE_NAME_RE (web/dist/assets/js/security.js).
func CleanName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.Is(unicode.M, r),
			r == '-', r == '_', r == '.', r == '\'':
			return r
		case r == ' ':
			return '-'
		}
		return -1
	}, strings.TrimSpace(name))
	if len([]rune(name)) > maxName {
		name = string([]rune(name)[:maxName])
	}
	return name
}

// Pair creates a device and returns its token (shown once).
// An empty or taken name gets a free one ("device", "phone-2" …).
func (s *Store) Pair(name string) (string, Public, error) { return s.PairScoped(name, "", "") }

// PairScoped is Pair for a scope: "" (a full device) or ScopeMCP (an MCP
// client, whose token starts with ClientPrefix), bound to a user id ("" =
// the legacy owner). A device acts with its user's role.
func (s *Store) PairScoped(name, scope, user string) (string, Public, error) {
	prefix, def := TokenPrefix, "device"
	switch scope {
	case "":
	case ScopeMCP:
		prefix, def = ClientPrefix, "mcp-client"
	default:
		return "", Public{}, fmt.Errorf("unknown device scope %q", scope)
	}
	name = CleanName(name)
	if name == "" {
		name = def
	}
	tok := make([]byte, 32)
	id := make([]byte, 6)
	if _, err := rand.Read(tok); err != nil {
		return "", Public{}, err
	}
	if _, err := rand.Read(id); err != nil {
		return "", Public{}, err
	}
	token := prefix + hex.EncodeToString(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	taken := map[string]bool{"admin": true} // "owner:admin" is the admin token
	for _, d := range s.devs {
		taken[d.Name] = true
	}
	base := name
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	d := Device{ID: hex.EncodeToString(id), Name: name, Hash: Hash(token), Created: s.now().UTC(), Scope: scope, User: user}
	s.devs = append(s.devs, d)
	s.byHash[d.Hash] = len(s.devs) - 1
	if err := s.saveLocked(); err != nil {
		s.devs = s.devs[:len(s.devs)-1]
		delete(s.byHash, d.Hash)
		return "", Public{}, err
	}
	return token, public(d), nil
}

// Verify returns the device whose token this is. The token's prefix must
// match the device's scope, so a stored scope can never be widened by a
// token of the other kind.
func (s *Store) Verify(token string) (Public, bool) {
	if s == nil || (!strings.HasPrefix(token, TokenPrefix) && !strings.HasPrefix(token, ClientPrefix)) {
		return Public{}, false
	}
	h := Hash(token) // a map lookup on a hash of a 256-bit secret leaks nothing usable
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	i, ok := s.byHash[h]
	if !ok {
		return Public{}, false
	}
	d := s.devs[i]
	client := strings.HasPrefix(token, ClientPrefix)
	if (client && d.Scope != ScopeMCP) || (!client && d.Scope != "") {
		return Public{}, false // a scope this build does not know opens nothing
	}
	return public(d), true
}

// Touch records that a device was just seen from addr. The file is written
// at most once a minute per device.
func (s *Store) Touch(id, addr string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	for i := range s.devs {
		if s.devs[i].ID != id {
			continue
		}
		s.devs[i].LastSeen, s.devs[i].LastAddr = now, addr
		if now.Sub(s.written[id]) >= touchEvery {
			s.written[id] = now
			_ = s.saveLocked()
		}
		return
	}
}

// List returns every device, oldest first, without hashes.
func (s *Store) List() []Public {
	if s == nil {
		return []Public{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := make([]Public, 0, len(s.devs))
	for _, d := range s.devs {
		out = append(out, public(d))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Revoke removes the device with id (or, failing that, the one named id)
// and returns it.
func (s *Store) Revoke(id string) (Public, error) {
	if s == nil {
		return Public{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	idx := -1
	for i, d := range s.devs {
		if d.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		for i, d := range s.devs {
			if d.Name == id {
				idx = i
			}
		}
	}
	if idx < 0 {
		return Public{}, ErrNotFound
	}
	d := s.devs[idx]
	// Build the new state aside and install it only once it is saved: a
	// failed save must leave the device listed and its token working, so
	// the owner sees the revocation did not happen.
	oldDevs, oldHash := s.devs, s.byHash
	oldWritten, hadWritten := s.written[d.ID]
	s.devs = append(s.devs[:idx:idx], s.devs[idx+1:]...)
	s.byHash = make(map[string]int, len(s.devs))
	for i, x := range s.devs {
		s.byHash[x.Hash] = i
	}
	delete(s.written, d.ID)
	if err := s.saveLocked(); err != nil {
		s.devs, s.byHash = oldDevs, oldHash
		if hadWritten {
			s.written[d.ID] = oldWritten
		}
		return Public{}, err
	}
	return public(d), nil
}

func public(d Device) Public {
	return Public{ID: d.ID, Name: d.Name, Scope: d.Scope, User: d.User, Created: d.Created, LastSeen: d.LastSeen, LastAddr: d.LastAddr}
}

// RevokeUser removes every device and MCP client whose user (resolved
// through owner, which maps a stored id to its current owner) is user,
// and returns them. A failed save keeps them all.
func (s *Store) RevokeUser(user string, owner func(string) string) ([]Public, error) {
	if s == nil {
		return nil, nil
	}
	if owner == nil {
		owner = func(id string) string { return id }
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	var gone []Public
	kept := make([]Device, 0, len(s.devs))
	for _, d := range s.devs {
		if owner(d.User) == user {
			gone = append(gone, public(d))
			continue
		}
		kept = append(kept, d)
	}
	if len(gone) == 0 {
		return nil, nil
	}
	oldDevs, oldHash := s.devs, s.byHash
	s.devs = kept
	s.byHash = make(map[string]int, len(kept))
	for i, x := range kept {
		s.byHash[x.Hash] = i
	}
	if err := s.saveLocked(); err != nil {
		s.devs, s.byHash = oldDevs, oldHash
		return nil, err
	}
	for _, d := range gone {
		delete(s.written, d.ID)
	}
	return gone, nil
}
