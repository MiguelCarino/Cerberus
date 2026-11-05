package flows

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// Errors of the store and the engine.
var (
	ErrNotFound = errors.New("not found")
	ErrStale    = errors.New("the flow changed elsewhere (stale version)")
	ErrState    = errors.New("the run is not in a state that allows this")
	ErrDisabled = errors.New("flows are turned off")
	ErrClosed   = errors.New("the flow engine is closed")
)

// KeepVersions is how many previous versions of a flow are kept.
const KeepVersions = 10

const maxFlowFile = 4 << 20

// Store keeps flows as DATA/flows/<id>.json (0600, written atomically).
// Every save bumps the version and keeps the previous file as
// <id>.<version>.json (the last KeepVersions). It is safe for concurrent
// use. The store checks structure (Validate without a catalog) and
// refuses flows with errors.
type Store struct {
	dir   string
	now   func() time.Time
	mu    sync.Mutex
	cache map[string]Flow // id → flow; nil until loaded
}

// OpenStore opens (and creates, 0700) the flows folder.
func OpenStore(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, now: now}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the flows folder.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *Store) load() error {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	s.cache = map[string]Flow{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !ValidID(id) {
			continue // versions (<id>.<n>.json) fail ValidID on the dot
		}
		f, err := readFlow(s.path(id))
		if err != nil || f.ID != id {
			continue // a damaged file is left for the owner, not listed
		}
		s.cache[id] = f
	}
	return nil
}

func readFlow(p string) (Flow, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Flow{}, err
	}
	if len(b) > maxFlowFile {
		return Flow{}, fmt.Errorf("%s: too big", p)
	}
	var f Flow
	if err := json.Unmarshal(b, &f); err != nil {
		return Flow{}, fmt.Errorf("%s: %w", p, err)
	}
	if f.Steps == nil {
		f.Steps = []Step{}
	}
	return f, nil
}

// List is every flow, by name then id.
func (s *Store) List() []Flow {
	s.mu.Lock()
	out := make([]Flow, 0, len(s.cache))
	for _, f := range s.cache {
		out = append(out, f.clone())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get is one flow, or ErrNotFound.
func (s *Store) Get(id string) (Flow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.cache[id]
	if !ok {
		return Flow{}, ErrNotFound
	}
	return f.clone(), nil
}

// normalize fills defaults that are part of the stored form.
func normalize(f *Flow) {
	f.Name = strings.TrimSpace(f.Name)
	if f.Trigger.Kind == "" {
		f.Trigger.Kind = TriggerManual
	}
	if f.Trigger.Kind == TriggerManual {
		f.Trigger.Schedule = ""
	} else {
		f.Trigger.Schedule = strings.Join(strings.Fields(f.Trigger.Schedule), " ")
	}
	if f.Steps == nil {
		f.Steps = []Step{}
	}
}

// Create saves a new flow with a fresh id from its name (fl-<slug>, then
// -2, -3, ... when taken) and version 1. An id already set is used when
// it is valid and free.
func (s *Store) Create(f Flow) (Flow, error) {
	f = f.clone()
	normalize(&f)
	if ps := Validate(f, nil); HasErrors(ps) {
		return Flow{}, &ValidationError{Problems: ps}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidID(f.ID) || s.taken(f.ID) {
		base := Slug(f.Name)
		f.ID = base
		for n := 2; s.taken(f.ID); n++ {
			f.ID = base + "-" + strconv.Itoa(n)
		}
	}
	f.Version = 1
	f.Updated = s.now().UTC()
	f.OwnerName = ""
	if err := s.writeLocked(f); err != nil {
		return Flow{}, err
	}
	s.cache[f.ID] = f
	return f.clone(), nil
}

// taken reports whether id is in use, or reserved: DraftID is the id of
// test runs of unsaved drafts, so no saved flow may get it.
func (s *Store) taken(id string) bool {
	if id == DraftID {
		return true
	}
	if _, ok := s.cache[id]; ok {
		return true
	}
	_, err := os.Stat(s.path(id))
	return err == nil
}

// Update replaces flow f.ID. f.Version must be the stored version
// (ErrStale otherwise); the saved flow has the next version and the old
// file is kept as <id>.<old version>.json.
func (s *Store) Update(f Flow) (Flow, error) {
	f = f.clone()
	normalize(&f)
	if ps := Validate(f, nil); HasErrors(ps) {
		return Flow{}, &ValidationError{Problems: ps}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.cache[f.ID]
	if !ok {
		return Flow{}, ErrNotFound
	}
	f.Owner, f.OwnerName = old.Owner, "" // an update never changes the owner (Phase 4a)
	if f.Version != old.Version {
		return Flow{}, ErrStale
	}
	if b, err := json.MarshalIndent(old, "", "  "); err == nil {
		if err := platform.WritePrivate(filepath.Join(s.dir, fmt.Sprintf("%s.%d.json", old.ID, old.Version)), b); err != nil {
			return Flow{}, err
		}
	}
	f.Version = old.Version + 1
	f.Updated = s.now().UTC()
	if err := s.writeLocked(f); err != nil {
		return Flow{}, err
	}
	s.cache[f.ID] = f
	s.pruneLocked(f.ID)
	return f.clone(), nil
}

// SetEnabled turns a flow's schedule on or off (a save: the version is
// bumped and the old one kept).
func (s *Store) SetEnabled(id string, on bool) (Flow, error) {
	f, err := s.Get(id)
	if err != nil {
		return Flow{}, err
	}
	f.Enabled = on
	return s.Update(f)
}

// SetOwner gives a flow a new stored owner (an admin adopting the flow of
// a removed user); the version is kept.
func (s *Store) SetOwner(id, owner string) (Flow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.cache[id]
	if !ok {
		return Flow{}, ErrNotFound
	}
	f = f.clone()
	f.Owner, f.OwnerName = owner, ""
	if err := s.writeLocked(f); err != nil {
		return Flow{}, err
	}
	return f.clone(), nil
}

// Delete removes a flow and its kept versions (runs stay).
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cache[id]; !ok {
		return ErrNotFound
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(s.cache, id)
	for _, v := range s.versionsLocked(id) {
		_ = os.Remove(filepath.Join(s.dir, fmt.Sprintf("%s.%d.json", id, v)))
	}
	return nil
}

// Versions lists the kept previous versions of a flow, newest first.
func (s *Store) Versions(id string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.versionsLocked(id)
}

// Version reads one kept previous version.
func (s *Store) Version(id string, v int) (Flow, error) {
	if !ValidID(id) || v <= 0 {
		return Flow{}, ErrNotFound
	}
	f, err := readFlow(filepath.Join(s.dir, fmt.Sprintf("%s.%d.json", id, v)))
	if errors.Is(err, os.ErrNotExist) {
		return Flow{}, ErrNotFound
	}
	return f, err
}

func (s *Store) versionsLocked(id string) []int {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var vs []int
	for _, e := range ents {
		rest, ok := strings.CutPrefix(e.Name(), id+".")
		if !ok {
			continue
		}
		n, ok := strings.CutSuffix(rest, ".json")
		if !ok {
			continue
		}
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			vs = append(vs, v)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(vs)))
	return vs
}

func (s *Store) pruneLocked(id string) {
	vs := s.versionsLocked(id)
	for _, v := range vs[min(len(vs), KeepVersions):] {
		_ = os.Remove(filepath.Join(s.dir, fmt.Sprintf("%s.%d.json", id, v)))
	}
}

func (s *Store) writeLocked(f Flow) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return platform.WritePrivate(s.path(f.ID), append(b, '\n'))
}
