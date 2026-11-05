package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalFilesRead(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("notes.txt", "alpha\nbeta\n")
	abs := write("sub/big.log", strings.Repeat("x", 3000))
	write("bin.dat", "a\x00b")
	write("secrets/.env", "TOKEN=1")
	l := &localFiles{root: root, deny: []string{"**/.env"}, maxKiB: 1}

	got, err := l.read([]string{"notes.txt", abs})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--- notes.txt ---\nalpha\nbeta") || !strings.Contains(got, "(first 1 KiB of 2)") ||
		!strings.HasSuffix(got, "---\n"+strings.Repeat("x", 1024)) {
		t.Fatalf("read: %q", got)
	}
	for name, want := range map[string]string{
		"bin.dat":      "not a text file",
		"secrets/.env": "protected path",
		"missing.txt":  "no such file",
		"sub":          "not a file",
	} {
		if _, err := l.read([]string{name}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", name, err, want)
		}
	}
	if _, err := l.read(make([]string, maxLocalFiles+1)); err == nil {
		t.Error("more than the file limit was read")
	}
	// A link to a protected file is protected too.
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(root, "secrets", ".env"), filepath.Join(root, "innocent.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := l.read([]string{"innocent.txt"}); err == nil || !strings.Contains(err.Error(), "protected path") {
			t.Errorf("symlink to a protected file: %v", err)
		}
	}
}
