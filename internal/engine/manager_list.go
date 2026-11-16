package engine

import (
	"context"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
)

// listed is one model as a backend reported it.
type listed struct {
	info    ModelInfo
	backend Backend
}

// ensureFresh keeps the listing fresh. With force it re-lists all backends
// now. Without it, only the very first listing blocks: a cache older than
// 5 s is served as it is while one background listing renews it
// (stale-while-revalidate), so a request does not wait for `lms ls` and a
// GGUF walk. A stale view is safe on the request path: ensureLoaded asks
// the backend for the endpoint, and the gateway retries a "model not
// loaded" answer once. Concurrent callers share one listing.
func (m *Manager) ensureFresh(ctx context.Context, force bool) {
	m.mu.Lock()
	before := m.listed
	fresh := !before.IsZero() && m.now().Sub(before) < listCacheTTL
	m.mu.Unlock()
	if fresh && !force {
		return
	}
	if !force && !before.IsZero() {
		if m.bgList.CompareAndSwap(false, true) {
			go func() {
				defer m.bgList.Store(false)
				m.relist(context.Background(), before)
			}()
		}
		return
	}
	m.relist(ctx, before)
}

// relist lists every backend and installs the result, unless another
// listing finished after before while this one waited for its turn.
func (m *Manager) relist(ctx context.Context, before time.Time) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.mu.Lock()
	done := m.listed.After(before) // someone else listed while we waited
	m.mu.Unlock()
	if done {
		return
	}
	gens := m.generations()
	gone := m.apply(m.listAll(ctx), gens)
	m.noteVanished(gone, before)
	m.wakeReaper() // a listing may reveal loaded models the reaper should watch
}

// vanished is a model Cerberus loaded that a listing no longer shows loaded.
type vanished struct {
	st  *modelState
	gen uint64
}

// noteVanished records models that were unloaded outside Cerberus between
// the previous listing (since) and now: railguard when its history says so,
// else LM Studio's TTL, Atrium's game mode or another client.
func (m *Manager) noteVanished(gone []vanished, since time.Time) {
	now := m.now()
	for _, v := range gone {
		m.noteFailure(m.classifySince(since, v.st, v.gen, now, 0, nil, vanishedFailure(now), false))
	}
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
//
// It returns the models Cerberus loaded that the listing shows unloaded
// although their idle TTL has not passed: something else unloaded them.
// llama.cpp is left out (its exits are reported by the backend itself).
func (m *Manager) apply(all []listed, gens map[string]uint64) []vanished {
	m.mu.Lock()
	defer m.mu.Unlock()
	var gone []vanished
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
		if st.loaded != info.Loaded {
			m.loadEvents.Add(1) // loaded or unloaded behind our back
			m.changed()
			if st.loaded && st.backend == l.backend && m.vanishedLocked(st) {
				gone = append(gone, vanished{st: st, gen: st.loadSeq})
			}
		}
		st.backend, st.info, st.loaded = l.backend, info, info.Loaded
		if !st.loaded {
			st.ours = false // loaded again later by someone else: not ours
		}
	}
	for id, st := range m.states {
		if g, ok := gens[id]; !seen[id] && (!ok || g == st.gen) {
			if st.loaded {
				m.loadEvents.Add(1)
				m.changed()
			}
			st.loaded = false
			st.ours = false
			st.info.Loaded = false
		}
	}
	m.order = order
	m.listed = m.now()
	return gone
}

// vanishedLocked reports whether st, loaded by Cerberus and now missing
// from its engine's loaded list, went away early: not through its idle
// TTL (LM Studio unloads a model Cerberus loaded with --ttl on its own,
// which is normal). Caller holds m.mu.
func (m *Manager) vanishedLocked(st *modelState) bool {
	if !st.info.Local || st.loadSeq == 0 || st.inFlight > 0 {
		return false // not ours, or a request in flight will report it
	}
	if _, ok := st.backend.(exitNotifier); ok {
		return false
	}
	c := m.conf()
	ttl := c.IdleTTL(modelConfig(c, st.info.ID))
	if ttl > 0 && !st.lastUsed.IsZero() {
		slack := min(ttl/10, 30*time.Second)
		if m.now().Sub(st.lastUsed) >= ttl-slack {
			return false
		}
	}
	return true
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
