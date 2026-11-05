package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// Delegation: agents hand small jobs to local models.
//
//	GET  /api/agent/models          → agent.DelegateModels   (agent scope)
//	POST /api/agent/ask             → agent.DelegateResult   (agent scope; 409 not loaded, 429 busy)
//	POST /api/agent/rate            → {kept}                 (agent scope)
//	GET  /api/delegation?period=    → usage.Delegated        (owner)
//	GET  /api/sessions/{id}/delegation?period= → usage.Delegated for one session (owner)
//
// The fine-tuning routes (/api/finetune…) are in finetune.go.

const maxAskFiles = 8

// delegationSource is the part of *usage.Store the delegation summaries use.
type delegationSource interface {
	Delegated(since time.Time, session, reference string, inPerM, outPerM float64) usage.Delegated
}

// delegateState counts asks in flight (busy check).
type delegateState struct {
	inFlight atomic.Int32
}

func (s *server) delegateRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/agent/models", s.handleAgentModels)
	mux.HandleFunc("POST /api/agent/ask", s.handleAgentAsk)
	mux.HandleFunc("POST /api/agent/rate", s.handleAgentRate)
	mux.HandleFunc("GET /api/delegation", s.handleDelegation)
	mux.HandleFunc("GET /api/sessions/{id}/delegation", s.handleSessionDelegation)
	s.finetuneRoutes(mux)
}

// delegateReady checks that delegation is on and returns the caller: the
// calling session, or the MCP client ("mcp:<name>"), under which usage and
// training examples are recorded.
func (s *server) delegateReady(w http.ResponseWriter, r *http.Request) (string, *config.Config, bool) {
	caller := callerID(r)
	if caller == "" {
		caller = clientID(r)
	}
	if caller == "" {
		writeError(w, http.StatusForbidden, "delegation is for agents in a session or MCP clients")
		return "", nil, false
	}
	cfg := s.conf()
	if !cfg.Delegate.Enabled {
		writeError(w, http.StatusForbidden, "delegation to local models is off (delegate.enabled)")
		return "", nil, false
	}
	if s.models == nil || s.gateway == nil {
		unavailable(w, "models")
		return "", nil, false
	}
	return caller, cfg, true
}

// delegateModels lists the local models agents may use, best first:
// loaded before not loaded, then faster.
func (s *server) delegateModels(ctx context.Context, cfg *config.Config) []agent.DelegateModel {
	speed := map[string]float64{}
	if s.usage != nil {
		for _, a := range s.usage.Aggregate(time.Now().Add(-30 * 24 * time.Hour)) {
			speed[a.Model] = a.AvgTokPerSec
		}
	}
	var out []agent.DelegateModel
	for _, m := range s.models.Models(ctx) {
		if !m.Local || (len(cfg.Delegate.Models) > 0 && !slices.Contains(cfg.Delegate.Models, m.ID)) {
			continue
		}
		out = append(out, agent.DelegateModel{ID: m.ID, Backend: m.Backend, Loaded: m.Loaded, Ctx: m.Ctx,
			TokPerS: speed[m.ID], Tags: cfg.Delegate.Tags[m.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Loaded != out[j].Loaded {
			return out[i].Loaded
		}
		if out[i].TokPerS != out[j].TokPerS {
			return out[i].TokPerS > out[j].TokPerS
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *server) handleAgentModels(w http.ResponseWriter, r *http.Request) {
	_, cfg, ok := s.delegateReady(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, agent.DelegateModels{Guidance: cfg.Delegate.Guidance, Load: cfg.Delegate.Load,
		Models: nonNilSlice(s.delegateModels(r.Context(), cfg))})
}

// pickDelegateModel resolves the ask's model to an allowed local model, or
// says why not (status 400 for a bad pick, 409 for one that is not loaded).
func (s *server) pickDelegateModel(ctx context.Context, cfg *config.Config, want string) (agent.DelegateModel, int, error) {
	models := s.delegateModels(ctx, cfg)
	want = strings.TrimSpace(want)
	if want == "" || want == "local" {
		for _, m := range models {
			if m.Loaded {
				return m, 0, nil
			}
		}
		if cfg.Delegate.Load && len(models) > 0 {
			return models[0], 0, nil
		}
		return agent.DelegateModel{}, http.StatusConflict, errors.New("no local model is loaded")
	}
	for _, m := range models {
		if m.ID == want || strings.HasSuffix(m.ID, "/"+want) {
			if !m.Loaded && !cfg.Delegate.Load {
				return agent.DelegateModel{}, http.StatusConflict, fmt.Errorf("%s is not loaded", m.ID)
			}
			return m, 0, nil
		}
	}
	return agent.DelegateModel{}, http.StatusBadRequest,
		fmt.Errorf("%q is not a local model agents may use (see cerberus_models)", want)
}

func (s *server) handleAgentAsk(w http.ResponseWriter, r *http.Request) {
	caller, cfg, ok := s.delegateReady(w, r)
	if !ok {
		return
	}
	var b agent.DelegateAsk
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	if strings.TrimSpace(b.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if len(b.Files) > maxAskFiles {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d files per ask", maxAskFiles))
		return
	}
	m, status, err := s.pickDelegateModel(r.Context(), cfg, b.Model)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if n := s.delegate.inFlight.Add(1); int(n) > max(cfg.Delegate.MaxQueue, 1) {
		s.delegate.inFlight.Add(-1)
		writeError(w, http.StatusTooManyRequests, "the local models are busy")
		return
	}
	defer s.delegate.inFlight.Add(-1)

	user := b.Prompt
	if len(b.Files) > 0 {
		text, err := s.askFiles(caller, b.Files, cfg.Delegate.MaxFileKiB)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		user += text
	}
	msgs := []finetune.Message{}
	if strings.TrimSpace(b.System) != "" {
		msgs = append(msgs, finetune.Message{Role: "system", Content: b.System})
	}
	msgs = append(msgs, finetune.Message{Role: "user", Content: user})
	maxTok := cfg.Delegate.MaxTokens
	if b.MaxTokens > 0 {
		maxTok = min(b.MaxTokens, max(maxTok, 16)*8, 32768)
	}
	maxTok = max(maxTok, 16)

	start := time.Now()
	reply, err := s.callLocal(r.Context(), caller, m.ID, msgs, maxTok)
	if err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && (ue.status == http.StatusNotFound || ue.status == http.StatusConflict) {
			writeError(w, http.StatusConflict, ue.msg)
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	res := agent.DelegateResult{Model: m.ID, Text: reply.text, PromptTokens: reply.prompt, CompletionTokens: reply.completion,
		Seconds: time.Since(start).Seconds(), Truncated: reply.finish == "length"}
	if res.Seconds > 0 && res.CompletionTokens > 0 {
		res.TokPerS = float64(res.CompletionTokens) / res.Seconds
	}
	if rec, ok := s.lastDelegated(caller, m.ID, start); ok {
		res.EnergyJ, res.TokPerS = rec.EnergyJ, orFloat(rec.TokPerSec, res.TokPerS)
	}
	if p, ok := priceFor(cfg, cfg.Delegate.ReferenceModel); ok {
		res.SavedUSD = (float64(res.PromptTokens)*p.In + float64(res.CompletionTokens)*p.Out) / 1e6
	}
	res.ID = "ask-" + randHex(8)
	if s.finetune != nil && cfg.Finetune.Capture != config.CaptureOff && cfg.Finetune.Capture != "" {
		if id, err := s.finetune.Add(finetune.Example{Kind: finetune.KindDelegated, Model: m.ID, Session: caller,
			Messages: msgs, Answer: reply.text}); err == nil {
			res.ID = id
		} else if !errors.Is(err, finetune.ErrTooBig) {
			s.log.Warn("finetune: example not kept", "err", err)
		}
	}
	noteAudit(r, fmt.Sprintf("%s %d+%d tokens", m.ID, res.PromptTokens, res.CompletionTokens), caller)
	writeJSON(w, http.StatusOK, res)
}

// askFiles reads workspace files for an ask: inside the caller's own
// workspace (os.Root: no escape by "..", absolute paths or symlinks), text
// only, each cut at maxKiB.
func (s *server) askFiles(caller string, names []string, maxKiB int) (string, error) {
	if s.sessions == nil {
		return "", errors.New("no session workspace to read files from")
	}
	if strings.HasPrefix(caller, "mcp:") {
		return "", errors.New("an MCP client has no workspace here: cerberus mcp --client reads files on its own side and sends them as text")
	}
	info, ok := s.sessions.Get(caller)
	if !ok || info.Worktree == "" {
		return "", errors.New("this session has no workspace")
	}
	root, err := os.OpenRoot(info.Worktree)
	if err != nil {
		return "", errors.New("the workspace cannot be opened")
	}
	defer root.Close()
	limit := int64(max(maxKiB, 1)) << 10
	deny := agent.DefaultDenyPaths(s.conf())
	var b strings.Builder
	for _, name := range names {
		rel := filepath.Clean(strings.TrimSpace(name))
		if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%q: use a path inside the workspace", name)
		}
		if agent.DeniedBy(deny, filepath.Join(info.Worktree, rel)) != "" {
			return "", fmt.Errorf("%q is a protected path", name)
		}
		f, err := root.Open(rel)
		if err != nil {
			return "", fmt.Errorf("%q: %v", name, cleanFSErr(err))
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			f.Close()
			return "", fmt.Errorf("%q is not a file", name)
		}
		raw, err := io.ReadAll(io.LimitReader(f, limit+1))
		f.Close()
		if err != nil {
			return "", fmt.Errorf("%q: %v", name, err)
		}
		cut := int64(len(raw)) > limit
		if cut {
			raw = raw[:limit]
		}
		if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(bytes.ToValidUTF8(raw, nil)) {
			return "", fmt.Errorf("%q is not a text file", name)
		}
		fmt.Fprintf(&b, "\n\n--- %s", filepath.ToSlash(rel))
		if cut {
			fmt.Fprintf(&b, " (first %d KiB of %d)", maxKiB, fi.Size()>>10)
		}
		b.WriteString(" ---\n")
		b.Write(bytes.ToValidUTF8(raw, []byte("�")))
	}
	return b.String(), nil
}

func cleanFSErr(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "no such file"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	}
	return "outside the workspace or unreadable"
}

// localReply is a parsed non-streaming chat completion.
type localReply struct {
	text               string
	finish             string
	prompt, completion int
}

// upstreamError is a gateway refusal (model not loaded, unknown, …).
type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string { return e.msg }

// callLocal runs one chat completion through this daemon's own gateway, so
// it is loaded, queued, measured and recorded like any other request; the
// record carries the caller session and the delegate client.
func (s *server) callLocal(ctx context.Context, caller, model string, msgs []finetune.Message, maxTok int) (localReply, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "max_tokens": maxTok,
		"temperature": 0.2, "stream": false})
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cerberus-Session", caller)
	req.Header.Set("X-Cerberus-Client", usage.ClientDelegate)
	rec := httptest.NewRecorder()
	s.gateway.ServeHTTP(rec, req)
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK {
		msg := strings.TrimSpace(rec.Body.String())
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(out.Error, &e) == nil && e.Message != "" {
			msg = e.Message
		} else if s := ""; json.Unmarshal(out.Error, &s) == nil && s != "" {
			msg = s
		}
		st := rec.Code
		if errors.Is(ctx.Err(), context.Canceled) {
			st = 499
		}
		return localReply{}, &upstreamError{status: st, msg: msg}
	}
	if len(out.Choices) == 0 {
		return localReply{}, errors.New("the local model gave no answer")
	}
	c := out.Choices[0]
	return localReply{text: stripThink(c.Message.Content), finish: c.FinishReason,
		prompt: out.Usage.Prompt, completion: out.Usage.Completion}, nil
}

// stripThink drops a leading <think>…</think> block some local models write.
func stripThink(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "<think>") {
		if i := strings.Index(t, "</think>"); i >= 0 {
			return strings.TrimSpace(t[i+len("</think>"):])
		}
	}
	return t
}

// lastDelegated finds the usage record of an ask that just finished.
func (s *server) lastDelegated(caller, model string, since time.Time) (usage.Record, bool) {
	if s.usage == nil {
		return usage.Record{}, false
	}
	recent := s.usage.Recent(32)
	for i := len(recent) - 1; i >= 0; i-- {
		r := recent[i]
		if r.SessionID == caller && r.Client == usage.ClientDelegate && r.Model == model && !r.Time.Before(since.Add(-time.Second)) {
			return r, true
		}
	}
	return usage.Record{}, false
}

func (s *server) handleAgentRate(w http.ResponseWriter, r *http.Request) {
	caller, cfg, ok := s.delegateReady(w, r)
	if !ok {
		return
	}
	var b agent.DelegateRate
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	b.ID = strings.TrimSpace(b.ID)
	if b.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	kept := false
	if s.finetune != nil && cfg.Finetune.Capture != config.CaptureOff && cfg.Finetune.Capture != "" && strings.HasPrefix(b.ID, "ex-") {
		// Only an example this session asked for, a few times at most: an
		// agent cannot grow ratings.jsonl with made-up ids or a loop.
		if err := s.finetune.RateAgent(caller, b.ID, b.Good, b.Better, b.Note); err != nil {
			status := http.StatusBadRequest
			switch {
			case errors.Is(err, finetune.ErrNotFound):
				status = http.StatusNotFound
			case errors.Is(err, finetune.ErrNotYours):
				status = http.StatusForbidden
			case errors.Is(err, finetune.ErrRatedEnough):
				status = http.StatusTooManyRequests
			}
			writeError(w, status, err.Error())
			return
		}
		kept = true
	}
	noteAudit(r, fmt.Sprintf("rate %s good=%v", b.ID, b.Good), caller)
	writeJSON(w, http.StatusOK, map[string]bool{"kept": kept})
}

// ---------- owner views ----------

func (s *server) delegationSince(r *http.Request) (time.Time, error) {
	switch p := r.URL.Query().Get("period"); p {
	case "", "30d":
		return time.Now().Add(-30 * 24 * time.Hour), nil
	case "today":
		y, m, d := time.Now().Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.Local), nil
	case "7d":
		return time.Now().Add(-7 * 24 * time.Hour), nil
	case "month":
		y, m, _ := time.Now().Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, time.Local), nil
	default:
		return time.Time{}, errors.New("period must be today, 7d, 30d or month")
	}
}

func (s *server) delegated(w http.ResponseWriter, r *http.Request, session string) {
	if s.delegation == nil {
		unavailable(w, "usage")
		return
	}
	since, err := s.delegationSince(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.conf()
	ref := cfg.Delegate.ReferenceModel
	p, ok := priceFor(cfg, ref)
	if !ok {
		ref = ""
	}
	d := s.delegation.Delegated(since, session, ref, p.In, p.Out)
	if pr := principalOf(r); session == "" && s.hasUsers() && !pr.admin() && pr.Role != "viewer" {
		// A user sees their own sessions only: per-session rows, totals
		// over them, and no per-model split (it mixes everyone's work).
		total := usage.DelegatedRow{Key: "total"}
		var mine []usage.DelegatedRow
		for _, row := range d.BySession {
			if info, ok := s.sessionInfo(row.Key); ok && s.sameOwner(pr, info.Owner) {
				mine = append(mine, row)
				total.Requests += row.Requests
				total.PromptTokens += row.PromptTokens
				total.CompletionTokens += row.CompletionTokens
				total.EnergyJ += row.EnergyJ
				total.Seconds += row.Seconds
				total.SavedUSD += row.SavedUSD
			}
		}
		d.Total, d.BySession, d.ByModel = total, nonNil(mine), []usage.DelegatedRow{}
	}
	writeJSON(w, http.StatusOK, d)
}

// sessionInfo looks a session up (false without sessions).
func (s *server) sessionInfo(id string) (session.Info, bool) {
	if s.sessions == nil {
		return session.Info{}, false
	}
	return s.sessions.Get(id)
}

func (s *server) handleDelegation(w http.ResponseWriter, r *http.Request) { s.delegated(w, r, "") }

func (s *server) handleSessionDelegation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.sessions != nil {
		if _, ok := s.sessions.Get(id); !ok {
			writeError(w, http.StatusNotFound, "no such session")
			return
		}
	}
	s.delegated(w, r, id)
}

// ---------- helpers ----------

// priceFor finds a model's price: its own entry, else "<provider>/*".
func priceFor(cfg *config.Config, model string) (config.Price, bool) {
	if model == "" {
		return config.Price{}, false
	}
	if p, ok := cfg.Prices[model]; ok {
		return p, true
	}
	if prov, _, ok := strings.Cut(model, "/"); ok {
		p, ok := cfg.Prices[prov+"/*"]
		return p, ok
	}
	return config.Price{}, false
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orFloat(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

func nonNilSlice[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
