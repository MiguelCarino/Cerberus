// Package vault keeps Cerberus' secrets: provider API keys and the tokens
// injected into agent sessions.
//
// Two backends exist. "secret-service" stores each value in the desktop
// keyring through the secret-tool CLI (values travel on stdin, never in argv)
// and keeps only metadata in vault-index.json. "file" encrypts each value with
// AES-256-GCM into vault.json, with the key in vault.key next to it. The file
// backend guards against accidental leaks (backups, grep, a screenshot of the
// config dir); it is not protection against a local attacker who can read
// vault.key.
//
// Only Get, Lookup and Values return values; nothing logs one.
package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
)

// Backend names.
const (
	BackendSecretService = "secret-service"
	BackendFile          = "file"
)

// MaxValueBytes is the largest value Set accepts.
const MaxValueBytes = 16 << 10

const maxNoteBytes = 1024

var (
	ErrNotFound     = errors.New("vault: secret not found")
	ErrInvalidName  = errors.New("vault: name must match ^[A-Za-z0-9_.-]{1,64}$")
	ErrValueSize    = errors.New("vault: value must be 1 to 16384 bytes")
	ErrNoteSize     = errors.New("vault: note must be at most 1024 bytes")
	ErrWrongKey     = errors.New("vault: vault.key does not decrypt vault.json")
	ErrUnknownStore = errors.New("vault: unknown backend")
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidName reports whether name is an acceptable secret name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Item describes one secret without its value.
type Item struct {
	Name    string
	Note    string
	Updated time.Time
	Length  int
}

// store is what a backend provides. Metadata is part of the store because
// the file backend keeps it beside the ciphertext.
type store interface {
	name() string
	list() ([]Item, error)
	get(name string) (string, error)
	set(name, value, note string, updated time.Time) error
	delete(name string) error
}

// Vault is safe for concurrent use. Backend operations are serialised by
// storeMu; mu guards only the in-memory cache, so a cached Get never waits
// for a slow secret-tool call. Lock order: storeMu, then mu.
type Vault struct {
	log   *slog.Logger
	store store

	storeMu sync.Mutex
	mu      sync.Mutex
	cache   map[string]string    // values seen so far
	all     bool                 // cache holds every value
	loaded  time.Time            // when a full load last ran (zero: never)
	retired map[string]time.Time // replaced or deleted values, still masked
}

// retryLoad is how long Values waits after an incomplete full load (keyring
// locked or slow) before trying again; meanwhile it uses what it has.
const retryLoad = time.Minute

// maxRetired bounds the replaced or deleted values kept for redaction.
const maxRetired = 256

// Option configures Open.
type Option func(*options)

type options struct {
	run Runner
}

// WithRunner replaces how secret-tool is executed (tests).
func WithRunner(r Runner) Option { return func(o *options) { o.run = r } }

// Open picks the backend from cfg.Vault.Backend, then imports a legacy
// secrets.json if one is present. "auto" probes the Secret Service once and
// falls back to the file backend when it does not answer.
func Open(cfg *config.Config, log *slog.Logger, opts ...Option) (*Vault, error) {
	if log == nil {
		log = slog.Default()
	}
	o := options{run: execRunner}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	dir := cfg.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var st store
	switch cfg.Vault.Backend {
	case "", "auto":
		// Auto sticks with the backend that already holds secrets, so a
		// start before the login keyring answers (a user unit at boot) does
		// not quietly switch to an empty file vault, or the other way round.
		hasFile := exists(filepath.Join(dir, "vault.json"))
		hasIndex := exists(filepath.Join(dir, "vault-index.json"))
		ss := newSecretService(dir, o.run)
		switch err := ss.probe(); {
		case hasFile && !hasIndex, err != nil && !hasIndex:
			if err != nil {
				log.Info("secret service unavailable; using the encrypted file vault", "reason", err.Error())
			}
			f, ferr := openFile(dir)
			if ferr != nil {
				return nil, ferr
			}
			st = f
		case err != nil:
			return nil, fmt.Errorf("vault: secrets are in the keyring, which does not answer now (locked or not started?); set vault.backend to pin one: %w", err)
		default:
			st = ss
		}
	case BackendSecretService:
		ss := newSecretService(dir, o.run)
		if err := ss.probe(); err != nil {
			return nil, fmt.Errorf("vault: secret service: %w", err)
		}
		st = ss
	case BackendFile:
		f, err := openFile(dir)
		if err != nil {
			return nil, err
		}
		st = f
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownStore, cfg.Vault.Backend)
	}
	v := &Vault{log: log, store: st, cache: map[string]string{}, retired: map[string]time.Time{}}
	log.Debug("vault open", "backend", st.name())
	if err := v.migrate(dir); err != nil {
		log.Warn("secrets.json not migrated; it stays in place", "err", err)
	}
	return v, nil
}

// Backend is "secret-service" or "file".
func (v *Vault) Backend() string { return v.store.name() }

// List returns every secret's name and metadata, sorted by name. Values are
// never read for it.
func (v *Vault) List() ([]Item, error) {
	v.storeMu.Lock()
	defer v.storeMu.Unlock()
	items, err := v.store.list()
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// Get returns one value, or ErrNotFound. Values are memoised so that hot
// paths (a provider key on every request) do not exec secret-tool each time.
func (v *Vault) Get(name string) (string, error) {
	if !ValidName(name) {
		return "", ErrInvalidName
	}
	if s, ok, done := v.cached(name); done {
		if !ok {
			return "", ErrNotFound
		}
		return s, nil
	}
	v.storeMu.Lock()
	defer v.storeMu.Unlock()
	if s, ok, done := v.cached(name); done { // filled while we waited
		if !ok {
			return "", ErrNotFound
		}
		return s, nil
	}
	s, err := v.store.get(name)
	if err != nil {
		return "", err
	}
	v.mu.Lock()
	v.cache[name] = s
	v.mu.Unlock()
	return s, nil
}

// cached answers name from the cache; done is false when the store must be
// asked.
func (v *Vault) cached(name string) (s string, ok, done bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.cache[name]; ok {
		return s, true, true
	}
	return "", false, v.all
}

// Lookup is Get without the error, for engine.WithSecrets.
func (v *Vault) Lookup(name string) string {
	s, _ := v.Get(name)
	return s
}

// Set stores or replaces a value with its note.
func (v *Vault) Set(name, value, note string) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	if len(value) == 0 || len(value) > MaxValueBytes {
		return ErrValueSize
	}
	if len(note) > maxNoteBytes {
		return ErrNoteSize
	}
	v.storeMu.Lock()
	defer v.storeMu.Unlock()
	if err := v.store.set(name, value, note, time.Now().UTC()); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if old, ok := v.cache[name]; ok && old != value {
		v.retireLocked(old)
	}
	v.cache[name] = value
	return nil
}

// Delete removes a secret. Deleting a missing one returns ErrNotFound.
func (v *Vault) Delete(name string) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	v.storeMu.Lock()
	defer v.storeMu.Unlock()
	if err := v.store.delete(name); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if old, ok := v.cache[name]; ok {
		v.retireLocked(old)
	}
	delete(v.cache, name)
	return nil
}

// retireLocked keeps a replaced or deleted value for redaction: sessions
// started before the change still hold it in their environment, and older
// conversations may print it again on Resume. Only Values returns it.
func (v *Vault) retireLocked(old string) {
	v.retired[old] = time.Now()
	if len(v.retired) <= maxRetired {
		return
	}
	oldest, at := "", time.Time{}
	for s, t := range v.retired {
		if oldest == "" || t.Before(at) {
			oldest, at = s, t
		}
	}
	delete(v.retired, oldest)
}

// Values returns every current value, plus values replaced or deleted
// since the vault opened, longest first (so redaction replaces the longest
// match), for output redaction. The first call reads them all; later calls
// use the cache that Set and Delete keep current. After an incomplete read
// (keyring locked or slow) it tries again at most once per retryLoad, and
// never waits behind another backend call to do so.
func (v *Vault) Values() []string {
	v.mu.Lock()
	first := v.loaded.IsZero()
	need := !v.all && (first || time.Since(v.loaded) >= retryLoad)
	v.mu.Unlock()
	if need {
		if first {
			v.storeMu.Lock()
			v.loadAll()
			v.storeMu.Unlock()
		} else if v.storeMu.TryLock() {
			v.loadAll()
			v.storeMu.Unlock()
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, 0, len(v.cache)+len(v.retired))
	seen := make(map[string]bool, cap(out))
	for _, s := range v.cache {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for s := range v.retired {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// loadAll reads every value into the cache. storeMu is held; mu is taken
// only to read and replace the cache, never across a backend call.
func (v *Vault) loadAll() {
	v.mu.Lock()
	if v.all || (!v.loaded.IsZero() && time.Since(v.loaded) < retryLoad) {
		v.mu.Unlock() // another load ran while we waited for storeMu
		return
	}
	v.loaded = time.Now()
	prev := maps.Clone(v.cache)
	v.mu.Unlock()
	items, err := v.store.list()
	if err != nil {
		v.log.Warn("vault list failed; redaction uses the values seen so far", "err", err)
		return
	}
	complete := true
	fresh := make(map[string]string, len(items))
	for _, it := range items {
		s, err := v.store.get(it.Name)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				complete = false
				v.log.Warn("vault read failed", "name", it.Name, "err", err)
				if old, ok := prev[it.Name]; ok {
					fresh[it.Name] = old
				}
			}
			continue
		}
		fresh[it.Name] = s
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for name, old := range v.cache {
		if s, ok := fresh[name]; !ok || s != old {
			v.retireLocked(old) // changed or removed outside Cerberus
		}
	}
	v.cache, v.all = fresh, complete
}

// migrate imports a legacy plaintext secrets.json and renames it to
// secrets.json.migrated (0600). Names already in the vault keep their vault
// value. Any failed write leaves the file in place for the next start.
func (v *Vault) migrate(dir string) error {
	path := filepath.Join(dir, "secrets.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	legacy := map[string]string{}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return fmt.Errorf("secrets.json: %w", err)
	}
	existing := map[string]bool{}
	items, err := v.store.list()
	if err != nil {
		return err
	}
	for _, it := range items {
		existing[it.Name] = true
	}
	imported, skipped := 0, 0
	for name, value := range legacy {
		if existing[name] {
			continue
		}
		if !ValidName(name) || len(value) == 0 || len(value) > MaxValueBytes {
			skipped++
			continue
		}
		if err := v.Set(name, value, "imported from secrets.json"); err != nil {
			return fmt.Errorf("import %s: %w", name, err)
		}
		imported++
	}
	dst := path + ".migrated"
	if err := os.Rename(path, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return err
	}
	v.log.Info("imported secrets.json into the vault", "imported", imported, "skipped_invalid", skipped,
		"backend", v.store.name(), "renamed_to", dst)
	return nil
}

// writeAtomic writes data to path through a 0600 temp file in the same
// folder, synced, then renamed over path.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
