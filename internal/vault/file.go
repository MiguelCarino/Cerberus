package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	fileVersion = 1
	keySize     = 32
	// checkText is sealed with the key so that a replaced or wrong vault.key
	// is reported instead of silently producing garbage or overwriting entries.
	checkText = "cerberus-vault-check"
	checkAAD  = "cerberus-vault-check"
)

type fileDoc struct {
	Version int                  `json:"version"`
	Check   string               `json:"check"`
	Entries map[string]fileEntry `json:"entries"`
}

// fileEntry is one sealed value: base64(nonce || ciphertext), with the name as
// additional data so that entries cannot be swapped between names.
type fileEntry struct {
	Value   string    `json:"value"`
	Note    string    `json:"note,omitempty"`
	Updated time.Time `json:"updated"`
	Length  int       `json:"length"`
}

type fileStore struct {
	path string
	aead cipher.AEAD
	doc  fileDoc
}

func openFile(dir string) (*fileStore, error) {
	path := filepath.Join(dir, "vault.json")
	keyPath := filepath.Join(dir, "vault.key")
	var doc fileDoc
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("vault.json: %w", err)
		}
	}
	existing := doc.Check != "" || len(doc.Entries) > 0
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		if existing {
			return nil, fmt.Errorf("%w (vault.key is missing)", ErrWrongKey)
		}
		key, err = createKey(keyPath)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("vault.key: want %d bytes, have %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	f := &fileStore{path: path, aead: aead, doc: doc}
	if f.doc.Entries == nil {
		f.doc.Entries = map[string]fileEntry{}
	}
	if f.doc.Check != "" {
		if got, err := f.open(f.doc.Check, checkAAD); err != nil || got != checkText {
			return nil, ErrWrongKey
		}
	} else if len(f.doc.Entries) > 0 {
		// A document without a check value (hand-edited): verify one entry.
		for name, e := range f.doc.Entries {
			if _, err := f.open(e.Value, name); err != nil {
				return nil, ErrWrongKey
			}
			break
		}
	}
	return f, nil
}

// createKey writes 32 random bytes to a 0600 temp file and links it into
// place, so the key is never seen half-written and a concurrent creator's
// key is never replaced (the loser reads the winner's).
func createKey(path string) ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".vault.key.tmp*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, werr := f.Write(key)
	if werr == nil {
		werr = f.Chmod(0o600)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, werr
	}
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return os.ReadFile(path)
		}
		// No hard links here (some file systems): exclusive create instead.
		return createKeyExclusive(path, key)
	}
	return key, nil
}

func createKeyExclusive(path string, key []byte) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("vault.key: %w", err)
	}
	_, werr := f.Write(key)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return nil, fmt.Errorf("vault.key: %w", werr)
	}
	return key, nil
}

func (f *fileStore) name() string { return BackendFile }

func (f *fileStore) seal(plain, aad string) (string, error) {
	nonce := make([]byte, f.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := f.aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return base64.StdEncoding.EncodeToString(out), nil
}

func (f *fileStore) open(sealed, aad string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	n := f.aead.NonceSize()
	if len(raw) < n+f.aead.Overhead() {
		return "", errors.New("vault: sealed value too short")
	}
	plain, err := f.aead.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (f *fileStore) list() ([]Item, error) {
	out := make([]Item, 0, len(f.doc.Entries))
	for name, e := range f.doc.Entries {
		out = append(out, Item{Name: name, Note: e.Note, Updated: e.Updated, Length: e.Length})
	}
	return out, nil
}

func (f *fileStore) get(name string) (string, error) {
	e, ok := f.doc.Entries[name]
	if !ok {
		return "", ErrNotFound
	}
	s, err := f.open(e.Value, name)
	if err != nil {
		return "", fmt.Errorf("vault: %s: %w", name, ErrWrongKey)
	}
	return s, nil
}

func (f *fileStore) set(name, value, note string, updated time.Time) error {
	sealed, err := f.seal(value, name)
	if err != nil {
		return err
	}
	next := f.cloneDoc()
	next.Entries[name] = fileEntry{Value: sealed, Note: note, Updated: updated, Length: len(value)}
	return f.save(next)
}

func (f *fileStore) delete(name string) error {
	if _, ok := f.doc.Entries[name]; !ok {
		return ErrNotFound
	}
	next := f.cloneDoc()
	delete(next.Entries, name)
	return f.save(next)
}

func (f *fileStore) cloneDoc() fileDoc {
	next := fileDoc{Version: fileVersion, Check: f.doc.Check, Entries: make(map[string]fileEntry, len(f.doc.Entries)+1)}
	for k, v := range f.doc.Entries {
		next.Entries[k] = v
	}
	return next
}

// save writes next and adopts it only once it is on disk.
func (f *fileStore) save(next fileDoc) error {
	if next.Check == "" {
		c, err := f.seal(checkText, checkAAD)
		if err != nil {
			return err
		}
		next.Check = c
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(f.path, append(raw, '\n')); err != nil {
		return err
	}
	f.doc = next
	return nil
}
