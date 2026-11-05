package session

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestQueueCompare(t *testing.T) {
	t0 := time.Now()
	infos := []Info{
		{ID: "a", Created: t0},
		{ID: "b", Created: t0.Add(1 * time.Second), Priority: 5},
		{ID: "c", Created: t0.Add(2 * time.Second), Priority: -1, Pinned: true},
		{ID: "d", Created: t0.Add(3 * time.Second), Priority: 5},
		{ID: "e", Created: t0.Add(4 * time.Second), Priority: 10, Pinned: true},
	}
	slices.SortStableFunc(infos, queueCompare)
	var got []string
	for _, i := range infos {
		got = append(got, i.ID)
	}
	if want := "e c b d a"; strings.Join(got, " ") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
}

func TestNormalizeMeta(t *testing.T) {
	tags, err := normalizeTags([]string{" Web ", "web", "", "ci-1"})
	if err != nil || !slices.Equal(tags, []string{"web", "ci-1"}) {
		t.Fatalf("tags %v %v", tags, err)
	}
	for _, bad := range [][]string{{"has space"}, {"under_score"}, {strings.Repeat("x", 25)}, {"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		if _, err := normalizeTags(bad); err == nil {
			t.Errorf("tags %v accepted", bad)
		}
	}
	if _, err := normalizeName("two\nlines"); err == nil {
		t.Error("multi-line name accepted")
	}
	if _, err := normalizeName(strings.Repeat("n", 81)); err == nil {
		t.Error("long name accepted")
	}
	if err := validatePriority(11); err == nil {
		t.Error("priority 11 accepted")
	}
	for _, l := range []Limits{{MemoryMax: "4G"}, {MemoryMax: "512M"}, {MemoryMax: "50%"}, {MemoryMax: "infinity"}, {MaxMinutes: 30, MaxOutputMiB: 64}} {
		if err := validateLimits(l); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	for _, l := range []Limits{{MemoryMax: "4 G"}, {MemoryMax: "-1"}, {MemoryMax: "1G; rm"}, {MaxMinutes: -1}, {MaxOutputMiB: -5}} {
		if err := validateLimits(l); err == nil {
			t.Errorf("%+v accepted", l)
		}
	}
}

func TestScopeFailureDetection(t *testing.T) {
	if !looksLikeScopeFailure([]byte("Failed to connect to bus: No medium found\r\n")) {
		t.Error("bus failure not recognised")
	}
	if !looksLikeScopeFailure([]byte("Failed to create bus connection: No such file or directory")) {
		t.Error("bus connection failure not recognised")
	}
	if looksLikeScopeFailure([]byte("hello from the agent")) {
		t.Error("ordinary output taken for a scope failure")
	}
}

func TestActivityHelpers(t *testing.T) {
	if l := lastNonEmptyLine([]byte("first\nsecond\n   \n\n")); string(l) != "second" {
		t.Fatalf("last line %q", l)
	}
	if l := lastNonEmptyLine([]byte("\n \n")); l != nil {
		t.Fatalf("blank text gave %q", l)
	}
	var r redactor
	r.set([]string{"sk-secret-0123"})
	if got := activityText([]byte("key sk-secret-0123 ok"), &r); got != "key "+redactMask+" ok" {
		t.Fatalf("masked %q", got)
	}
	long := strings.Repeat("é", 300)
	if got := activityText([]byte(long), &redactor{}); len([]rune(got)) != activityRunes {
		t.Fatalf("clip gave %d runes", len([]rune(got)))
	}
}

func TestFeedRingRotationAndRedaction(t *testing.T) {
	old := feedRotateBytes
	feedRotateBytes = 64 << 10
	t.Cleanup(func() { feedRotateBytes = old })
	const secret = "sk-feed-secret-42"
	path := filepath.Join(t.TempDir(), feedName)
	fd := newFeed(path, func() []string { return []string{secret} })
	for i := range feedRing + 150 {
		fd.record(FeedItem{SessionID: "s1", Name: "n", Kind: FeedInput, Text: fmt.Sprintf("%d typed %s and more text that is long enough to be clipped at eighty characters for sure", i, secret)})
		if i%100 == 0 {
			fd.drain()
		}
	}
	items := fd.items(time.Time{}, 0)
	if len(items) != feedRing {
		t.Fatalf("ring holds %d, want %d", len(items), feedRing)
	}
	for _, it := range items {
		if strings.Contains(it.Text, secret) || strings.Contains(it.Text, "sk-feed") {
			t.Fatalf("secret in feed: %q", it.Text)
		}
		if n := len([]rune(it.Text)); n > inputClip {
			t.Fatalf("input not clipped: %d runes", n)
		}
	}
	if got := fd.items(time.Time{}, 10); len(got) != 10 || got[9] != items[len(items)-1] {
		t.Fatalf("limit gave %d items", len(got))
	}
	if got := fd.items(items[len(items)-3].Time, 0); len(got) > 2 {
		t.Fatalf("since gave %d items", len(got))
	}
	fd.close()
	if st, err := os.Stat(path); err != nil || st.Size() > feedRotateBytes {
		t.Fatalf("feed.jsonl not rotated: %v %v", st.Size(), err)
	}
	raw, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("secret written to disk")
	}
	// A restart reads the newest items back.
	again := newFeed(path, nil)
	if len(again.ring) == 0 || again.ring[len(again.ring)-1].Text != items[len(items)-1].Text {
		t.Fatalf("reload got %d items", len(again.ring))
	}
}

func TestFeedSubscribe(t *testing.T) {
	fd := newFeed("", nil)
	ch, unsub := fd.subscribe()
	defer unsub()
	fd.record(FeedItem{SessionID: "x", Kind: FeedState, Text: "running"})
	fd.drain()
	select {
	case it := <-ch:
		if it.SessionID != "x" || it.Text != "running" {
			t.Fatalf("item %+v", it)
		}
	case <-time.After(time.Second):
		t.Fatal("no item")
	}
}
