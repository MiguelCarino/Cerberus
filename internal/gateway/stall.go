package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/notify"
)

// Gateway phase G5: the stall watchdog. A connect is not a health check: an
// engine whose GPU queue hung still accepts connections, and llama.cpp
// answers /health from its HTTP thread while every slot is stuck. So while
// local requests are in flight, and only then, the gateway watches for one
// that has received nothing from upstream for StallSec. Its engine is then
// probed through a route that goes through the engine's own work queue
// (llama.cpp /slots, Ollama /api/ps, LM Studio /v1/models); two failed
// probes in a row mark it stalled: its requests fail with 503
// engine_stalled, the model is marked for reload, and a notification goes
// out. Nothing runs while no local request is in flight.

var errStalled = errors.New("engine stalled")

// probeTimeout bounds one probe; probesToStall is how many failures in a
// row mark an engine stalled.
var (
	probeTimeout  = 5 * time.Second
	probesToStall = 2
)

// flight is one local request in flight.
type flight struct {
	id     string
	ep     engine.Endpoint
	start  time.Time
	last   atomic.Int64 // unix nanos of the last upstream byte (or the send)
	cancel context.CancelCauseFunc
}

func (f *flight) touch() { f.last.Store(time.Now().UnixNano()) }

type watchdog struct {
	mu      sync.Mutex
	flights map[*flight]struct{}
	running bool
	fails   map[string]int // model → probes failed in a row
	stalls  map[string]int // model → stalls since start
	probes  atomic.Int64   // probes sent (tests: none while idle)
	stallNs atomic.Int64   // StallSec as a duration; 0 = off
}

// SetStall sets the stall threshold (Reload calls it again).
func (g *Gateway) SetStall(sec int) {
	g.wd.stallNs.Store(int64(time.Duration(max(sec, 0)) * time.Second))
}

// track registers a local request; nil when the watchdog is off.
func (g *Gateway) track(id string, ep engine.Endpoint, cancel context.CancelCauseFunc) *flight {
	if !ep.Local || g.wd.stallNs.Load() <= 0 {
		return nil
	}
	f := &flight{id: id, ep: ep, start: time.Now(), cancel: cancel}
	f.touch()
	w := &g.wd
	w.mu.Lock()
	if w.flights == nil {
		w.flights, w.fails, w.stalls = map[*flight]struct{}{}, map[string]int{}, map[string]int{}
	}
	w.flights[f] = struct{}{}
	start := !w.running
	w.running = true
	w.mu.Unlock()
	if start {
		go g.watch()
	}
	return f
}

func (g *Gateway) untrack(f *flight) {
	if f == nil {
		return
	}
	g.wd.mu.Lock()
	delete(g.wd.flights, f)
	g.wd.mu.Unlock()
}

// watch runs while local requests are in flight.
func (g *Gateway) watch() {
	w := &g.wd
	for {
		stall := time.Duration(w.stallNs.Load())
		tick := min(max(stall/4, 250*time.Millisecond), 30*time.Second)
		time.Sleep(tick)
		w.mu.Lock()
		if len(w.flights) == 0 || stall <= 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		now := time.Now().UnixNano()
		quiet := map[string][]*flight{}
		busy := map[string]bool{}
		for f := range w.flights {
			if now-f.last.Load() >= int64(stall) {
				quiet[f.id] = append(quiet[f.id], f)
			} else {
				busy[f.id] = true
			}
		}
		for id := range w.fails {
			if len(quiet[id]) == 0 || busy[id] {
				delete(w.fails, id) // output arrived: start over
			}
		}
		w.mu.Unlock()
		for id, fs := range quiet {
			if busy[id] {
				continue // another request to the same engine is getting output
			}
			ok := g.probe(fs[0].ep)
			w.mu.Lock()
			if ok {
				delete(w.fails, id)
				w.mu.Unlock()
				continue
			}
			w.fails[id]++
			if w.fails[id] < probesToStall {
				w.mu.Unlock()
				continue
			}
			delete(w.fails, id)
			w.stalls[id]++
			var victims []*flight
			for f := range w.flights {
				if f.id == id {
					victims = append(victims, f)
				}
			}
			w.mu.Unlock()
			g.stalled(id, fs[0].ep, victims, stall)
		}
	}
}

// stalled fails a stalled engine's requests and has it reloaded.
func (g *Gateway) stalled(id string, ep engine.Endpoint, victims []*flight, stall time.Duration) {
	for _, f := range victims {
		f.cancel(errStalled)
	}
	g.markCrashed(id, ep.Gen)
	g.log.Warn("local engine stalled: no output and no answer to checks; failing its requests and reloading it",
		"model", id, "requests", len(victims), "quiet_for", stall)
	if g.notifier != nil {
		g.notifier(notify.Message{Title: "Model engine stalled", Urgency: "normal", Kind: "engine", Level: "warn",
			Body: fmt.Sprintf("%s stopped responding (nothing for %s and no answer to two checks). %d request(s) failed; it will be reloaded on the next request.",
				id, stall.Round(time.Second), len(victims))})
	}
}

// probe asks the engine through a route that waits on its work queue.
// Unknown engines and routes the engine does not have count as healthy.
func (g *Gateway) probe(ep engine.Endpoint) bool {
	g.wd.probes.Add(1)
	base := strings.TrimRight(ep.BaseURL, "/")
	root := strings.TrimSuffix(base, "/v1")
	var url string
	switch ep.Backend {
	case engine.BackendLlamaCpp:
		url = root + "/slots"
	case engine.BackendOllama:
		url = root + "/api/ps"
	case engine.BackendLMStudio:
		url = base + "/models"
	default:
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return true
	}
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented:
		return true // the route is off (llama-server --no-slots): no signal
	case resp.StatusCode >= 500:
		return false
	}
	return true
}

// Busy is a model's in-flight picture for /api/status.
type Busy struct {
	BusyForS float64
	Stalls   int
}

// Busy reports, per model, how long its oldest local request has been in
// flight and how many stalls it had since start.
func (g *Gateway) Busy() map[string]Busy {
	w := &g.wd
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]Busy{}
	now := time.Now()
	for f := range w.flights {
		b := out[f.id]
		b.BusyForS = max(b.BusyForS, now.Sub(f.start).Seconds())
		out[f.id] = b
	}
	for id, n := range w.stalls {
		b := out[id]
		b.Stalls = n
		out[id] = b
	}
	return out
}

// touchReader marks a flight on every read that returns bytes.
type touchReader struct {
	r io.Reader
	f *flight
}

func (t touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.f.touch()
	}
	return n, err
}

// stalledText is the error a stalled request gets.
func stalledText(stall time.Duration) string {
	return fmt.Sprintf("the local engine stopped responding (no output for %s and no answer to checks); it will be reloaded, retry the request", stall.Round(time.Second))
}
