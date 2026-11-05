package vault

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner runs one command. stdin nil means no input. It returns the
// command's stdout and stderr; err is non-nil when it could not start or
// exited non-zero.
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) (stdout, stderr []byte, err error)

func execRunner(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

const (
	secretTool   = "secret-tool"
	probeTimeout = 5 * time.Second
	readTimeout  = 15 * time.Second
	// Writes may need the user to unlock the keyring in a desktop prompt.
	writeTimeout = 60 * time.Second
)

// secretService stores values in the desktop keyring through secret-tool,
// with attributes app=cerberus name=NAME. Metadata lives in vault-index.json.
type secretService struct {
	run       Runner
	indexPath string
	index     map[string]indexEntry
}

type indexEntry struct {
	Note    string    `json:"note,omitempty"`
	Updated time.Time `json:"updated"`
	Length  int       `json:"length"`
}

func newSecretService(dir string, run Runner) *secretService {
	return &secretService{run: run, indexPath: filepath.Join(dir, "vault-index.json")}
}

func (s *secretService) name() string { return BackendSecretService }

// probe checks that a provider answers a harmless search, then loads the index.
func (s *secretService) probe() error {
	if _, err := s.search(probeTimeout); err != nil {
		return err
	}
	return s.loadIndex()
}

func (s *secretService) loadIndex() error {
	s.index = map[string]indexEntry{}
	raw, err := os.ReadFile(s.indexPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &s.index); err != nil {
		return fmt.Errorf("vault-index.json: %w", err)
	}
	return nil
}

func (s *secretService) saveIndex(next map[string]indexEntry) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.indexPath, append(raw, '\n')); err != nil {
		return err
	}
	s.index = next
	return nil
}

func (s *secretService) cloneIndex() map[string]indexEntry {
	next := make(map[string]indexEntry, len(s.index)+1)
	for k, v := range s.index {
		next[k] = v
	}
	return next
}

// found is one item seen in search output.
type found struct {
	name     string
	modified time.Time
}

// search lists the items with app=cerberus. secret-tool prints each item's
// secret too; those lines are dropped while parsing and never kept.
func (s *secretService) search(timeout time.Duration) ([]found, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, errb, err := s.run(ctx, nil, secretTool, "search", "--all", "app", "cerberus")
	items := parseSearch(out)
	// Some secret-tool versions print item details on stderr.
	items = append(items, parseSearch(errb)...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("secret-tool search: timed out (keyring locked or no provider)")
		}
		// No match exits 1 with nothing printed: that is an empty, working keyring.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(out)) == 0 && len(bytes.TrimSpace(errb)) == 0 {
			return nil, nil
		}
		return nil, toolErr("search", err, errb)
	}
	return dedupe(items), nil
}

// parseSearch reads secret-tool search output blocks:
//
//	[/org/freedesktop/secrets/collection/login/12]
//	label = Cerberus: NAME
//	secret = …            (dropped)
//	modified = 2026-10-31 12:00:00
//	attribute.name = NAME
func parseSearch(b []byte) []found {
	var out []found
	var cur found
	flush := func() {
		if ValidName(cur.name) {
			out = append(out, cur)
		}
		cur = found{}
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			continue
		}
		key, val, ok := strings.Cut(line, " = ")
		if !ok {
			continue // includes continuation lines of a multi-line secret
		}
		switch key {
		case "attribute.name":
			if cur.name != "" { // stream without [path] headers
				flush()
			}
			cur.name = val
		case "modified":
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", val, time.Local); err == nil {
				cur.modified = t.UTC()
			}
		}
	}
	flush()
	return out
}

func dedupe(in []found) []found {
	seen := map[string]int{}
	var out []found
	for _, f := range in {
		if i, ok := seen[f.name]; ok {
			if out[i].modified.IsZero() {
				out[i].modified = f.modified
			}
			continue
		}
		seen[f.name] = len(out)
		out = append(out, f)
	}
	return out
}

// toolErr describes a failed secret-tool run. stderr is included with any
// secret lines removed; stdout never is, since lookup prints the value there.
func toolErr(op string, err error, stderr []byte) error {
	var keep []string
	for _, l := range strings.Split(string(stderr), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "secret") || strings.HasPrefix(t, "attribute.") || strings.HasPrefix(t, "label") {
			continue
		}
		keep = append(keep, t)
	}
	msg := strings.Join(keep, "; ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if msg == "" {
		return fmt.Errorf("secret-tool %s: %w", op, err)
	}
	return fmt.Errorf("secret-tool %s: %w: %s", op, err, msg)
}

func (s *secretService) list() ([]Item, error) {
	items, err := s.search(readTimeout)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(items))
	for _, f := range items {
		it := Item{Name: f.name, Updated: f.modified}
		if e, ok := s.index[f.name]; ok {
			it.Note, it.Length = e.Note, e.Length
			if !e.Updated.IsZero() {
				it.Updated = e.Updated
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func (s *secretService) get(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	out, errb, err := s.run(ctx, nil, secretTool, "lookup", "app", "cerberus", "name", name)
	if err != nil {
		var ee *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &ee) && len(bytes.TrimSpace(errb)) == 0 {
			return "", ErrNotFound
		}
		return "", toolErr("lookup", err, errb)
	}
	if len(out) == 0 {
		return "", ErrNotFound
	}
	return string(out), nil
}

func (s *secretService) set(name, value, note string, updated time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	_, errb, err := s.run(ctx, []byte(value), secretTool, "store", "--label=Cerberus: "+name, "app", "cerberus", "name", name)
	if err != nil {
		return toolErr("store", err, errb)
	}
	next := s.cloneIndex()
	next[name] = indexEntry{Note: note, Updated: updated, Length: len(value)}
	return s.saveIndex(next)
}

func (s *secretService) delete(name string) error {
	_, indexed := s.index[name]
	if !indexed {
		items, err := s.search(readTimeout)
		if err != nil {
			return err
		}
		present := false
		for _, f := range items {
			present = present || f.name == name
		}
		if !present {
			return ErrNotFound
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if _, errb, err := s.run(ctx, nil, secretTool, "clear", "app", "cerberus", "name", name); err != nil {
		// Exit 1 with no message: nothing matched (already gone from the keyring).
		var ee *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &ee) || len(bytes.TrimSpace(errb)) != 0 {
			return toolErr("clear", err, errb)
		}
	}
	if !indexed {
		return nil
	}
	next := s.cloneIndex()
	delete(next, name)
	return s.saveIndex(next)
}
