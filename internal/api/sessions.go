package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/evals"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/session"
)

func (s *server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	pr := principalOf(r)
	ownerQ := r.URL.Query().Get("owner")
	out := []session.Info{}
	for _, info := range s.sessions.List() {
		if !s.canSeeSession(pr, info.Owner) {
			continue
		}
		info = s.decorate(info)
		if ownerQ != "" && info.Owner != ownerQ {
			continue
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// startRefused applies confinement and the budget to a new task for the
// caller; it writes the refusal and returns true when the start may not go.
func (s *server) startRefused(w http.ResponseWriter, r *http.Request, t *session.TaskSpec) bool {
	pr := principalOf(r)
	if ce := s.confine(r, pr, t); ce != nil {
		writeConfine(w, ce)
		return true
	}
	return s.budgetRefused(w, t.Owner)
}

// stagingRefused checks that an upload staging folder named by a task
// belongs to the caller (else it is not found).
func (s *server) stagingRefused(w http.ResponseWriter, r *http.Request, staging string) bool {
	if staging == "" || s.files == nil {
		return false
	}
	if !s.canOwn(principalOf(r), s.files.StagingOwner(staging)) {
		writeError(w, http.StatusBadRequest, "attachments: staging not found")
		return true
	}
	return false
}

func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	var spec session.TaskSpec
	if err := decodeBody(w, r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid task: "+err.Error())
		return
	}
	// Parent is set only by an agent's spawn (agentscope.go), where the
	// spawn limits apply; TaskSpec has no JSON tags, so a body could
	// otherwise name one.
	spec.Parent, spec.Client = "", ""
	spec.Owner, spec.Confined = "", false
	if s.stagingRefused(w, r, spec.Staging) || s.startRefused(w, r, &spec) {
		return
	}
	out, err := s.sessions.Create(r.Context(), spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ids := make([]string, 0, len(out))
	for _, info := range out {
		ids = append(ids, info.ID)
	}
	noteAudit(r, "agent "+orDefault(spec.Agent, "default")+": "+spec.Task, ids...) // masked and clipped by the log
	for i := range out {
		out[i] = s.decorate(out[i])
	}
	writeJSON(w, http.StatusCreated, nonNil(out))
}

func (s *server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	info, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, session.ErrNotFound.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.decorate(info))
}

func (s *server) handleSessionDiff(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	d, err := s.sessions.Diff(r.PathValue("id"))
	if err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(d))
}

// actionBody is the union of the POST bodies of the session actions.
type actionBody struct {
	Data      string `json:"data"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	Text      string `json:"text"`
	Message   string `json:"message"`
	Push      bool   `json:"push"`
	Dir       string `json:"dir"`
	Overwrite bool   `json:"overwrite"`
	Approve   *bool  `json:"approve"`
	Answer    string `json:"answer"` // approval: y | a | n | a one-line reason
	Seq       int    `json:"seq"`    // approval: the Info.AskSeq being answered
	Option    int    `json:"option"` // approval "save": an index into Info.WaitingAlways
}

// handleSessionAction serves POST /api/sessions/{id}/<action>.
func (s *server) handleSessionAction(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	id, action := r.PathValue("id"), r.PathValue("action")
	var b actionBody
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	var err error
	pr := principalOf(r)
	info, _ := s.sessions.Get(id)
	switch action {
	case "followup", "checks", "resume":
		// These start a run: the owner's budget applies.
		if s.budgetRefused(w, info.Owner) {
			return
		}
	case "accept":
		if !s.mayAccept(pr, info) {
			writeJSON(w, http.StatusForbidden, map[string]string{"code": "accept",
				"error": "You may not accept into this repo's main: save the diff or ask an admin."})
			return
		}
	case "save":
		if !pr.admin() && s.hasUsers() {
			root := ""
			if d, err := s.users.EnsureFilesDir(pr.User); err == nil {
				root = d
			}
			if root == "" || !underDir(b.Dir, root) {
				writeError(w, http.StatusForbidden, "you can save only under your own folder "+root)
				return
			}
			// Confined: written only through the folder's os.Root, so a
			// symlink planted in it cannot redirect the files.
			n, err := s.sessions.SaveInto(id, root, b.Dir, b.Overwrite)
			if err != nil {
				writeError(w, sessionErrStatus(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": n})
			return
		}
	}
	switch action {
	case "input":
		// Echo is read before the input goes in: a password prompt turns
		// echo back on as soon as Enter arrives.
		hidden := s.control == nil || s.control.InputHidden(id)
		if hidden {
			noteAudit(r, "input (hidden, "+strconv.Itoa(len(b.Data))+" bytes)")
		} else {
			noteAudit(r, "input: "+b.Data)
		}
		if err = s.sessions.Input(id, []byte(b.Data)); err == nil {
			s.recordInput(id, []byte(b.Data), hidden)
			// A terminal sends each key as its own request: join them into
			// one record per line, so vault redaction sees whole values and
			// typing does not flood the log. Failures keep their own record.
			if s.audit != nil && s.inAudit != nil {
				skipAudit(r)
				s.inAudit.add(s, r, id, b.Data, hidden)
			}
		}
	case "kill":
		s.sessionKill(w, id)
		return
	case "answer":
		if strings.TrimSpace(b.Answer) == "save" {
			// "Every time": save one of the offered rules for the session's
			// owner, then allow this call.
			before, sv, aerr := s.sessions.AnswerSave(id, b.Seq, b.Option)
			if aerr != nil {
				if sv.ID != "" {
					noteAudit(r, "approval always (saved, prompt already answered): "+sv.Text, "permit:"+sv.ID)
				}
				writeError(w, sessionErrStatus(aerr), aerr.Error())
				return
			}
			noteAudit(r, "approval always: "+sv.Text+" ("+before.WaitingAsk+")", "permit:"+sv.ID)
			s.recordInput(id, []byte(session.ApproveOnce+"\r"), false)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": sv})
			return
		}
		// An approval prompt answered from outside the terminal (the
		// session list, the bell): typed only while that prompt still waits.
		before, aerr := s.sessions.AnswerApproval(id, b.Seq, b.Answer)
		if aerr != nil {
			writeError(w, sessionErrStatus(aerr), aerr.Error())
			return
		}
		noteAudit(r, "approval "+approvalWord(b.Answer)+": "+before.WaitingAsk)
		s.recordInput(id, []byte(strings.TrimSpace(b.Answer)+"\r"), false)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	case "approve":
		s.sessionApprove(w, id, b.Approve)
		return
	case "resize":
		err = s.sessions.Resize(id, b.Cols, b.Rows)
	case "followup":
		noteAudit(r, "followup: "+b.Text)
		err = s.sessions.FollowUp(r.Context(), id, b.Text)
	case "checks":
		err = s.sessions.RunChecks(r.Context(), id)
	case "stop":
		err = s.sessions.Stop(id)
	case "discard":
		err = s.sessions.Discard(id)
	case "accept":
		noteAudit(r, "accept (push "+strconv.FormatBool(b.Push)+"): "+b.Message)
		s.accept(w, r, id, b)
		return
	case "resume":
		noteAudit(r, "resume: "+b.Text)
		s.resume(w, r, id, b.Text)
		return
	case "save":
		n, err := s.sessions.SaveTo(id, b.Dir, b.Overwrite)
		if err != nil {
			writeError(w, sessionErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": n})
		return
	default:
		writeError(w, http.StatusNotFound, "unknown session action "+action)
		return
	}
	if err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSessionArchive downloads a scratch session's files as a zip.
func (s *server) handleSessionArchive(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	id := r.PathValue("id")
	lw := &lazyZip{w: w, name: "cerberus-" + id + ".zip"}
	if err := s.sessions.Archive(id, lw); err != nil {
		if !lw.started {
			writeError(w, sessionErrStatus(err), err.Error())
			return
		}
		s.log.Warn("archive interrupted", "id", id, "err", err)
		return
	}
	lw.start() // an empty workspace still gets a (valid, empty) zip
}

// lazyZip sends the download headers only with the first byte, so an error
// before it can still be reported as JSON.
type lazyZip struct {
	w       http.ResponseWriter
	name    string
	started bool
}

func (l *lazyZip) start() {
	if l.started {
		return
	}
	l.started = true
	h := l.w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", `attachment; filename="`+l.name+`"`)
	h.Set("Cache-Control", "no-store")
	l.w.WriteHeader(http.StatusOK)
}

func (l *lazyZip) Write(p []byte) (int, error) {
	l.start()
	return l.w.Write(p)
}

// handleFS lists folders for the folder picker: GET /api/fs?path=ABS[&hidden=1].
func (s *server) handleFS(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	q := r.URL.Query()
	l, err := s.sessions.Browse(q.Get("path"), q.Get("hidden") == "1")
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, fs.ErrNotExist) {
			status = http.StatusNotFound
		} else if errors.Is(err, fs.ErrPermission) {
			status = http.StatusForbidden
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// accept runs detached from the request: a phone dropping off Wi-Fi must not
// abort a rebase or push halfway.
func (s *server) accept(w http.ResponseWriter, r *http.Request, id string, b actionBody) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), acceptTimeout)
	defer cancel()
	sha, err := s.sessions.Accept(ctx, id, session.AcceptOpts{Message: b.Message, Push: b.Push})
	switch {
	case err != nil && sha != "":
		// main was fast-forwarded and only the push failed: the session is
		// accepted, so report success with the push error as a warning.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "commit": sha, "warning": err.Error()})
	case err != nil:
		writeError(w, sessionErrStatus(err), err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "commit": sha})
	}
}

// sessionErrStatus maps a session error to a status: 404 for an unknown id,
// otherwise 409, since the failure depends on the session's state (not
// running, busy, nothing to accept, main not checked out, ...).
// handleSessionDelete is DELETE /api/sessions/{id}[?force=1]: the session
// is stopped if it runs and removed for good with Cerberus' own workspace.
// A repo worktree with unaccepted work answers 409 {error, unaccepted:
// true} unless force.
func (s *server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	id := r.PathValue("id")
	force := r.URL.Query().Get("force") == "1"
	noteAudit(r, "delete (force "+strconv.FormatBool(force)+")", id)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	if err := s.sessions.Delete(ctx, id, force); err != nil {
		if session.IsUnaccepted(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "unaccepted": true})
			return
		}
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// approvalWord is an approval answer for the audit log.
func approvalWord(a string) string {
	switch strings.TrimSpace(a) {
	case session.ApproveOnce:
		return "yes"
	case session.ApproveAlways:
		return "always"
	case session.ApproveNo:
		return "no"
	}
	return "no (with a reason)"
}

func sessionErrStatus(err error) int {
	if errors.Is(err, session.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, session.ErrNoApproval) {
		return http.StatusConflict
	}
	if errors.Is(err, session.ErrBadAnswer) || errors.Is(err, session.ErrBadOption) {
		return http.StatusBadRequest
	}
	return http.StatusConflict
}

// ---------- SSE ----------

// startSSE sends the stream headers and flushes them.
func startSSE(w http.ResponseWriter) *http.ResponseController {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	return rc
}

// sendEvent writes one SSE event and flushes it.
func sendEvent(w http.ResponseWriter, rc *http.ResponseController, event string, data []byte) error {
	if _, err := w.Write([]byte("event: " + event + "\ndata: ")); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return rc.Flush()
}

func sendPing(w http.ResponseWriter, rc *http.ResponseController) error {
	if _, err := w.Write([]byte(": ping\n\n")); err != nil {
		return err
	}
	return rc.Flush()
}

// handleSessionStream streams the terminal: one "backlog" event, then "out"
// events, each base64 bytes. It ends when the client leaves or the session's
// output channel closes (the client reconnects for a fresh backlog).
func (s *server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	backlog, ch, detach, err := s.sessions.Attach(r.PathValue("id"))
	if err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	defer detach()
	rc := startSSE(w)
	if sendEvent(w, rc, "backlog", b64(backlog)) != nil {
		return
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
		case chunk, ok := <-ch:
			if !ok || sendEvent(w, rc, "out", b64(chunk)) != nil {
				return
			}
		}
	}
}

// mayAccept reports whether the caller may accept a session into its
// repo's main: admins always; others only on a repo shared with Accept.
func (s *server) mayAccept(pr principal, info session.Info) bool {
	if pr.admin() || !s.hasUsers() {
		return true
	}
	u, ok := s.users.Get(pr.User)
	if !ok {
		return false
	}
	sh, ok := u.Share(orDefault(info.Repo, info.Dir))
	return ok && sh.Accept
}

// underDir reports whether p (absolute) is root or inside it.
func underDir(p, root string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// handleEvents is the one live event stream of the UI: session events as
// "session" (JSON session.Event), inbox changes as "inbox" (inbox.Event),
// download changes as "download" (downloads.Item) and, at most about once a
// second per session, "usage" ({session}) when a gateway request charged to
// that session finished (a refresh hint for the Flow graph). When the stream
// falls behind the inbox or the downloads, it sends "resync" ({what})
// and goes on with a fresh subscription; the client refetches that list.
// It answers 503 only when none of the sources exists.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil && s.inbox == nil && s.dlFeed == nil && s.flowFeed == nil && s.chats == nil {
		unavailable(w, "sessions")
		return
	}
	pr := principalOf(r)
	if pr.Must {
		s.pingsOnly(w, r)
		return
	}
	seeSessions := s.hasUsers() == false || pr.admin() || pr.Role == "viewer" || pr.Role == "user"
	seeSession := func(owner string) bool { return seeSessions && s.canSeeSession(pr, owner) }
	inboxKeep := s.visibleInbox(pr)
	askKeep := s.ownFilter(pr, false)
	var sessCh <-chan session.Event
	if s.sessions != nil {
		ch, cancel := s.sessions.Subscribe()
		defer cancel()
		sessCh = ch
	}
	var inboxCh <-chan inbox.Event
	inboxCancel := func() {}
	inboxGot := 0 // events since the last (re)subscription
	if s.inbox != nil {
		inboxCh, inboxCancel = s.inbox.Subscribe()
	}
	defer func() { inboxCancel() }()
	var dlCh <-chan downloads.Item
	dlCancel := func() {}
	dlGot := 0
	if s.dlFeed != nil {
		dlCh, dlCancel = s.dlFeed.Subscribe()
	}
	defer func() { dlCancel() }()
	var flowCh <-chan flows.Event
	flowCancel := func() {}
	if s.flowFeed != nil {
		flowCh, flowCancel = s.flowFeed.Subscribe()
	}
	defer func() { flowCancel() }()
	var evalCh <-chan evals.Event
	evalCancel := func() {}
	if s.evalFeed != nil {
		evalCh, evalCancel = s.evalFeed.Subscribe()
	}
	defer func() { evalCancel() }()
	// Open chat permission requests (owners with the chat on): "asks"
	// {keys} goes out when the set changes, so the bell can show them.
	var askCh <-chan struct{}
	askSent := ""
	if s.chats != nil && principalOf(r).owner() && s.conf().Chat.Enabled {
		ch, cancel := s.chats.SubscribeAsks()
		defer cancel()
		askCh = ch
		askSent = strings.Join(s.chats.AskKeysWhere(askKeep), " ")
	}

	// Usage hints, coalesced: ids gather in usePending and go out together
	// when useFlush fires (it runs only while hints are pending).
	var useCh <-chan string
	if s.usageFeed != nil {
		ch, cancel := s.usageFeed.Subscribe()
		defer cancel()
		useCh = ch
	}
	usePending := map[string]bool{}
	var useFlush <-chan time.Time
	var useTimer *time.Timer
	defer func() {
		if useTimer != nil {
			useTimer.Stop()
		}
	}()

	rc := startSSE(w)
	ping := time.NewTicker(s.ping)
	defer ping.Stop()
	send := func(event string, v any) bool {
		raw, err := json.Marshal(v)
		return err == nil && sendEvent(w, rc, event, raw) == nil
	}
	resync := func(what string) bool { return send("resync", map[string]string{"what": what}) }
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if sendPing(w, rc) != nil {
				return
			}
		case ev, ok := <-sessCh:
			if !ok {
				return
			}
			if !seeSession(ev.Session.Owner) {
				continue
			}
			ev.Session = s.decorate(ev.Session)
			if !send("session", ev) {
				return
			}
		case id, ok := <-useCh:
			if !ok {
				useCh = nil
				continue
			}
			if !s.usageHintVisible(pr, id, seeSession) {
				continue
			}
			usePending[id] = true
			if useFlush == nil {
				useTimer = time.NewTimer(usageCoalesce)
				useFlush = useTimer.C
			}
		case <-useFlush:
			useFlush, useTimer = nil, nil
			for id := range usePending {
				delete(usePending, id)
				if !send("usage", map[string]string{"session": id}) {
					return
				}
			}
		case ev, ok := <-inboxCh:
			if !ok {
				// Dropped for falling behind (it delivered its buffer
				// first): resubscribe. A channel closed with nothing in it
				// means the inbox itself closed: stop listening to it.
				inboxCancel()
				if inboxGot == 0 {
					inboxCh, inboxCancel = nil, func() {}
					continue
				}
				inboxCh, inboxCancel = s.inbox.Subscribe()
				inboxGot = 0
				if !resync("inbox") {
					return
				}
				continue
			}
			inboxGot++
			if s.hasUsers() {
				if !s.inboxOwnerVisible(pr, ev.Owner) {
					continue
				}
				ev.Unread = s.inbox.UnreadWhere(inboxKeep)
			}
			if !send("inbox", ev) {
				return
			}
		case it, ok := <-dlCh:
			if !ok {
				dlCancel()
				if dlGot == 0 {
					dlCh, dlCancel = nil, func() {}
					continue
				}
				dlCh, dlCancel = s.dlFeed.Subscribe()
				dlGot = 0
				if !resync("downloads") {
					return
				}
				continue
			}
			dlGot++
			if !pr.admin() {
				continue
			}
			if !send("download", it) {
				return
			}
		case <-askCh:
			keys := s.chats.AskKeysWhere(askKeep)
			if sig := strings.Join(keys, " "); sig != askSent {
				askSent = sig
				if !send("asks", map[string]any{"keys": nonNil(keys)}) {
					return
				}
			}
		case ev, ok := <-flowCh:
			if !ok {
				// Dropped for falling behind (the feed never closes
				// otherwise): resubscribe and tell the page to refetch.
				flowCancel()
				flowCh, flowCancel = s.flowFeed.Subscribe()
				if !resync("flows") {
					return
				}
				continue
			}
			if !s.flowVisible(pr, ev.Owner) {
				continue
			}
			if !send(flows.EventType, ev) {
				return
			}
		case ev, ok := <-evalCh:
			if !ok {
				// Dropped for falling behind: resubscribe and tell the page
				// to refetch.
				evalCancel()
				evalCh, evalCancel = s.evalFeed.Subscribe()
				if !resync("evals") {
					return
				}
				continue
			}
			if !s.canOwn(pr, ev.Owner) {
				continue
			}
			if !send(evals.EventType, ev) {
				return
			}
		}
	}
}

// usageHintVisible reports whether a usage hint (a session, chat or flow
// run id) may reach the caller: admins all; others only objects they may
// see.
func (s *server) usageHintVisible(pr principal, id string, seeSession func(string) bool) bool {
	if !s.hasUsers() || pr.admin() {
		return true
	}
	if s.sessions != nil {
		if info, ok := s.sessions.Get(id); ok {
			return seeSession(info.Owner)
		}
	}
	if o, ok := s.objectOwner(id); ok {
		return s.sameOwner(pr, o)
	}
	return false
}

// pingsOnly is /api/events for someone who must change their password
// first: pings until the request ends.
func (s *server) pingsOnly(w http.ResponseWriter, r *http.Request) {
	rc := startSSE(w)
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
		}
	}
}

func b64(p []byte) []byte {
	out := make([]byte, base64.StdEncoding.EncodedLen(len(p)))
	base64.StdEncoding.Encode(out, p)
	return out
}
