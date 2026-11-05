package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"carino.systems/cerberus/internal/files/fsafe"
)

// NewStaging creates an empty staging folder (DATA/uploads/<id>/, 0700) for
// New task attachments and returns its id. Put with Target{Staging: id}
// fills it; session.TaskSpec.Staging hands it to the new session.
func (s *Store) NewStaging() (string, error) {
	if s.uploads == "" {
		return "", fmt.Errorf("no data folder for uploads")
	}
	if err := os.MkdirAll(s.uploads, 0o700); err != nil {
		return "", err
	}
	for tries := 0; tries < 5; tries++ {
		id := randomHex(16)
		err := os.Mkdir(filepath.Join(s.uploads, id), 0o700)
		if err == nil {
			return id, fsafe.InitHeld(filepath.Join(s.uploads, id))
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free staging id")
}

// DeleteStaging removes a staging folder and everything in it.
func (s *Store) DeleteStaging(id string) error {
	if !fsafe.ValidStaging(id) || s.uploads == "" {
		return fmt.Errorf("%w: staging %q", ErrNotFound, id)
	}
	dir := filepath.Join(s.uploads, id)
	if _, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("%w: staging %s", ErrNotFound, id)
	}
	_ = os.Remove(s.stagingOwnerPath(id))
	return os.RemoveAll(dir)
}

// Sweep removes staging folders untouched for StagingTTL (the newest of the
// folder's and its files' modification times counts) and returns how many
// it removed. Anything in DATA/uploads that is not a staging folder stays.
func (s *Store) Sweep(now time.Time) int {
	if s.uploads == "" {
		return 0
	}
	entries, err := os.ReadDir(s.uploads)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".owner"); ok && !e.IsDir() && fsafe.ValidStaging(id) {
			// An owner sidecar whose folder is gone (taken by a session).
			if _, err := os.Lstat(filepath.Join(s.uploads, id)); os.IsNotExist(err) {
				_ = os.Remove(filepath.Join(s.uploads, e.Name()))
			}
			continue
		}
		if !e.IsDir() || !fsafe.ValidStaging(e.Name()) {
			continue
		}
		dir := filepath.Join(s.uploads, e.Name())
		if now.Sub(lastTouched(dir)) < StagingTTL {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			s.log.Warn("remove expired staging", "dir", dir, "err", err)
			continue
		}
		removed++
	}
	return removed
}

// lastTouched is the newest modification time of dir and its entries.
func lastTouched(dir string) time.Time {
	var t time.Time
	if st, err := os.Stat(dir); err == nil {
		t = st.ModTime()
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == fsafe.PrivateList {
			continue // bookkeeping, not activity
		}
		if st, err := e.Info(); err == nil && st.ModTime().After(t) {
			t = st.ModTime()
		}
	}
	return t
}

// Run expires staging folders: once now, then hourly, until ctx ends. It
// sleeps on a timer in between.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := s.Sweep(now); n > 0 {
				s.log.Info("expired staged uploads", "folders", n)
			}
			t.Reset(sweepEvery)
		}
	}
}

// stagingOwnerPath is the owner sidecar of a staging folder (Phase 4a):
// DATA/uploads/<id>.owner, beside the folder and not in it, because
// everything in the folder moves into the session that takes it.
func (s *Store) stagingOwnerPath(id string) string {
	return filepath.Join(s.uploads, id+".owner")
}

// SetStagingOwner records the user who made a staging folder (0600).
func (s *Store) SetStagingOwner(id, owner string) error {
	if !fsafe.ValidStaging(id) || s.uploads == "" {
		return fmt.Errorf("%w: staging %q", ErrNotFound, id)
	}
	return os.WriteFile(s.stagingOwnerPath(id), []byte(owner), 0o600)
}

// StagingOwner is the user who made a staging folder ("" when none was
// recorded: from before users, or the admin token before users).
func (s *Store) StagingOwner(id string) string {
	if !fsafe.ValidStaging(id) || s.uploads == "" {
		return ""
	}
	raw, err := os.ReadFile(s.stagingOwnerPath(id))
	if err != nil {
		return ""
	}
	return string(raw)
}
