package gateway

import "carino.systems/cerberus/internal/config"

// Reload applies a new config's price table and budgets while serving (day
// phase 4 live reload). Requests already priced keep their cost; the next
// request is priced and budget-checked with the new values.
func (g *Gateway) Reload(cfg *config.Config) {
	if cfg == nil {
		return
	}
	g.SetPricing(cfg.Prices, cfg.Budgets)
}
