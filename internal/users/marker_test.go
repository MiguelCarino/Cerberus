package users

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Once an account existed, a missing users.json is an error (at open and
// while running), never a silent switch back to "no users", where every
// device and agent acts as the admin.
func TestMissingAccountsFileFailsClosed(t *testing.T) {
	t.Cleanup(SetCostForTest(1000))
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("ana", "", RoleAdmin, "a long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerName)); err != nil {
		t.Fatalf("no marker: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, FileName), filepath.Join(dir, "users.json.bak")); err != nil {
		t.Fatal(err)
	}
	if !st.HasUsers() {
		t.Fatal("a running store forgot its accounts when users.json went missing")
	}
	if _, err := Open(dir); !errors.Is(err, ErrAccountsMissing) {
		t.Fatalf("open without users.json: %v", err)
	}
	// Deleting the marker is the deliberate way back to single-user mode.
	if err := os.Remove(filepath.Join(dir, MarkerName)); err != nil {
		t.Fatal(err)
	}
	if st2, err := Open(dir); err != nil || st2.HasUsers() {
		t.Fatalf("single-user mode on purpose: %v", err)
	}
}
