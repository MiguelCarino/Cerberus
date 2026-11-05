package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJailRefusesOutsideCwd(t *testing.T) {
	cwd := t.TempDir()
	out := t.TempDir()
	_ = os.WriteFile(filepath.Join(out, "secret.txt"), []byte("x"), 0o600)
	tb := newToolbox(cwd, nil, 0)
	tb.jail = true
	if why := tb.refused(filepath.Join(out, "secret.txt")); !strings.Contains(why, "outside the working folder") {
		t.Fatalf("outside read allowed: %q", why)
	}
	if why := tb.refused(filepath.Join(cwd, "a.txt")); why != "" {
		t.Fatalf("inside refused: %q", why)
	}
	// A symlink out of the folder is outside too.
	_ = os.Symlink(out, filepath.Join(cwd, "link"))
	if why := tb.refused(filepath.Join(cwd, "link", "secret.txt")); why == "" {
		t.Fatal("a symlink out of the jail was allowed")
	}
	// Without Wrap, commands are refused (and not offered).
	if _, err := tb.runCommand(context.Background(), args{"command": "true"}); err == nil {
		t.Fatal("a jailed command ran without a sandbox")
	}
	e := &engine{o: RunOptions{Jail: true}}
	for _, s := range e.toolSpecs() {
		if s.Function.Name == "run_command" {
			t.Fatal("run_command offered without Wrap")
		}
	}
	e.o.Wrap = func(a []string) []string { return a }
	found := false
	for _, s := range e.toolSpecs() {
		found = found || s.Function.Name == "run_command"
	}
	if !found {
		t.Fatal("run_command not offered with Wrap")
	}
}

func TestWrapRunsCommandInside(t *testing.T) {
	cwd := t.TempDir()
	tb := newToolbox(cwd, nil, 0)
	tb.jail = true
	var got []string
	tb.wrap = func(argv []string) []string {
		got = argv
		return append([]string{"env", "WRAPPED=1"}, argv...)
	}
	out, err := tb.runCommand(context.Background(), args{"command": "echo $WRAPPED"})
	if err != nil || strings.TrimSpace(out) != "1" {
		t.Fatalf("wrapped run: %q %v", out, err)
	}
	if len(got) != 3 || got[0] != "bash" || got[1] != "-c" {
		t.Fatalf("wrap got %v", got)
	}
}
