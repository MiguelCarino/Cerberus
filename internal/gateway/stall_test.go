package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/notify"
)

// slotsFn answers the fake engine's /slots; release closes when the test
// lets the stream end.
type slotsFn func(w http.ResponseWriter, r *http.Request, release <-chan struct{})

// slotsHang is a wedged engine: /slots never answers.
func slotsHang(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
	select {
	case <-release:
	case <-r.Context().Done():
	}
}

// slotsCounting is a slow engine still decoding: each answer shows a
// counter one higher.
func slotsCounting() slotsFn {
	var n atomic.Int64
	return func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
		fmt.Fprintf(w, `[{"id":0,"is_processing":true,"next_token":[{"n_decoded":%d}]}]`, n.Add(1))
	}
}

// slotsFrozen is a hang the engine still answers for: a slot processing,
// and nothing moving.
func slotsFrozen(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
	_, _ = io.WriteString(w, `[{"id":0,"is_processing":true,"next_token":[{"n_decoded":7}]}]`)
}

// stallHarness: a local engine that streams one chunk and then goes quiet
// until released; slots answers its /slots.
func stallHarness(t *testing.T, slots slotsFn) (*Gateway, *crashingSource, func(), func() []notify.Message) {
	t.Helper()
	oldTimeout := probeTimeout
	probeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { probeTimeout = oldTimeout })
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	gw, src, _ := crashGateway(t, true, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots" {
			slots(w, r, release)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" end\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	var mu sync.Mutex
	var msgs []notify.Message
	gw.notifier = func(m notify.Message) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() }
	gw.wd.stallNs.Store(int64(200 * time.Millisecond))
	gw.wd.cpuTime = func(int) (time.Duration, bool) { return 0, false }
	get := func() []notify.Message {
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			out := append([]notify.Message(nil), msgs...)
			mu.Unlock()
			if len(out) > 0 || time.Now().After(deadline) {
				return out
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return gw, src, free, get
}

// A wedged engine (no output, /slots never answers, no CPU time used) is
// stalled: one notification, a reload without the crash path's slot cut.
func TestStalledEngineFailsAndReloads(t *testing.T) {
	gw, src, _, msgs := stallHarness(t, slotsHang)
	src.pid = 4242
	gw.wd.cpuTime = func(int) (time.Duration, bool) { return time.Second, true } // flat
	done := make(chan string, 1)
	go func() { done <- postChat(gw, `{"model":"qwen","stream":true}`).Body.String() }()
	var body string
	select {
	case body = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled request never ended")
	}
	if !strings.Contains(body, "start") || !strings.Contains(body, "engine_stalled") {
		t.Errorf("body %q", body)
	}
	if src.stalled.Load() != 1 || src.crashed.Load() != 0 {
		t.Errorf("MarkStalledGen %d times, MarkCrashed %d times: want 1, 0", src.stalled.Load(), src.crashed.Load())
	}
	if m := msgs(); len(m) != 1 || !strings.Contains(m[0].Body, "qwen") {
		t.Errorf("notifications %+v", m)
	}
	if b := gw.Busy()["qwen"]; b.Stalls != 1 {
		t.Errorf("busy %+v", b)
	}
}

// A source without MarkStalledGen still gets the crash report.
func TestStallFallsBackToCrashReport(t *testing.T) {
	gw, _, _, _ := stallHarness(t, slotsHang)
	plain := &plainCrashSource{crashingSource: gw.src.(*crashingSource)}
	gw.src = plain
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if !strings.Contains(body, "engine_stalled") || plain.crashed.Load() != 1 {
		t.Errorf("crashed %d body %q", plain.crashed.Load(), body)
	}
}

// plainCrashSource hides crashingSource's MarkStalledGen.
type plainCrashSource struct{ *crashingSource }

func (p *plainCrashSource) MarkStalledGen() {}

func TestStalledTranslatedStreamEndsWithError(t *testing.T) {
	gw, _, _, _ := stallHarness(t, slotsHang)
	rec := postMessages(gw, `{"model":"qwen","max_tokens":5,"stream":true,"messages":[]}`)
	if b := rec.Body.String(); !strings.Contains(b, "event: error") || !strings.Contains(b, "stopped responding") {
		t.Errorf("translated stream %q", b)
	}
}

// A slow engine whose /slots counters move is never stalled.
func TestSlowButAliveIsNotStalled(t *testing.T) {
	gw, src, release, _ := stallHarness(t, slotsCounting())
	go func() { time.Sleep(900 * time.Millisecond); release() }()
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if !strings.Contains(body, " end") || strings.Contains(body, "engine_stalled") {
		t.Errorf("a slow engine that answers checks was cut off: %q", body)
	}
	if src.crashed.Load() != 0 || src.stalled.Load() != 0 {
		t.Error("marked crashed or stalled")
	}
	if gw.wd.probes.Load() == 0 {
		t.Error("never probed a quiet request")
	}
}

// An engine that answers /slots with a slot processing and nothing moving
// (and no CPU time used) is hung: the old watchdog took any answer as
// healthy.
func TestFrozenSlotsAreStalled(t *testing.T) {
	gw, src, _, msgs := stallHarness(t, slotsFrozen)
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if !strings.Contains(body, "engine_stalled") || src.stalled.Load() != 1 {
		t.Errorf("stalled %d body %q", src.stalled.Load(), body)
	}
	if m := msgs(); len(m) != 1 {
		t.Errorf("notifications %+v", m)
	}
}

// The same frozen answer, but llama-server keeps using CPU time: it is
// working on something the counters do not show (a long prompt read).
func TestFrozenSlotsWithCPUTimeAreBusy(t *testing.T) {
	gw, src, release, _ := stallHarness(t, slotsFrozen)
	src.pid = 4242
	var cpu atomic.Int64
	gw.wd.cpuTime = func(pid int) (time.Duration, bool) {
		if pid != 4242 {
			t.Errorf("cpu time of pid %d", pid)
		}
		return time.Duration(cpu.Add(int64(time.Second))), true
	}
	go func() { time.Sleep(900 * time.Millisecond); release() }()
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if !strings.Contains(body, " end") || src.stalled.Load() != 0 {
		t.Errorf("stalled %d body %q", src.stalled.Load(), body)
	}
}

// llama-server is inside a long batch: /slots answered once with a slot
// processing, then times out while the process uses CPU time. That is
// busy, not dead.
func TestLongBatchIsBusyNotStalled(t *testing.T) {
	var answered atomic.Bool
	gw, src, release, _ := stallHarness(t, func(w http.ResponseWriter, r *http.Request, rel <-chan struct{}) {
		if answered.CompareAndSwap(false, true) {
			slotsFrozen(w, r, rel)
			return
		}
		slotsHang(w, r, rel) // a batch longer than the probe timeout
	})
	src.pid = 4242
	var cpu atomic.Int64
	gw.wd.cpuTime = func(int) (time.Duration, bool) { return time.Duration(cpu.Add(int64(time.Second))), true }
	go func() { time.Sleep(1200 * time.Millisecond); release() }()
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if !strings.Contains(body, " end") || src.stalled.Load() != 0 || src.crashed.Load() != 0 {
		t.Errorf("stalled %d crashed %d body %q", src.stalled.Load(), src.crashed.Load(), body)
	}
	if gw.wd.probes.Load() < 3 {
		t.Errorf("%d probes: the busy path was not exercised", gw.wd.probes.Load())
	}
}

// Busy has a bound: a spinning GPU hang also burns CPU time.
func TestBusyProbesAreBounded(t *testing.T) {
	gw, src, _, _ := stallHarness(t, slotsHang)
	gw.wd.busyMax = 1
	src.pid = 4242
	var cpu atomic.Int64
	gw.wd.cpuTime = func(int) (time.Duration, bool) { return time.Duration(cpu.Add(int64(time.Second))), true }
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if !strings.Contains(body, "engine_stalled") || src.stalled.Load() != 1 {
		t.Errorf("stalled %d body %q", src.stalled.Load(), body)
	}
}

// Failures must span StallSec: two quick failures are not enough.
func TestStallNeedsFailuresToSpanStallSec(t *testing.T) {
	gw, src, release, _ := stallHarness(t, slotsHang)
	gw.wd.stallNs.Store(int64(700 * time.Millisecond)) // tick 250 ms, failures every ~350 ms
	go func() { time.Sleep(1500 * time.Millisecond); release() }()
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	// Quiet from 0; probes fail from ~0.75 s; two failures span ~0.35 s < 0.7 s.
	if strings.Contains(body, "engine_stalled") || src.stalled.Load() != 0 {
		t.Errorf("stalled before the failures spanned StallSec: %q", body)
	}
}

func TestIdleMeansNoProbes(t *testing.T) {
	gw, _, release, _ := stallHarness(t, slotsCounting())
	release()
	postChat(gw, `{"model":"qwen","stream":true}`)
	time.Sleep(400 * time.Millisecond) // the loop notices nothing is in flight and stops
	n := gw.wd.probes.Load()
	time.Sleep(600 * time.Millisecond)
	if got := gw.wd.probes.Load(); got != n {
		t.Errorf("probes while idle: %d → %d", n, got)
	}
	gw.wd.mu.Lock()
	running := gw.wd.running
	gw.wd.mu.Unlock()
	if running {
		t.Error("watchdog still running with nothing in flight")
	}
}

func TestWatchdogOffWhenZero(t *testing.T) {
	gw, _, release, _ := stallHarness(t, slotsHang)
	gw.SetStall(0)
	go func() { time.Sleep(500 * time.Millisecond); release() }()
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if strings.Contains(body, "engine_stalled") || gw.wd.probes.Load() != 0 {
		t.Errorf("watchdog ran while off: %q", body)
	}
}

func postMessages(gw *Gateway, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/messages", strings.NewReader(body)))
	return rec
}

const progressStream = `data: {"choices":[],"prompt_progress":{"total":9000,"cache":0,"processed":4096,"time_ms":900}}

data: {"choices":[],"prompt_progress":{"total":9000,"cache":0,"processed":9000,"time_ms":1900}}

data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: [DONE]

`

func TestPromptProgressAskedAndStripped(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, progressStream)
	})
	body := readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`).Body)
	if h.up.body["return_progress"] != true {
		t.Errorf("return_progress not asked for: %v", h.up.body)
	}
	if strings.Contains(body, "prompt_progress") || !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("progress frames reached a client that did not ask:\n%s", body)
	}
	// A client that asked gets them.
	body = readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true,"return_progress":true}`).Body)
	if strings.Count(body, "prompt_progress") != 2 {
		t.Errorf("asked-for progress frames dropped:\n%s", body)
	}
	// Not for a non-streamed request, nor for a cloud model.
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen"}`).Body)
	if _, ok := h.up.body["return_progress"]; ok {
		t.Error("return_progress on a non-streamed request")
	}
}
