package engine

import (
	"context"
	"sync"
)

// Change hints (step web-api): the Models view follows load, unload,
// death, busy and queue changes from /api/events instead of polling three
// endpoints every 10 s. A hint carries nothing; the subscriber reads
// ModelsCached. Hints are coalesced by the channel (capacity 1), so a busy
// manager never blocks on a slow subscriber.

type changeFeed struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// SubscribeChanges returns a channel that receives a hint after the model
// list may have changed, and its cancel function.
func (m *Manager) SubscribeChanges() (<-chan struct{}, func()) {
	f := &m.changes
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	if f.subs == nil {
		f.subs = map[chan struct{}]struct{}{}
	}
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subs, ch)
			f.mu.Unlock()
		})
	}
}

// changed pokes every subscriber. Safe with m.mu held (the feed's lock is
// a leaf).
func (m *Manager) changed() {
	f := &m.changes
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default: // one hint pending is enough
		}
	}
}

// ModelsCached is Models from the listing at hand: it never lists, so it
// runs no `lms` and walks no folder. Before the first listing it is Models.
func (m *Manager) ModelsCached(ctx context.Context) []ModelInfo {
	m.mu.Lock()
	listed := !m.listed.IsZero()
	m.mu.Unlock()
	if !listed {
		return m.Models(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelInfo, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.view(m.states[id]))
	}
	return out
}
