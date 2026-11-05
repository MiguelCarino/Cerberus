package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSaveAndRotateTokenStayPrivate: a stale config.json.tmp left at 0644 (a
// crash, an editor, an older build) never makes the saved config, which
// holds the admin token, readable by others.
func TestSaveAndRotateTokenStayPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Defaults()
	c.SetPath(p)
	check := func(what string) {
		t.Helper()
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: config.json is %v", what, st.Mode().Perm())
		}
	}
	stale := func() {
		t.Helper()
		if err := os.WriteFile(p+".tmp", []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p+".tmp", 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stale()
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	check("Save")
	stale()
	if err := c.RotateToken("tok-2"); err != nil {
		t.Fatal(err)
	}
	check("RotateToken")
	if got, err := Load(p); err != nil || got.TokenSnapshot() != "tok-2" {
		t.Fatalf("token after RotateToken: %v", err)
	}
}
