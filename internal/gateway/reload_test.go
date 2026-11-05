package gateway

import (
	"net/http"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// TestReloadBudgets proves a live budget change by behaviour: requests
// refused by a hard cap go through once Reload raises the cap.
func TestReloadBudgets(t *testing.T) {
	cfg := config.Defaults()
	cfg.Prices = map[string]config.Price{"acme/*": {In: 1, Out: 1}} // $2 per request
	cfg.Budgets = []config.Budget{{Provider: "acme", Period: "day", HardUSD: 2}}
	h := newSpendHarness(t, cfg)
	if st, body := h.post(t, "big"); st != 200 {
		t.Fatalf("first request: %d %s", st, body)
	}
	if st, _ := h.post(t, "big"); st != http.StatusTooManyRequests {
		t.Fatalf("over the cap: status %d, want 429", st)
	}
	next := cfg.Clone()
	next.Budgets[0].HardUSD = 100
	h.gw.Reload(next)
	if st, body := h.post(t, "big"); st != 200 {
		t.Fatalf("after raising the cap: %d %s", st, body)
	}
	next = next.Clone()
	next.Budgets = nil
	next.Prices = map[string]config.Price{"acme/*": {In: 10, Out: 10}}
	h.gw.Reload(next)
	if st, _ := h.post(t, "big"); st != 200 {
		t.Fatalf("budget removed: status %d", st)
	}
	if r := h.store.Recent(1)[0]; r.CostUSD < 19 {
		t.Errorf("new price not applied: cost %v", r.CostUSD)
	}
	h.gw.Reload(nil)
}
