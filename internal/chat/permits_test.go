package chat

import (
	"errors"
	"path/filepath"
	"testing"

	"carino.systems/cerberus/internal/permits"
)

// TestAlwaysAllow: "Always allow" saves the offered rule for the chat's
// owner and answers the card; a later identical request, in this chat or
// a new one of the same owner, runs without a card; a request the policy
// forces still asks (and cannot be saved); another owner's chat asks.
func TestAlwaysAllow(t *testing.T) {
	f, base := newFakeAI(t,
		sse(toolCall("c1", "run_command", `{"command":"echo one"}`)...),
		sse(toolCall("c2", "run_command", `{"command":"echo one"}`)...),
		sse(toolCall("c3", "run_command", `{"command":"echo forced"}`)...),
		sse(text("first done")),
		sse(toolCall("b1", "run_command", `{"command":"echo one"}`)...),
		sse(text("bob done")),
		sse(toolCall("d1", "run_command", `{"command":"echo one"}`)...),
		sse(text("second done")),
	)
	e := newEnv(t, base, nil)
	e.cfg.Agent.Policy.AskAlways = []string{"echo forced"}
	store, err := permits.Open(filepath.Join(e.home, "perm"))
	if err != nil {
		t.Fatal(err)
	}
	e.m.o.Permits = store

	id := e.create(t, "ask")
	w := e.watch(t, id)
	e.send(t, id, "go")
	ask := w.until("ask", nil)
	req := ask["req"].(map[string]any)
	always, _ := req["always"].([]any)
	if len(always) != 2 || always[0].(map[string]any)["content"] != "echo one" || always[1].(map[string]any)["content"] != "echo:*" {
		t.Fatalf("always options: %v", req["always"])
	}
	if _, err := e.m.AnswerAlways(id, req["id"].(string), 5); err == nil {
		t.Fatal("an option out of range was taken")
	}
	rule, err := e.m.AnswerAlways(id, req["id"].(string), 0)
	if err != nil || rule == nil || rule.Content != "echo one" {
		t.Fatalf("always: %+v %v", rule, err)
	}
	ans := w.until("answered", nil)
	if ans["decision"] != DecideAlways || ans["rule"].(map[string]any)["content"] != "echo one" {
		t.Fatalf("answered: %v", ans)
	}
	if l := store.List(""); len(l) != 1 || l[0].Source != permits.SourceChat {
		t.Fatalf("saved: %+v", l)
	}
	w.until("tool_done", func(m map[string]any) bool { return m["id"] == "c1" })
	w.until("tool_done", func(m map[string]any) bool { return m["id"] == "c2" }) // covered: no card
	ask = w.until("ask", nil)
	req = ask["req"].(map[string]any)
	if req["forced"] != true || req["always"] != nil {
		t.Fatalf("forced card: %v", req)
	}
	if _, err := e.m.AnswerAlways(id, req["id"].(string), 0); !errors.Is(err, ErrForced) {
		t.Fatalf("always on a forced card: %v", err)
	}
	if _, err := e.m.Answer(id, req["id"].(string), DecideOnce, ""); err != nil {
		t.Fatal(err)
	}
	w.until("done", nil)
	w.until("state", idle)
	if n := w.count("ask"); n != 2 {
		t.Fatalf("%d cards, want 2", n)
	}

	// Bob's chat: Alice's rule is not his.
	bob, err := e.m.Create(CreateOptions{Cwd: e.home, Model: "api/model", Mode: "ask", Owner: "u-bob"})
	if err != nil {
		t.Fatal(err)
	}
	wb := e.watch(t, bob.ID)
	e.send(t, bob.ID, "go")
	ask = wb.until("ask", nil)
	if _, err := e.m.Answer(bob.ID, ask["req"].(map[string]any)["id"].(string), DecideDeny, ""); err != nil {
		t.Fatal(err)
	}
	wb.until("done", nil)
	wb.until("state", idle)

	// A new chat of the same owner: no card.
	id2 := e.create(t, "ask")
	w2 := e.watch(t, id2)
	e.send(t, id2, "again")
	w2.until("tool_done", func(m map[string]any) bool { return m["id"] == "d1" })
	w2.until("done", nil)
	if n := w2.count("ask"); n != 0 {
		t.Fatalf("%d cards in the new chat, want 0", n)
	}
	if l := store.List(""); l[0].Uses < 2 {
		t.Fatalf("uses = %d", l[0].Uses)
	}
	tm := toolMessages(f.requests()[7])
	if len(tm) != 1 || tm[0] != "one" {
		t.Fatalf("tool messages %q", tm)
	}
}
