package api

import (
	"net/http"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/users"
)

// Role changes leave a user's running sessions alone (their agent tokens
// keep working); disabling stops those sessions cleanly, and a removed
// user's session agent never acts for the admin that receives the data.
func TestUserChangesAndRunningSessions(t *testing.T) {
	e := newUsersEnv(t)
	e.srv.gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	bob := e.u["bob"]
	e.ss.add("sb", bob.ID)
	v1 := func() int {
		return e.do("POST", "/v1/chat/completions", "{}", withBearer(e.srv.cfg.SessionAgentToken("sb")),
			withHeader("Content-Type", "application/json")).Code
	}
	if c := v1(); c != 200 {
		t.Fatalf("agent before: %d", c)
	}
	if k := revokeKeys(principal{Kind: kindAgent, User: bob.ID}); len(k) != 0 {
		t.Fatalf("an agent request is cut with its owner's sign-ins: %v", k)
	}
	if w := e.call("ana", "PATCH", "/api/users/"+bob.ID, `{"role":"admin"}`); w.Code != 200 {
		t.Fatalf("promote: %d %s", w.Code, w.Body)
	}
	if info, _ := e.ss.Get("sb"); info.State != session.StateRunning || v1() != 200 {
		t.Fatalf("a role change broke the session: %+v", info)
	}
	e.call("ana", "PATCH", "/api/users/"+bob.ID, `{"role":"user"}`)
	w := e.call("ana", "PATCH", "/api/users/"+bob.ID, `{"disabled":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sessions_stopped":1`) {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	if info, _ := e.ss.Get("sb"); info.State != session.StateStopped {
		t.Fatalf("disabled user's session still running: %+v", info)
	}
	// Removed: the session's agent token does not turn into the admin.
	e.call("ana", "PATCH", "/api/users/"+bob.ID, `{"disabled":false}`)
	e.ss.add("sb", bob.ID)
	if w := e.call("ana", "DELETE", "/api/users/"+bob.ID, ``); w.Code != 200 || !strings.Contains(w.Body.String(), `"sessions_stopped":1`) {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if c := v1(); c != http.StatusUnauthorized {
		t.Fatalf("removed user's agent token on /v1: %d", c)
	}
}

// With no accounts (single-user mode), a device or confined session bound
// to an account does not turn into the admin.
func TestBoundCredentialsFailClosedWithoutAccounts(t *testing.T) {
	env := newSafetyEnv(t, nil)
	st, err := users.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.users = st // no accounts
	pr := principal{Kind: kindDevice}
	if env.srv.bindUser(&pr, "u-gone") {
		t.Fatal("a device of a missing account bound as admin")
	}
	pr = principal{Kind: kindDevice}
	if !env.srv.bindUser(&pr, "") || pr.Role != users.RoleAdmin {
		t.Fatal("an unbound device in single-user mode is not admin")
	}
	ss := newOwnedSessions()
	env.srv.sessions = ss
	ss.mu.Lock()
	ss.infos["c"] = session.Info{ID: "c", Owner: "u-gone", Confined: true}
	ss.infos["a"] = session.Info{ID: "a", Owner: "u-ana"}
	ss.mu.Unlock()
	if pr := (principal{Kind: kindAgent}); env.srv.bindSessionOwner(&pr, "c") {
		t.Fatal("a confined session's agent became admin without accounts")
	}
	if pr := (principal{Kind: kindAgent}); !env.srv.bindSessionOwner(&pr, "a") {
		t.Fatal("an unconfined session's agent lost /v1 in single-user mode")
	}
}
