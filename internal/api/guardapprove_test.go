package api

import (
	"strings"
	"testing"

	"carino.systems/cerberus/internal/inbox"
)

// TestGuardApproveOnce: the bell's "Approve once" types one approval into
// the held session, marks the item read and is audited; a second press, a
// wrong session and other kinds of item are refused (web-api-ops-11).
func TestGuardApproveOnce(t *testing.T) {
	env := newSafetyEnv(t, nil)
	ib := newInbox(t)
	env.srv.inbox = ib
	held := ib.Add(inbox.Item{Kind: "guard", Level: "warn", Title: "Agent guard held a tool call", Link: "#/s/s1",
		Held: []string{"bash", "write\x1b[2J\nfile"}})
	plain := ib.Add(inbox.Item{Kind: "session", Title: "done", Link: "#/s/s1"})
	other := ib.Add(inbox.Item{Kind: "guard", Title: "held", Link: "#/s/s2"})

	w := env.do("POST", "/api/sessions/s1/guard/approve", `{"item":"`+held.ID+`"}`, adminTok)
	if w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	want := "input:Go ahead: run bash, write[2Jfile this once.\r"
	if got := env.sessions.lastCall(); got != want {
		t.Fatalf("typed %q, want %q", got, want)
	}
	if it := ib.List(false, 0); !itemRead(it, held.ID) {
		t.Fatal("the held item is still unread")
	}

	// A second press is refused and types nothing.
	n := len(env.sessions.calls)
	if w := env.do("POST", "/api/sessions/s1/guard/approve", `{"item":"`+held.ID+`"}`, adminTok); w.Code != 409 {
		t.Fatalf("second press: %d", w.Code)
	}
	for name, c := range map[string]struct {
		url, body string
		code      int
	}{
		"not a guard item": {"/api/sessions/s1/guard/approve", `{"item":"` + plain.ID + `"}`, 404},
		"another session":  {"/api/sessions/s1/guard/approve", `{"item":"` + other.ID + `"}`, 404},
		"unknown item":     {"/api/sessions/s1/guard/approve", `{"item":"nope"}`, 404},
		"no item":          {"/api/sessions/s1/guard/approve", `{}`, 400},
		"unknown session":  {"/api/sessions/zz/guard/approve", `{"item":"` + held.ID + `"}`, 404},
		"cookie, no csrf":  {"/api/sessions/s1/guard/approve", `{"item":"` + other.ID + `"}`, 403},
	} {
		var w = env.do("POST", c.url, c.body, adminTok)
		if name == "cookie, no csrf" {
			w = env.do("POST", c.url, c.body, withCookie(testToken))
		}
		if w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if len(env.sessions.calls) != n {
		t.Fatalf("a refused approve typed: %v", env.sessions.calls[n:])
	}

	found := false
	for _, r := range env.records(t) {
		if r.Route == "POST /api/sessions/{id}/guard/approve" && strings.Contains(r.Detail, "guard approve once: Go ahead: run bash") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit record: %+v", env.records(t))
	}
}

func itemRead(items []inbox.Item, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return it.Read
		}
	}
	return false
}

func TestGuardApproveText(t *testing.T) {
	if got := guardApproveText(nil); !strings.Contains(got, "this once") {
		t.Fatal(got)
	}
	if got := guardApproveText([]string{strings.Repeat("a", 200)}); len(got) > 100 {
		t.Fatalf("long name not clipped: %d", len(got))
	}
	if got := guardApproveText([]string{"x\ry z"}); strings.ContainsAny(got, "\r\n ") {
		t.Fatalf("control characters kept: %q", got)
	}
}

// A non-admin owner answers the hold in her own session: the gateway
// stores it under her (web-api-1), and opening the item first, or "Mark
// all read", does not take the button away (web-api-3).
func TestGuardApproveByOwnerAfterReading(t *testing.T) {
	e := newUsersEnv(t)
	bob, cy := e.u["bob"], e.u["cy"]
	e.ss.add("s1", bob.ID) // the fake types into s1 only
	held := e.ib.Add(inbox.Item{Kind: "guard", Level: "warn", Title: "Agent guard held a tool call", Link: "#/s/s1",
		Held: []string{"bash"}, Owner: bob.ID})
	// bob sees it in his bell.
	if w := e.call("bob", "GET", "/api/inbox", ""); w.Code != 200 || !strings.Contains(w.Body.String(), held.ID) {
		t.Fatalf("bob's inbox: %d %s", w.Code, w.Body)
	}
	// He opens it (read), then marks everything read.
	if w := e.call("bob", "POST", "/api/inbox/read", `{"ids":["`+held.ID+`"]}`); w.Code != 200 {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	if w := e.call("bob", "POST", "/api/inbox/read", `{"all":true}`); w.Code != 200 {
		t.Fatalf("read all: %d %s", w.Code, w.Body)
	}
	// cy cannot answer bob's hold.
	if w := e.call("cy", "POST", "/api/sessions/s1/guard/approve", `{"item":"`+held.ID+`"}`); w.Code != 404 {
		t.Fatalf("cy approved bob's hold: %d %s", w.Code, w.Body)
	}
	_ = cy
	if w := e.call("bob", "POST", "/api/sessions/s1/guard/approve", `{"item":"`+held.ID+`"}`); w.Code != 200 {
		t.Fatalf("bob approve after reading: %d %s", w.Code, w.Body)
	}
	if it, _ := e.ib.Get(held.ID); !it.Answered || !it.Read {
		t.Fatalf("item %+v not answered", it)
	}
	if w := e.call("bob", "POST", "/api/sessions/s1/guard/approve", `{"item":"`+held.ID+`"}`); w.Code != 409 {
		t.Fatalf("second press: %d %s", w.Code, w.Body)
	}
}
