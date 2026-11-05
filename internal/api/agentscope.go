package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"carino.systems/cerberus/internal/session"
)

// The restricted agent scope. A session's agent (or `cerberus mcp` running
// for it) calls these with its session agent token and X-Cerberus-Session
// (see agentAuth in auth.go). It can start child sessions within
// cfg.Agent.Spawn and see and steer only its own descendants; every other
// id answers 404, as if it did not exist.
//
// An MCP client (an outside agent paired with scope "mcp", see clientAuth)
// uses the same spawn, children, tail, send, wait and stop endpoints as a
// root caller: its sessions are roots tagged with its Client name, in the
// scratch workspace, and it sees and steers those and their descendants
// only. The files and runs endpoints are a session's own workspace and
// answer 403 to a client.
//
//	POST /api/agent/spawn                   {task, agent?, model?, workspace?: "scratch"|"same", name?} → Info
//	GET  /api/agent/children                → [Info] (every descendant, breadth first)
//	GET  /api/agent/sessions/{id}/tail?kib= → {id, state, text, truncated} (plain transcript, ≤ 64 KiB)
//	POST /api/agent/sessions/{id}/send      {text} → {ok}
//	GET  /api/agent/sessions/{id}/wait?timeout=SECONDS → {done, session}
//	POST /api/agent/sessions/{id}/stop      → {ok}
//	GET  /api/agent/files?dir=REL           → Listing of the caller's own workspace (files.go)
//	GET  /api/agent/files/raw?path=REL      → a file of the caller's own workspace
//	GET  /api/agent/runs                    → the caller's own run recordings (extras.go)
//	GET  /api/agent/runs/{N}.cast           → one of them (asciicast v2)

const (
	tailDefaultKiB = 16
	tailMaxKiB     = 64
	waitDefault    = 60 * time.Second
	waitMax        = 600 * time.Second
)

func (s *server) agentRoutes(mux *routeMux) {
	mux.HandleFunc("POST /api/agent/spawn", s.handleAgentSpawn)
	mux.HandleFunc("GET /api/agent/children", s.handleAgentChildren)
	mux.HandleFunc("GET /api/agent/sessions/{id}/tail", s.handleAgentTail)
	mux.HandleFunc("POST /api/agent/sessions/{id}/send", s.handleAgentSend)
	mux.HandleFunc("GET /api/agent/sessions/{id}/wait", s.handleAgentWait)
	mux.HandleFunc("POST /api/agent/sessions/{id}/stop", s.handleAgentStop)
	s.agentFileRoutes(mux)
	mux.HandleFunc("/api/agent/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown agent endpoint "+r.Method+" "+r.URL.Path)
	})
}

// spawnBody is what an agent may choose about a child. Anything else
// (folders, counts, priorities, limits) is not the agent's to pick.
type spawnBody struct {
	Task      string `json:"task"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`
	Workspace string `json:"workspace"` // "scratch" (default) | "same"
	Name      string `json:"name"`
}

// agentReady checks the services and returns the calling session, or
// answers. It is for endpoints about the caller's own session (files,
// runs): an MCP client has none and gets 403.
func (s *server) agentReady(w http.ResponseWriter, r *http.Request) (string, bool) {
	if clientID(r) != "" {
		writeError(w, http.StatusForbidden, "this endpoint is for agents in a session; an MCP client has no workspace")
		return "", false
	}
	caller := callerID(r)
	if s.sessions == nil || s.control == nil || caller == "" {
		unavailable(w, "sessions")
		return "", false
	}
	return caller, true
}

// agentCaller is who calls the spawn endpoints: a session or an MCP client.
type agentCaller struct {
	session string // the calling session (session agent token)
	client  string // the calling MCP client, "mcp:<name>#<device id>" (client token)
	pr      principal
}

// spawnCaller checks the services and returns the caller of a spawn
// endpoint, or answers.
func (s *server) spawnCaller(w http.ResponseWriter, r *http.Request) (agentCaller, bool) {
	c := agentCaller{session: callerID(r), client: clientID(r), pr: principalOf(r)}
	if s.sessions == nil || s.control == nil || (c.session == "" && c.client == "") {
		unavailable(w, "sessions")
		return agentCaller{}, false
	}
	return c, true
}

// owns reports whether id is one of the caller's sessions: a descendant of
// the calling session, or a session the client started (or below one),
// and in either case owned by the caller's user.
func (s *server) owns(c agentCaller, id string) bool {
	tree := false
	if c.client != "" {
		tree = s.control.IsClientSession(c.client, id)
	} else {
		tree = id != c.session && s.control.IsDescendant(c.session, id)
	}
	if !tree {
		return false
	}
	info, ok := s.sessions.Get(id)
	return ok && s.sameOwner(c.pr, info.Owner)
}

// sameOwnerOnly keeps the sessions owned by the caller's user.
func (s *server) sameOwnerOnly(c agentCaller, in []session.Info) []session.Info {
	out := in[:0:0]
	for _, info := range in {
		if s.sameOwner(c.pr, info.Owner) {
			out = append(out, info)
		}
	}
	return out
}

// child resolves {id} to one of the caller's sessions, or answers 404.
func (s *server) child(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := s.spawnCaller(w, r)
	if !ok {
		return "", false
	}
	id := r.PathValue("id")
	if id == "" || !s.owns(c, id) {
		writeError(w, http.StatusNotFound, "no such child session "+strconv.Quote(id))
		return "", false
	}
	return id, true
}

func (s *server) handleAgentSpawn(w http.ResponseWriter, r *http.Request) {
	c, ok := s.spawnCaller(w, r)
	if !ok {
		return
	}
	var b spawnBody
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	ws := strings.TrimSpace(b.Workspace)
	switch ws {
	case "", session.WorkspaceScratch:
		ws = session.WorkspaceScratch
	case session.WorkspaceSame:
		if c.client != "" {
			writeError(w, http.StatusBadRequest, `an MCP client's sessions use the "scratch" workspace`)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, `workspace must be "scratch" or "same"`)
		return
	}
	spec := session.TaskSpec{Task: b.Task, Agent: b.Agent, Model: b.Model, Workspace: ws,
		Name: b.Name, Count: 1, Parent: c.session, Client: c.client}
	// The child acts for the caller's owner (a parent's children inherit
	// its owner in the session manager), confined and budgeted like them.
	pr := principalOf(r)
	spec.Owner = pr.User
	if s.hasUsers() && !pr.admin() {
		if ce := confineTask(r.Context(), s.users, s.conf().DefaultAgent, &spec); ce != nil {
			writeConfine(w, ce)
			return
		}
	}
	if s.budgetRefused(w, spec.Owner) {
		return
	}
	out, err := s.sessions.Create(r.Context(), spec)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, session.ErrSpawnLimit) {
			status = http.StatusForbidden
		}
		writeError(w, status, err.Error())
		return
	}
	if len(out) == 0 {
		writeError(w, http.StatusInternalServerError, "no session was created")
		return
	}
	writeJSON(w, http.StatusCreated, out[0])
}

func (s *server) handleAgentChildren(w http.ResponseWriter, r *http.Request) {
	c, ok := s.spawnCaller(w, r)
	if !ok {
		return
	}
	if c.client != "" {
		writeJSON(w, http.StatusOK, nonNil(s.sameOwnerOnly(c, s.control.ClientSessions(c.client))))
		return
	}
	writeJSON(w, http.StatusOK, nonNil(s.sameOwnerOnly(c, s.control.Descendants(c.session))))
}

// tailReply is a child's transcript tail.
type tailReply struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

func (s *server) handleAgentTail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.child(w, r)
	if !ok {
		return
	}
	if s.history == nil {
		unavailable(w, "transcripts")
		return
	}
	kib := tailDefaultKiB
	if v := r.URL.Query().Get("kib"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "kib must be a positive number")
			return
		}
		kib = min(n, tailMaxKiB)
	}
	raw, err := s.history.Transcript(id)
	if err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	text, cut := tail(raw, kib<<10)
	info, _ := s.sessions.Get(id)
	writeJSON(w, http.StatusOK, tailReply{ID: id, State: info.State, Text: text, Truncated: cut})
}

// tail is the last n bytes of raw as valid UTF-8, starting at a line when
// one begins within the first 2 KiB of the cut.
func tail(raw []byte, n int) (string, bool) {
	if len(raw) <= n {
		return strings.ToValidUTF8(string(raw), "�"), false
	}
	raw = raw[len(raw)-n:]
	for len(raw) > 0 && !utf8.RuneStart(raw[0]) {
		raw = raw[1:]
	}
	head := raw[:min(len(raw), 2048)]
	if i := strings.IndexByte(string(head), '\n'); i >= 0 {
		raw = raw[i+1:]
	}
	return strings.ToValidUTF8(string(raw), "�"), true
}

func (s *server) handleAgentSend(w http.ResponseWriter, r *http.Request) {
	id, ok := s.child(w, r)
	if !ok {
		return
	}
	var b struct {
		Text string `json:"text"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	noteAudit(r, "send: "+b.Text)
	// Never typed into a running child: that would let an agent answer its
	// child's approval prompts (FollowUpIdle answers 409 instead).
	if err := s.control.FollowUpIdle(r.Context(), id, b.Text); err != nil {
		status := sessionErrStatus(err)
		if strings.TrimSpace(b.Text) == "" || errors.Is(err, session.ErrAgentArg) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	id, ok := s.child(w, r)
	if !ok {
		return
	}
	if err := s.sessions.Stop(id); err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// active reports whether a child is still at work (or about to be) for
// wait: queued, starting, running, waiting for input, checking, retrying or
// escalating.
func active(state string) bool {
	switch state {
	case session.StateQueued, session.StateStarting, session.StateRunning, session.StateWaiting,
		session.StateChecking, session.StateRetrying, session.StateEscalated:
		return true
	}
	return false
}

// waitReply is the answer of wait: done is false when the timeout came
// first.
type waitReply struct {
	Done    bool         `json:"done"`
	Session session.Info `json:"session"`
}

// handleAgentWait blocks (on session events, never polling) until the
// child is no longer active, the timeout passes or the caller leaves.
func (s *server) handleAgentWait(w http.ResponseWriter, r *http.Request) {
	id, ok := s.child(w, r)
	if !ok {
		return
	}
	timeout := waitDefault
	if v := r.URL.Query().Get("timeout"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "timeout must be seconds (0 to 600)")
			return
		}
		timeout = min(time.Duration(n*float64(time.Second)), waitMax)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		events, unsub := s.sessions.Subscribe()
		info, ok := s.sessions.Get(id)
		if !ok {
			unsub()
			writeError(w, http.StatusNotFound, session.ErrNotFound.Error())
			return
		}
		if !active(info.State) {
			unsub()
			writeJSON(w, http.StatusOK, waitReply{Done: true, Session: info})
			return
		}
		resubscribe := false
		for !resubscribe {
			select {
			case <-r.Context().Done():
				unsub()
				return
			case <-deadline.C:
				unsub()
				info, _ = s.sessions.Get(id)
				writeJSON(w, http.StatusOK, waitReply{Done: !active(info.State), Session: info})
				return
			case ev, ok := <-events:
				switch {
				case !ok:
					resubscribe = true // dropped as a slow reader: check again
				case ev.Session.ID == id && (ev.Type == session.EventRemoved || !active(ev.Session.State)):
					resubscribe = true
				}
			}
		}
		unsub()
	}
}
