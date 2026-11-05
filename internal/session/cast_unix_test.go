//go:build unix

package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCastIgnoresPlantedSymlinks: an agent that can write in its session
// folder cannot redirect a recording through a symlink, neither at the
// cast's name nor in place of the runs folder.
func TestCastIgnoresPlantedSymlinks(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "runs", "1.cast")); err != nil {
		t.Fatal(err)
	}
	c, err := openCast(dir, 1, 80, 24, "", 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.write([]byte("output"))
	c.close()
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("victim overwritten: %q", b)
	}
	if st, err := os.Lstat(filepath.Join(dir, "runs", "1.cast")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("cast is not a fresh regular file: %v %v", st, err)
	}

	// runs itself replaced by a link to a folder outside the session.
	dir2 := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir2, "runs")); err != nil {
		t.Fatal(err)
	}
	c, err = openCast(dir2, 1, 80, 24, "", 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.close()
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote outside the session folder: %v", ents)
	}
	if st, err := os.Lstat(filepath.Join(dir2, "runs")); err != nil || !st.IsDir() {
		t.Fatalf("runs not a real folder: %v %v", st, err)
	}
}
