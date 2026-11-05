package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestByUser(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.jsonl"), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.Add(Record{Time: now, Model: "m", User: "u-a", CostUSD: 1, PromptTokens: 10, CompletionTokens: 5})
	s.Add(Record{Time: now, Model: "m", User: "u-a", CostUSD: 0.5, PromptTokens: 1})
	s.Add(Record{Time: now, Model: "m", User: "u-b", CostUSD: 2})
	s.Add(Record{Time: now, Model: "m", CompletionTokens: 7}) // before users
	got := s.ByUser(now.Add(-time.Hour))
	if a := got["u-a"]; a.CostUSD != 1.5 || a.Tokens != 16 {
		t.Fatalf("a %+v", a)
	}
	if got["u-b"].CostUSD != 2 || got[""].Tokens != 7 {
		t.Fatalf("got %+v", got)
	}
	// Past the ring (keep 3), the buckets still have it.
	s.Add(Record{Time: now, Model: "m", User: "u-c", PromptTokens: 1})
	if got := s.ByUser(now.Add(-time.Hour)); got["u-a"].CostUSD != 1.5 {
		t.Fatalf("from buckets %+v", got)
	}
	// The user field round-trips through the file.
	s2, err := Open(filepath.Join(filepath.Dir(s.f.Name()), "usage.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.ByUser(now.Add(-time.Hour))["u-b"].CostUSD != 2 {
		t.Fatal("not reloaded")
	}
}
