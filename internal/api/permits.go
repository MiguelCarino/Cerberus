package api

import (
	"errors"
	"net/http"
	"strings"

	"carino.systems/cerberus/internal/permits"
)

// Saved permissions ("Always allow", docs/SCOPE.md "As built: saved
// permissions"). Each person sees and changes only their own rules; an
// admin too (no "all" view: a rule is someone's own yes).
//
//	GET    /api/permits       {rules: [Saved], policy: {allow, ask_always, deny}, policy_link}
//	POST   /api/permits       {tool, content} → {rule: Saved, added}   (validated, source "settings")
//	DELETE /api/permits/{id}  {ok}

func (s *server) permitRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/permits", s.handlePermitList)
	mux.HandleFunc("POST /api/permits", s.handlePermitAdd)
	mux.HandleFunc("DELETE /api/permits/{id}", s.handlePermitRemove)
}

func (s *server) permitsReady(w http.ResponseWriter) bool {
	if s.permits == nil {
		unavailable(w, "saved permissions")
		return false
	}
	return true
}

func (s *server) handlePermitList(w http.ResponseWriter, r *http.Request) {
	if !s.permitsReady(w) {
		return
	}
	pr := principalOf(r)
	pol := s.conf().Agent.Policy
	nz := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	out := map[string]any{
		"rules": s.permits.List(pr.User),
		// The command policy also decides what runs without asking: shown
		// read-only next to the saved rules (edited in Settings → Safety).
		"policy": map[string][]string{"allow": nz(pol.Allow), "ask_always": nz(pol.AskAlways), "deny": nz(pol.Deny)},
	}
	if pr.admin() {
		out["policy_link"] = "#/settings/safety"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handlePermitAdd(w http.ResponseWriter, r *http.Request) {
	if !s.permitsReady(w) {
		return
	}
	var b struct {
		Tool    string `json:"tool"`
		Content string `json:"content"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	rule := permits.NewRule(strings.TrimSpace(b.Tool), strings.TrimSpace(b.Content))
	if err := permits.Validate(rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sv, added, err := s.permits.Add(principalOf(r).User, rule, permits.SourceSettings)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	noteAudit(r, "saved permission added: "+sv.Text, "permit:"+sv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"rule": sv, "added": added})
}

func (s *server) handlePermitRemove(w http.ResponseWriter, r *http.Request) {
	if !s.permitsReady(w) {
		return
	}
	sv, err := s.permits.Remove(principalOf(r).User, r.PathValue("id"))
	if errors.Is(err, permits.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	noteAudit(r, "saved permission removed: "+sv.Text, "permit:"+sv.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
