//go:build !windows

package subs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// A *.jsonl FIFO or symlink is skipped: the scan neither blocks nor reads
// outside the data folder.
func TestScanSkipsFIFOsAndSymlinks(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true})
	t0 := f.now.Add(-time.Hour)
	f.write("-p/real.jsonl", line("a", t0, 1, 0, 0, 0))
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(line("b", t0, 100, 0, 0, 0)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "-p", "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(f.root, "-p", "pipe.jsonl"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan PlanUsage, 1)
	go func() { done <- f.snap() }()
	select {
	case u := <-done:
		if u.WeeklyTokens != 1 {
			t.Fatalf("usage %+v: only the regular file counts", u)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scan blocked on a FIFO")
	}
	// openRegular refuses a symlink even when asked directly.
	if fh, err := openRegular(filepath.Join(f.root, "-p", "link.jsonl")); err == nil {
		fh.Close()
		t.Fatal("openRegular followed a symlink")
	}
	if fh, err := openRegular(filepath.Join(f.root, "-p", "pipe.jsonl")); err == nil {
		fh.Close()
		t.Fatal("openRegular opened a FIFO")
	}
}
