//go:build linux

package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessCPUTime(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A command name with spaces and a ')' of its own.
	stat := "42 (llama server) x) R 1 42 42 0 -1 4194304 100 0 0 0 250 50 0 0 20 0 8 0 100 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, ok := processCPUTime(root, 42); !ok || d != 3*time.Second {
		t.Errorf("cpu time %v %v, want 3s", d, ok)
	}
	if _, ok := processCPUTime(root, 43); ok {
		t.Error("a missing pid read as ok")
	}
	if _, ok := processCPUTime(root, 0); ok {
		t.Error("pid 0 read as ok")
	}
	// This process has used some CPU.
	if d, ok := ProcessCPUTime(os.Getpid()); !ok || d < 0 {
		t.Errorf("own cpu time %v %v", d, ok)
	}
}
