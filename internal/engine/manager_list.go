package engine

import (
	"context"
	"sync"

	"carino.systems/cerberus/internal/config"
)

// listed is one model as a backend reported it.
type listed struct {
	info    ModelInfo
	backend Backend
}

// ensureFresh re-lists all backends when the cache is older than 5 s (or
// always with force). Concurrent callers share one listing.
func (m *Manager) ensureFresh(ctx context.Context, force bool) {
	m.mu.Lock()
	before := m.listed
	fresh := !before.IsZero() && m.now().Sub(before) < listCacheTTL
	m.mu.Unlock()
	if fresh && !force {
		return
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.mu.Lock()
	done := m.listed.After(before) // someone else listed while we waited
	m.mu.Unlock()
	if done {
		return
	}
	gens := m.generations()
	m.apply(m.listAll(ctx), gens)
	m.wakeReaper() // a listing may reveal loaded models the reaper should watch
}

// listAll asks every available backend in parallel. A backend whose listing
// fails keeps its previous models so a hiccup doesn't hide them.
func (m *Manager) listAll(ctx context.Context) []listed {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	results := make([][]listed, len(m.backends))
	var wg sync.WaitGroup
	for i, b := range m.backends {
		if !b.Available() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			infos, err := b.List(ctx)
			if err != nil {
				m.log.Debug("model list failed", "backend", b.Name(), "err", err)
				results[i] = m.previous(b)
				return
			}
			for _, info := range infos {
				results[i] = append(results[i], listed{info: info, backend: b})
			}
		}()
	}
	wg.Wait()
	var out []listed
	for _, r := range results {
		out = append(out, r...)
	}
	return out
}

func (m *Manager) previous(b Backend) []listed {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []listed
	for _, id := range m.order {
		if st := m.states[id]; st.backend == b {
			out = append(out, listed{info: st.info, backend: b})
		}
	}
	return out
}

// generations records each model's load generation before a listing starts.
func (m *Manager) generations() map[string]uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	gens := make(map[string]uint64, len(m.states))
	for id, st := range m.states {
		gens[id] = st.gen
	}
	return gens
}

// apply installs a new listing: overrides by ID, first backend wins a
// duplicate ID, and models no longer listed count as unloaded. A model we
// loaded or unloaded while the listing ran (its generation moved since gens)
// keeps our loaded flag: the listing predates that change.
func (m *Manager) apply(all []listed, gens map[string]uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order := make([]string, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, l := range all {
		info := withOverride(l.info, m.cfg.Models)
		if info.ID == "" || seen[info.ID] {
			continue
		}
		seen[info.ID] = true
		order = append(order, info.ID)
		st := m.states[info.ID]
		if st == nil {
			st = &modelState{}
			m.states[info.ID] = st
		}
		if g, ok := gens[info.ID]; ok && g != st.gen {
			info.Loaded = st.loaded
		}
		st.backend, st.info, st.loaded = l.backend, info, info.Loaded
	}
	for id, st := range m.states {
		if g, ok := gens[id]; !seen[id] && (!ok || g == st.gen) {
			st.loaded = false
			st.info.Loaded = false
		}
	}
	m.order = order
	m.listed = m.now()
}

// withOverride applies the ModelConfig with the same ID (and backend, if set).
func withOverride(info ModelInfo, models []config.ModelConfig) ModelInfo {
	for _, mc := range models {
		if mc.ID != info.ID || (mc.Backend != "" && mc.Backend != info.Backend) {
			continue
		}
		if mc.Name != "" {
			info.Name = mc.Name
		}
		if mc.Ctx > 0 {
			info.Ctx = mc.Ctx
		}
		if mc.SizeMiB > 0 {
			info.SizeMiB = mc.SizeMiB
		}
		if mc.Vision {
			info.Vision = true
		}
		break
	}
	return info
}
