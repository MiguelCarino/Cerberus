package api

import (
	"testing"
	"time"

	"carino.systems/cerberus/internal/usage"
)

// The leaderboard shows a regular user only their own sessions (and their
// rows' spend), a viewer every session but no chats or direct traffic, and
// an admin everything.
func TestLeaderboardOwnerScoped(t *testing.T) {
	e := newUsersEnv(t)
	e.ss.add("s-bob", e.u["bob"].ID)
	e.ss.add("s-cy", e.u["cy"].ID)
	e.ss.add("s-ana", e.u["ana"].ID)
	now := time.Now()
	for _, id := range []string{"s-bob", "s-cy", "s-ana", ""} {
		e.used.Add(usage.Record{Time: now.Add(-time.Minute), Model: "m-" + id, SessionID: id, CostUSD: 1, PromptTokens: 10})
	}
	e.srv.board = newBoard(e.used, e.ss.List, nil)
	keys := func(who, by string) map[string]bool {
		t.Helper()
		w := e.call(who, "GET", "/api/leaderboard?period=today&by="+by, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", who, w.Code, w.Body)
		}
		out := map[string]bool{}
		for _, r := range decodeAs[usage.Board](t, w).Rows {
			out[r.Key] = true
		}
		return out
	}
	if k := keys("bob", "session"); len(k) != 1 || !k["s-bob"] {
		t.Fatalf("bob sees %v", k)
	}
	if k := keys("bob", "model"); len(k) != 1 || !k["m-s-bob"] {
		t.Fatalf("bob's models %v", k)
	}
	if k := keys("vic", "session"); len(k) != 3 || k[""] {
		t.Fatalf("viewer sees %v", k)
	}
	if k := keys("ana", "session"); len(k) != 4 {
		t.Fatalf("admin sees %v", k)
	}
}
