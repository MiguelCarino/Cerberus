//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func TestDelete(t *testing.T) {
	var plain string
	e := newTestEnv(t, nil, func(e *testEnv) {
		dirty := e.script("dirty.sh", "echo work > work.txt\nexit 0\n")
		e.profiles(
			config.AgentProfile{Name: "noop", Command: []string{"bash", "-c", "exit 0"}},
			config.AgentProfile{Name: "dirty", Command: []string{"bash", dirty}},
			config.AgentProfile{Name: "sleepy", Command: []string{"bash", "-c", "sleep 60"}},
		)
		plain = filepath.Join(e.home, "plain")
		os.MkdirAll(plain, 0o755)
		os.WriteFile(filepath.Join(plain, "keep.txt"), []byte("mine"), 0o644)
	})
	ctx := context.Background()
	mk := func(ts TaskSpec) string {
		infos, err := e.m.Create(ctx, ts)
		if err != nil {
			t.Fatal(err)
		}
		return infos[0].ID
	}
	gone := func(id string) bool {
		_, ok := e.m.Get(id)
		_, err := os.Stat(filepath.Join(e.m.sessDir, id))
		return !ok && errors.Is(err, os.ErrNotExist)
	}

	// A finished repo session with nothing unaccepted: record and worktree go.
	clean := mk(TaskSpec{Repo: e.repo, Task: "clean", Agent: "noop"})
	wt := e.waitState(clean, StatePassed).Worktree
	if err := e.m.Delete(ctx, clean, false); err != nil {
		t.Fatal(err)
	}
	if !gone(clean) {
		t.Fatal("clean session still there")
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree %s kept", wt)
	}

	// Unaccepted work is kept unless forced.
	dirty := mk(TaskSpec{Repo: e.repo, Task: "dirty", Agent: "dirty"})
	e.waitState(dirty, StatePassed)
	err := e.m.Delete(ctx, dirty, false)
	if !IsUnaccepted(err) {
		t.Fatalf("dirty without force: %v", err)
	}
	if _, ok := e.m.Get(dirty); !ok {
		t.Fatal("refused delete removed the session")
	}
	if err := e.m.Delete(ctx, dirty, true); err != nil || !gone(dirty) {
		t.Fatalf("forced delete: %v", err)
	}

	// A folder session: only Cerberus' record goes, never the user's folder.
	folder := mk(TaskSpec{Dir: plain, Task: "folder", Agent: "noop"})
	e.waitState(folder, StatePassed)
	if err := e.m.Delete(ctx, folder, false); err != nil || !gone(folder) {
		t.Fatalf("folder: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(plain, "keep.txt")); err != nil || string(b) != "mine" {
		t.Fatalf("the user's folder was touched: %v %q", err, b)
	}

	// A running session is stopped first, then deleted.
	run := mk(TaskSpec{Task: "sleep", Agent: "sleepy"})
	e.waitState(run, StateRunning)
	start := time.Now()
	if err := e.m.Delete(ctx, run, false); err != nil || !gone(run) {
		t.Fatalf("running: %v", err)
	}
	if time.Since(start) > discardTimeout {
		t.Errorf("delete of a running session took %s", time.Since(start))
	}

	if err := e.m.Delete(ctx, "nope", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}

	// Bulk: unaccepted work is skipped, not an error.
	a := mk(TaskSpec{Repo: e.repo, Task: "a", Agent: "noop"})
	b := mk(TaskSpec{Repo: e.repo, Task: "b", Agent: "dirty"})
	e.waitState(a, StatePassed)
	e.waitState(b, StatePassed)
	res := e.m.Bulk(ctx, []string{a, b}, BulkOp{Kind: BulkDelete})
	if res[0].Result != BulkOK || res[1].Result != BulkSkipped || !gone(a) {
		t.Fatalf("bulk %+v", res)
	}
	res = e.m.Bulk(ctx, []string{b}, BulkOp{Kind: BulkDelete, Force: true})
	if res[0].Result != BulkOK || !gone(b) {
		t.Fatalf("bulk force %+v", res)
	}
}
