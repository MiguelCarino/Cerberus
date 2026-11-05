package session

import "carino.systems/cerberus/internal/config"

// Live reload (day phase 4). Reload hands the manager a new config; every
// setting the manager reads is read through conf(), so the change applies
// from the next decision on: the guard (max sessions, free memory, systemd
// slice and memory cap) at the next admission pass, which Reload triggers at
// once; agent profiles, the default agent, repos and repo roots, deny paths,
// the built-in agent's settings and command policy (written into each
// launch's agent-config.json), spawn limits, the sandbox, upload settings,
// session timings and the commit identity for the next launch, check or
// accept. Runs already started keep what they started with. The admin
// token is always read from the startup config, which token rotation
// updates. cfg must not be changed afterwards: callers pass a fresh clone.
func (m *Manager) Reload(cfg *config.Config) {
	if m == nil || cfg == nil {
		return
	}
	m.live.Store(cfg)
	m.signal() // a higher session limit may admit queued sessions now
}

// conf returns the config from the last Reload, else the startup one.
func (m *Manager) conf() *config.Config {
	if c := m.live.Load(); c != nil {
		return c
	}
	return m.cfg
}
