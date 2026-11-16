package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/platform"
)

// Gateway phase G5: the stall watchdog. A connect is not a health check: an
// engine whose GPU queue hung still accepts connections, and llama.cpp
// answers /health from its HTTP thread while every slot is stuck. So while
// local requests are in flight, and only then, the gateway watches for one
// that has received nothing from upstream for StallSec. Its engine is then
// probed through a route that goes through the engine's own work queue
// (llama.cpp /slots, Ollama /api/ps, LM Studio /v1/models).
//
// A quiet request is not a hung engine: a non-streamed request sends
// nothing until it ends, and llama-server answers /slots only between two
// decode batches, which can take longer than a probe on a model partly on
// the CPU. So each probe is read for progress (probeOutcome):
//
//   - progress: the /slots answer changed since the last one (a counter
//     moved), or llama-server answered and used CPU time since the last
//     probe, or no slot is processing, or the engine gives no signal;
//   - busy: the probe timed out while the engine showed work (its last
//     answer had a slot processing, or llama-server's CPU time rose). It
//     neither clears nor adds a failure, up to maxBusyProbes in a row;
//   - failure: refused, reset, a 5xx, a timeout with no sign of work, or
//     the same /slots answer twice while a slot is processing and no CPU
//     time was used (the hang the old watchdog missed).
//
// A model is stalled after probesToStall failures that span at least
// StallSec: its requests fail with 503 engine_stalled, the model is marked
// for reload (without the crash path's slot cut), and a notification goes
// out. Nothing runs while no local request is in flight.

var errStalled = errors.New("engine stalled")

// probeTimeout is the shortest a probe may take (it is max(probeTimeout,
// StallSec/4)); probesToStall is how many failures mark an engine
// stalled; maxBusyProbes is how many timed-out probes in a row may count
// as busy before they count as failures (a spinning GPU hang also burns
// CPU time). cpuProgress is the CPU time llama-server must use between two
// probes to count as working.
var (
	probeTimeout  = 5 * time.Second
	probesToStall = 2
	maxBusyProbes = 8
	cpuProgress   = 100 * time.Millisecond
)

// flight is one local request in flight.
type flight struct {
	id      string
	session string // X-Cerberus-Session, for prefill hints
	ep      engine.Endpoint
	start   time.Time
	last    atomic.Int64 // unix nanos of the last upstream byte (or the send)
	cancel  context.CancelCauseFunc

	// Prompt reading (step web-api): set from prompt_progress frames until
	// the first output.
	reading   atomic.Bool
	processed atomic.Int64
	total     atomic.Int64
}

func (f *flight) touch() { f.last.Store(time.Now().UnixNano()) }

type watchdog struct {
	mu      sync.Mutex
	flights map[*flight]struct{}
	running bool
	quiet   map[string]*engineWatch // model → its probes while its requests are quiet
	stalls  map[string]int          // model → stalls since start
	probes  atomic.Int64            // probes sent (tests: none while idle)
	stallNs atomic.Int64            // StallSec as a duration; 0 = off

	// cpuTime reads a process' CPU time (platform.ProcessCPUTime; tests
	// fake it). busyMax, when >0, replaces maxBusyProbes (tests).
	cpuTime func(pid int) (time.Duration, bool)
	busyMax int
}

// engineWatch is what the probes of one quiet model have seen.
type engineWatch struct {
	fails      int       // failures since the last progress
	firstFail  time.Time // when the first of them was seen
	busy       int       // timed-out probes in a row counted as busy
	answered   bool      // a /slots answer was read (snap is set)
	snap       uint64    // a hash of the last /slots answer
	processing bool      // the last /slots answer had a slot processing
	cpu        time.Duration
	cpuOK      bool // cpu was read
	pid        int  // the process cpu belongs to
}

// probeOutcome is one probe's verdict.
type probeOutcome int

const (
	probeProgress probeOutcome = iota
	probeBusy
	probeFailed
)

// SetStall sets the stall threshold (Reload calls it again).
func (g *Gateway) SetStall(sec int) {
	g.wd.stallNs.Store(int64(time.Duration(max(sec, 0)) * time.Second))
}

// track registers a local request; nil when the watchdog is off.
func (g *Gateway) track(id, session string, ep engine.Endpoint, cancel context.CancelCauseFunc) *flight {
	if !ep.Local || g.wd.stallNs.Load() <= 0 {
		return nil
	}
	f := &flight{id: id, session: session, ep: ep, start: time.Now(), cancel: cancel}
	f.touch()
	w := &g.wd
	w.mu.Lock()
	if w.flights == nil {
		w.flights, w.quiet, w.stalls = map[*flight]struct{}{}, map[string]*engineWatch{}, map[string]int{}
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
			clear(w.quiet)
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
		for id := range w.quiet {
			if len(quiet[id]) == 0 || busy[id] {
				delete(w.quiet, id) // output arrived: start over
			}
		}
		w.mu.Unlock()
		for id, fs := range quiet {
			if busy[id] {
				continue // another request to the same engine is getting output
			}
			w.mu.Lock()
			ew := w.quiet[id]
			if ew == nil {
				ew = &engineWatch{}
				w.quiet[id] = ew
			}
			w.mu.Unlock()
			// Only this goroutine touches ew's fields.
			out := g.probe(fs[0].ep, ew, max(probeTimeout, stall/4))
			at := time.Now()
			switch out {
			case probeProgress:
				ew.fails, ew.busy = 0, 0
				continue
			case probeBusy:
				continue
			}
			if ew.fails == 0 {
				ew.firstFail = at
			}
			ew.fails++
			if ew.fails < probesToStall || at.Sub(ew.firstFail) < stall {
				continue
			}
			w.mu.Lock()
			delete(w.quiet, id)
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
	g.markStalled(id, ep.Gen)
	g.log.Warn("local engine stalled: no output and no progress in checks; failing its requests and reloading it",
		"model", id, "requests", len(victims), "quiet_for", stall)
	if g.notifier != nil {
		g.notifier(notify.Message{Title: "Model engine stalled", Urgency: "normal", Kind: "engine", Level: "warn",
			Body: fmt.Sprintf("%s stopped responding (no output for %s and no progress in checks). %d request(s) failed; it will be reloaded on the next request.",
				id, stall.Round(time.Second), len(victims))})
	}
}

// stallMarker is optionally implemented by a modelSource: MarkStalledGen
// marks a hung model for reload without the crash path's slot cut.
type stallMarker interface {
	MarkStalledGen(id string, gen uint64) bool
}

// markStalled tells the engine a model hung; a source without
// MarkStalledGen gets the crash report.
func (g *Gateway) markStalled(id string, gen uint64) {
	if sm, ok := g.src.(stallMarker); ok {
		if sm.MarkStalledGen(id, gen) {
			g.log.Warn("local engine stalled; marked for reload", "model", id)
		}
		return
	}
	g.markCrashed(id, gen)
}

// probe asks the engine through a route that waits on its work queue and
// reads the answer for progress (see the comment at the top). Unknown
// engines and routes the engine does not have count as progress: no
// signal is no reason to cut a request.
func (g *Gateway) probe(ep engine.Endpoint, ew *engineWatch, timeout time.Duration) probeOutcome {
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
		return probeProgress
	}
	worked := g.cpuWorked(ep.PID, ew)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probeProgress
	}
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	resp, err := g.client.Do(req)
	var body []byte
	if err == nil {
		body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
	if err != nil {
		if ctx.Err() == nil {
			return probeFailed // refused or reset: nothing is serving
		}
		// Timed out: the engine's loop did not get to the probe. Busy when
		// it showed work, for a while.
		worked = g.cpuWorked(ep.PID, ew) || worked
		limit := maxBusyProbes
		if g.wd.busyMax > 0 {
			limit = g.wd.busyMax
		}
		if (worked || ew.processing) && ew.busy < limit {
			ew.busy++
			return probeBusy
		}
		return probeFailed
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented:
		return probeProgress // the route is off (llama-server --no-slots): no signal
	case resp.StatusCode >= 500:
		return probeFailed
	case ep.Backend != engine.BackendLlamaCpp:
		return probeProgress // Ollama and LM Studio lists show no progress
	}
	ew.busy = 0
	processing, ok := slotsProcessing(body)
	if !ok {
		return probeProgress // not the /slots shape this reads
	}
	h := fnv.New64a()
	_, _ = h.Write(body)
	sum := h.Sum64()
	changed := !ew.answered || sum != ew.snap
	ew.answered, ew.snap, ew.processing = true, sum, processing
	if changed || !processing || worked {
		// A counter moved, nothing is being processed, or the loop that
		// answered is using CPU: the engine is working.
		return probeProgress
	}
	return probeFailed
}

// cpuWorked reads llama-server's CPU time and reports whether it rose by
// cpuProgress since the last reading. It is false when the time cannot be
// read (another OS, another engine).
func (g *Gateway) cpuWorked(pid int, ew *engineWatch) bool {
	read := g.wd.cpuTime
	if read == nil {
		read = platform.ProcessCPUTime
	}
	if pid <= 0 {
		return false
	}
	cpu, ok := read(pid)
	if !ok {
		ew.cpuOK = false
		return false
	}
	prev, had := ew.cpu, ew.cpuOK && ew.pid == pid
	ew.cpu, ew.cpuOK, ew.pid = cpu, true, pid
	return had && cpu-prev >= cpuProgress
}

// slotsProcessing reads a llama-server /slots answer: whether any slot is
// processing. ok is false when the body is not a list of slots.
func slotsProcessing(body []byte) (processing, ok bool) {
	var slots []struct {
		IsProcessing bool `json:"is_processing"`
	}
	if json.Unmarshal(body, &slots) != nil {
		return false, false
	}
	for _, s := range slots {
		processing = processing || s.IsProcessing
	}
	return processing, true
}

// Busy is a model's in-flight picture for /api/status.
type Busy struct {
	BusyForS float64
	Stalls   int
	Prefill  *engine.Prefill // the oldest request still reading its prompt
}

// Busy reports, per model, how long its oldest local request has been in
// flight and how many stalls it had since start.
func (g *Gateway) Busy() map[string]Busy {
	w := &g.wd
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]Busy{}
	now := time.Now()
	oldest := map[string]time.Time{} // model → start of the request its Prefill shows
	for f := range w.flights {
		b := out[f.id]
		b.BusyForS = max(b.BusyForS, now.Sub(f.start).Seconds())
		if f.reading.Load() {
			if t, ok := oldest[f.id]; !ok || f.start.Before(t) {
				oldest[f.id] = f.start
				b.Prefill = &engine.Prefill{Model: f.id, Session: f.session,
					Processed: int(f.processed.Load()), Total: int(f.total.Load())}
			}
		}
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
	return fmt.Sprintf("the local engine stopped responding (no output for %s and no progress in checks); it will be reloaded, retry the request", stall.Round(time.Second))
}
