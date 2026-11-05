package inbox

import (
	"path/filepath"
	"testing"
)

func TestOwnerScoping(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "inbox.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ch, cancel := s.Subscribe()
	defer cancel()
	s.Add(Item{Title: "a1", Owner: "u-a"})
	a2 := s.Add(Item{Title: "a2", Owner: "u-a"})
	b1 := s.Add(Item{Title: "b1", Owner: "u-b"})
	s.Add(Item{Title: "sys"})
	for range 4 {
		<-ch
	}
	isA := func(it Item) bool { return it.Owner == "u-a" }
	if n := len(s.ListWhere(isA, false, 0)); n != 2 {
		t.Fatalf("a's items %d", n)
	}
	if s.UnreadWhere(isA) != 2 || s.Unread() != 4 {
		t.Fatal("unread counts")
	}
	if ok, _ := s.DeleteWhere(b1.ID, isA); ok {
		t.Fatal("a deleted b's item")
	}
	n, _ := s.MarkReadWhere(nil, true, isA)
	if n != 2 || s.UnreadWhere(func(it Item) bool { return it.Owner == "u-b" }) != 1 {
		t.Fatalf("read all for a: %d", n)
	}
	ev := <-ch
	if ev.Type != "read" || ev.Owner != "u-a" {
		t.Fatalf("event %+v", ev)
	}
	if ok, _ := s.DeleteWhere(a2.ID, isA); !ok {
		t.Fatal("a cannot delete own item")
	}
	if ev := <-ch; ev.Owner != "u-a" || ev.Type != "deleted" {
		t.Fatalf("delete event %+v", ev)
	}
	// Owners survive a reload.
	s2, err := Open(s.path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if it := s2.ListWhere(func(it Item) bool { return it.Owner == "u-b" }, false, 0); len(it) != 1 || it[0].Title != "b1" {
		t.Fatalf("reloaded %+v", it)
	}
}
