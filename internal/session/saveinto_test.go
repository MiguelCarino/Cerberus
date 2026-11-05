//go:build !windows

package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"carino.systems/cerberus/internal/platform"
)

// A confined save lands in the user's own folder (which sits under the data
// folder, refused by SaveTo) and a symlink planted there cannot redirect it.
func TestSaveIntoStaysInRoot(t *testing.T) {
	e := writerEnv(t)
	infos, err := e.m.Create(context.Background(), TaskSpec{Task: "write"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(infos[0].ID, StatePassed)
	data, err := platform.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(data, "users", "u-aaaaaaaaaaaa", "files")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := e.m.SaveInto(infos[0].ID, root, filepath.Join(root, "out"), false); err != nil || n != 1 {
		t.Fatalf("save into own folder: %d %v", n, err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "out", "out.txt")); err != nil || string(raw) != "hi\n" {
		t.Fatalf("saved file: %q %v", raw, err)
	}
	victim := filepath.Join(homeOf(e), ".config", "autostart")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.SaveInto(infos[0].ID, root, filepath.Join(root, "link"), true); err == nil {
		t.Fatal("save through a symlink out of the root succeeded")
	}
	if _, err := e.m.SaveInto(infos[0].ID, root, filepath.Join(root, "link", "sub"), true); err == nil {
		t.Fatal("save below a symlink out of the root succeeded")
	}
	if _, err := os.Stat(filepath.Join(victim, "out.txt")); err == nil {
		t.Fatal("wrote outside the user's folder")
	}
	if _, err := e.m.SaveInto(infos[0].ID, root, filepath.Join(root, ".."), true); err == nil {
		t.Fatal("save to the parent of the root succeeded")
	}
}
