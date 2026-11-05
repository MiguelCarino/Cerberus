package api

import (
	"net/http"
	"testing"
	"time"

	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// Keyless loopback /v1 (server.local_no_auth) acts as the admin. A
// confined session shares the host network, so while one runs the keyless
// rule is paused: its agent could otherwise use the models outside its
// owner's budget, charged to the admin. A personal key from loopback counts
// for its user, never as the keyless admin.
func TestLocalNoAuthPausedWhileConfinedWorkRuns(t *testing.T) {
	e := newUsersEnv(t)
	var seen []string
	e.srv.gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(userHeader))
		w.WriteHeader(200)
	})
	e.srv.conf().Server.LocalNoAuth = true
	bob := e.u["bob"]
	if _, err := e.st.SetBudget(bob.ID, users.Budget{MonthlyUSD: 1}); err != nil {
		t.Fatal(err)
	}
	e.used.Add(usage.Record{Time: time.Now(), Model: "x", CostUSD: 5, User: bob.ID})
	loop := func(r *http.Request) { r.RemoteAddr = "127.0.0.1:40000"; r.Host = "127.0.0.1:7340" }
	json := withHeader("Content-Type", "application/json")

	// No confined work: keyless local works, as the admin.
	if w := e.do("POST", "/v1/chat/completions", "{}", loop, json); w.Code != 200 || seen[len(seen)-1] != e.u["ana"].ID {
		t.Fatalf("keyless idle: %d %v", w.Code, seen)
	}
	// bob's confined session runs: keyless local is refused.
	e.ss.mu.Lock()
	e.ss.infos["sb"] = session.Info{ID: "sb", Owner: bob.ID, State: session.StateRunning, Confined: true,
		Workspace: session.WorkspaceScratch}
	e.ss.mu.Unlock()
	w := e.do("POST", "/v1/chat/completions", "{}", loop, json)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("BUDGET BYPASS: keyless local admitted while a confined session runs: %d, charged to %v", w.Code, seen)
	}
	// The admin token still works from loopback meanwhile.
	if w := e.do("POST", "/v1/chat/completions", "{}", loop, json, withBearer(e.srv.token())); w.Code != 200 {
		t.Fatalf("admin token while paused: %d %s", w.Code, w.Body)
	}
	// bob's own personal key from loopback counts for bob (and his budget).
	key, err := e.st.NewAPIKey(bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "/v1/chat/completions", "{}", loop, json, withBearer(key)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("bob's key from loopback: %d %s (charged to %v)", w.Code, w.Body, seen)
	}
	// The session ends: keyless local is back.
	e.ss.mu.Lock()
	e.ss.infos["sb"] = session.Info{ID: "sb", Owner: bob.ID, State: session.StatePassed, Confined: true}
	e.ss.mu.Unlock()
	if w := e.do("POST", "/v1/chat/completions", "{}", loop, json); w.Code != 200 {
		t.Fatalf("keyless after the session: %d", w.Code)
	}
}
