package api

import (
	"sync"
	"time"

	"carino.systems/cerberus/internal/usage"
)

// usageFeed turns the gateway's new usage records into refresh hints on the
// live event stream: "usage" events carrying {session}, so a view that
// shows a session's traffic (the Flow graph) can re-fetch when a delegated
// job or one of the session's own requests finishes, even though the
// session's state did not change. Only records with a session id count.
//
// Hints are lossy on purpose: a subscriber that is not reading misses some,
// and each stream coalesces them (usageCoalesce), so a busy agent never
// floods the stream. Nothing here polls; an idle feed costs nothing.
type usageFeed struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

// usageCoalesce is how long a stream gathers usage hints before sending
// them, one event per session.
const usageCoalesce = time.Second

func newUsageFeed() *usageFeed { return &usageFeed{subs: map[chan string]struct{}{}} }

// publish is the usage store's OnAdd hook.
func (f *usageFeed) publish(r usage.Record) {
	if r.SessionID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- r.SessionID:
		default: // a hint, not a record: dropping one is harmless
		}
	}
}

// Subscribe returns a channel of session ids and its cancel function.
func (f *usageFeed) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 32)
	f.mu.Lock()
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
