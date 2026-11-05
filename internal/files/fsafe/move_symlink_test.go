//go:build !windows

package fsafe

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyExclusiveRefusesSymlink: a copy never follows a symlink put in
// the source's place (a file moved back out of a workspace), so it cannot
// pull in a file the agent could not read.
func TestCopyExclusiveRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "inbox-file")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "copy")
	if err := copyExclusive(link, to, 0); err == nil {
		t.Fatal("copied through a symlink")
	}
	if _, err := os.Lstat(to); !os.IsNotExist(err) {
		t.Fatalf("destination left behind: %v", err)
	}
}

// TestMoveIntoModeSetBeforeLink: the mode is applied to the file before it
// appears under its name (so nothing chmods the destination path, which an
// agent could have swapped for a symlink, afterwards): a delivered file
// already has its final mode, and modeFor is asked once per name tried.
func TestMoveIntoModeSetBeforeLink(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	from := filepath.Join(src, "notes.txt")
	if err := os.WriteFile(from, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var asked []string
	final, err := placeNoClobber(from, dst, "notes.txt", true, func(n string) os.FileMode {
		asked = append(asked, n)
		return 0o640
	})
	if err != nil || final != "notes (2).txt" {
		t.Fatalf("place: %q %v", final, err)
	}
	st, err := os.Lstat(filepath.Join(dst, final))
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v %v, want 0640", st.Mode(), err)
	}
	if len(asked) != 1 || asked[0] != final {
		t.Fatalf("modeFor asked for %v", asked)
	}
}
