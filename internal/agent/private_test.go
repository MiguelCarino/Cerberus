package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The built-in agent cannot read another session's records (unredacted
// conversation.json, scrollback, transcript) or worktree, but works in its
// own worktree as usual.
func TestSessionDataIsPrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "data"))
	area := sessionData("own", "")
	if len(area.roots) != 3 {
		t.Fatalf("roots %v", area.roots)
	}
	sessions, worktrees, chats := area.roots[0], area.roots[1], area.roots[2]
	cwd := filepath.Join(worktrees, "own")
	area = sessionData("own", cwd)
	for _, f := range []string{
		filepath.Join(sessions, "other", "conversation.json"),
		filepath.Join(sessions, "own", "conversation.json"),
		filepath.Join(worktrees, "other", ".env"),
		filepath.Join(cwd, "main.go"),
		filepath.Join(sessions, "own", "workspace", "notes.txt"),
		filepath.Join(chats, "chat-20261101-000000-abcdef.json"),
	} {
		write(t, f, "STRIPE_KEY=sk_live_x\n")
	}
	tb := newToolbox(cwd, nil, 30*time.Second)
	tb.private = area.resolved()
	ctx := context.Background()
	for _, p := range []string{
		"../../sessions/other/conversation.json",
		filepath.Join(sessions, "own", "conversation.json"),
		filepath.Join(worktrees, "other", ".env"),
		sessions,
		filepath.Join(chats, "chat-20261101-000000-abcdef.json"), // the owner's LLM chats too
	} {
		if out, bad := tb.run(ctx, "read_file", args{"path": p}); !bad || strings.Contains(out, "sk_live") {
			t.Errorf("read %s: bad=%v %q", p, bad, out)
		}
	}
	if out, bad := tb.run(ctx, "read_file", args{"path": "main.go"}); bad || !strings.Contains(out, "STRIPE") {
		t.Errorf("own worktree refused: %q", out)
	}
	// A link from the worktree into another session is refused too.
	if err := os.Symlink(filepath.Join(sessions, "other"), filepath.Join(cwd, "peek")); err == nil {
		if out, bad := tb.run(ctx, "read_file", args{"path": "peek/conversation.json"}); !bad || strings.Contains(out, "sk_live") {
			t.Errorf("read through a link: %q", out)
		}
	}
	// A scratch session's own workspace stays usable.
	scratch := sessionData("own", filepath.Join(sessions, "own", "workspace"))
	if scratch.hides(filepath.Join(sessions, "own", "workspace", "notes.txt")) {
		t.Error("own scratch workspace hidden")
	}
	if !scratch.hides(filepath.Join(sessions, "own", "task.md")) {
		t.Error("own task.md readable")
	}
}
