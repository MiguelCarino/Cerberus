package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"carino.systems/cerberus/internal/session"
)

const (
	searchMaxLimit = 500
	pruneTimeout   = 10 * time.Minute
)

// handleSearch serves GET /api/search?q=&repo=&agent=&state=&since=&until=&limit=.
// since and until take RFC 3339 or a local date (YYYY-MM-DD); a date for
// until includes that whole day.
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		unavailable(w, "sessions")
		return
	}
	q, err := parseSearch(r.URL.Query().Get, time.Local)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if pr := principalOf(r); s.hasUsers() && !pr.admin() && pr.Role != "viewer" {
		q.Owners = s.ownersOf(pr.User)
	}
	hits := s.history.Search(q)
	for i := range hits {
		hits[i].Session = s.decorate(hits[i].Session)
	}
	writeJSON(w, http.StatusOK, nonNil(hits))
}

// parseSearch builds a SearchQuery from query parameters.
func parseSearch(get func(string) string, loc *time.Location) (session.SearchQuery, error) {
	q := session.SearchQuery{Text: get("q"), Repo: get("repo"), Agent: get("agent"), State: get("state")}
	var err error
	if q.Since, err = parseWhen(get("since"), false, loc); err != nil {
		return q, fmt.Errorf("since: %w", err)
	}
	if q.Until, err = parseWhen(get("until"), true, loc); err != nil {
		return q, fmt.Errorf("until: %w", err)
	}
	if v := get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return q, fmt.Errorf("limit must be a positive number")
		}
		q.Limit = min(n, searchMaxLimit)
	}
	return q, nil
}

// parseWhen reads RFC 3339 or YYYY-MM-DD. With endOfDay, a bare date means
// the start of the next day (so "until=2026-11-01" includes that day).
func parseWhen(v string, endOfDay bool, loc *time.Location) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation(time.DateOnly, v, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC 3339 nor YYYY-MM-DD", v)
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// resume serves POST /api/sessions/{id}/resume {"text"}.
func (s *server) resume(w http.ResponseWriter, r *http.Request, id, text string) {
	if s.history == nil {
		unavailable(w, "sessions")
		return
	}
	// Resume only queues the new attempt; a dropped request must not undo it.
	if err := s.history.Resume(context.WithoutCancel(r.Context()), id, text); err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePrune runs a retention prune now and returns the PruneReport.
func (s *server) handlePrune(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		unavailable(w, "sessions")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pruneTimeout)
	defer cancel()
	rep, err := s.history.Prune(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rep.Removed, rep.Kept, rep.Errors = nonNil(rep.Removed), nonNil(rep.Kept), nonNil(rep.Errors)
	writeJSON(w, http.StatusOK, rep)
}

// handleTranscript serves the session's plain-text transcript.
func (s *server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		unavailable(w, "sessions")
		return
	}
	text, err := s.history.Transcript(r.PathValue("id"))
	if err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(text)
}
