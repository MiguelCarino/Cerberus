//go:build !windows

package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReturnFileRefusesSymlink: undoing a delivery moves a file back out of
// the workspace inbox, where an agent may have put a symlink in its place.
// The symlink is refused: nothing lands in staging and the link's target
// keeps its mode (a chmod by path would have followed it).
func TestReturnFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(target, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(dir, "ws", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(inbox, "report.txt")
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := returnFile(p, staging, "report.txt", true); err == nil {
		t.Fatal("a symlink was returned to staging")
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("target mode %v %v, want 0644", st.Mode(), err)
	}
	if _, err := os.Lstat(filepath.Join(staging, "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("staging got the symlink: %v", err)
	}
}
