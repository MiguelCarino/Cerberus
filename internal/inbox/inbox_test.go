package inbox

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func countLines(t *testing.T, p string) int {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
	}
	return n
}

func TestAddListAndDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "data", "inbox.jsonl")
	s, err := Open(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Add(Item{Kind: "session", Level: "critical", Title: "Session error: x", Link: "#/s/x"})
	b := s.Add(Item{Title: "plain"})
	if a.ID == "" || a.Time.IsZero() || a.Level != LevelCrit || a.Read {
		t.Fatalf("a = %+v", a)
	}
	if b.Kind != "system" || b.Level != LevelInfo {
		t.Fatalf("b = %+v", b)
	}
	got := s.List(false, 0)
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("not newest first: %+v", got)
	}
	if got := s.List(false, 1); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("limit: %+v", got)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	long := s.Add(Item{Title: strings.Repeat("é", 1000)})
	if r := []rune(long.Title); len(r) != maxTitle {
		t.Fatalf("title not clipped: %d runes", len(r))
	}
}

func TestRingAndRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inbox.jsonl")
	s, err := Open(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 23 {
		s.Add(Item{Title: fmt.Sprint("n", i)})
		if n := countLines(t, p); n >= 10 {
			t.Fatalf("file not rotated after %d adds: %d lines", i+1, n)
		}
	}
	got := s.List(false, 0)
	if len(got) != 5 || got[0].Title != "n22" || got[4].Title != "n18" {
		t.Fatalf("ring = %+v", got)
	}
	s.Close()

	// Reopen: the ring is rebuilt from the file and the file is compacted.
	s2, err := Open(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got = s2.List(false, 0)
	if len(got) != 5 || got[0].Title != "n22" || got[4].Title != "n18" {
		t.Fatalf("reloaded ring = %+v", got)
	}
	if n := countLines(t, p); n != 5 {
		t.Fatalf("not compacted on open: %d lines", n)
	}
}

func TestReadStateAndDeletePersist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inbox.jsonl")
	s, err := Open(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Add(Item{Title: "a"})
	b := s.Add(Item{Title: "b"})
	c := s.Add(Item{Title: "c"})
	if s.Unread() != 3 {
		t.Fatal("unread")
	}
	if n, err := s.MarkRead([]string{a.ID, "nope"}, false); n != 1 || err != nil {
		t.Fatalf("mark = %d %v", n, err)
	}
	if n, _ := s.MarkRead([]string{a.ID}, false); n != 0 {
		t.Fatal("re-marking counted")
	}
	if u := s.List(true, 0); len(u) != 2 || u[0].ID != c.ID || u[1].ID != b.ID {
		t.Fatalf("unread list = %+v", u)
	}
	if ok, err := s.Delete(b.ID); !ok || err != nil {
		t.Fatalf("delete = %v %v", ok, err)
	}
	if ok, _ := s.Delete(b.ID); ok {
		t.Fatal("deleted twice")
	}
	s.Close()

	s2, err := Open(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	all := s2.List(false, 0)
	if len(all) != 2 || all[0].ID != c.ID || all[0].Read || all[1].ID != a.ID || !all[1].Read {
		t.Fatalf("reloaded = %+v", all)
	}
	if n, _ := s2.MarkRead(nil, true); n != 1 || s2.Unread() != 0 {
		t.Fatalf("mark all = %d, unread %d", n, s2.Unread())
	}
	s2.Close()
}

func TestMalformedAndDuplicateLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inbox.jsonl")
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	content := `{"ID":"x","Time":"` + t0.Format(time.RFC3339) + `","Title":"first"}
not json
{"Title":"no id"}

{"ID":"x","Time":"` + t0.Format(time.RFC3339) + `","Title":"first","Read":true}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := s.List(false, 0)
	if len(got) != 1 || !got[0].Read {
		t.Fatalf("got %+v", got)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode not tightened: %v", st.Mode().Perm())
	}
}

func TestSubscribe(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "inbox.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ch, cancel := s.Subscribe()
	a := s.Add(Item{Title: "a", Link: "#/chat/c1", Kind: "chat"})
	ev := <-ch
	if ev.Type != "added" || ev.Item == nil || ev.Item.ID != a.ID || ev.Item.Link != "#/chat/c1" || ev.Unread != 1 {
		t.Fatalf("added ev = %+v", ev)
	}
	s.MarkRead([]string{a.ID}, false)
	if ev = <-ch; ev.Type != "read" || len(ev.IDs) != 1 || ev.Unread != 0 {
		t.Fatalf("read ev = %+v", ev)
	}
	s.Delete(a.ID)
	if ev = <-ch; ev.Type != "deleted" || ev.IDs[0] != a.ID {
		t.Fatalf("deleted ev = %+v", ev)
	}
	cancel()
	cancel() // idempotent
	if _, ok := <-ch; ok {
		t.Fatal("channel open after cancel")
	}

	// A subscriber that never reads is dropped, not blocking Add.
	slow, cancelSlow := s.Subscribe()
	defer cancelSlow()
	for range subBuffer + 5 {
		s.Add(Item{Title: "flood"})
	}
	n := 0
	for range slow {
		n++
	}
	if n != subBuffer {
		t.Fatalf("slow subscriber got %d before close", n)
	}
}

func TestCloseClosesSubscribers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "inbox.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := s.Subscribe()
	s.Close()
	if _, ok := <-ch; ok {
		t.Fatal("open after Close")
	}
	s.Add(Item{Title: "memory only"})
	if len(s.List(false, 0)) != 1 {
		t.Fatal("memory add after close")
	}
	late, _ := s.Subscribe()
	if _, ok := <-late; ok {
		t.Fatal("subscribe after close must be closed")
	}
}
