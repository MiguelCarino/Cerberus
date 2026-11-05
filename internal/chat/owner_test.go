package chat

import (
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/agent"
)

// Per-owner access: role chat gets no tools, role user works in its own
// folder (Jail), the claude backend is admin only, asks are per owner and
// non-admins get inbox items instead of host notifications.
func TestOwnerAccess(t *testing.T) {
	f, base := newFakeAI(t, sse(text("hi")), sse(text("hello")))
	e := newEnv(t, base, func() string { return "/bin/true" })
	dir := filepath.Join(e.home, "users", "u-bob", "files")
	access := map[string]Access{
		"u-admin": {Admin: true},
		"u-bob":   {Cwd: dir, Jail: true},
		"u-cat":   {NoTools: true, Cwd: filepath.Join(e.home, "users", "u-cat", "files")},
	}
	e.m.o.Access = func(owner string) Access { return access[owner] }
	e.m.o.IsAdmin = func(owner string) bool { return access[owner].Admin }

	// Create: owner kept, cwd forced, claude refused to non-admins.
	c, err := e.m.Create(CreateOptions{Cwd: "/", Model: "api/model", Owner: "u-bob"})
	if err != nil || c.Owner != "u-bob" || c.Cwd != dir {
		t.Fatalf("bob's chat %+v %v", c, err)
	}
	if _, err := e.m.Create(CreateOptions{Model: "claude:sonnet", Owner: "u-bob"}); err != ErrBackend {
		t.Fatalf("claude for bob: %v", err)
	}
	if _, err := e.m.Create(CreateOptions{Model: "claude:sonnet", Owner: "u-admin", Cwd: e.home}); err != nil {
		t.Fatalf("claude for admin: %v", err)
	}
	cat, err := e.m.Create(CreateOptions{Model: "api/model", Owner: "u-cat"})
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := e.m.Owner(cat.ID); !ok || o != "u-cat" {
		t.Fatalf("Owner %q %v", o, ok)
	}

	// role chat: the request carries no tools.
	w := e.watch(t, cat.ID)
	e.send(t, cat.ID, "hey")
	w.until("state", idle)
	reqs := f.requests()
	if _, has := reqs[len(reqs)-1]["tools"]; has {
		t.Fatalf("role chat sent tools: %v", reqs[len(reqs)-1])
	}
	// role user: tools, minus run_command (no Wrap).
	wb := e.watch(t, c.ID)
	e.send(t, c.ID, "hey")
	wb.until("state", idle)
	reqs = f.requests()
	raw := strings.ToLower(strings.Join(func() []string {
		var out []string
		if tools, ok := reqs[len(reqs)-1]["tools"].([]any); ok {
			for _, x := range tools {
				out = append(out, x.(map[string]any)["function"].(map[string]any)["name"].(string))
			}
		}
		return out
	}(), ","))
	if !strings.Contains(raw, "read_file") || strings.Contains(raw, "run_command") {
		t.Fatalf("bob's tools: %s", raw)
	}

	// Search and lists carry the owner; searches filter by it.
	if hits := e.m.SearchWhere("hey", func(o string) bool { return o == "u-bob" }); len(hits) != 1 || hits[0].ID != c.ID {
		t.Fatalf("search %+v", hits)
	}
	for _, s := range e.m.List() {
		if s.ID == cat.ID && s.Owner != "u-cat" {
			t.Fatalf("summary %+v", s)
		}
	}

	// Asks per owner.
	e.m.mu.Lock()
	e.m.lives[c.ID].asks = append(e.m.lives[c.ID].asks, &pendingAsk{req: agent.AskRequest{ID: "r1"}})
	e.m.mu.Unlock()
	if k := e.m.AskKeysWhere(func(o string) bool { return o == "u-cat" }); len(k) != 0 {
		t.Fatalf("cat sees bob's asks: %v", k)
	}
	if k := e.m.AskKeysWhere(func(o string) bool { return o == "u-bob" }); len(k) != 1 {
		t.Fatalf("bob's asks: %v", k)
	}
	if a := e.m.OpenAsksWhere(func(o string) bool { return o == "u-bob" }); len(a) != 1 {
		t.Fatalf("open asks %v", a)
	}
	e.m.mu.Lock()
	e.m.lives[c.ID].asks = nil
	e.m.mu.Unlock()
}
