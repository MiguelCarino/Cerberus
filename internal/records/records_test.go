package records

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, text string) *Store {
	t.Helper()
	s, err := Open(Config{Enabled: true, Text: text, Dir: t.TempDir()}, func() []string { return []string{"hunter2-secret"} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOffMeansNil(t *testing.T) {
	s, err := Open(Config{Enabled: false, Dir: t.TempDir()}, nil)
	if s != nil || err != nil {
		t.Fatalf("%v %v", s, err)
	}
	s.Add(Line{}) // a nil store records nothing and does not panic
	if l, _ := s.List(Filter{}); l != nil {
		t.Error("nil store listed lines")
	}
}

func TestTextLevels(t *testing.T) {
	full := &Text{System: "sys", Messages: "user: hi", Reply: "hello", Reasoning: "hmm"}
	for _, c := range []struct {
		level string
		want  *Text
	}{
		{TextNone, nil},
		{TextUser, &Text{Messages: "user: hi", Reply: "hello"}},
		{TextAll, full},
	} {
		s := open(t, c.level)
		s.Add(Line{Time: time.Now(), Model: "m", Text: full})
		ls, err := s.List(Filter{})
		if err != nil || len(ls) != 1 {
			t.Fatalf("%s: %v %v", c.level, ls, err)
		}
		if ls[0].Text != nil {
			t.Errorf("%s: List returned text", c.level)
		}
		parts := strings.Split(ls[0].Ref, "/")
		off, _ := strconv.ParseInt(parts[2], 10, 64)
		got, err := s.Get(parts[0], parts[1], off)
		if err != nil {
			t.Fatal(err)
		}
		if (got.Text == nil) != (c.want == nil) || (got.Text != nil && *got.Text != *c.want) {
			t.Errorf("%s: text %+v want %+v", c.level, got.Text, c.want)
		}
		raw, _ := os.ReadFile(firstFile(t, s.Dir()))
		if c.level == TextNone && strings.Contains(string(raw), "hello") {
			t.Errorf("text written at level none: %s", raw)
		}
	}
}

func firstFile(t *testing.T, dir string) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "turns", "*", "*.jsonl"))
	if len(m) == 0 {
		t.Fatal("no record file")
	}
	return m[0]
}

func TestRedaction(t *testing.T) {
	s := open(t, TextAll)
	s.Add(Line{Time: time.Now(), Text: &Text{Messages: "my key is hunter2-secret and sk-abcdefghijklmnopqrstu, gh token ghp_abcdefghijklmnopqrstuvwxyz"}})
	raw, _ := os.ReadFile(firstFile(t, s.Dir()))
	for _, leak := range []string{"hunter2-secret", "sk-abcdefghijklmnopqrstu", "ghp_abcdefghij"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%s written: %s", leak, raw)
		}
	}
	if !strings.Contains(string(raw), "[redacted]") {
		t.Error("no mask")
	}
}

func TestFilesArePrivate(t *testing.T) {
	s := open(t, TextNone)
	s.Add(Line{Time: time.Now()})
	fi, err := os.Stat(firstFile(t, s.Dir()))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

func TestRotationAndRetention(t *testing.T) {
	old := fileCap
	fileCap = 200
	t.Cleanup(func() { fileCap = old })
	s := open(t, TextNone)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ms := int64(0)
	s.now = func() time.Time { ms++; return now.Add(time.Duration(ms) * time.Millisecond) }
	for i := 0; i < 5; i++ {
		s.Add(Line{Time: now, Model: strings.Repeat("m", 80)})
	}
	files, _ := filepath.Glob(filepath.Join(s.Dir(), "turns", "2026-11-05", "*.jsonl"))
	if len(files) < 2 {
		t.Errorf("no size rotation: %v", files)
	}
	// An old day is swept when a new day starts.
	oldDay := filepath.Join(s.Dir(), "turns", "2026-10-01")
	_ = os.MkdirAll(oldDay, 0o700)
	_ = os.WriteFile(filepath.Join(oldDay, "1-1.jsonl"), []byte("{}\n"), 0o600)
	s.Add(Line{Time: now.AddDate(0, 0, 1)})
	if _, err := os.Stat(oldDay); !os.IsNotExist(err) {
		t.Error("a day past retention was kept")
	}
	if days := s.Days(); len(days) != 2 || days[0] != "2026-11-06" {
		t.Errorf("days %v", days)
	}
	if n, _ := s.DeleteBefore("2026-11-06"); n != 1 {
		t.Errorf("deleted %d", n)
	}
}

func TestListFilters(t *testing.T) {
	s := open(t, TextNone)
	now := time.Now()
	s.Add(Line{Time: now, Session: "a", Client: "codex"})
	s.Add(Line{Time: now.Add(time.Second), Session: "b", Client: "claude-code"})
	ls, _ := s.List(Filter{Client: "codex"})
	if len(ls) != 1 || ls[0].Session != "a" {
		t.Errorf("client filter %v", ls)
	}
	ls, _ = s.List(Filter{})
	if len(ls) != 2 || ls[0].Session != "b" {
		t.Errorf("newest first %v", ls)
	}
	if _, err := s.Get("../../etc", "passwd", 0); err == nil {
		t.Error("path escape accepted")
	}
	if _, err := s.List(Filter{Day: "../x"}); err == nil {
		t.Error("bad day accepted")
	}
}

func TestClientFamily(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.0.14 (external, cli)": "claude-code",
		"codex_cli_rs/0.46.0 (Fedora 44)":   "codex",
		"opencode/0.15":                     "opencode",
		"litellm/1.0":                       "aider",
		"Anthropic/Python 0.70":             "anthropic-sdk",
		"OpenAI/Python 1.99":                "openai-sdk",
		"curl/8.9":                          "curl",
		"":                                  "other",
	} {
		if got := ClientFamily("", ua); got != want {
			t.Errorf("%q → %q want %q", ua, got, want)
		}
	}
	if ClientFamily("cerberus-agent", "curl/8") != "cerberus-agent" {
		t.Error("X-Cerberus-Client should win")
	}
}

func TestSessionLines(t *testing.T) {
	s := open(t, TextUser)
	base := time.Date(2026, 11, 3, 23, 59, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	// Out of order within a day, across two days, mixed with another session.
	s.Add(Line{Time: base.Add(30 * time.Second), Session: "s-1", Model: "b", Text: &Text{Reply: "second"}})
	s.Add(Line{Time: base.Add(10 * time.Second), Session: "s-1", Model: "a", Text: &Text{Reply: "first"}})
	s.Add(Line{Time: base.Add(20 * time.Second), Session: "s-10", Model: "x"})
	s.now = func() time.Time { return base.Add(2 * time.Minute) }
	s.Add(Line{Time: base.Add(2 * time.Minute), Session: "s-1", Model: "c"})

	got, more, err := s.SessionLines("s-1", "", 0)
	if err != nil || more || len(got) != 3 {
		t.Fatalf("%d lines, more %v, %v", len(got), more, err)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Line.Model != want || got[i].Line.Session != "s-1" {
			t.Fatalf("line %d: %+v", i, got[i].Line)
		}
		if b := got[i].Bytes; len(b) == 0 || b[len(b)-1] != '\n' || !strings.Contains(string(b), `"model":"`+want+`"`) {
			t.Fatalf("raw %d: %q", i, b)
		}
	}
	if got[0].Line.Text == nil || got[0].Line.Text.Reply != "first" {
		t.Fatalf("text not kept: %+v", got[0].Line.Text)
	}
	if got, _, _ := s.SessionLines("s-1", "2026-11-04", 0); len(got) != 1 || got[0].Line.Model != "c" {
		t.Fatalf("from a day: %+v", got)
	}
	if got, more, _ := s.SessionLines("s-1", "", 2); len(got) != 2 || !more || got[1].Line.Model != "b" {
		t.Fatalf("limit: %d %v", len(got), more)
	}
	if _, _, err := s.SessionLines("s-1", "nope", 0); err == nil {
		t.Fatal("bad day accepted")
	}
	if d := s.OldestDay(); d != "2026-11-03" {
		t.Fatalf("oldest %q", d)
	}
	if s.RetainDays() != 14 {
		t.Fatalf("retain %d", s.RetainDays())
	}
	var nilStore *Store
	if l, _, _ := nilStore.SessionLines("s-1", "", 0); l != nil || nilStore.OldestDay() != "" {
		t.Fatal("nil store")
	}
}
