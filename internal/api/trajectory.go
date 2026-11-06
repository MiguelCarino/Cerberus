package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/records"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/trajectory"
)

// Trajectory export (docs/SCOPE.md "E4 Trajectory export (ATIF)"):
//
//	GET /api/sessions/{id}/trajectory?format=atif|jsonl
//
// atif (the default) is an ATIF trajectory built from the session's request
// records with its setup snapshot as metadata; jsonl is the records' own
// lines. Owners and admins (WORK): request records are admin data, and a
// session's are its owner's. 409 when there is nothing to build from:
// records off, the session's days swept, or no records for it.

func (s *server) trajectoryRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/sessions/{id}/trajectory", s.handleSessionTrajectory)
}

// trajectoryConflict answers 409 with a reason a client can branch on.
func trajectoryConflict(w http.ResponseWriter, reason, msg string) {
	writeJSON(w, http.StatusConflict, map[string]string{"error": msg, "reason": reason})
}

var trajectoryFileName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (s *server) handleSessionTrajectory(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	switch format {
	case "":
		format = "atif"
	case "atif", "jsonl":
	default:
		writeError(w, http.StatusBadRequest, "format must be atif or jsonl")
		return
	}
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	id := r.PathValue("id")
	info, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, session.ErrNotFound.Error())
		return
	}
	if s.records == nil {
		trajectoryConflict(w, "records_off", "request records are off (gateway.records.enabled), so there is no trajectory to export")
		return
	}
	from := info.Created.UTC().Format(time.DateOnly)
	if info.Created.IsZero() {
		from = ""
	}
	raws, more, err := s.records.SessionLines(id, from, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The days before the oldest kept one are gone: swept by retention or
	// the size cap (or records were off then; the files cannot tell).
	oldest := s.records.OldestDay()
	if oldest == "" {
		oldest = time.Now().UTC().AddDate(0, 0, -s.records.RetainDays()).Format(time.DateOnly)
	}
	gone := from != "" && from < oldest
	if len(raws) == 0 {
		if gone {
			trajectoryConflict(w, "swept", "this session's request records are gone: it started on "+from+
				", before the oldest kept day "+oldest+" (gateway.records.retain_days "+strconv.Itoa(s.records.RetainDays())+
				" and max_mib sweep old days)")
			return
		}
		trajectoryConflict(w, "none", "there are no request records for this session (its agent did not call the gateway, or records were off while it ran)")
		return
	}
	partial := ""
	if gone {
		partial = "The session started on " + from + ", before the oldest kept records day " + oldest + ": its first model calls may be missing."
	}
	withText := 0
	for _, x := range raws {
		if x.Line.Text != nil {
			withText++
		}
	}
	name := "session"
	if trajectoryFileName.MatchString(id) {
		name = id
	}
	if withText > 0 {
		// A read of record text is audited, as for GET /api/records/{...}.
		pr := principalOf(r)
		s.auditLog(audit.Record{Actor: orDefault(pr.Actor, audit.ActorSystem), User: pr.Name, Remote: limitHost(r.RemoteAddr),
			Method: r.Method, Route: r.Pattern, Targets: []string{id}, Status: http.StatusOK,
			Detail: "exported the trajectory (" + format + ") of a session: " + strconv.Itoa(withText) + " request record(s) with text"})
	}
	w.Header().Set("Cache-Control", "no-store")
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="cerberus-`+name+`.records.jsonl"`)
		if more {
			w.Header().Set("X-Cerberus-Truncated", strconv.Itoa(records.MaxSessionLines))
		}
		w.WriteHeader(http.StatusOK)
		for _, x := range raws {
			if _, err := w.Write(x.Bytes); err != nil {
				return
			}
		}
		return
	}
	lines := make([]records.Line, len(raws))
	for i, x := range raws {
		lines[i] = x.Line
	}
	in := trajectory.Input{Session: info, Lines: lines, TextLevel: s.records.TextLevel(), More: more, Partial: partial,
		Cerberus: s.version, Now: time.Now()}
	if svc, ok := s.sessions.(setupService); ok {
		if v, err := svc.Setup(id); err == nil {
			in.Setup = &v
		}
	}
	tr, err := trajectory.Build(in)
	if errors.Is(err, trajectory.ErrNoLines) {
		trajectoryConflict(w, "none", err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="cerberus-`+name+`.atif.json"`)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(tr)
}
