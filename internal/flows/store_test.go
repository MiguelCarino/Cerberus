package flows

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, func() time.Time { return time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	f := validFlow()
	f.Name = "My Flow"
	a, err := st.Create(f)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "fl-my-flow" || a.Version != 1 || a.Updated.IsZero() {
		t.Fatalf("created %+v", a)
	}
	b, err := st.Create(f)
	if err != nil || b.ID != "fl-my-flow-2" {
		t.Fatalf("second create: %v %v", b.ID, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "fl-my-flow.json"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode: %v %v", fi.Mode(), err)
		}
	}
	// Update with the right version bumps it and keeps the old file.
	a.Description = "v2"
	a2, err := st.Update(a)
	if err != nil || a2.Version != 2 {
		t.Fatalf("update: %+v %v", a2, err)
	}
	if _, err := st.Update(a); !errors.Is(err, ErrStale) {
		t.Fatalf("stale update: %v", err)
	}
	old, err := st.Version(a.ID, 1)
	if err != nil || old.Description != "" {
		t.Fatalf("version 1: %+v %v", old, err)
	}
	// Keep the last 10 versions.
	cur := a2
	for i := 0; i < 14; i++ {
		if cur, err = st.Update(cur); err != nil {
			t.Fatal(err)
		}
	}
	vs := st.Versions(a.ID)
	if len(vs) != KeepVersions || vs[0] != cur.Version-1 {
		t.Fatalf("versions %v (current %d)", vs, cur.Version)
	}
	// Invalid flows are refused.
	bad := cur
	bad.Steps = append(bad.Steps, Step{ID: "x", Kind: "nope"})
	var ve *ValidationError
	if _, err := st.Update(bad); !errors.As(err, &ve) {
		t.Fatalf("invalid update: %v", err)
	}
	// Reopen: flows load back; version files are not flows.
	st2, err := OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l := st2.List(); len(l) != 2 || l[0].ID != "fl-my-flow" || l[0].Version != cur.Version {
		t.Fatalf("reloaded list: %+v", l)
	}
	if err := st2.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Get(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted flow still there")
	}
	if len(st2.Versions(a.ID)) != 0 {
		t.Fatal("versions left after delete")
	}
	if err := st2.Delete(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete")
	}
	if _, err := st2.Get("fl-../../x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("traversal id")
	}
}

// No saved flow gets DraftID, the id test runs of unsaved drafts carry, so
// their runs are never listed or pruned with a real flow's.
func TestStoreReservesDraftID(t *testing.T) {
	st, err := OpenStore(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []Flow{func() Flow { f := validFlow(); f.Name = "Draft"; return f }(),
		func() Flow { f := validFlow(); f.Name = "x"; f.ID = DraftID; return f }()} {
		got, err := st.Create(f)
		if err != nil || got.ID == DraftID {
			t.Fatalf("created %q (%v)", got.ID, err)
		}
	}
}
