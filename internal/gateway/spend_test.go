package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/usage"
)

// cloudSource serves "acme/big" (cloud) and "qwen" (local) from one
// upstream, and resolves IDs like the engine manager does.
type cloudSource struct {
	base     string
	acquired atomic.Int32
}

func (s *cloudSource) info(id string) (engine.ModelInfo, bool) {
	switch id {
	case "acme/big", "big":
		return engine.ModelInfo{ID: "acme/big", Backend: engine.BackendOpenAI, Name: "big"}, true
	case "qwen":
		return engine.ModelInfo{ID: "qwen", Backend: engine.BackendLlamaCpp, Local: true}, true
	}
	return engine.ModelInfo{}, false
}

func (s *cloudSource) Models(context.Context) []engine.ModelInfo { return nil }

func (s *cloudSource) Resolve(_ context.Context, id string) (engine.ModelInfo, error) {
	if m, ok := s.info(id); ok {
		return m, nil
	}
	return engine.ModelInfo{}, fmt.Errorf("%w: %s", engine.ErrUnknownModel, id)
}

func (s *cloudSource) Acquire(_ context.Context, id string) (engine.Endpoint, func(), error) {
	m, ok := s.info(id)
	if !ok {
		return engine.Endpoint{}, nil, engine.ErrUnknownModel
	}
	s.acquired.Add(1)
	return engine.Endpoint{BaseURL: s.base + "/v1", Model: m.Name, Local: m.Local, Backend: m.Backend}, func() {}, nil
}

type spendHarness struct {
	gw    *Gateway
	store *usage.Store
	srv   *httptest.Server
	hits  *atomic.Int32
	mu    sync.Mutex
	sent  []notify.Message
}

// newSpendHarness answers every request with 1M prompt tokens (half of them
// cached) and 1M completion tokens.
func newSpendHarness(t *testing.T, cfg *config.Config) *spendHarness {
	t.Helper()
	h := &spendHarness{hits: &atomic.Int32{}}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"prompt_tokens_details":{"cached_tokens":500000}}}`)
	}))
	t.Cleanup(up.Close)
	store, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h.store = store
	h.gw = newGateway(&cloudSource{base: up.URL}, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithConfig(cfg), WithNotifier(func(m notify.Message) {
			h.mu.Lock()
			h.sent = append(h.sent, m)
			h.mu.Unlock()
		}))
	h.srv = httptest.NewServer(h.gw.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *spendHarness) post(t *testing.T, model string) (int, string) {
	t.Helper()
	resp, err := http.Post(h.srv.URL+"/chat/completions", "application/json",
		strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *spendHarness) notifications() []notify.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]notify.Message(nil), h.sent...)
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostUSD(t *testing.T) {
	p := config.Price{In: 3, Out: 15, CachedIn: 0.3}
	if got := costUSD(p, 1_000_000, 500_000, 1_000_000); !approx(got, 1.5+0.15+15) {
		t.Errorf("cost = %v", got)
	}
	// No cached price: cached tokens bill at the input rate.
	if got := costUSD(config.Price{In: 2, Out: 4}, 1_000_000, 400_000, 0); !approx(got, 2) {
		t.Errorf("cost without cached price = %v", got)
	}
	// Cached beyond prompt (a confused upstream) is clamped.
	if got := costUSD(p, 10, 50, 0); !approx(got, 10*0.3/1e6) {
		t.Errorf("clamped cost = %v", got)
	}
}

func TestPriceLookupAndProvider(t *testing.T) {
	cfg := config.Defaults()
	cfg.Prices = map[string]config.Price{"acme/*": {In: 1, Out: 1}, "qwen": {In: 99, Out: 99}}
	h := newSpendHarness(t, cfg)
	if st, body := h.post(t, "big"); st != 200 {
		t.Fatalf("status %d: %s", st, body)
	}
	if st, _ := h.post(t, "qwen"); st != 200 {
		t.Fatal(st)
	}
	recs := h.store.Recent(0)
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	local, cloud := recs[0], recs[1]
	if cloud.Model != "acme/big" || cloud.Provider != "acme" || cloud.CachedTokens != 500_000 || !approx(cloud.CostUSD, 2) {
		t.Errorf("cloud record = %+v", cloud)
	}
	// Local models never cost money, even with a price entry.
	if local.Provider != engine.BackendLlamaCpp || local.CostUSD != 0 {
		t.Errorf("local record = %+v", local)
	}

	// A model-specific price wins over the provider wildcard.
	h.gw.SetPricing(map[string]config.Price{"acme/*": {In: 1, Out: 1}, "acme/big": {In: 2, Out: 2, CachedIn: 1}}, nil)
	h.post(t, "acme/big")
	if r := h.store.Recent(1)[0]; !approx(r.CostUSD, 0.5+1+2) {
		t.Errorf("specific price cost = %v", r.CostUSD)
	}
}

func TestBudgetHardCapRefusesBeforeUpstream(t *testing.T) {
	cfg := config.Defaults()
	cfg.Prices = map[string]config.Price{"acme/*": {In: 1, Out: 1}} // $2 per request
	cfg.Budgets = []config.Budget{{Provider: "acme", Period: "day", SoftUSD: 1, HardUSD: 3}}
	h := newSpendHarness(t, cfg)

	for i := range 2 {
		if st, body := h.post(t, "big"); st != 200 {
			t.Fatalf("request %d: %d %s", i, st, body)
		}
	}
	st, body := h.post(t, "big")
	if st != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429: %s", st, body)
	}
	var e struct {
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error.Type != "budget_exceeded" ||
		e.Error.Message != "acme day budget of $3.00 reached" {
		t.Errorf("error body = %s", body)
	}
	if n := h.hits.Load(); n != 2 {
		t.Errorf("upstream hits = %d, want 2 (refused request must not reach it)", n)
	}
	if r := h.store.Recent(1)[0]; r.Status != 429 || r.Provider != "acme" || r.CostUSD != 0 {
		t.Errorf("refused record = %+v", r)
	}
	// Local models are never budgeted.
	if st, _ := h.post(t, "qwen"); st != 200 {
		t.Errorf("local request refused: %d", st)
	}

	// Soft cap: crossed by the first request, announced exactly once.
	msgs := h.notifications()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "soft cap of $1.00") {
		t.Errorf("notifications = %+v", msgs)
	}

	bs := h.gw.Budgets()
	if len(bs) != 1 || !approx(bs[0].Spent, 4) || bs[0].State != "hard" {
		t.Errorf("budgets = %+v", bs)
	}
}

func TestSoftCapOncePerPeriodAndWildcard(t *testing.T) {
	cfg := config.Defaults()
	cfg.Prices = map[string]config.Price{"acme/*": {In: 1, Out: 1}}
	cfg.Budgets = []config.Budget{{Provider: "*", Period: "month", SoftUSD: 1}}
	h := newSpendHarness(t, cfg)
	now := time.Now()
	h.gw.now = func() time.Time { return now }
	for range 3 {
		h.post(t, "big")
	}
	if msgs := h.notifications(); len(msgs) != 1 || msgs[0].Title != "Cerberus: total budget" {
		t.Fatalf("notifications = %+v", msgs)
	}
	// Next month announces again, once, when spend there passes the cap.
	next := time.Date(now.Year(), now.Month()+1, 2, 12, 0, 0, 0, now.Location())
	h.gw.now = func() time.Time { return next }
	h.store.Add(usage.Record{Time: next, Model: "acme/big", Provider: "acme", CostUSD: 5})
	h.gw.checkBudgets("acme")
	h.gw.checkBudgets("acme")
	if msgs := h.notifications(); len(msgs) != 2 {
		t.Fatalf("after new period: %d notifications, want 2", len(msgs))
	}
}

func TestPeriodStart(t *testing.T) {
	loc := time.FixedZone("X", -6*3600)
	now := time.Date(2026, 11, 17, 0, 30, 0, 0, loc)
	if d, _ := periodStart("day", now); !d.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, loc)) {
		t.Errorf("day = %v", d)
	}
	if m, _ := periodStart("month", now); !m.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("month = %v", m)
	}
	if _, ok := periodStart("week", now); ok {
		t.Error("unknown period accepted")
	}
}

func TestUSDFormat(t *testing.T) {
	for v, want := range map[float64]string{0: "0.00", 3: "3.00", 0.015: "0.01", 0.000001: "0.000001", 0.0012345: "0.0012"} {
		if got := usd(v); got != want {
			t.Errorf("usd(%v) = %q, want %q", v, got, want)
		}
	}
}
