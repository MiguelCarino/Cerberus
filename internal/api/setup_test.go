//go:build !windows

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"carino.systems/cerberus/internal/session"
)

// TestSessionSetupRoute: GET /api/sessions/{id}/setup returns the snapshot
// the run started with; an unknown session is 404.
func TestSessionSetupRoute(t *testing.T) {
	e := newCCEnv(t, nil)
	id := e.person("quick", "hello")
	e.waitFor("the run to end", func() bool {
		st := e.state(id)
		return st == session.StatePassed || st == session.StateFailed || st == session.StateError
	})
	code, body := e.call("GET", "/api/sessions/"+id+"/setup", "", "admin")
	if code != http.StatusOK {
		t.Fatalf("setup: %d %s", code, body)
	}
	var v session.SetupView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	info, _ := e.m.Get(id)
	if v.Setup.Agent.Name != "quick" || v.Hash == "" || v.Hash != info.SetupHash || !v.Verified ||
		v.Setup.Workspace != session.WorkspaceScratch {
		t.Fatalf("view %+v (info hash %q)", v, info.SetupHash)
	}
	if code, _ := e.call("GET", "/api/sessions/nope/setup", "", "admin"); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", code)
	}
}
