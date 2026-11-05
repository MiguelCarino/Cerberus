package api

import (
	"encoding/json"
	"testing"
	"time"

	"carino.systems/cerberus/internal/usage"
)

// TestEventsUsageHints: gateway records charged to a session reach the
// event stream as "usage" {session}, coalesced to one per session; records
// without a session id never do.
func TestEventsUsageHints(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.usageFeed = newUsageFeed()
	sse, cancel := openSSE(t, env, "/api/events")
	defer cancel()
	for _, id := range []string{"s1", "", "s1", "s2", "s1"} {
		env.srv.usageFeed.publish(usage.Record{Time: time.Now(), SessionID: id})
	}
	got := map[string]int{}
	for len(got) < 2 {
		ev, data := sse.next(t)
		if ev == "" {
			continue // ping
		}
		var p struct{ Session string }
		if ev != "usage" || json.Unmarshal([]byte(data), &p) != nil {
			t.Fatalf("frame %q %q", ev, data)
		}
		got[p.Session]++
	}
	if got["s1"] != 1 || got["s2"] != 1 {
		t.Fatalf("hints %v, want one per session", got)
	}
}

// TestUsageFeedCancel: a cancelled subscription gets nothing more, and a
// full subscriber never blocks the publisher.
func TestUsageFeedCancel(t *testing.T) {
	f := newUsageFeed()
	ch, cancel := f.Subscribe()
	for i := 0; i < 100; i++ { // more than the buffer: must not block
		f.publish(usage.Record{SessionID: "s1"})
	}
	if len(ch) != cap(ch) {
		t.Fatalf("buffered %d of %d", len(ch), cap(ch))
	}
	cancel()
	cancel() // twice is fine
	if len(f.subs) != 0 {
		t.Fatal("still subscribed")
	}
}
