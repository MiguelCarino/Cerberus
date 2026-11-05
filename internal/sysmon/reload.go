package sysmon

import (
	"slices"

	"carino.systems/cerberus/internal/config"
)

// Reload applies a new config while running (day phase 4). Only the
// protected process names apply live; whether the monitor runs and its
// sampling intervals need a restart.
func (m *Monitor) Reload(cfg *config.Config) {
	if m == nil || cfg == nil {
		return
	}
	p := slices.Clone(cfg.Monitor.Protected)
	m.protected.Store(&p)
}

// protectedNames returns the live protected list (Reload's, else the
// startup config's).
func (m *Monitor) protectedNames() []string {
	if p := m.protected.Load(); p != nil {
		return *p
	}
	return m.cfg.Protected
}
