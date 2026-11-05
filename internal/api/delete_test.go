package api

import (
	"context"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/session"
)

// unacceptedSessions refuses deletes without force.
type unacceptedSessions struct{ *fakeSessions }

func (u unacceptedSessions) Delete(_ context.Context, id string, force bool) error {
	if !force {
		return &session.UnacceptedError{Reason: "2 commits not accepted"}
	}
	return u.fakeSessions.Delete(context.Background(), id, force)
}

func TestSessionDeleteEndpoint(t *testing.T) {
	env := newEnv(t, nil)
	if w := env.do("DELETE", "/api/sessions/s1", "", withBearer(testToken)); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := env.do("DELETE", "/api/sessions/nope", "", withBearer(testToken)); w.Code != 404 {
		t.Errorf("unknown: %d", w.Code)
	}
	env.srv.sessions = unacceptedSessions{env.sessions}
	env.h = env.srv.handler()
	if w := env.do("DELETE", "/api/sessions/s1", "", withBearer(testToken)); w.Code != 409 || !strings.Contains(w.Body.String(), `"unaccepted":true`) {
		t.Errorf("unaccepted: %d %s", w.Code, w.Body)
	}
	if w := env.do("DELETE", "/api/sessions/s1?force=1", "", withBearer(testToken)); w.Code != 204 {
		t.Errorf("forced: %d %s", w.Code, w.Body)
	}
}

// refusingModels answers model deletes with the engine's refusals.
type refusingModels struct{ fakeModels }

func (r *refusingModels) Delete(_ context.Context, id string) error {
	switch id {
	case "busy":
		return engine.ErrInUse
	case "openrouter/x":
		return engine.ErrNotDeletable
	}
	return r.do("delete", id)
}

func TestModelDeleteAction(t *testing.T) {
	env := newEnv(t, nil)
	if w := env.do("POST", "/api/models/qwen3:8b/delete", "{}", withBearer(testToken)); w.Code != 200 || env.models.last != "delete qwen3:8b" {
		t.Fatalf("delete: %d %s (%s)", w.Code, w.Body, env.models.last)
	}
	if w := env.do("POST", "/api/models/missing/delete", "{}", withBearer(testToken)); w.Code != 404 {
		t.Errorf("missing: %d", w.Code)
	}
	env.srv.models = &refusingModels{}
	env.h = env.srv.handler()
	if w := env.do("POST", "/api/models/busy/delete", "{}", withBearer(testToken)); w.Code != 409 {
		t.Errorf("in use: %d", w.Code)
	}
	if w := env.do("POST", "/api/models/openrouter/x/delete", "{}", withBearer(testToken)); w.Code != 400 {
		t.Errorf("cloud: %d", w.Code)
	}
}
