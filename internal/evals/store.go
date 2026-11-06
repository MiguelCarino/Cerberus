package evals

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
	ErrStale    = errors.New("the eval changed elsewhere (stale version)")
	ErrState    = errors.New("the run is not in a state that allows this")
	ErrDisabled = errors.New("evals are turned off")
	ErrClosed   = errors.New("the eval engine is closed")
)

// Store keeps eval definitions as DATA/evals/<id>.json (0600, written
// atomically). Every save bumps the version; an update must name the
// version it was read at (ErrStale otherwise). It checks structure and
// refuses definitions with errors.
type Store struct {
	dir   string
	now   func() time.Time
	mu    sync.Mutex
	cache map[string]Eval
}

// OpenStore opens (and creates, 0700) the evals folder.
func OpenStore(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, now: now, cache: map[string]Eval{}}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !ValidID(id) {
			continue
		}
		ev, err := readEval(s.path(id))
		if err != nil || ev.ID != id {
			continue // a damaged file is left for the owner, not listed
		}
		s.cache[id] = ev
	}
	return s, nil
}

// Dir is the evals folder.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func readEval(p string) (Eval, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Eval{}, err
	}
	if len(b) > maxEvalFileBytes {
		return Eval{}, fmt.Errorf("%s: too big", p)
	}
	var e Eval
	if err := json.Unmarshal(b, &e); err != nil {
		return Eval{}, fmt.Errorf("%s: %w", p, err)
	}
	normalize(&e)
	e.OwnerName = ""
	return e, nil
}

// List is every eval, by name then id.
func (s *Store) List() []Eval {
	s.mu.Lock()
	out := make([]Eval, 0, len(s.cache))
	for _, e := range s.cache {
		out = append(out, e.clone())
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

// Get is one eval, or ErrNotFound.
func (s *Store) Get(id string) (Eval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[id]
	if !ok {
		return Eval{}, ErrNotFound
	}
	return e.clone(), nil
}

// Create saves a new eval with version 1 and an id from its name
// (ev-<slug>, then -2, -3, ... when taken); a valid free id already set is
// kept.
func (s *Store) Create(e Eval) (Eval, error) {
	e = e.clone()
	normalize(&e)
	if ps := Validate(e, nil); HasErrors(ps) {
		return Eval{}, &ValidationError{Problems: ps}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidID(e.ID) || s.taken(e.ID) {
		base := Slug(e.Name)
		e.ID = base
		for n := 2; s.taken(e.ID); n++ {
			e.ID = base + "-" + strconv.Itoa(n)
		}
	}
	e.Version = 1
	e.Updated = s.now().UTC()
	e.OwnerName = ""
	if err := s.writeLocked(e); err != nil {
		return Eval{}, err
	}
	s.cache[e.ID] = e
	return e.clone(), nil
}

func (s *Store) taken(id string) bool {
	if _, ok := s.cache[id]; ok {
		return true
	}
	_, err := os.Lstat(s.path(id))
	return err == nil
}

// Update replaces eval e.ID. e.Version must be the stored version; the
// owner is kept.
func (s *Store) Update(e Eval) (Eval, error) {
	e = e.clone()
	normalize(&e)
	if ps := Validate(e, nil); HasErrors(ps) {
		return Eval{}, &ValidationError{Problems: ps}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.cache[e.ID]
	if !ok {
		return Eval{}, ErrNotFound
	}
	if e.Version != old.Version {
		return Eval{}, ErrStale
	}
	e.Owner, e.OwnerName = old.Owner, ""
	e.Version = old.Version + 1
	e.Updated = s.now().UTC()
	if err := s.writeLocked(e); err != nil {
		return Eval{}, err
	}
	s.cache[e.ID] = e
	return e.clone(), nil
}

// Delete removes an eval (its runs stay).
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
	return nil
}

func (s *Store) writeLocked(e Eval) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return platform.WritePrivate(s.path(e.ID), append(b, '\n'))
}
