package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritePrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	// An old file and a stale fixed-name temp with wide bits change nothing.
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".tmp", []byte("planted"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil || string(raw) != "new" {
		t.Fatalf("content %q %v", raw, err)
	}
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 { // state.json and the untouched planted file
		t.Fatalf("leftovers: %v", ents)
	}
	if err := WritePrivate(filepath.Join(dir, "missing", "x"), nil); err == nil {
		t.Fatal("a missing folder must fail")
	}
}
