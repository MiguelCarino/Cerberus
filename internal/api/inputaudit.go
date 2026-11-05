package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/audit"
)

// Terminal input in the audit log. xterm sends every key as its own POST
// /api/sessions/{id}/input; one audit record per key would store a vault
// secret typed by hand a character or two per record (redaction masks
// whole values only) and bury real events. inputAudit joins the input of
// one session and actor into one record, written at Enter, after a short
// idle pause, or when the line grows long. It never polls: each pending
// line holds one timer.

const (
	inputIdle    = 2 * time.Second // write a pending line after this pause
	inputMaxLine = 4096            // or once it holds this many bytes
)

type inputAudit struct {
	mu      sync.Mutex
	pending map[string]*pendingInput
	idle    time.Duration
}

type pendingInput struct {
	rec    audit.Record // the template: actor, remote, route, targets
	buf    strings.Builder
	n      int // bytes, for hidden input
	hidden bool
	timer  *time.Timer
}

func newInputAudit() *inputAudit {
	return &inputAudit{pending: map[string]*pendingInput{}, idle: inputIdle}
}

// add joins data (sent to session id by r's principal) into the pending
// record and writes it when the line is complete.
func (a *inputAudit) add(s *server, r *http.Request, id, data string, hidden bool) {
	pr := principalOf(r)
	route := r.Pattern
	if route == "" {
		route = s.routePattern(r)
	}
	tmpl := audit.Record{Actor: orDefault(pr.Actor, audit.ActorSystem), Remote: limitHost(r.RemoteAddr),
		Method: r.Method, Route: route, Targets: patternTargets(r, route), Status: http.StatusOK}
	key := id + "\x00" + tmpl.Actor
	var out []audit.Record
	a.mu.Lock()
	p := a.pending[key]
	if p != nil && p.hidden != hidden {
		out = append(out, a.takeLocked(key, p))
		p = nil
	}
	if p == nil {
		p = &pendingInput{rec: tmpl, hidden: hidden}
		a.pending[key] = p
		p.timer = time.AfterFunc(a.idle, func() { a.flush(s, key, p) })
	} else {
		p.timer.Reset(a.idle)
	}
	p.n += len(data)
	if !hidden {
		p.buf.WriteString(data)
	}
	if strings.ContainsAny(data, "\r\n") || p.n >= inputMaxLine {
		out = append(out, a.takeLocked(key, p))
	}
	a.mu.Unlock()
	for _, rec := range out {
		s.auditLog(rec)
	}
}

// flush writes key's pending line if it is still p (the idle timer).
func (a *inputAudit) flush(s *server, key string, p *pendingInput) {
	a.mu.Lock()
	if a.pending[key] != p {
		a.mu.Unlock()
		return
	}
	rec := a.takeLocked(key, p)
	a.mu.Unlock()
	s.auditLog(rec)
}

// flushAll writes every pending line (shutdown, tests).
func (a *inputAudit) flushAll(s *server) {
	a.mu.Lock()
	var out []audit.Record
	for k, p := range a.pending {
		out = append(out, a.takeLocked(k, p))
	}
	a.mu.Unlock()
	for _, rec := range out {
		s.auditLog(rec)
	}
}

// takeLocked removes p and returns its finished record.
func (a *inputAudit) takeLocked(key string, p *pendingInput) audit.Record {
	p.timer.Stop()
	delete(a.pending, key)
	rec := p.rec
	if p.hidden {
		rec.Detail = "input (hidden, " + strconv.Itoa(p.n) + " bytes)"
	} else {
		rec.Detail = "input: " + p.buf.String()
	}
	return rec
}
