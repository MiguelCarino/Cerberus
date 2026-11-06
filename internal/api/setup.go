package api

import (
	"errors"
	"net/http"

	"carino.systems/cerberus/internal/session"
)

// Setup snapshots (docs/SCOPE.md "E3 Setup snapshot"):
//
//	GET /api/sessions/{id}/setup → session.SetupView (owners, admins and viewers, as for the session)
//
// A session with no setup.json (it has not run yet, or it is from before
// snapshots) is 404.

// setupService is the part of the session manager that reads setup
// snapshots. It is optional, so test fakes of sessionService need not
// implement it.
type setupService interface {
	Setup(id string) (session.SetupView, error)
}

func (s *server) setupRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/sessions/{id}/setup", s.handleSessionSetup)
}

func (s *server) handleSessionSetup(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.sessions.(setupService)
	if s.sessions == nil || !ok {
		unavailable(w, "sessions")
		return
	}
	v, err := svc.Setup(r.PathValue("id"))
	switch {
	case errors.Is(err, session.ErrNotFound), errors.Is(err, session.ErrNoSetup):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}
