package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/gateway"
	"carino.systems/cerberus/internal/gpu"
)

// Models view without polling (step web-api, web-api-ops-6 and -8).
//
//	GET /api/gpu → {gpu, vram_reserve}: the GPU snapshot and the VRAM
//	reserve in effect, nothing else (no model listing, no repo scan).
//
// /api/events gains:
//   - "models" {models: [modelLive]}: the live part of every model (loaded,
//     in flight, queue, stalls, last failure), coalesced to one a second and
//     sent only when it changed. It is read from the engine's listing at
//     hand (engine.Manager.ModelsCached), so it never runs `lms` or walks a
//     folder. A model id the page does not know means it refetches
//     /api/models.
//   - "prefill" {model, session, processed, total, done}: a llama.cpp
//     request reading its prompt, coalesced to one a second per request.
//
// Both go to the roles that see models in /api/status (admin, user, chat).

// modelChanges is the engine side of the "models" event
// (*engine.Manager); the test fakes need not have it.
type modelChanges interface {
	SubscribeChanges() (<-chan struct{}, func())
	ModelsCached(ctx context.Context) []engine.ModelInfo
}

// reserveSource reports the VRAM reserve in effect (*engine.Manager).
type reserveSource interface {
	VRAMReserveNow() engine.Reserve
}

// VRAMReserve is the reserve a load leaves free below the card's top, and
// where it comes from: "railguard" (its line, line_pct), "fixed"
// (engines.vram_reserve_mib), "default" (no railguard) or "off".
type VRAMReserve struct {
	MiB     int     `json:"mib"`
	Source  string  `json:"source"`
	LinePct float64 `json:"line_pct,omitempty"`
}

// GPUStatus is GET /api/gpu.
type GPUStatus struct {
	GPU         gpu.Snapshot `json:"gpu"`
	VRAMReserve *VRAMReserve `json:"vram_reserve,omitempty"`
}

// modelLive is the part of a model that changes while it serves. Times
// are absolute, so a busy model's event does not change every second.
type modelLive struct {
	ID          string          `json:"id"`
	Loaded      bool            `json:"loaded"`
	InFlight    int             `json:"in_flight"`
	MaxParallel int             `json:"max_parallel"`
	Waiting     int             `json:"waiting"`
	WaitSince   *time.Time      `json:"wait_since,omitempty"`
	BusySince   *time.Time      `json:"busy_since,omitempty"`
	Stalls      int             `json:"stalls"`
	LastUsed    time.Time       `json:"last_used"`
	LastFailure *engine.Failure `json:"last_failure,omitempty"`
}

// modelsCoalesce is how long the event stream gathers model changes.
const modelsCoalesce = time.Second

func (s *server) vramReserve() *VRAMReserve {
	rs, ok := s.models.(reserveSource)
	if !ok {
		return nil
	}
	r := rs.VRAMReserveNow()
	if r.Source == "unknown" || r.Source == "" {
		return nil
	}
	return &VRAMReserve{MiB: r.MiB, Source: r.Source, LinePct: r.LinePct}
}

func (s *server) handleGPU(w http.ResponseWriter, r *http.Request) {
	var st GPUStatus
	if s.gpu != nil {
		st.GPU = s.gpu.Snapshot()
	}
	st.VRAMReserve = s.vramReserve()
	writeJSON(w, http.StatusOK, st)
}

// seesModels is the /api/status rule for the model list.
func (s *server) seesModels(pr principal) bool {
	if !s.hasUsers() {
		return true
	}
	role := orDefault(pr.Role, "admin")
	return role == "admin" || role == "user" || role == "chat"
}

// liveModels is the "models" event body: the live part of every local
// model, and of any other model with requests in flight.
func (s *server) liveModels(ctx context.Context, mc modelChanges) []modelLive {
	ms := mc.ModelsCached(ctx)
	var busy map[string]gateway.Busy
	if s.busy != nil {
		busy = s.busy()
	}
	now := time.Now()
	out := []modelLive{}
	for _, m := range ms {
		if !m.Local && m.InFlight == 0 {
			continue
		}
		l := modelLive{ID: m.ID, Loaded: m.Loaded, InFlight: m.InFlight, MaxParallel: m.MaxParallel,
			Waiting: m.Waiting, LastUsed: m.LastUsed, LastFailure: m.LastFailure}
		if m.Waiting > 0 && m.LongestWaitS > 0 {
			t := now.Add(-time.Duration(m.LongestWaitS * float64(time.Second))).Truncate(time.Second)
			l.WaitSince = &t
		}
		if b, ok := busy[m.ID]; ok {
			l.Stalls = b.Stalls
			if b.BusyForS > 0 {
				t := now.Add(-time.Duration(b.BusyForS * float64(time.Second))).Truncate(time.Second)
				l.BusySince = &t
			}
		}
		out = append(out, l)
	}
	return out
}

// modelEvents is the "models" part of one /api/events stream: a change
// hint starts a one-second timer; when it fires, the live list goes out if
// it differs from the last one sent.
type modelEvents struct {
	ch     <-chan struct{}
	cancel func()
	timer  *time.Timer
	flush  <-chan time.Time
	sent   []byte
}

func (s *server) startModelEvents(pr principal) *modelEvents {
	me := &modelEvents{cancel: func() {}}
	mc, ok := s.models.(modelChanges)
	if !ok || !s.seesModels(pr) {
		return me
	}
	me.ch, me.cancel = mc.SubscribeChanges()
	return me
}

func (me *modelEvents) stop() {
	me.cancel()
	if me.timer != nil {
		me.timer.Stop()
	}
}

// hint starts the coalescing timer (once per window).
func (me *modelEvents) hint() {
	if me.flush == nil {
		me.timer = time.NewTimer(modelsCoalesce)
		me.flush = me.timer.C
	}
}

// body is the event to send now, or nil when nothing changed.
func (s *server) modelEventBody(ctx context.Context, me *modelEvents) []byte {
	me.flush, me.timer = nil, nil
	mc, ok := s.models.(modelChanges)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"models": s.liveModels(ctx, mc)})
	if err != nil || string(raw) == string(me.sent) {
		return nil
	}
	me.sent = raw
	return raw
}

// prefillEvents is the "prefill" part of one /api/events stream: updates
// gather per request (model and session) and go out once a second; the
// last one of a request is Done.
type prefillEvents struct {
	ch      <-chan engine.Prefill
	cancel  func()
	pending map[[2]string]engine.Prefill
	timer   *time.Timer
	flush   <-chan time.Time
}

func (s *server) startPrefillEvents(pr principal) *prefillEvents {
	pe := &prefillEvents{cancel: func() {}, pending: map[[2]string]engine.Prefill{}}
	if s.prefillSub == nil || !s.seesModels(pr) {
		return pe
	}
	pe.ch, pe.cancel = s.prefillSub()
	return pe
}

func (pe *prefillEvents) stop() {
	pe.cancel()
	if pe.timer != nil {
		pe.timer.Stop()
	}
}

func (pe *prefillEvents) add(p engine.Prefill) {
	pe.pending[[2]string{p.Model, p.Session}] = p
	if pe.flush == nil {
		pe.timer = time.NewTimer(modelsCoalesce)
		pe.flush = pe.timer.C
	}
}

// take returns the gathered updates and empties the window.
func (pe *prefillEvents) take() []engine.Prefill {
	pe.flush, pe.timer = nil, nil
	out := make([]engine.Prefill, 0, len(pe.pending))
	for k, p := range pe.pending {
		out = append(out, p)
		delete(pe.pending, k)
	}
	return out
}

// prefillFor hides a session id the caller may not see.
func (s *server) prefillFor(pr principal, p engine.Prefill, seeSession func(string) bool) engine.Prefill {
	if p.Session != "" && !s.usageHintVisible(pr, p.Session, seeSession) {
		p.Session = ""
	}
	return p
}
