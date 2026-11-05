package usage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// In a half-hour zone, today's spend from the buckets must not include the
// half hour before local midnight.
func TestSpendInHalfHourZone(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+30*60)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, loc)
	s, err := Open(filepath.Join(t.TempDir(), "u.jsonl"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return now }
	midnight := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	s.Add(Record{Time: midnight.Add(-10 * time.Minute), Provider: "p", CostUSD: 5}) // yesterday 23:50
	s.Add(Record{Time: midnight.Add(10 * time.Minute), Provider: "p", CostUSD: 1})
	s.Add(Record{Time: now.Add(-time.Minute), Provider: "p", CostUSD: 2})
	s.Add(Record{Time: now, Provider: "p", CostUSD: 3}) // the ring (2) no longer covers midnight
	if got := s.Spend("*", midnight); math.Abs(got-6) > 1e-9 {
		t.Fatalf("today's spend = %v, want 6 (yesterday's 5 excluded)", got)
	}
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	if got := s.SpendByProvider(first)["p"]; math.Abs(got-6) > 1e-9 {
		t.Fatalf("month spend = %v, want 6", got)
	}
}

// After a restart with a file larger than tailBytes, the month's spend still
// counts records before the tail (but not those past bucketKeep).
func TestOpenReadsWholeBudgetWindow(t *testing.T) {
	old := tailBytes
	tailBytes = 64 << 10
	t.Cleanup(func() { tailBytes = old })
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	now := time.Now()
	var b strings.Builder
	pad := strings.Repeat("x", 300)
	write := func(r Record) {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	// 100 days back, one record per hour; only the last 62 days count.
	start := now.Add(-100 * 24 * time.Hour)
	n, inWindow := 0, 0
	for at := start; at.Before(now); at = at.Add(time.Hour) {
		write(Record{Time: at, Model: "m", Provider: "p", CostUSD: 1, Error: pad})
		n++
		if now.Sub(at) <= bucketKeep-time.Hour { // clear of the window edge
			inWindow++
		}
	}
	if int64(b.Len()) < 10*tailBytes {
		t.Fatalf("fixture too small: %d bytes", b.Len())
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	since := now.Add(-(bucketKeep - time.Hour))
	got := s.Spend("*", since)
	if math.Abs(got-float64(inWindow)) > 2 {
		t.Fatalf("spend since %v = %v, want about %d (of %d records)", since, got, inWindow, n)
	}
	if s.ringCovers(since) {
		t.Fatal("the ring must not claim to cover the whole window")
	}
}
