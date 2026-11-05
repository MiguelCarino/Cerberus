package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
)

// The LLM chat (day phase 5): the routes over internal/chat. Only a person
// (the admin token or a paired device) reaches them; the agent scope and
// the /v1-only agent tokens never do (withAuth admits those only on
// /api/agent/* and /v1). Writes need the CSRF header with the cookie and
// are audited (never the message text: the detail names the model, mode
// and image count). Every route answers 403 while chat.enabled is off.
//
//	GET    /api/chats                      → [Summary] (newest first)
//	POST   /api/chats                      {cwd?, model?, mode?} → Chat (201)
//	GET    /api/chats/models               → picker data (models with loaded marks, defaults, modes, claude)
//	GET    /api/chats/search?q=            → [Hit]
//	GET    /api/chats/atrium               → [AtriumSession] (importable); ?id= opens one read-only → Chat
//	POST   /api/chats/atrium/{id}/continue → Chat (a copy in Cerberus)
//	GET    /api/chats/claude               → {available, sessions: [ClaudeSession]}
//	POST   /api/chats/claude/{id}/continue → Chat (resumes that claude CLI session)
//	GET    /api/chats/rules?chat=ID        → {rules}; DELETE forgets them
//	GET    /api/chats/asks                 → [OpenAsk]: every chat's open permission requests (the bell answers them)
//	GET    /api/chats/{id}                 → View (messages and live state)
//	PATCH  /api/chats/{id}                 {cwd?, model?, mode?} → Chat (for the next message)
//	DELETE /api/chats/{id}                 → {ok} (409 while it runs)
//	POST   /api/chats/{id}/messages        {text, images[], model?, mode?} → SendResult
//	GET    /api/chats/{id}/stream?from=N   → SSE of the chat's events from index N (replay, then live)
//	POST   /api/chats/{id}/answer          {req, decision: once|session|deny, note?} → {ok, rule?}
//	POST   /api/chats/{id}/stop            → {ok}
//	GET    /api/chats/{id}/rules           → {rules}; DELETE forgets them
//	POST   /api/quick                      {text, model?} → SSE: text…, then done {ok, model} or error {error}
//
// The chat stream counts toward the per-actor stream cap (its path ends in
// /stream) and ends when the credential is revoked; the quick answer is a
// POST, so it takes its stream slot itself.

// maxChatBody bounds POST /api/chats/{id}/messages: the text (≤ 256 KiB)
// and images that the browser has already scaled to ≤ 1568 px (a few MB
// each as data: URLs). internal/chat checks each image (≤ 12 MB, ≤ 8).
// A variable so tests can lower it.
var maxChatBody int64 = 64 << 20

// claudeModels are the picker's claude CLI choices ("claude" is the CLI's
// own default model).
var claudeModels = []string{"claude", "claude:opus", "claude:sonnet", "claude:haiku"}

func (s *server) chatRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/chats", s.handleChatList)
	mux.HandleFunc("POST /api/chats", s.handleChatCreate)
	mux.HandleFunc("GET /api/chats/models", s.handleChatModels)
	mux.HandleFunc("GET /api/chats/search", s.handleChatSearch)
	mux.HandleFunc("GET /api/chats/atrium", s.handleChatAtrium)
	mux.HandleFunc("POST /api/chats/atrium/{id}/continue", s.handleChatAtriumContinue)
	mux.HandleFunc("GET /api/chats/claude", s.handleChatClaude)
	mux.HandleFunc("POST /api/chats/claude/{id}/continue", s.handleChatClaudeContinue)
	mux.HandleFunc("GET /api/chats/asks", s.handleChatAsks)
	mux.HandleFunc("GET /api/chats/rules", s.handleChatRules)
	mux.HandleFunc("DELETE /api/chats/rules", s.handleChatRulesForget)
	mux.HandleFunc("GET /api/chats/{id}", s.handleChatGet)
	mux.HandleFunc("PATCH /api/chats/{id}", s.handleChatPatch)
	mux.HandleFunc("DELETE /api/chats/{id}", s.handleChatDelete)
	mux.HandleFunc("POST /api/chats/{id}/messages", s.handleChatSend)
	mux.HandleFunc("GET /api/chats/{id}/stream", s.handleChatStream)
	mux.HandleFunc("POST /api/chats/{id}/answer", s.handleChatAnswer)
	mux.HandleFunc("POST /api/chats/{id}/stop", s.handleChatStop)
	mux.HandleFunc("GET /api/chats/{id}/rules", s.handleChatRules)
	mux.HandleFunc("DELETE /api/chats/{id}/rules", s.handleChatRulesForget)
	mux.HandleFunc("POST /api/quick", s.handleQuick)
}

// chatReady answers unless the chat service is there, turned on, and the
// caller is a person.
func (s *server) chatReady(w http.ResponseWriter, r *http.Request) bool {
	if s.chats == nil {
		unavailable(w, "chat")
		return false
	}
	if !principalOf(r).owner() {
		writeError(w, http.StatusForbidden, "the LLM chat is for people, not agents")
		return false
	}
	if !s.conf().Chat.Enabled {
		writeError(w, http.StatusForbidden, chat.ErrDisabled.Error())
		return false
	}
	return true
}

// chatID resolves {id} (or ?chat= on the /api/chats/rules form) to a chat
// id, or answers 404.
func (s *server) chatID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.chatReady(w, r) {
		return "", false
	}
	id := r.PathValue("id")
	byQuery := id == ""
	if byQuery {
		id = r.URL.Query().Get("chat")
	}
	if !chat.IsID(id) {
		writeError(w, http.StatusNotFound, "no such chat "+strconv.Quote(id))
		return "", false
	}
	if byQuery {
		// The /api/chats/rules form: the owner only (the {id} routes are
		// checked by the route gate).
		if owner, ok := s.chats.Owner(id); ok && !s.sameOwner(principalOf(r), owner) {
			notFound(w)
			return "", false
		}
	}
	return id, true
}

// chatError maps internal/chat errors to statuses.
func chatError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, chat.ErrNotFound), errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, chat.ErrBusy), errors.Is(err, chat.ErrNoRequest):
		status = http.StatusConflict
	case errors.Is(err, chat.ErrDisabled):
		status = http.StatusForbidden
	case errors.Is(err, chat.ErrClosing):
		status = http.StatusServiceUnavailable
	case errors.Is(err, chat.ErrBackend):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error(), "code": "backend"})
		return
	}
	writeError(w, status, err.Error())
}

func (s *server) handleChatList(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	pr := principalOf(r)
	keep := s.ownFilter(pr, r.URL.Query().Get("all") == "1")
	out := []chat.Summary{}
	for _, c := range s.chats.List() {
		if keep != nil && !keep(c.Owner) {
			continue
		}
		if s.hasUsers() {
			c.Owner = s.ownerOf(c.Owner)
			c.OwnerName = s.ownerName(c.Owner)
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

// chatOptions strips what a role may not choose: the working folder (it is
// the user's own folder) for non-admins, and the mode for role chat.
func (s *server) chatOptions(pr principal, cwd, mode *string) {
	if pr.admin() || !s.hasUsers() {
		return
	}
	*cwd = ""
	if pr.Role == "chat" {
		*mode = ""
	}
}

// claudeRefused answers 403 backend when a non-admin picks a claude model.
func (s *server) claudeRefused(w http.ResponseWriter, pr principal, model string) bool {
	if pr.admin() || strings.TrimSpace(model) == "" || chat.BackendOf(model) != chat.BackendClaude {
		return false
	}
	chatError(w, chat.ErrBackend)
	return true
}

// handleChatAsks lists the open permission requests of every chat, so a
// page other than the chat can answer them (POST /api/chats/{id}/answer).
func (s *server) handleChatAsks(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.chats.OpenAsksWhere(s.ownFilter(principalOf(r), false)))
}

func (s *server) handleChatCreate(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	var b chat.CreateOptions
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	pr := principalOf(r)
	s.chatOptions(pr, &b.Cwd, &b.Mode)
	if s.claudeRefused(w, pr, b.Model) {
		return
	}
	b.Owner = creator(pr)
	c, err := s.chats.Create(b)
	if err != nil {
		chatError(w, err)
		return
	}
	noteAudit(r, chatDetail(c.Model, c.Mode, 0), c.ID)
	writeJSON(w, http.StatusCreated, c)
}

// ChatModels is the model picker's data (GET /api/chats/models).
type ChatModels struct {
	Models      []engine.ModelInfo `json:"models"` // every gateway model, Loaded marks the loaded ones
	Default     string             `json:"default"`
	QuickModel  string             `json:"quick_model"`
	DefaultMode string             `json:"default_mode"`
	Modes       []string           `json:"modes"`
	Claude      ChatClaude         `json:"claude"`
	Home        string             `json:"home"` // the default working folder; the UI shows paths under it as ~/…
}

// ChatClaude says whether the claude CLI backend can run, and its choices.
type ChatClaude struct {
	Available bool     `json:"available"`
	Models    []string `json:"models"`
}

func (s *server) handleChatModels(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	cfg := s.conf()
	out := ChatModels{Models: []engine.ModelInfo{}, Default: cfg.Engines.DefaultModel, QuickModel: cfg.Chat.QuickModel,
		DefaultMode: cfg.Chat.DefaultMode, Modes: config.ChatModes, Claude: ChatClaude{Models: []string{}}, Home: s.chats.Home()}
	if out.Default == "" {
		out.Default = "default"
	}
	if s.models != nil {
		out.Models = nonNil(s.withModelCost(s.models.Models(r.Context())))
	}
	if s.chats.ClaudeAvailable() {
		out.Claude = ChatClaude{Available: true, Models: claudeModels}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleChatSearch(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, nonNil(s.chats.SearchWhere(r.URL.Query().Get("q"), s.ownFilter(principalOf(r), false))))
}

func (s *server) handleChatAtrium(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	if id := r.URL.Query().Get("id"); id != "" {
		c, err := s.chats.AtriumGet(id)
		if err != nil {
			chatError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(s.chats.AtriumSessions()))
}

func (s *server) handleChatAtriumContinue(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	c, err := s.chats.ContinueAtrium(r.PathValue("id"))
	if err != nil {
		chatError(w, err)
		return
	}
	noteAudit(r, "atrium → "+c.ID, c.ID)
	writeJSON(w, http.StatusOK, c)
}

func (s *server) handleChatClaude(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": s.chats.ClaudeAvailable(),
		"sessions": nonNil(s.chats.ClaudeSessions(0))})
}

func (s *server) handleChatClaudeContinue(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	c, err := s.chats.ContinueClaude(r.PathValue("id"))
	if err != nil {
		chatError(w, err)
		return
	}
	noteAudit(r, "claude → "+c.ID, c.ID)
	writeJSON(w, http.StatusOK, c)
}

func (s *server) handleChatGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	v, err := s.chats.Get(id)
	if err != nil {
		chatError(w, err)
		return
	}
	if s.hasUsers() && v.Chat != nil {
		v.Chat.Owner = s.ownerOf(v.Chat.Owner)
		v.Chat.OwnerName = s.ownerName(v.Chat.Owner)
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleChatPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	var b chat.CreateOptions
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	pr := principalOf(r)
	s.chatOptions(pr, &b.Cwd, &b.Mode)
	if s.claudeRefused(w, pr, b.Model) {
		return
	}
	c, err := s.chats.SetOptions(id, b)
	if err != nil {
		chatError(w, err)
		return
	}
	noteAudit(r, chatDetail(b.Model, b.Mode, 0))
	writeJSON(w, http.StatusOK, c)
}

func (s *server) handleChatDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	if err := s.chats.Delete(id); err != nil {
		chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	var b chat.SendOptions
	if err := decodeBodyLimit(w, r, &b, maxChatBody); err != nil {
		status := bodyErrStatus(err)
		msg := "invalid body: " + err.Error()
		if status == http.StatusRequestEntityTooLarge {
			msg = fmt.Sprintf("the message is too big (over %d MB): scale the images down", maxChatBody>>20)
		}
		writeError(w, status, msg)
		return
	}
	pr := principalOf(r)
	cwdIgnored := ""
	s.chatOptions(pr, &cwdIgnored, &b.Mode)
	if s.claudeRefused(w, pr, b.Model) {
		return
	}
	if owner, ok := s.chats.Owner(id); ok && s.budgetRefused(w, s.ownerOf(owner)) {
		return
	}
	res, err := s.chats.Send(id, b)
	if err != nil {
		chatError(w, err)
		return
	}
	detail := chatDetail(b.Model, b.Mode, len(b.Images))
	if res.Queued {
		detail = strings.TrimSpace(detail + " queued")
	}
	if res.Switched {
		detail = strings.TrimSpace(detail + " → " + res.ID)
	}
	noteAudit(r, detail)
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	from := 0
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "from must be an event index (0 or more)")
			return
		}
		from = n
	}
	s.chats.ServeSSE(w, r, id, from)
}

func (s *server) handleChatAnswer(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	var b struct {
		Req      string `json:"req"`
		Decision string `json:"decision"`
		Note     string `json:"note"`
		Option   int    `json:"option"` // always: an index into the request's "always" list
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	if b.Req == "" {
		writeError(w, http.StatusBadRequest, "req is required")
		return
	}
	var rule *chat.Rule
	var err error
	if b.Decision == chat.DecideAlways {
		// Saved rules are own work (GET/DELETE /api/permits): the chat-only
		// role, which has no tools anyway, never saves one.
		if pr := principalOf(r); pr.Kind != "" && !classAdmits(clsWork, pr) {
			roleError(w, pr.Role)
			return
		}
		rule, err = s.chats.AnswerAlways(id, b.Req, b.Option)
	} else {
		rule, err = s.chats.Answer(id, b.Req, b.Decision, b.Note)
	}
	if err != nil {
		chatError(w, err)
		return
	}
	detail := b.Decision
	if rule != nil {
		detail += " rule " + rule.Text
	}
	noteAudit(r, detail, b.Req)
	out := map[string]any{"ok": true}
	if rule != nil {
		out["rule"] = rule
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleChatStop(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	if err := s.chats.Stop(id); err != nil {
		chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleChatRules(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	v, err := s.chats.Get(id)
	if err != nil {
		chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": id, "rules": nonNil(v.Rules)})
}

func (s *server) handleChatRulesForget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	if err := s.chats.ForgetRules(id); err != nil {
		chatError(w, err)
		return
	}
	noteAudit(r, "forget session rules", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chat": id, "rules": []chat.Rule{}})
}

// chatDetail is a write's audit detail: never the words, only the choices.
func chatDetail(model, mode string, images int) string {
	var parts []string
	if model != "" {
		parts = append(parts, "model="+model)
	}
	if mode != "" {
		parts = append(parts, "mode="+mode)
	}
	if images > 0 {
		parts = append(parts, "images="+strconv.Itoa(images))
	}
	return strings.Join(parts, " ")
}

// maxQuickText is the longest quick question (as internal/chat).
const maxQuickText = 256 << 10

// handleQuick answers POST /api/quick {text, model?} (Atrium's "??": at
// most 3 short sentences, no tools) as a plain SSE stream: "event: text"
// {"text"} pieces, then "event: done" {"ok","model"} or "event: error"
// {"error"}. A request refused before the first word gets a JSON error
// instead (400, or 502 when the model could not be reached).
func (s *server) handleQuick(w http.ResponseWriter, r *http.Request) {
	if !s.chatReady(w, r) {
		return
	}
	var b struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := decodeBodyLimit(w, r, &b, maxQuickText+4096); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	switch {
	case strings.TrimSpace(b.Text) == "":
		writeError(w, http.StatusBadRequest, "nothing to ask")
		return
	case len(b.Text) > maxQuickText:
		writeError(w, http.StatusRequestEntityTooLarge, "the question is too long")
		return
	case chat.BackendOf(b.Model) == chat.BackendClaude && strings.TrimSpace(b.Model) != "":
		writeError(w, http.StatusBadRequest, "quick answers go through the gateway: pick a gateway model, not "+b.Model)
		return
	}
	if s.budgetRefused(w, creator(principalOf(r))) {
		return
	}
	actor := principalOf(r).Actor
	if !s.conns.acquire(actor) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "too many open streams for "+actor)
		return
	}
	defer s.conns.release(actor)
	noteAudit(r, chatDetail(b.Model, "", 0))

	qs := &quickStream{w: w}
	model, err := s.chats.Quick(chat.WithOwner(r.Context(), creator(principalOf(r))), b.Text, b.Model, func(t string) { qs.send("text", map[string]string{"text": t}) })
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		msg := quickError(err)
		if !qs.started() {
			status := http.StatusBadGateway
			if errors.Is(err, chat.ErrDisabled) {
				status = http.StatusForbidden
			} else if errors.Is(err, chat.ErrEmpty) {
				status = http.StatusBadRequest
			}
			writeError(w, status, msg)
			return
		}
		qs.send("error", map[string]string{"error": msg})
		return
	}
	qs.send("done", map[string]any{"ok": true, "model": model})
}

// quickError words a quick-answer failure.
func quickError(err error) string {
	var se *agent.ServerError
	if errors.As(err, &se) && se.Status != 0 {
		return fmt.Sprintf("the server said %d: %s", se.Status, se.Message)
	}
	return err.Error()
}

// quickStream starts the SSE reply with its first event, so a failure
// before any word can still be a plain JSON error.
type quickStream struct {
	mu sync.Mutex
	w  http.ResponseWriter
	on bool
}

func (q *quickStream) started() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.on
}

func (q *quickStream) send(event string, v any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.on {
		h := q.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		q.w.WriteHeader(http.StatusOK)
		q.on = true
	}
	raw, _ := json.Marshal(v)
	fmt.Fprintf(q.w, "event: %s\ndata: %s\n\n", event, raw)
	if f, ok := q.w.(http.Flusher); ok {
		f.Flush()
	}
}
