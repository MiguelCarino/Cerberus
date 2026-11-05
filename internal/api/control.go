package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/session"
)

// Control center (day phase 1): session metadata, bulk actions, kill,
// spawn approval, the activity feed and one multiplexed output stream for
// a wall of terminals.
//
//	PATCH /api/sessions/{id}            session.Patch JSON → Info
//	POST  /api/sessions/bulk            {ids, op: BulkOp} → {results, ok, skipped, error}
//	POST  /api/sessions/{id}/kill       → {ok}
//	POST  /api/sessions/{id}/approve    {"approve": bool} → {ok}
//	GET   /api/feed?since=&limit=       → [FeedItem], oldest first
//	GET   /api/feed/stream[?since=]     SSE "feed" events (FeedItem JSON)
//	GET   /api/wall/stream?ids=a,b,c    SSE "out" events {"id","b64"[,"backlog":true]}

const (
	maxBulkIDs      = 100
	bulkTimeout     = 30 * time.Minute // accept, checks and resume of up to 100 sessions
	wallBacklogKiB  = 8
	maxWallIDs      = 16
	feedReplayLimit = 500
)

// controlService is the session manager's control-center surface.
type controlService interface {
	Update(id string, p session.Patch) (session.Info, error)
	Bulk(ctx context.Context, ids []string, op session.BulkOp) []session.BulkResult
	Kill(id string) error
	Approve(id string, ok bool) error
	Feed(since time.Time, limit int) []session.FeedItem
	SubscribeFeed() (<-chan session.FeedItem, func())
	Record(id, kind, text string)
	AttachMany(ids []string, backlogKiB int) (map[string][]byte, <-chan session.WallChunk, func(), error)
	Descendants(root string) []session.Info
	IsDescendant(root, id string) bool
	ClientSessions(client string) []session.Info
	IsClientSession(client, id string) bool
	FollowUpIdle(ctx context.Context, id, text string) error
	InputHidden(id string) bool
	ScopesEnforced() bool
}

func (s *server) controlRoutes(mux *routeMux) {
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handleSessionPatch)
	mux.HandleFunc("POST /api/sessions/bulk", s.handleBulk)
	mux.HandleFunc("GET /api/feed", s.handleFeed)
	mux.HandleFunc("GET /api/feed/stream", s.handleFeedStream)
	mux.HandleFunc("GET /api/wall/stream", s.handleWallStream)
	s.agentRoutes(mux)
	s.delegateRoutes(mux)
}

// controlErrStatus maps a control error: 404 unknown session, 409 for a
// state conflict (not running, busy, not waiting for approval), else 400
// (validation).
func controlErrStatus(err error) int {
	switch {
	case errors.Is(err, session.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, session.ErrNotRunning), errors.Is(err, session.ErrBusy):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (s *server) handleSessionPatch(w http.ResponseWriter, r *http.Request) {
	if s.control == nil {
		unavailable(w, "sessions")
		return
	}
	var p session.Patch
	if err := decodeBody(w, r, &p); err != nil {
		writeError(w, bodyErrStatus(err), "invalid patch: "+err.Error())
		return
	}
	info, err := s.control.Update(r.PathValue("id"), p)
	if err != nil {
		writeError(w, controlErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// bulkBody is POST /api/sessions/bulk.
type bulkBody struct {
	IDs []string       `json:"ids"`
	Op  session.BulkOp `json:"op"`
}

// bulkReply is the bulk answer: every session's outcome plus counts.
type bulkReply struct {
	Results []session.BulkResult `json:"results"`
	OK      int                  `json:"ok"`
	Skipped int                  `json:"skipped"`
	Error   int                  `json:"error"`
}

func (s *server) handleBulk(w http.ResponseWriter, r *http.Request) {
	if s.control == nil {
		unavailable(w, "sessions")
		return
	}
	var b bulkBody
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	if len(b.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "no sessions selected")
		return
	}
	if len(b.IDs) > maxBulkIDs {
		writeError(w, http.StatusBadRequest, "at most "+strconv.Itoa(maxBulkIDs)+" sessions per bulk action")
		return
	}
	// Like a single accept: a phone dropping off Wi-Fi must not abort a
	// rebase or push halfway through the list.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), bulkTimeout)
	defer cancel()
	// Only the caller's own sessions (all for admins); others are reported
	// as not found, never acted on.
	pr := principalOf(r)
	var mine []string
	var foreign []session.BulkResult
	for _, id := range b.IDs {
		info, ok := s.sessions.Get(id)
		if ok && !s.canOwn(pr, info.Owner) {
			foreign = append(foreign, session.BulkResult{ID: id, Result: session.BulkError, Detail: session.ErrNotFound.Error()})
			continue
		}
		if ok && (b.Op.Kind == session.BulkResume || b.Op.Kind == "followup" || b.Op.Kind == "checks") && s.budgetRefusedQuiet(info.Owner) {
			foreign = append(foreign, session.BulkResult{ID: id, Result: session.BulkError, Detail: "monthly budget used up"})
			continue
		}
		if ok && b.Op.Kind == session.BulkAccept && !s.mayAccept(pr, info) {
			foreign = append(foreign, session.BulkResult{ID: id, Result: session.BulkError, Detail: "you may not accept into this repo's main"})
			continue
		}
		mine = append(mine, id)
	}
	var res []session.BulkResult
	if len(mine) > 0 {
		res = s.control.Bulk(ctx, mine, b.Op)
	}
	res = append(res, foreign...)
	out := bulkReply{Results: nonNil(res)}
	for _, x := range res {
		switch x.Result {
		case session.BulkOK:
			out.OK++
		case session.BulkSkipped:
			out.Skipped++
		default:
			out.Error++
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// sessionKill is POST /api/sessions/{id}/kill.
func (s *server) sessionKill(w http.ResponseWriter, id string) {
	if s.control == nil {
		unavailable(w, "sessions")
		return
	}
	if err := s.control.Kill(id); err != nil {
		writeError(w, controlErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// sessionApprove is POST /api/sessions/{id}/approve {"approve": bool}.
func (s *server) sessionApprove(w http.ResponseWriter, id string, approve *bool) {
	if s.control == nil {
		unavailable(w, "sessions")
		return
	}
	if approve == nil {
		writeError(w, http.StatusBadRequest, `body must be {"approve": true|false}`)
		return
	}
	if err := s.control.Approve(id, *approve); err != nil {
		status := http.StatusConflict
		if errors.Is(err, session.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// parseFeedSince accepts RFC 3339 (with or without fractions) or Unix
// milliseconds; "" is the zero time.
func parseFeedSince(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, errors.New("since must be RFC 3339 or Unix milliseconds")
	}
	return t, nil
}

func (s *server) handleFeed(w http.ResponseWriter, r *http.Request) {
	if s.control == nil {
		unavailable(w, "sessions")
		return
	}
	q := r.URL.Query()
	since, err := parseFeedSince(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, 2000)
	}
	keep := s.feedFilter(principalOf(r))
	out := []session.FeedItem{}
	for _, it := range s.control.Feed(since, limit) {
		if keep(it) {
			out = append(out, it)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// feedFilter keeps the feed items of sessions the caller can see.
func (s *server) feedFilter(pr principal) func(session.FeedItem) bool {
	if !s.hasUsers() || pr.admin() || pr.Role == "viewer" {
		return func(session.FeedItem) bool { return true }
	}
	return func(it session.FeedItem) bool {
		if s.sessions == nil {
			return false
		}
		info, ok := s.sessions.Get(it.SessionID)
		return ok && s.canSeeSession(pr, info.Owner)
	}
}

// handleFeedStream streams new feed items as "feed" events. With ?since=
// the items kept after that time are replayed first (at most 500), so a
// client that fetched /api/feed can reconnect without a gap. The stream
// ends when the subscriber falls too far behind; the client reconnects.
func (s *server) handleFeedStream(w http.ResponseWriter, r *http.Request) {
	if s.control == nil {
		unavailable(w, "sessions")
		return
	}
	q := r.URL.Query()
	since, err := parseFeedSince(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Subscribe before replaying so nothing recorded in between is lost;
	// items seen in the replay are not sent twice.
	ch, cancel := s.control.SubscribeFeed()
	defer cancel()
	keep := s.feedFilter(principalOf(r))
	rc := startSSE(w)
	seen := map[session.FeedItem]bool{}
	if q.Has("since") {
		for _, it := range s.control.Feed(since, feedReplayLimit) {
			if !keep(it) {
				continue
			}
			seen[it] = true
			if !s.sendFeed(w, rc, it) {
				return
			}
		}
	}
	ping := time.NewTicker(s.ping)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if sendPing(w, rc) != nil {
				return
			}
		case it, ok := <-ch:
			if !ok {
				return
			}
			if seen[it] {
				delete(seen, it)
				continue
			}
			if !keep(it) {
				continue
			}
			if !s.sendFeed(w, rc, it) {
				return
			}
		}
	}
}

func (s *server) sendFeed(w http.ResponseWriter, rc *http.ResponseController, it session.FeedItem) bool {
	raw, err := json.Marshal(it)
	return err == nil && sendEvent(w, rc, "feed", raw) == nil
}

// wallChunk is the data of one wall stream event.
type wallChunk struct {
	ID      string `json:"id"`
	B64     string `json:"b64"`
	Backlog bool   `json:"backlog,omitempty"`
}

// handleWallStream multiplexes several sessions' terminals on one SSE: one
// "out" event per session with backlog:true (its last 8 KiB), then "out"
// events as output arrives. When any session's stream ends the whole
// stream ends, and the client reconnects (resetting its terminals on the
// fresh backlogs).
func (s *server) handleWallStream(w http.ResponseWriter, r *http.Request) {
	if s.control == nil {
		unavailable(w, "sessions")
		return
	}
	ids := splitIDs(r.URL.Query().Get("ids"))
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids is empty")
		return
	}
	if len(ids) > maxWallIDs {
		writeError(w, http.StatusBadRequest, "at most "+strconv.Itoa(maxWallIDs)+" sessions per wall stream")
		return
	}
	// Ids the caller cannot see are dropped (never confirmed).
	pr := principalOf(r)
	var visible []string
	for _, id := range ids {
		// Unknown ids stay (AttachMany answers 404 as before).
		if info, ok := s.sessions.Get(id); !ok || s.canSeeSession(pr, info.Owner) {
			visible = append(visible, id)
		}
	}
	if len(visible) == 0 {
		notFound(w)
		return
	}
	ids = visible
	backlogs, ch, detach, err := s.control.AttachMany(ids, wallBacklogKiB)
	if err != nil {
		writeError(w, controlErrStatus(err), err.Error())
		return
	}
	defer detach()
	rc := startSSE(w)
	send := func(c wallChunk) bool {
		raw, err := json.Marshal(c)
		return err == nil && sendEvent(w, rc, "out", raw) == nil
	}
	for _, id := range ids {
		if !send(wallChunk{ID: id, B64: string(b64(backlogs[id])), Backlog: true}) {
			return
		}
	}
	ping := time.NewTicker(s.ping)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if sendPing(w, rc) != nil {
				return
			}
		case c, ok := <-ch:
			if !ok || !send(wallChunk{ID: c.ID, B64: string(b64(c.Data))}) {
				return
			}
		}
	}
}

// splitIDs splits a comma list, dropping blanks and duplicates.
func splitIDs(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// recordInput adds typed terminal input to the feed (see
// session.InputLog); hidden means the terminal had echo off when it came.
func (s *server) recordInput(id string, data []byte, hidden bool) {
	if s.control == nil {
		return
	}
	for _, text := range s.inputs.Observe(id, data, hidden) {
		s.control.Record(id, session.FeedInput, text) // the feed masks, then clips
	}
}
