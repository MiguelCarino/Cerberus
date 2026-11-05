package engine

import "carino.systems/cerberus/internal/config"

// Live reload (day phase 4). Reload hands the manager a new config; the
// settings it applies without a restart are the idle TTL (engines.idle_ttl_sec
// and the per-model ttl_sec it reads alongside), the default model and
// allow_overcommit. Everything that shapes the backends (which engines run,
// their binaries, URLs, model folders, providers, load timeout) needs a
// restart. The config passed in must not be changed afterwards: callers hand
// over a fresh clone.

// reloader is a backend that reads live settings.
type reloader interface{ reload(cfg *config.Config) }

// Reload applies cfg's live settings and wakes the reaper so a shorter TTL
// takes effect at its next pass.
func (m *Manager) Reload(cfg *config.Config) {
	if cfg == nil {
		return
	}
	m.live.Store(cfg)
	for _, b := range m.backends {
		if r, ok := b.(reloader); ok {
			r.reload(cfg)
		}
	}
	m.wakeReaper()
}

// conf returns the live config (the one from the last Reload, else the
// startup one).
func (m *Manager) conf() *config.Config {
	if c := m.live.Load(); c != nil {
		return c
	}
	return m.cfg
}

func (b *lmStudio) reload(cfg *config.Config) { b.live.Store(cfg) }

func (b *lmStudio) conf() *config.Config {
	if c := b.live.Load(); c != nil {
		return c
	}
	return b.cfg
}

func (b *ollama) reload(cfg *config.Config) { b.live.Store(cfg) }

func (b *ollama) conf() *config.Config {
	if c := b.live.Load(); c != nil {
		return c
	}
	return b.cfg
}
