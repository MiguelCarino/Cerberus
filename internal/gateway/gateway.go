// Package gateway is Cerberus's OpenAI-compatible /v1 endpoint. It resolves
// the requested model through the engine manager (loading it on demand),
// rewrites the model name to what the backend expects, proxies the request
// (streaming SSE line by line) and records a usage.Record per request with
// tokens, time to first token, throughput and, for local models, GPU energy.
//
// For cloud models it also prices each request from the configured price
// table (Record.CostUSD) and enforces spend budgets: a request whose
// provider is at its hard cap is refused with 429 before anything is sent
// upstream, and crossing a soft cap sends one notification per provider and
// period.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/usage"
)

// maxBody bounds request bodies (images in vision requests can be large).
const maxBody = 32 << 20

// modelSource is the slice of *engine.Manager the gateway uses (fakeable).
type modelSource interface {
	Models(ctx context.Context) []engine.ModelInfo
	Acquire(ctx context.Context, id string) (engine.Endpoint, func(), error)
}

// resolver is optionally implemented by a modelSource; it lets the gateway
// record usage under the canonical model ID rather than an alias or prefix.
type resolver interface {
	Resolve(ctx context.Context, id string) (engine.ModelInfo, error)
}

// forgetter is optionally implemented by a modelSource: Forget marks a local
// model unloaded after its engine answered "not loaded", so the gateway can
// load it and retry once.
type forgetter interface {
	Forget(id string) bool
}

// crashMarker is optionally implemented by a modelSource: MarkCrashed tells
// the engine a local model's engine failed while serving, so it is reloaded
// (with one slot fewer) on the next request.
type crashMarker interface {
	MarkCrashed(id string) bool
}

// genCrashMarker is crashMarker that also knows which load served the
// request (Endpoint.Gen), and ignores a failure of an earlier load.
type genCrashMarker interface {
	MarkCrashedGen(id string, gen uint64) bool
}

// recorder is the slice of *usage.Store the gateway uses.
type recorder interface {
	Add(r usage.Record)
}

// Gateway proxies OpenAI-compatible requests to the engine's backends.
type Gateway struct {
	src    modelSource
	energy func() float64 // cumulative joules; nil when there is no GPU monitor
	store  recorder       // nil disables recording
	log    *slog.Logger
	client *http.Client
	now    func() time.Time

	localInFlight atomic.Int64 // proxied requests to local models right now

	notifier func(notify.Message) // nil: soft caps are not announced

	priceMu  sync.Mutex
	prices   map[string]config.Price
	budgets  []config.Budget
	notified map[string]time.Time // provider|period → start of the period already announced

	exOn func() bool    // WithExchange: keep cloud answers now?
	exFn func(Exchange) // WithExchange: where they go
}

// Option configures optional gateway features.
type Option func(*Gateway)

// WithConfig takes the price table and budgets from cfg (copied; use
// SetPricing to change them while serving).
func WithConfig(cfg *config.Config) Option {
	return func(g *Gateway) {
		if cfg != nil {
			g.SetPricing(cfg.Prices, cfg.Budgets)
		}
	}
}

// WithNotifier sends soft-cap notifications through fn (e.g. a
// notify.Notifier's Send).
func WithNotifier(fn func(notify.Message)) Option {
	return func(g *Gateway) { g.notifier = fn }
}

// New builds a gateway on top of the engine manager. mon and store may be nil.
func New(eng *engine.Manager, mon *gpu.Monitor, store *usage.Store, log *slog.Logger, opts ...Option) *Gateway {
	var energy func() float64
	if mon != nil {
		energy = mon.EnergyJ
	}
	var rec recorder
	if store != nil {
		rec = store
	}
	var src modelSource
	if eng != nil {
		src = eng
	}
	return newGateway(src, energy, rec, log, opts...)
}

func newGateway(src modelSource, energy func() float64, rec recorder, log *slog.Logger, opts ...Option) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	// No client timeout: streams last as long as generation; the client's
	// request context bounds every upstream call.
	g := &Gateway{src: src, energy: energy, store: rec, log: log, client: &http.Client{},
		now: time.Now, notified: map[string]time.Time{}}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Handler serves the /v1 API; paths are relative to the /v1 mount point.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /models", g.handleModels)
	for _, p := range []string{"/chat/completions", "/completions", "/embeddings"} {
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, r *http.Request) { g.proxy(w, r, p) })
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "unknown endpoint "+r.Method+" "+r.URL.Path)
	})
	return mux
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Local   bool   `json:"local"`
	Loaded  bool   `json:"loaded"`
	Vision  bool   `json:"vision,omitempty"`
	Ctx     int    `json:"context_length,omitempty"`
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	data := []modelEntry{}
	if g.src != nil {
		for _, m := range g.src.Models(r.Context()) {
			data = append(data, modelEntry{ID: m.ID, Object: "model", OwnedBy: m.Backend,
				Local: m.Local, Loaded: m.Loaded, Vision: m.Vision, Ctx: m.Ctx})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// acquireStatus maps an engine error to an HTTP status and OpenAI error type.
func acquireStatus(err error) (int, string) {
	switch {
	case errors.Is(err, engine.ErrUnknownModel):
		return http.StatusNotFound, "model_not_found"
	case errors.Is(err, engine.ErrNoRoom):
		return http.StatusServiceUnavailable, "insufficient_vram"
	case errors.Is(err, context.Canceled):
		return statusClientClosed, "cancelled"
	default:
		return http.StatusServiceUnavailable, "model_load_failed"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the OpenAI error shape {"error":{"message","type"}}.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg, "type": typ}})
}
