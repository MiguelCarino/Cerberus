package energy

import "carino.systems/cerberus/internal/config"

// Reload applies a new config while running (day phase 4): the tariff and
// the power model (idle, full-load, baseline and extra watts, power supply
// efficiency) take effect at the next sample and bill. The CPU power source,
// sampling intervals and history length need a restart. cfg must not be
// changed afterwards (callers pass a fresh clone).
func (e *Energy) Reload(cfg *config.Config) {
	if e == nil || cfg == nil {
		return
	}
	e.lcfg.Store(cfg)
}

// conf returns the config from the last Reload, else the startup one.
func (e *Energy) conf() *config.Config {
	if c := e.lcfg.Load(); c != nil {
		return c
	}
	return e.cfg
}
