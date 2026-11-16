package api

import (
	"errors"
	"net/http"
	"strconv"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/records"
)

// Request records (gateway phase G3). Admin only: the lines say what every
// client sent, and with text on they hold conversations.

// handleRecords lists a day's lines, without text.
func (s *server) handleRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	lines, err := s.records.List(records.Filter{Day: q.Get("day"), Session: q.Get("session"),
		Client: q.Get("client"), Limit: limit})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if lines == nil {
		lines = []records.Line{}
	}
	days := s.records.Days()
	if days == nil {
		days = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": s.records.Stats(), "days": days, "lines": lines})
}

// handleRecord returns one line with its text; a read of text is audited.
// A line's text holds only its new turn (prefix counts the messages
// before it); ?thread=1 answers the conversation stitched together from the
// earlier lines of the same client and session, with "earlier" (lines
// joined) and "missing" (its start was not found).
func (s *server) handleRecord(w http.ResponseWriter, r *http.Request) {
	off, err := strconv.ParseInt(r.PathValue("offset"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such record")
		return
	}
	l, err := s.records.Get(r.PathValue("day"), r.PathValue("file"), off)
	if errors.Is(err, records.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such record")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	thread := r.URL.Query().Get("thread") == "1" && l.Text != nil && l.Prefix > 0
	if l.Text != nil {
		detail := "read the text of a request record"
		if thread {
			detail = "read the text of a request record and its earlier turns"
		}
		pr := principalOf(r)
		s.auditLog(audit.Record{Actor: orDefault(pr.Actor, audit.ActorSystem), User: pr.Name, Remote: limitHost(r.RemoteAddr),
			Method: r.Method, Route: r.Pattern, Targets: []string{l.Ref}, Status: http.StatusOK,
			Detail: detail})
	}
	if thread {
		writeJSON(w, http.StatusOK, s.records.Stitch(l))
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// handleRecordsDelete removes the days before ?before=YYYY-MM-DD.
func (s *server) handleRecordsDelete(w http.ResponseWriter, r *http.Request) {
	before := r.URL.Query().Get("before")
	n, err := s.records.DeleteBefore(before)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	noteAudit(r, "deleted "+strconv.Itoa(n)+" day(s) of request records before "+before)
	writeJSON(w, http.StatusOK, map[string]int{"deleted_days": n})
}
