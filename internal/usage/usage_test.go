package usage

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAddRecentAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "usage.jsonl")
	s, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		s.Add(Record{Time: t0.Add(time.Duration(i) * time.Second), Model: "m", PromptTokens: i})
	}
	got := s.Recent(10)
	if len(got) != 3 || got[0].PromptTokens != 4 || got[2].PromptTokens != 2 {
		t.Fatalf("recent = %+v", got)
	}
	if r := s.Recent(1); len(r) != 1 || r[0].PromptTokens != 4 {
		t.Fatalf("recent(1) = %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if n := countLines(t, path); n != 5 {
		t.Fatalf("file has %d lines, want 5", n)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}

	s2, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got = s2.Recent(0)
	if len(got) != 2 || got[0].PromptTokens != 4 || got[1].PromptTokens != 3 || !got[0].Time.Equal(t0.Add(4*time.Second)) {
		t.Fatalf("reloaded = %+v", got)
	}
	s2.Add(Record{Model: "m", PromptTokens: 9})
	if n := countLines(t, path); n != 6 {
		t.Fatalf("after append file has %d lines, want 6", n)
	}
}

func TestOpenSkipsBadLinesAndReadsTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	var b strings.Builder
	// Enough padding that the first records fall outside the 4 MiB tail.
	pad := strings.Repeat("x", 1000)
	for i := 0; int64(b.Len()) < tailBytes+200_000; i++ {
		line, _ := json.Marshal(Record{Model: "old", Error: pad, PromptTokens: i})
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString("not json\n\n")
	line, _ := json.Marshal(Record{Model: "last"})
	b.Write(line)
	b.WriteByte('\n')
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all := s.Recent(0)
	if all[0].Model != "last" {
		t.Fatalf("newest = %q", all[0].Model)
	}
	oldest := all[len(all)-1]
	if oldest.PromptTokens == 0 || int64(len(all)) > tailBytes/1000 {
		t.Fatalf("loaded %d records, oldest #%d: tail limit not applied", len(all), oldest.PromptTokens)
	}
}

func TestAggregate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "u.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	recs := []Record{
		{Time: now.Add(-48 * time.Hour), Model: "a", PromptTokens: 1000}, // too old
		{Time: now.Add(-time.Hour), Model: "a", PromptTokens: 10, CompletionTokens: 20, EnergyJ: 5, TokPerSec: 40, TokPerJ: 2},
		{Time: now.Add(-time.Minute), Model: "a", PromptTokens: 1, CompletionTokens: 2, EnergyJ: 1, TokPerSec: 60},
		{Time: now, Model: "a", Status: 500},
		{Time: now, Model: "b", CompletionTokens: 7, TokPerJ: 3},
	}
	for _, r := range recs {
		s.Add(r)
	}
	got := s.Aggregate(now.Add(-24 * time.Hour))
	want := []Agg{
		{Model: "a", Requests: 3, PromptTokens: 11, CompletionTokens: 22, EnergyJ: 6, AvgTokPerSec: 50, AvgTokPerJ: 2},
		{Model: "b", Requests: 1, CompletionTokens: 7, AvgTokPerJ: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		g := got[i]
		if g.Model != want[i].Model || g.Requests != want[i].Requests || g.PromptTokens != want[i].PromptTokens ||
			g.CompletionTokens != want[i].CompletionTokens || !near(g.EnergyJ, want[i].EnergyJ) ||
			!near(g.AvgTokPerSec, want[i].AvgTokPerSec) || !near(g.AvgTokPerJ, want[i].AvgTokPerJ) {
			t.Errorf("agg %d = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestConcurrentAdd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "u.jsonl")
	s, err := Open(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				s.Add(Record{Model: "m", Error: strings.Repeat("e", 300)})
				_ = s.Recent(5)
			}
		})
	}
	wg.Wait()
	s.Close()
	if n := countLines(t, path); n != 200 {
		t.Fatalf("lines = %d, want 200", n)
	}
	if len(s.Recent(0)) != 50 {
		t.Fatal("ring not capped")
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d: %v", n+1, err)
		}
		n++
	}
	return n
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAggregateCoversWindowBeyondRing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	s, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 5 {
		s.Add(Record{Time: now.Add(-time.Duration(i) * time.Minute), Model: "m", PromptTokens: 1, TokPerSec: 10})
	}
	s.Add(Record{Time: now.Add(-30 * time.Hour), Model: "m"}) // outside the window
	check := func(s *Store, when string) {
		t.Helper()
		agg := s.Aggregate(now.Add(-24 * time.Hour))
		if len(agg) != 1 || agg[0].Requests != 5 || agg[0].PromptTokens != 5 || agg[0].AvgTokPerSec != 10 {
			t.Fatalf("%s: agg = %+v, want 5 requests although only 3 records are kept", when, agg)
		}
	}
	check(s, "live")
	s.Close()
	s2, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	check(s2, "after reopen")
}

func TestSpendFromRingAndBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	now := time.Now()
	recs := []Record{
		{Time: now.Add(-70 * 24 * time.Hour), Model: "anthropic/x", Provider: "anthropic", CostUSD: 100}, // past bucketKeep
		{Time: now.Add(-40 * 24 * time.Hour), Model: "anthropic/x", Provider: "anthropic", CostUSD: 2},
		{Time: now.Add(-3 * time.Hour), Model: "openai/y", Provider: "openai", CostUSD: 0.5},
		{Time: now.Add(-2 * time.Hour), Model: "qwen", Provider: "llamacpp", Local: true},
		{Time: now.Add(-time.Hour), Model: "anthropic/x", Provider: "anthropic", CostUSD: 0.25, CachedTokens: 10},
	}
	s, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		s.Add(r)
	}
	// The ring holds everything: exact, including the 70-day-old record.
	if got := s.Spend("anthropic", now.Add(-90*24*time.Hour)); !near(got, 102.25) {
		t.Errorf("ring spend = %v", got)
	}
	if got := s.Spend("*", now.Add(-24*time.Hour)); !near(got, 0.75) {
		t.Errorf("total spend = %v", got)
	}
	s.Close()

	// Reopened with a tiny ring, spend comes from the reloaded hourly buckets,
	// which dropped the record older than 62 days.
	s2, err := Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Spend("anthropic", now.Add(-90*24*time.Hour)); !near(got, 2.25) {
		t.Errorf("bucket spend = %v", got)
	}
	by := s2.SpendByProvider(now.Add(-24 * time.Hour))
	if len(by) != 2 || !near(by["openai"], 0.5) || !near(by["anthropic"], 0.25) {
		t.Errorf("by provider = %v", by)
	}
	if got := s2.Spend("", now.Add(-24*time.Hour)); !near(got, 0.75) {
		t.Errorf("all spend = %v", got)
	}
	agg := s2.Aggregate(now.Add(-24 * time.Hour))
	var cost float64
	for _, a := range agg {
		cost += a.CostUSD
	}
	if !near(cost, 0.75) {
		t.Errorf("aggregate cost = %v (%+v)", cost, agg)
	}
	if r := s2.Recent(1); len(r) != 1 || r[0].Provider != "anthropic" || r[0].CachedTokens != 10 {
		t.Errorf("reloaded record = %+v", r)
	}
}

func TestOnAdd(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "u.jsonl"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got []Record
	s.OnAdd(func(r Record) {
		got = append(got, r)
		_ = s.Recent(1) // hooks run outside the lock
	})
	s.Add(Record{Time: time.Now(), Model: "m", SessionID: "x", EnergyJ: 5})
	if len(got) != 1 || got[0].SessionID != "x" {
		t.Fatalf("hook got %+v", got)
	}
}
