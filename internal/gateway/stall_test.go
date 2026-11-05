package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/notify"
)

// stallHarness: a local engine that streams one chunk and then goes quiet
// until released; its /slots hangs (wedged) or answers (just slow).
func stallHarness(t *testing.T, slotsOK bool) (*Gateway, *crashingSource, func(), func() []notify.Message) {
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
			if !slotsOK {
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return
			}
			_, _ = io.WriteString(w, `[{"id":0,"is_processing":true}]`)
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

func TestStalledEngineFailsAndReloads(t *testing.T) {
	gw, src, _, msgs := stallHarness(t, false)
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
	if src.crashed.Load() != 1 {
		t.Errorf("MarkCrashed %d times", src.crashed.Load())
	}
	if m := msgs(); len(m) != 1 || !strings.Contains(m[0].Body, "qwen") {
		t.Errorf("notifications %+v", m)
	}
	if b := gw.Busy()["qwen"]; b.Stalls != 1 {
		t.Errorf("busy %+v", b)
	}
}

func TestStalledTranslatedStreamEndsWithError(t *testing.T) {
	gw, _, _, _ := stallHarness(t, false)
	rec := postMessages(gw, `{"model":"qwen","max_tokens":5,"stream":true,"messages":[]}`)
	if b := rec.Body.String(); !strings.Contains(b, "event: error") || !strings.Contains(b, "stopped responding") {
		t.Errorf("translated stream %q", b)
	}
}

func TestSlowButAliveIsNotStalled(t *testing.T) {
	gw, src, release, _ := stallHarness(t, true)
	go func() { time.Sleep(900 * time.Millisecond); release() }()
	body := postChat(gw, `{"model":"qwen","stream":true}`).Body.String()
	if !strings.Contains(body, " end") || strings.Contains(body, "engine_stalled") {
		t.Errorf("a slow engine that answers checks was cut off: %q", body)
	}
	if src.crashed.Load() != 0 {
		t.Error("marked crashed")
	}
	if gw.wd.probes.Load() == 0 {
		t.Error("never probed a quiet request")
	}
}

func TestIdleMeansNoProbes(t *testing.T) {
	gw, _, release, _ := stallHarness(t, true)
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
	gw, _, release, _ := stallHarness(t, false)
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
