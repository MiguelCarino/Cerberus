package alerts

import "carino.systems/cerberus/internal/config"

// Reload replaces the alert thresholds and the bill currency (day phase 4
// live reload); the next sample is checked against them. Rate-limit and
// high-power state carry over.
func (a *Alerts) Reload(cfg *config.Config) {
	if a == nil || cfg == nil {
		return
	}
	a.mu.Lock()
	a.cfg, a.cur = cfg.Monitor.Alerts, cfg.Tariff.Currency
	a.mu.Unlock()
}
