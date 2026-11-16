package api

import (
	"net/http"
	"strings"
	"sync"
	"unicode"

	"carino.systems/cerberus/internal/inbox"
)

// "Approve once" for a tool call the agent guard held in a Cerberus
// session (SCOPE G2; as built in step web-api):
//
//	POST /api/sessions/{id}/guard/approve {item} → {ok, text}
//
// item is the bell's inbox item for the hold (Kind "guard", not answered
// yet, linked to this session; reading it does not answer it). The
// gateway stores it under the session's owner (serve's gwNotify), so the
// owner sees it in the bell. The answer is typed into the session as the user's
// next message, which starts a new turn the guard carries no flag into, so
// the call goes through. It is audited. Only the session's owner or an
// admin may send it (route class WORK and the session owner check); an
// agent token never reaches this route, so an agent cannot approve its own
// hold. Chats and outside /v1 clients have no button: the user answers in
// the chat itself.

type guardApproveBody struct {
	Item string `json:"item"`
}

const guardNameMax = 64

// guardApproveMu makes check-then-send one step: two presses of the same
// button type the answer once.
var guardApproveMu sync.Mutex

// guardApproveText is what is typed into the session for held calls.
func guardApproveText(held []string) string {
	names := make([]string, 0, len(held))
	for _, h := range held {
		if n := cleanToolName(h); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "Go ahead: run the tool call the guard held, this once."
	}
	return "Go ahead: run " + strings.Join(names, ", ") + " this once."
}

// cleanToolName keeps a tool name to one printable line: the model wrote
// it, and it is typed into a terminal.
func cleanToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= guardNameMax {
			break
		}
		if unicode.IsPrint(r) && r != ' ' && r != ' ' {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func (s *server) handleGuardApprove(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	if s.inbox == nil {
		unavailable(w, "inbox")
		return
	}
	id := r.PathValue("id")
	var b guardApproveBody
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	if b.Item == "" {
		writeError(w, http.StatusBadRequest, `give "item"`)
		return
	}
	if _, ok := s.sessions.Get(id); !ok {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	guardApproveMu.Lock()
	defer guardApproveMu.Unlock()
	visible := s.visibleInbox(principalOf(r))
	var it inbox.Item
	found := false
	for _, x := range s.inbox.ListWhere(func(x inbox.Item) bool {
		return x.ID == b.Item && (visible == nil || visible(x))
	}, false, 1) {
		it, found = x, true
	}
	if !found || it.Kind != "guard" || it.Link != "#/s/"+id {
		writeError(w, http.StatusNotFound, "no held call for this session")
		return
	}
	if it.Answered {
		writeError(w, http.StatusConflict, "this held call was already answered")
		return
	}
	text := guardApproveText(it.Held)
	noteAudit(r, "guard approve once: "+text)
	if err := s.sessions.Input(id, []byte(text+"\r")); err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	s.recordInput(id, []byte(text+"\r"), false)
	if _, err := s.inbox.MarkAnswered(it.ID, visible); err != nil {
		s.log.Warn("inbox write failed", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": text})
}
