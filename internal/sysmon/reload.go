package sysmon

import (
	"slices"

	"carino.systems/cerberus/internal/config"
)

// Reload applies a new config while running (day phase 4). The protected
// process names and the sessions slice (process grouping) apply live;
// whether the monitor runs and its sampling intervals need a restart.
func (m *Monitor) Reload(cfg *config.Config) {
	if m == nil || cfg == nil {
		return
	}
	p := slices.Clone(cfg.Monitor.Protected)
	m.protected.Store(&p)
	sl := cfg.Guard.SystemdSlice
	m.slice.Store(&sl)
}

// sessionSlice is the slice sessions run in ("" none).
func (m *Monitor) sessionSlice() string {
	if p := m.slice.Load(); p != nil {
		return *p
	}
	return ""
}

// protectedNames returns the live protected list (Reload's, else the
// startup config's).
func (m *Monitor) protectedNames() []string {
	if p := m.protected.Load(); p != nil {
		return *p
	}
	return m.cfg.Protected
}
