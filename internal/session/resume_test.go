//go:build !windows

package session

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutputResumeFrom covers the stream position arithmetic (web-api-ops-7):
// exact, behind within the tail, a gap, ahead, another epoch, malformed,
// and after release.
func TestOutputResumeFrom(t *testing.T) {
	o := newOutput(filepath.Join(t.TempDir(), "scrollback.log"), 1<<20, 1024)
	o.Write([]byte("hello "))
	first := o.attachFrom("", 1024)
	first.Detach()
	if !first.Reset || string(first.Backlog) != "hello " {
		t.Fatalf("first attach: %+v", first)
	}

	// Exactly current: nothing to send, no reset.
	a := o.attachFrom(first.Pos, 1024)
	a.Detach()
	if a.Reset || len(a.Backlog) != 0 || a.Pos != first.Pos {
		t.Fatalf("current: reset=%v %q %s", a.Reset, a.Backlog, a.Pos)
	}

	// Behind: only the missed bytes.
	o.Write([]byte("world"))
	a = o.attachFrom(first.Pos, 1024)
	a.Detach()
	if a.Reset || string(a.Backlog) != "world" {
		t.Fatalf("behind: reset=%v %q", a.Reset, a.Backlog)
	}
	ep, off, ok := parsePos(a.Pos)
	if !ok || off != 11 || ep != o.epoch {
		t.Fatalf("pos %q", a.Pos)
	}

	// A chunk on the channel moves the position on by its length.
	b := o.attachFrom(a.Pos, 1024)
	o.Write([]byte("!"))
	chunk := <-b.Out
	b.Detach()
	_, boff, _ := parsePos(b.Pos)
	c := o.attachFrom(makePos(o.epoch, boff+int64(len(chunk))), 1024)
	c.Detach()
	if c.Reset || len(c.Backlog) != 0 {
		t.Fatalf("after counting a chunk: reset=%v %q", c.Reset, c.Backlog)
	}

	// Ahead (a daemon restart reuses nothing, but a forged or stale offset
	// may be): a reset.
	for name, from := range map[string]StreamPos{
		"ahead":       makePos(o.epoch, 999),
		"other epoch": makePos("x", 3),
		"malformed":   "nope",
		"negative":    o.epoch + ".-1",
		"empty":       "",
	} {
		a := o.attachFrom(from, 1024)
		a.Detach()
		if !a.Reset || string(a.Backlog) != "hello world!" {
			t.Errorf("%s: reset=%v %q", name, a.Reset, a.Backlog)
		}
	}

	// A gap larger than the limit: reset with the last limit bytes.
	big := strings.Repeat("z", 3000)
	o.Write([]byte(big))
	a = o.attachFrom(first.Pos, 1024)
	a.Detach()
	if !a.Reset || len(a.Backlog) != 1024 || !bytes.Equal(a.Backlog, []byte(big[:1024])) {
		t.Fatalf("gap: reset=%v len %d", a.Reset, len(a.Backlog))
	}
	pos := a.Pos

	// After release the held bytes come from disk; positions still hold.
	o.release()
	a = o.attachFrom(pos, 1024)
	a.Detach()
	if a.Reset || len(a.Backlog) != 0 || a.Pos != pos {
		t.Fatalf("after release: reset=%v %q %s", a.Reset, a.Backlog, a.Pos)
	}
	o.Write([]byte("more"))
	a = o.attachFrom(pos, 1024)
	a.Detach()
	if a.Reset || string(a.Backlog) != "more" {
		t.Fatalf("after release and a write: reset=%v %q", a.Reset, a.Backlog)
	}

	// A fresh output (a daemon restart) has a new epoch: the old position
	// resets even where the offset happens to be valid.
	o2 := newOutput(o.sb.path, 1<<20, 1024)
	if o2.epoch == o.epoch {
		t.Fatal("two outputs share an epoch")
	}
	a = o2.attachFrom(makePos(o.epoch, 0), 1024)
	a.Detach()
	if !a.Reset {
		t.Fatal("a position from another output resumed")
	}
}

// TestAttachManyFromResumes: the wall resumes per session.
func TestAttachManyFromResumes(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) { writer(e, "a.txt", "a") })
	m := e.m
	x := &session{dir: t.TempDir(), out: m.newOutput(t.TempDir()), info: Info{ID: "x", Repo: e.repo, State: StateRunning}}
	y := &session{dir: t.TempDir(), out: m.newOutput(t.TempDir()), info: Info{ID: "y", Repo: e.repo, State: StateRunning}}
	m.mu.Lock()
	m.sessions["x"], m.sessions["y"] = x, y
	m.order = append(m.order, "x", "y")
	m.mu.Unlock()
	x.out.Write([]byte("xx"))
	y.out.Write([]byte("yy"))
	bl, _, detach, err := m.AttachManyFrom([]string{"x", "y"}, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	detach()
	x.out.Write([]byte("+x"))
	bl2, _, detach, err := m.AttachManyFrom([]string{"x", "y"}, 8, map[string]StreamPos{"x": bl["x"].Pos, "y": "stale.0"})
	if err != nil {
		t.Fatal(err)
	}
	detach()
	if bl2["x"].Reset || string(bl2["x"].Backlog) != "+x" {
		t.Fatalf("x: %+v", bl2["x"])
	}
	if !bl2["y"].Reset || string(bl2["y"].Backlog) != "yy" {
		t.Fatalf("y: %+v", bl2["y"])
	}
}
