package gateway

import (
	"encoding/json"
	"sync"

	"carino.systems/cerberus/internal/engine"
)

// Prompt reading progress (G5 "reading prompt P/T"; step web-api). The
// gateway already asks llama.cpp for prompt_progress frames on every
// streamed local request (the stall watchdog reads them as output). Each
// frame now also updates the request's flight, which Busy reports per
// model, and goes out to subscribers (the API's /api/events "prefill"
// hint, coalesced there). Only llama.cpp sends them; a request with the
// stall watchdog off has no flight and so no progress.

// prefillFeed fans prefill updates out to subscribers without blocking.
type prefillFeed struct {
	mu   sync.Mutex
	subs map[chan engine.Prefill]struct{}
}

// SubscribePrefill returns a channel of prompt reading updates and its
// cancel function. Updates are hints: a subscriber that is not reading
// misses some (the last one per request is Done).
func (g *Gateway) SubscribePrefill() (<-chan engine.Prefill, func()) {
	f := &g.prefill
	ch := make(chan engine.Prefill, 64)
	f.mu.Lock()
	if f.subs == nil {
		f.subs = map[chan engine.Prefill]struct{}{}
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

func (g *Gateway) publishPrefill(p engine.Prefill) {
	f := &g.prefill
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

// progressFrame is llama.cpp's prompt_progress object.
type progressFrame struct {
	Total     int `json:"total"`
	Cache     int `json:"cache"`
	Processed int `json:"processed"`
}

// noteProgress records one prompt_progress frame on the request's flight.
func (g *Gateway) noteProgress(f *flight, raw json.RawMessage) {
	if f == nil {
		return
	}
	var p progressFrame
	if json.Unmarshal(raw, &p) != nil || p.Total <= 0 || p.Processed < 0 {
		return
	}
	done := min(p.Processed, p.Total)
	f.processed.Store(int64(done))
	f.total.Store(int64(p.Total))
	f.reading.Store(true)
	g.publishPrefill(engine.Prefill{Model: f.id, Session: f.session, Processed: done, Total: p.Total})
}

// endPrefill ends a flight's prompt reading, once: the first output came,
// or the request ended.
func (g *Gateway) endPrefill(f *flight) {
	if f == nil || !f.reading.CompareAndSwap(true, false) {
		return
	}
	g.publishPrefill(engine.Prefill{Model: f.id, Session: f.session, Processed: int(f.total.Load()), Total: int(f.total.Load()), Done: true})
}
