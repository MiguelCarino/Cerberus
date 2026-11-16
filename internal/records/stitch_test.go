package records

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSameDayCapTrimsClosedFiles(t *testing.T) {
	old := fileCap
	fileCap = 64 << 10
	t.Cleanup(func() { fileCap = old })
	s, err := Open(Config{Enabled: true, Text: TextNone, Dir: t.TempDir(), MaxMiB: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ms := int64(0)
	s.now = func() time.Time { ms++; return now.Add(time.Duration(ms) * time.Millisecond) }
	big := strings.Repeat("m", 1000)
	for i := 0; i < 3000; i++ { // about 3 MiB in one day, cap 1 MiB
		s.Add(Line{Time: now, Model: big})
	}
	day := filepath.Join(s.Dir(), "turns", "2026-11-05")
	if n := dirSize(day); n > 1<<20+fileCap {
		t.Errorf("the day holds %d bytes, cap 1 MiB", n)
	}
	if _, err := os.Stat(s.name); err != nil {
		t.Errorf("the open file was removed: %v", err)
	}
	// The newest lines are kept: the open file is the last one written.
	ls, _ := s.List(Filter{Limit: 5000})
	if len(ls) == 0 || len(ls) >= 3000 {
		t.Errorf("%d lines kept", len(ls))
	}
	if st := s.Stats(); st.Written != 3000 || st.Dropped != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestSweepOnRotationKeepsOtherDaysUnderCap(t *testing.T) {
	old := fileCap
	fileCap = 64 << 10
	t.Cleanup(func() { fileCap = old })
	dir := t.TempDir()
	// Yesterday already holds 900 KiB.
	y := filepath.Join(dir, "turns", "2026-11-04")
	_ = os.MkdirAll(y, 0o700)
	_ = os.WriteFile(filepath.Join(y, "1-1.jsonl"), make([]byte, 900<<10), 0o600)
	s, err := Open(Config{Enabled: true, Dir: dir, MaxMiB: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ { // about 400 KiB today: the total passes the cap mid-day
		s.Add(Line{Time: now, Model: strings.Repeat("m", 1000)})
	}
	if _, err := os.Stat(y); !os.IsNotExist(err) {
		t.Error("an older day was kept past the cap within the day")
	}
}

func TestRedactSortsOnce(t *testing.T) {
	got := longestFirst([]string{"short", "abcdef", "abcdefghij"})
	if len(got) != 2 || got[0] != "abcdefghij" || got[1] != "abcdef" {
		t.Errorf("longestFirst %v", got)
	}
	if r := redact("key abcdefghij and abcdef", got); r != "key [redacted] and [redacted]" {
		t.Errorf("redact %q", r)
	}
}

func TestStitch(t *testing.T) {
	s := open(t, TextUser)
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	// One conversation over three requests, plus another client's line.
	s.Add(Line{Time: at(1), Session: "s1", Client: "opencode", Messages: 2,
		Text: &Text{Messages: "user: fix the bug", Reply: "Looking"}})
	s.Add(Line{Time: at(2), Session: "s1", Client: "opencode", Messages: 4, Prefix: 3,
		Text: &Text{Messages: "user: it is in main.go", Reply: "Fixed"}})
	s.Add(Line{Time: at(3), Session: "s1", Client: "other", Messages: 4, Prefix: 3,
		Text: &Text{Messages: "user: not this one", Reply: "x"}})
	last := Line{Time: at(4), Session: "s1", Client: "opencode", Messages: 6, Prefix: 5,
		Text: &Text{Messages: "user: thanks", Reply: "ok"}}
	s.Add(last)

	st := s.Stitch(last)
	want := "user: fix the bug\nassistant: Looking\nuser: it is in main.go\nassistant: Fixed\nuser: thanks"
	if st.Text.Messages != want || st.Earlier != 2 || st.Missing {
		t.Errorf("stitched %d %v:\n%s", st.Earlier, st.Missing, st.Text.Messages)
	}
	if last.Text.Messages != "user: thanks" {
		t.Error("Stitch changed the line it was given")
	}

	// A line whose earlier turns are not in the records says so.
	lone := Line{Time: at(5), Session: "s9", Client: "opencode", Messages: 9, Prefix: 8, Text: &Text{Messages: "user: hm"}}
	if st := s.Stitch(lone); st.Earlier != 0 || !st.Missing || st.Text.Messages != "user: hm" {
		t.Errorf("lone %+v", st)
	}
	// A gap in the counts is marked.
	gap := Line{Time: at(6), Session: "s1", Client: "opencode", Messages: 12, Prefix: 9, Text: &Text{Messages: "user: later"}}
	if st := s.Stitch(gap); !strings.Contains(st.Text.Messages, "[2 messages not recorded]") || !strings.HasSuffix(st.Text.Messages, "user: later") {
		t.Errorf("gap:\n%s", st.Text.Messages)
	}
	// A whole-conversation line (prefix 0) is left as it is.
	if st := s.Stitch(Line{Time: at(7), Text: &Text{Messages: "user: a"}}); st.Earlier != 0 || st.Missing {
		t.Errorf("prefix 0 %+v", st)
	}
}
