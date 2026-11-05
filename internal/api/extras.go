package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// Extras (day phase 6): the notification inbox, session recordings, the
// model download manager, the leaderboard and the PWA files.
//
//	GET    /api/inbox?unread=1&limit=N         → {items: [Item] newest first, unread}
//	POST   /api/inbox/read {ids: [..]} | {all: true} → {changed, unread}
//	DELETE /api/inbox/{id}                     → {ok, unread}
//	GET    /api/sessions/{id}/runs             → [RunCast] oldest attempt first
//	GET    /api/sessions/{id}/runs/{N}.cast    → asciicast v2 (application/x-asciicast)
//	GET    /api/agent/runs                     → the calling session's own recordings
//	GET    /api/agent/runs/{N}.cast            → one of them
//	GET    /api/downloads                      → [downloads.Item] newest first
//	POST   /api/downloads {engine?, ref, sha256?} → 202 downloads.Item
//	DELETE /api/downloads/{id}                 → {ok} (cancels, or forgets a finished one)
//	GET    /api/leaderboard?period=&by=&metric=&limit= → usage.Board
//	GET    /manifest.webmanifest, /sw.js       → public PWA files from web/dist
//
// The event stream (GET /api/events) carries, next to "session" events,
// "inbox" events (inbox.Event), "download" events (downloads.Item: every
// state change and throttled progress) and "resync" events ({what:
// "inbox"|"downloads"}) when the stream fell behind and the client should
// refetch that list.

const (
	inboxDefaultLimit = 200
	inboxMaxIDs       = inbox.DefaultKeep
	boardDefaultLimit = 50
	boardMaxLimit     = 500
	feedBuffer        = 64
	castContentType   = "application/x-asciicast"
)

// inboxService is what the inbox routes use (*inbox.Store).
type inboxService interface {
	List(unreadOnly bool, limit int) []inbox.Item
	Unread() int
	MarkRead(ids []string, all bool) (int, error)
	Delete(id string) (bool, error)
	Subscribe() (<-chan inbox.Event, func())

	// Owner scoping (Phase 4a).
	ListWhere(keep func(inbox.Item) bool, unreadOnly bool, limit int) []inbox.Item
	UnreadWhere(keep func(inbox.Item) bool) int
	MarkReadWhere(ids []string, all bool, keep func(inbox.Item) bool) (int, error)
	DeleteWhere(id string, keep func(inbox.Item) bool) (bool, error)
	Add(it inbox.Item) inbox.Item
}

// downloadService is what the download routes use (*downloads.Manager).
type downloadService interface {
	Add(req downloads.Request) (downloads.Item, error)
	Cancel(id string) error
	List() []downloads.Item
}

// runService lists and opens session recordings (*session.Manager).
type runService interface {
	Runs(id string) ([]session.RunCast, error)
	OpenRun(id string, attempt int) (*os.File, error)
}

// boardFunc builds one leaderboard.
type boardFunc func(period, by, metric string, limit int, keep func(string) bool) (usage.Board, error)

// DownloadFeed fans download changes out to the event streams. The daemon
// passes Publish as downloads.Options.OnChange and the feed as
// Deps.DownloadFeed. Publish never blocks: a subscriber whose buffer is
// full is dropped (its channel closed) and its stream sends a resync.
type DownloadFeed struct {
	mu   sync.Mutex
	subs map[chan downloads.Item]struct{}
}

// NewDownloadFeed returns an empty feed.
func NewDownloadFeed() *DownloadFeed {
	return &DownloadFeed{subs: map[chan downloads.Item]struct{}{}}
}

// Publish sends it to every subscriber.
func (f *DownloadFeed) Publish(it downloads.Item) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- it:
		default:
			delete(f.subs, ch)
			close(ch)
		}
	}
}

// Subscribe streams items until cancel is called.
func (f *DownloadFeed) Subscribe() (<-chan downloads.Item, func()) {
	ch := make(chan downloads.Item, feedBuffer)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[ch]; ok {
			delete(f.subs, ch)
			close(ch)
		}
	}
}

func (s *server) extrasRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/inbox", s.handleInboxList)
	mux.HandleFunc("POST /api/inbox/read", s.handleInboxRead)
	mux.HandleFunc("DELETE /api/inbox/{id}", s.handleInboxDelete)
	mux.HandleFunc("GET /api/sessions/{id}/runs", s.handleRuns)
	mux.HandleFunc("GET /api/sessions/{id}/runs/{name}", s.handleRunCast)
	mux.HandleFunc("GET /api/agent/runs", s.handleAgentRuns)
	mux.HandleFunc("GET /api/agent/runs/{name}", s.handleAgentRunCast)
	mux.HandleFunc("GET /api/downloads", s.handleDownloadList)
	mux.HandleFunc("POST /api/downloads", s.handleDownloadAdd)
	mux.HandleFunc("DELETE /api/downloads/{id}", s.handleDownloadCancel)
	mux.HandleFunc("GET /api/leaderboard", s.handleLeaderboard)
}

// ---------- inbox ----------

func (s *server) handleInboxList(w http.ResponseWriter, r *http.Request) {
	if s.inbox == nil {
		unavailable(w, "inbox")
		return
	}
	q := r.URL.Query()
	limit := inboxDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, inbox.DefaultKeep)
	}
	unread := q.Get("unread")
	only := unread == "1" || unread == "true"
	keep := s.visibleInbox(principalOf(r))
	items := s.inbox.ListWhere(keep, only, limit)
	if items == nil {
		items = []inbox.Item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unread": s.inbox.UnreadWhere(keep)})
}

type inboxReadBody struct {
	IDs []string `json:"ids"`
	All bool     `json:"all"`
}

func (s *server) handleInboxRead(w http.ResponseWriter, r *http.Request) {
	if s.inbox == nil {
		unavailable(w, "inbox")
		return
	}
	var b inboxReadBody
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	if !b.All && len(b.IDs) == 0 {
		writeError(w, http.StatusBadRequest, `give "ids" or "all": true`)
		return
	}
	if len(b.IDs) > inboxMaxIDs {
		writeError(w, http.StatusBadRequest, "too many ids")
		return
	}
	if b.All {
		noteAudit(r, "all")
	} else {
		noteAudit(r, strconv.Itoa(len(b.IDs))+" item(s)")
	}
	keep := s.visibleInbox(principalOf(r))
	n, err := s.inbox.MarkReadWhere(b.IDs, b.All, keep)
	if err != nil {
		s.log.Warn("inbox write failed", "err", err) // memory is updated; the file catches up on the next write
	}
	writeJSON(w, http.StatusOK, map[string]int{"changed": n, "unread": s.inbox.UnreadWhere(keep)})
}

func (s *server) handleInboxDelete(w http.ResponseWriter, r *http.Request) {
	if s.inbox == nil {
		unavailable(w, "inbox")
		return
	}
	keep := s.visibleInbox(principalOf(r))
	ok, err := s.inbox.DeleteWhere(r.PathValue("id"), keep)
	if err != nil {
		s.log.Warn("inbox write failed", "err", err)
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no such inbox item")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unread": s.inbox.UnreadWhere(keep)})
}

// ---------- session recordings ----------

func (s *server) handleRuns(w http.ResponseWriter, r *http.Request) {
	s.listRuns(w, r.PathValue("id"))
}

func (s *server) handleRunCast(w http.ResponseWriter, r *http.Request) {
	s.serveCast(w, r, r.PathValue("id"), r.PathValue("name"))
}

// ownSession is the calling session of an endpoint about the caller's own
// recordings: an MCP client has none (403).
func (s *server) ownSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	if clientID(r) != "" {
		writeError(w, http.StatusForbidden, "this endpoint is for agents in a session; an MCP client has no workspace")
		return "", false
	}
	caller := callerID(r)
	if caller == "" {
		unavailable(w, "sessions")
		return "", false
	}
	return caller, true
}

// handleAgentRuns lists the calling session's own recordings; an agent
// never sees another session's.
func (s *server) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.ownSession(w, r)
	if !ok {
		return
	}
	s.listRuns(w, caller)
}

func (s *server) handleAgentRunCast(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.ownSession(w, r)
	if !ok {
		return
	}
	s.serveCast(w, r, caller, r.PathValue("name"))
}

func (s *server) listRuns(w http.ResponseWriter, id string) {
	if s.runs == nil {
		unavailable(w, "session recordings")
		return
	}
	runs, err := s.runs.Runs(id)
	if err != nil {
		runError(w, err)
		return
	}
	if runs == nil {
		runs = []session.RunCast{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// serveCast streams one recording. Range requests work (a player can fetch
// the head first); a live run's file is served up to its current size.
func (s *server) serveCast(w http.ResponseWriter, r *http.Request, id, name string) {
	if s.runs == nil {
		unavailable(w, "session recordings")
		return
	}
	attempt, ok := session.ParseRunName(name)
	if !ok {
		writeError(w, http.StatusNotFound, "recordings are named <attempt>.cast")
		return
	}
	f, err := s.runs.OpenRun(id, attempt)
	if err != nil {
		runError(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", castContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `inline; filename="`+safeFileName(id)+"-"+strconv.Itoa(attempt)+`.cast"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func runError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such session")
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "no such recording")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// safeFileName keeps an id usable inside a quoted header value.
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, s)
}

// ---------- downloads ----------

func (s *server) handleDownloadList(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		unavailable(w, "downloads")
		return
	}
	items := s.downloads.List()
	if items == nil {
		items = []downloads.Item{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *server) handleDownloadAdd(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		unavailable(w, "downloads")
		return
	}
	var req downloads.Request
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Ref) == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return
	}
	noteAudit(r, strings.TrimSpace(req.Engine+" "+req.Ref))
	it, err := s.downloads.Add(req)
	if err != nil {
		writeError(w, downloadStatus(err), err.Error())
		return
	}
	noteAudit(r, "", it.ID)
	writeJSON(w, http.StatusAccepted, it)
}

func downloadStatus(err error) int {
	switch {
	case errors.Is(err, downloads.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, downloads.ErrUnavailable):
		return http.StatusConflict
	case errors.Is(err, downloads.ErrNoSpace):
		return http.StatusInsufficientStorage
	case errors.Is(err, downloads.ErrClosed):
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

func (s *server) handleDownloadCancel(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		unavailable(w, "downloads")
		return
	}
	if err := s.downloads.Cancel(r.PathValue("id")); err != nil {
		writeError(w, downloadStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- leaderboard ----------

func (s *server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	if s.board == nil {
		unavailable(w, "usage")
		return
	}
	q := r.URL.Query()
	period, by, metric := orDefault(q.Get("period"), "7d"), orDefault(q.Get("by"), "session"), orDefault(q.Get("metric"), "cost")
	limit := boardDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, boardMaxLimit)
	}
	b, err := s.board(period, by, metric, limit, s.boardKeep(principalOf(r)))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, usage.ErrPeriod) || errors.Is(err, usage.ErrBy) || errors.Is(err, usage.ErrMetric) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// boardKeep limits the leaderboard to what the caller may see: everything
// for admins; every session (no chats, no direct traffic) for viewers; the
// caller's own sessions and chats for everyone else.
func (s *server) boardKeep(pr principal) func(string) bool {
	if pr.admin() || !s.hasUsers() {
		return nil
	}
	return func(id string) bool {
		if id == "" {
			return false // direct /v1 traffic is not attributed to a session
		}
		if s.sessions != nil {
			if info, ok := s.sessions.Get(id); ok {
				return s.canSeeSession(pr, info.Owner)
			}
		}
		if s.chats != nil && pr.Role != users.RoleViewer {
			if owner, ok := s.chats.Owner(id); ok {
				return s.sameOwner(pr, owner)
			}
		}
		return false
	}
}

// newBoard ranks the usage store's records, with the sessions' metadata and
// (when there is an accountant) its energy attribution and tariff.
func newBoard(u *usage.Store, sessions func() []session.Info, e *energy.Energy) boardFunc {
	return func(period, by, metric string, limit int, keep func(string) bool) (usage.Board, error) {
		var infos []usage.SessionInfo
		if sessions != nil {
			infos = boardSessions(sessions(), time.Now())
		}
		if e != nil {
			return e.Leaderboard(u, infos, period, by, metric, limit, keep)
		}
		return usage.Leaderboard(usage.Sources{Usage: u, Sessions: infos, Limit: limit, Keep: keep}, period, by, metric)
	}
}

// boardSessions maps session infos to what the leaderboard needs. The life
// is the run window: from the first run's start to the latest run's end
// (Created and Updated would count queue time and later accepts, renames
// and restarts). A session that is queued or running has no end yet; one
// that never ran has an empty life at its creation. Sessions recorded
// before the run window existed fall back to Created/Updated.
func boardSessions(list []session.Info, now time.Time) []usage.SessionInfo {
	out := make([]usage.SessionInfo, 0, len(list))
	for _, in := range list {
		label := in.Name
		if label == "" {
			label = in.Title
		}
		repo := in.Repo
		if repo == "" && in.Dir != "" && in.Workspace != session.WorkspaceScratch {
			repo = filepath.Base(in.Dir)
		}
		si := usage.SessionInfo{ID: in.ID, Label: label, Agent: in.Agent, Repo: repo, WallJ: in.EnergyJ}
		switch {
		case !in.RunStarted.IsZero():
			si.Start, si.End = in.RunStarted, in.RunEnded
			if si.End.Before(si.Start) {
				si.End = si.Start
			}
		case in.Attempt == 0:
			si.Start, si.End = in.Created, in.Created // never ran
		default:
			si.Start, si.End = in.Created, in.Updated // from before RunStarted/RunEnded
		}
		switch in.State {
		case session.StateQueued, session.StateStarting, session.StateRunning, session.StateWaiting,
			session.StateChecking, session.StateRetrying, session.StateEscalated:
			si.End = time.Time{}
			if in.RunStarted.IsZero() && in.Attempt == 0 {
				si.Start = now // still waiting for its first run: nothing has run yet
			}
		}
		if si.Start.IsZero() || si.Start.After(now) {
			si.Start = now
		}
		out = append(out, si)
	}
	return out
}

// ---------- PWA files ----------

// pwaFile is one of the root files a browser installs the app from.
type pwaFile struct {
	body  []byte
	etag  string
	ctype string
}

// pwaFiles are served at the site root, without auth: a browser fetches the
// manifest without credentials, and neither file holds anything private.
var pwaFiles = map[string]string{
	"manifest.webmanifest": "application/manifest+json",
	"sw.js":                "text/javascript; charset=utf-8",
}

func (s *server) handlePWA(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	f, ok := s.pwa[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", f.etag)
	if name == "sw.js" {
		// Only the root scope: the worker controls the whole app and no
		// narrower or wider path is ever granted.
		h.Set("Service-Worker-Allowed", "/")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(f.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(f.body)
	}
}
