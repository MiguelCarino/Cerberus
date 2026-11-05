package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"carino.systems/cerberus/internal/agent"
)

// quickSystem is Atrium's QUICK_SYSTEM, for the web.
const quickSystem = "You give quick answers. Answer in at most 3 short sentences, plain text: no markdown, " +
	"no lists, no headings."

// Quick answers one question without tools (Atrium's launcher "??"): at
// most 3 short sentences, max_tokens 400, temperature 0.3, through the
// gateway. onText gets the words as they stream; the returned model is the
// one that answered.
func (m *Manager) Quick(ctx context.Context, text, model string, onText func(string)) (string, error) {
	cfg := m.cfg()
	if !cfg.Chat.Enabled {
		return "", ErrDisabled
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrEmpty
	}
	if len(text) > maxTextBytes {
		return "", fmt.Errorf("the question is too long")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = cfg.Chat.QuickModel
	}
	if model == "" {
		model = m.defaultModel(cfg)
	}
	if BackendOf(model) == BackendClaude {
		return "", fmt.Errorf("quick answers go through the gateway: %s is a claude CLI model", model)
	}
	base, key := m.gateway(cfg)
	temp := 0.3
	res := agent.Run(ctx, agent.RunOptions{
		Base: base, Key: key, Model: model, Client: "cerberus-quick", Cwd: m.o.Home, Mode: agent.ModePlan, Owner: OwnerFrom(ctx),
		Messages: []agent.Message{{Role: "user", Content: text}}, NoTools: true, MaxSteps: 0,
		MaxTokens: 400, Temperature: &temp, System: func(bool) string { return quickSystem },
	}, func(ev agent.Event) {
		if ev.Type == agent.EvText && onText != nil {
			onText(ev.Text)
		}
	})
	return model, res.Err
}

// ServeQuick answers POST /api/quick {text, model?} as a plain SSE stream:
// "event: text" with {"text"} pieces, then "event: done" {"ok","model"} or
// "event: error" {"error"}.
func (m *Manager) ServeQuick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTextBytes+4096)).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		http.Error(w, `{"error":"nothing to ask"}`, http.StatusBadRequest)
		return
	}
	fl, _ := w.(http.Flusher)
	sseHeaders(w)
	send := func(event string, v any) {
		raw, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		if fl != nil {
			fl.Flush()
		}
	}
	model, err := m.Quick(r.Context(), body.Text, body.Model, func(t string) { send("text", map[string]string{"text": t}) })
	if err != nil {
		if r.Context().Err() == nil {
			send("error", map[string]string{"error": describe(err)})
		}
		return
	}
	send("done", map[string]any{"ok": true, "model": model})
}

func describe(err error) string {
	var se *agent.ServerError
	if errors.As(err, &se) && se.Status != 0 {
		return fmt.Sprintf("the server said %d: %s", se.Status, se.Message)
	}
	return err.Error()
}

func sseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// keepAlive is how often an idle stream sends a comment, so proxies and
// phones keep the connection.
const keepAlive = 25 * time.Second

// ServeSSE streams a chat's events (GET /api/chats/{id}/stream): first the
// log from index since (the Last-Event-ID header wins when it is later),
// then live events. Each is "id: N", "event: TYPE", "data: JSON". The
// first event is "hello" {epoch, base, next} without an id (see Hello). The
// request counts as a watcher while it is open. When the subscriber falls
// behind, the stream ends; the browser reconnects from its last id.
func (m *Manager) ServeSSE(w http.ResponseWriter, r *http.Request, id string, since int) {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil && n+1 > since {
			since = n + 1
		}
	}
	backlog, ch, cancel, hello, err := m.subscribe(id, since)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, `{"error":"no such chat"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	defer cancel()
	fl, _ := w.(http.Flusher)
	sseHeaders(w)
	write := func(ev Event) bool {
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Index, ev.Type, ev.Raw); err != nil {
			return false
		}
		return true
	}
	fmt.Fprint(w, "retry: 2000\n\n")
	// No id: line, so the browser's Last-Event-ID stays the last real event.
	if raw, err := json.Marshal(hello); err == nil {
		fmt.Fprintf(w, "event: hello\ndata: %s\n\n", raw)
	}
	for _, ev := range backlog {
		if !write(ev) {
			return
		}
	}
	if fl != nil {
		fl.Flush()
	}
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !write(ev) {
				return
			}
			// Drain what is already queued before flushing: fewer writes
			// while a model streams fast.
			for n := len(ch); n > 0; n-- {
				ev, ok := <-ch
				if !ok || !write(ev) {
					return
				}
			}
			if fl != nil {
				fl.Flush()
			}
		case <-tick.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

type ownerKey struct{}

// WithOwner marks ctx as acting for a user (Phase 4a): Quick charges its
// gateway call to that user.
func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

// OwnerFrom is the user WithOwner put in ctx ("").
func OwnerFrom(ctx context.Context) string {
	o, _ := ctx.Value(ownerKey{}).(string)
	return o
}
