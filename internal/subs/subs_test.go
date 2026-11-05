package subs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// line builds a transcript line shaped like Claude Code's (same keys, fake
// values). Fields the tracker ignores are included so parsing is realistic.
func line(id string, at time.Time, in, out, create, read int) string {
	return fmt.Sprintf(`{"parentUuid":"p","isSidechain":false,"type":"assistant","uuid":"u-%s-%d","timestamp":%q,`+
		`"sessionId":"sess-1","cwd":"/x","version":"2","gitBranch":"main","requestId":"req",`+
		`"message":{"model":"claude-x","id":%q,"type":"message","role":"assistant","content":[{"type":"text","text":"assistant says hi"}],`+
		`"stop_reason":null,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,`+
		`"output_tokens":%d,"service_tier":"standard","cache_creation":{"ephemeral_5m_input_tokens":0}}}}`+"\n",
		id, at.UnixNano(), at.UTC().Format(time.RFC3339Nano), id, in, create, read, out)
}

const noise = `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"permission-mode","mode":"default"}
not json at all
{"type":"assistant","timestamp":"bad","message":{"id":"zz","usage":{"input_tokens":5}}}
`

type fixture struct {
	t    *testing.T
	root string // DataDir
	now  time.Time
	tr   *Tracker
}

func newFixture(t *testing.T, sc config.SubscriptionConfig) *fixture {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "share"))
	root := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if sc.Kind == "" {
		sc.Kind = KindClaudeCode
	}
	sc.Name, sc.DataDir = "claude", root
	cfg := config.Defaults()
	cfg.Subscriptions = []config.SubscriptionConfig{sc}
	f := &fixture{t: t, root: root, now: time.Now().Truncate(time.Second)}
	f.tr = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.tr.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) append(rel, content string) {
	f.t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.root, rel), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := fh.WriteString(content); err != nil {
		f.t.Fatal(err)
	}
	fh.Close()
}

func (f *fixture) snap() PlanUsage {
	f.t.Helper()
	f.tr.scan()
	s := f.tr.Snapshot()
	if len(s) != 1 {
		f.t.Fatalf("snapshot = %+v", s)
	}
	return s[0]
}

func TestDuplicatesGapsAndBlocks(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true, Window5hLimit: 1000, WeeklyLimit: 9000})
	t0 := f.now.Add(-10 * time.Hour)
	f.write("-home-u-proj/sess-1.jsonl", noise+
		line("m1", t0, 10, 1, 100, 1000)+
		line("m2", t0.Add(time.Hour), 20, 2, 0, 0)+
		line("m2", t0.Add(time.Hour), 20, 2, 0, 0)+ // the CLI repeats a message per content block
		line("m3", t0.Add(2*time.Hour), 30, 3, 0, 0)+
		// 6 h gap: a new block opens at m4.
		line("m4", t0.Add(8*time.Hour), 40, 4, 0, 500)+
		line("m4", t0.Add(8*time.Hour), 40, 4, 0, 500)+
		line("old", f.now.Add(-9*24*time.Hour), 999, 999, 0, 0)) // beyond the week
	// A sub-agent transcript two levels down counts too.
	f.write("-home-u-proj/sess-1/subagents/agent-a.jsonl", line("m5", t0.Add(9*time.Hour), 50, 5, 0, 0))

	u := f.snap()
	if u.Err != "" {
		t.Fatal(u.Err)
	}
	if u.Name != "claude" || u.Kind != KindClaudeCode || u.Window5hLimit != 1000 || u.WeeklyLimit != 9000 {
		t.Errorf("identity = %+v", u)
	}
	if want := 44 + 55; u.Window5hTokens != want {
		t.Errorf("window tokens = %d, want %d", u.Window5hTokens, want)
	}
	if want := 111 + 22 + 33 + 44 + 55; u.WeeklyTokens != want {
		t.Errorf("weekly tokens = %d, want %d", u.WeeklyTokens, want)
	}
	if u.CacheReadTokens != 500 || u.WeeklyCacheReadTokens != 1500 {
		t.Errorf("cache reads = %d / %d", u.CacheReadTokens, u.WeeklyCacheReadTokens)
	}
	if want := t0.Add(13 * time.Hour); !u.Window5hResetAt.Equal(want) {
		t.Errorf("reset = %v, want %v", u.Window5hResetAt, want)
	}
	if !u.LastActivity.Equal(t0.Add(9*time.Hour)) || u.Sessions != 1 {
		t.Errorf("last = %v sessions = %d", u.LastActivity, u.Sessions)
	}

	// After the block's 5 h the window is closed; the week still counts.
	f.now = t0.Add(14 * time.Hour)
	u = f.snap()
	if u.Window5hTokens != 0 || !u.Window5hResetAt.IsZero() || u.Sessions != 0 || u.WeeklyTokens != 265 {
		t.Errorf("after reset = %+v", u)
	}
}

func TestBlocksRollWithoutGap(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true})
	t0 := f.now.Add(-7*time.Hour - 30*time.Minute)
	var b strings.Builder
	for i := range 8 { // hourly, no 5 h gap: blocks open at t0 and t0+5h
		b.WriteString(line(fmt.Sprint("h", i), t0.Add(time.Duration(i)*time.Hour), 1, 0, 0, 0))
	}
	f.write("-p/s.jsonl", b.String())
	u := f.snap()
	if !u.Window5hResetAt.Equal(t0.Add(10*time.Hour)) || u.Window5hTokens != 3 || u.WeeklyTokens != 8 {
		t.Errorf("rolling blocks = %+v", u)
	}
}

func TestIncrementalAppend(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true})
	t0 := f.now.Add(-time.Hour)
	f.write("-p/s.jsonl", line("a", t0, 1, 0, 0, 0))
	if u := f.snap(); u.WeeklyTokens != 1 {
		t.Fatalf("first = %+v", u)
	}
	st := f.tr.plans[0].files[filepath.Join(f.root, "-p/s.jsonl")]
	off := st.off

	// An appended line plus a partial one still being written.
	next := line("c", t0.Add(2*time.Minute), 100, 0, 0, 0)
	f.append("-p/s.jsonl", line("b", t0.Add(time.Minute), 10, 0, 0, 0)+next[:40])
	if u := f.snap(); u.WeeklyTokens != 11 {
		t.Fatalf("after append = %+v", u)
	}
	if st.off <= off || st.off >= st.size {
		t.Errorf("offset %d should stop before the partial line (size %d)", st.off, st.size)
	}
	f.append("-p/s.jsonl", next[40:]+line("a", t0, 1, 0, 0, 0)) // completes c; repeats a
	if u := f.snap(); u.WeeklyTokens != 111 {
		t.Fatalf("after completing = %+v", u)
	}
	// Unchanged file: no re-read (the offset stays at the end).
	before := st.off
	if u := f.snap(); u.WeeklyTokens != 111 || st.off != before {
		t.Errorf("rescan changed state: %+v off %d→%d", u, before, st.off)
	}
}

func TestTruncatedAndRotatedFiles(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true})
	t0 := f.now.Add(-time.Hour)
	path := f.write("-p/s.jsonl", line("a", t0, 1, 0, 0, 0)+line("b", t0, 10, 0, 0, 0)+line("c", t0, 100, 0, 0, 0))
	if u := f.snap(); u.WeeklyTokens != 111 {
		t.Fatalf("first = %+v", u)
	}
	// Truncated in place to something shorter, with one new message.
	f.write("-p/s.jsonl", line("a", t0, 1, 0, 0, 0)+line("d", t0, 1000, 0, 0, 0))
	if u := f.snap(); u.WeeklyTokens != 1111 {
		t.Fatalf("after truncate = %+v", u)
	}
	// Rotated: the old file moved away and a new one, longer than the old
	// offset, takes its place.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(line("d", t0, 1000, 0, 0, 0))
	for i := range 3 {
		b.WriteString(line(fmt.Sprint("e", i), t0, 10000, 0, 0, 0))
	}
	f.write("-p/s.jsonl", b.String())
	if err := os.Remove(path + ".old"); err != nil {
		t.Fatal(err)
	}
	if u := f.snap(); u.WeeklyTokens != 31111 {
		t.Fatalf("after rotate = %+v", u)
	}
	if n := len(f.tr.plans[0].files); n != 1 {
		t.Errorf("tracked files = %d, want 1", n)
	}
}

func TestOnlyCerberusSessions(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: false})
	t0 := f.now.Add(-time.Hour)
	wt := encodeProject(f.tr.cerberusDirs[0]) // …/cerberus/worktrees
	f.write(wt+"-abc123/s.jsonl", line("in", t0, 7, 0, 0, 0))
	f.write("-home-u-Github-Other/s.jsonl", line("out", t0, 1000, 0, 0, 0))
	f.write(wt+"extra/s.jsonl", line("sibling", t0, 1000, 0, 0, 0)) // "worktreesextra" is not inside
	if u := f.snap(); u.WeeklyTokens != 7 {
		t.Errorf("filtered = %+v", u)
	}
	if got := encodeProject("/home/u/.config/x_y"); got != "-home-u--config-x-y" {
		t.Errorf("encode = %q", got)
	}
}

func TestOldFilesNotRead(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true})
	p := f.write("-p/old.jsonl", line("x", f.now.Add(-time.Hour), 5, 0, 0, 0))
	old := f.now.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	u := f.snap()
	if u.WeeklyTokens != 0 {
		t.Errorf("stale file read: %+v", u)
	}
	if st := f.tr.plans[0].files[p]; st == nil || st.off != st.size {
		t.Errorf("stale file state = %+v", st)
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true})
	os.RemoveAll(f.root)
	if u := f.snap(); !strings.Contains(u.Err, "no Claude Code transcripts") {
		t.Errorf("missing dir err = %q", u.Err)
	}
	g := newFixture(t, config.SubscriptionConfig{Kind: "codex"})
	if u := g.snap(); !strings.Contains(u.Err, "unsupported") {
		t.Errorf("kind err = %q", u.Err)
	}
	if New(nil, nil).Snapshot() == nil {
		t.Error("nil config snapshot should be empty, not nil")
	}
}

func TestRunAndSetActive(t *testing.T) {
	f := newFixture(t, config.SubscriptionConfig{AllSessions: true})
	f.write("-p/s.jsonl", line("a", f.now.Add(-time.Minute), 3, 0, 0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.tr.Run(ctx); close(done) }()

	waitFor := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if s := f.tr.Snapshot(); s[0].WeeklyTokens == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("weekly tokens never reached %d: %+v", want, f.tr.Snapshot())
	}
	waitFor(3) // the initial scan
	if got := f.tr.interval(); got != idleInterval {
		t.Errorf("idle interval = %v", got)
	}
	f.append("-p/s.jsonl", line("b", f.now, 4, 0, 0, 0))
	f.tr.SetActive(true) // becoming active scans at once
	waitFor(7)
	if got := f.tr.interval(); got != activeInterval {
		t.Errorf("active interval = %v", got)
	}
	f.tr.SetActive(false)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
