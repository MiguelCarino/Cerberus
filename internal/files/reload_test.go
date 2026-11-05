package files

import (
	"context"
	"errors"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// A per-file limit raised by Reload lets a refused upload through, without
// a restart; the startup config stays as it was.
func TestReloadSizeLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Files.MaxFileMiB = 1 })
	body := strings.Repeat("x", 2<<20)
	if _, err := e.s.Put(context.Background(), e.target(), "big.bin", strings.NewReader(body), -1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("before reload: %v", err)
	}
	next := e.cfg.Clone()
	next.Files.MaxFileMiB = 4
	e.s.Reload(next)
	if _, err := e.s.Put(context.Background(), e.target(), "big.bin", strings.NewReader(body), -1); err != nil {
		t.Fatalf("after reload: %v", err)
	}
	if e.cfg.Files.MaxFileMiB != 1 {
		t.Fatal("startup config changed")
	}
	e.s.Reload(nil) // ignored
}
