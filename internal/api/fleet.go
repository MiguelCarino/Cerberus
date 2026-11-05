package api

import (
	"context"
	"net/http"
	"time"

	"carino.systems/cerberus/internal/session"
)

// batchAcceptTimeout bounds accepting a whole fleet task (one accept after
// another, each possibly pushing).
const batchAcceptTimeout = 30 * time.Minute

// BatchView is GET /api/batches/{id} and the reply to POST /api/batches.
type BatchView struct {
	Batch    session.BatchInfo `json:"batch"`
	Sessions []session.Info    `json:"sessions"`
}

// handleRepoFind serves GET /api/repos/find?has=GLOB&name=GLOB.
func (s *server) handleRepoFind(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		unavailable(w, "sessions")
		return
	}
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, nonNil(s.fleet.FindRepos(session.RepoSelector{HasFile: q.Get("has"), NameGlob: q.Get("name")})))
}

func (s *server) handleBatchList(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		unavailable(w, "sessions")
		return
	}
	pr := principalOf(r)
	out := []session.BatchInfo{}
	for _, b := range s.fleet.Batches() {
		if s.canSeeSession(pr, b.Owner) {
			b.Owner = s.ownerOf(b.Owner)
			out = append(out, b)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBatchCreate takes a BatchSpec (field names as in Go, matched
// case-insensitively, like POST /api/sessions).
func (s *server) handleBatchCreate(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		unavailable(w, "sessions")
		return
	}
	var spec session.BatchSpec
	if err := decodeBody(w, r, &spec); err != nil {
		writeError(w, bodyErrStatus(err), "invalid fleet task: "+err.Error())
		return
	}
	// Each repo goes through the same rules as a single session.
	spec.Owner, spec.Confined = "", false
	for _, repo := range spec.Repos {
		t := session.TaskSpec{Workspace: session.WorkspaceRepo, Dir: repo, Agent: spec.Agent, EscalateTo: spec.EscalateTo}
		if s.startRefused(w, r, &t) {
			return
		}
		spec.Owner, spec.Confined = t.Owner, t.Confined
	}
	b, infos, err := s.fleet.CreateBatch(r.Context(), spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, BatchView{Batch: b, Sessions: nonNil(infos)})
}

func (s *server) handleBatchGet(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		unavailable(w, "sessions")
		return
	}
	b, infos, ok := s.fleet.Batch(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "fleet task not found")
		return
	}
	writeJSON(w, http.StatusOK, BatchView{Batch: b, Sessions: nonNil(infos)})
}

// handleBatchAction serves POST /api/batches/{id}/accept {"message","push"}
// and /discard; both answer [BatchResult].
func (s *server) handleBatchAction(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		unavailable(w, "sessions")
		return
	}
	id, action := r.PathValue("id"), r.PathValue("action")
	if action != "accept" && action != "discard" {
		writeError(w, http.StatusNotFound, "unknown fleet task action "+action)
		return
	}
	var b struct {
		Message string `json:"message"`
		Push    bool   `json:"push"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	_, members, ok := s.fleet.Batch(id)
	if !ok {
		writeError(w, http.StatusNotFound, "fleet task not found")
		return
	}
	if action == "accept" {
		pr := principalOf(r)
		for _, info := range members {
			if !s.mayAccept(pr, info) {
				writeJSON(w, http.StatusForbidden, map[string]string{"code": "accept",
					"error": "You may not accept into the main of " + orDefault(info.Repo, info.Dir) + "."})
				return
			}
		}
	}
	var res []session.BatchResult
	if action == "accept" {
		// Detached like a single accept: a phone dropping off Wi-Fi must not
		// stop the run halfway.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), batchAcceptTimeout)
		defer cancel()
		res = s.fleet.AcceptBatch(ctx, id, session.AcceptOpts{Message: b.Message, Push: b.Push})
	} else {
		res = s.fleet.DiscardBatch(id)
	}
	writeJSON(w, http.StatusOK, nonNil(res))
}
