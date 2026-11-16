package api

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/gateway"
)

// liveModelsFake is a model source with change hints, a cached listing and
// a VRAM reserve, like *engine.Manager.
type liveModelsFake struct {
	fakeModels
	mu      sync.Mutex
	ms      []engine.ModelInfo
	hints   chan struct{}
	full    int // Models calls (a listing)
	cached  int // ModelsCached calls
	reserve engine.Reserve
}

func newLiveModels() *liveModelsFake {
	return &liveModelsFake{hints: make(chan struct{}, 1),
		ms:      []engine.ModelInfo{{ID: "qwen", Backend: "llamacpp", Local: true}, {ID: "cloud/x", Backend: "openai"}},
		reserve: engine.Reserve{MiB: 1505, Source: "railguard", LinePct: 97}}
}

func (f *liveModelsFake) Models(ctx context.Context) []engine.ModelInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.full++
	return append([]engine.ModelInfo(nil), f.ms...)
}
func (f *liveModelsFake) ModelsCached(ctx context.Context) []engine.ModelInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cached++
	return append([]engine.ModelInfo(nil), f.ms...)
}
func (f *liveModelsFake) SubscribeChanges() (<-chan struct{}, func()) { return f.hints, func() {} }
func (f *liveModelsFake) VRAMReserveNow() engine.Reserve              { return f.reserve }
func (f *liveModelsFake) set(fn func(ms []engine.ModelInfo)) {
	f.mu.Lock()
	fn(f.ms)
	f.mu.Unlock()
}
func (f *liveModelsFake) poke() {
	select {
	case f.hints <- struct{}{}:
	default:
	}
}

// nextEvent returns the next frame named name, skipping pings and others.
func nextEvent(t *testing.T, sse sseReader, name string) string {
	t.Helper()
	for {
		ev, data := sse.next(t)
		if ev == name {
			return data
		}
	}
}

// TestModelsEvents: a change hint sends the live part of the local models
// once, read from the cached listing; an unchanged list sends nothing
// (web-api-ops-6).
func TestModelsEvents(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.ping = 50 * time.Millisecond
	lm := newLiveModels()
	env.srv.models = lm
	start := time.Now().Add(-90 * time.Second).Truncate(time.Second).Add(500 * time.Millisecond)
	env.srv.busy = func() map[string]gateway.Busy {
		return map[string]gateway.Busy{"qwen": {BusyForS: time.Since(start).Seconds(), Stalls: 1}}
	}
	sse, cancel := openSSE(t, env, "/api/events")
	defer cancel()

	lm.set(func(ms []engine.ModelInfo) { ms[0].Loaded, ms[0].InFlight = true, 1 })
	lm.poke()
	data := nextEvent(t, sse, "models")
	var got struct {
		Models []modelLive `json:"models"`
	}
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "qwen" || !got.Models[0].Loaded || got.Models[0].InFlight != 1 ||
		got.Models[0].Stalls != 1 || got.Models[0].BusySince == nil || time.Since(*got.Models[0].BusySince) < 80*time.Second {
		t.Fatalf("models event %s", data)
	}
	// The same state again: no event (pings only).
	lm.poke()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ev, d := sse.next(t); ev == "models" {
			t.Fatalf("unchanged models sent again: %s", d)
		}
	}
	lm.set(func(ms []engine.ModelInfo) { ms[0].InFlight = 0 })
	lm.poke()
	if data := nextEvent(t, sse, "models"); !json.Valid([]byte(data)) {
		t.Fatal(data)
	}
	lm.mu.Lock()
	full := lm.full
	lm.mu.Unlock()
	if full != 0 {
		t.Fatalf("the events listed models %d times (want the cached view)", full)
	}
}

// TestPrefillEventsAndStatus: prompt reading shows in /api/status and goes
// out on /api/events, coalesced, ending with done (web-api-ops-8).
func TestPrefillEventsAndStatus(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.ping = 50 * time.Millisecond
	lm := newLiveModels()
	env.srv.models = lm
	env.srv.busy = func() map[string]gateway.Busy {
		return map[string]gateway.Busy{"qwen": {BusyForS: 3, Prefill: &engine.Prefill{Model: "qwen", Session: "s1", Processed: 12000, Total: 48000}}}
	}
	feed := make(chan engine.Prefill, 8)
	env.srv.prefillSub = func() (<-chan engine.Prefill, func()) { return feed, func() {} }

	w := env.do("GET", "/api/status", "", withBearer(testToken))
	var st Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	var p *engine.Prefill
	for _, m := range st.Models {
		if m.ID == "qwen" {
			p = m.Prefill
		}
	}
	if p == nil || p.Processed != 12000 || p.Total != 48000 || p.Session != "s1" {
		t.Fatalf("status prefill %+v", p)
	}
	if st.VRAMReserve == nil || st.VRAMReserve.MiB != 1505 || st.VRAMReserve.LinePct != 97 {
		t.Fatalf("status reserve %+v", st.VRAMReserve)
	}

	sse, cancel := openSSE(t, env, "/api/events")
	defer cancel()
	// Three updates of one request in a second: one event, the last.
	feed <- engine.Prefill{Model: "qwen", Session: "s1", Processed: 4000, Total: 48000}
	feed <- engine.Prefill{Model: "qwen", Session: "s1", Processed: 8000, Total: 48000}
	feed <- engine.Prefill{Model: "qwen", Session: "s1", Processed: 12000, Total: 48000}
	var got engine.Prefill
	if err := json.Unmarshal([]byte(nextEvent(t, sse, "prefill")), &got); err != nil || got.Processed != 12000 {
		t.Fatalf("prefill %+v %v", got, err)
	}
	feed <- engine.Prefill{Model: "qwen", Session: "s1", Processed: 48000, Total: 48000, Done: true}
	if err := json.Unmarshal([]byte(nextEvent(t, sse, "prefill")), &got); err != nil || !got.Done {
		t.Fatalf("done %+v %v", got, err)
	}
}

func TestGPUEndpoint(t *testing.T) {
	env := newEnv(t, nil)
	lm := newLiveModels()
	env.srv.models = lm
	w := env.do("GET", "/api/gpu", "", withBearer(testToken))
	var st GPUStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); w.Code != 200 || err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(st.GPU.GPUs) != 1 || st.VRAMReserve == nil || st.VRAMReserve.Source != "railguard" {
		t.Fatalf("gpu %s", w.Body)
	}
	lm.mu.Lock()
	n := lm.full + lm.cached
	lm.mu.Unlock()
	if n != 0 {
		t.Fatal("/api/gpu read the model list")
	}
	// No reserve known: omitted.
	lm.reserve = engine.Reserve{Source: "unknown"}
	var st2 GPUStatus
	if w := env.do("GET", "/api/gpu", "", withBearer(testToken)); json.Unmarshal(w.Body.Bytes(), &st2) != nil || st2.VRAMReserve != nil {
		t.Fatalf("unknown reserve shown: %s", w.Body)
	}
}

// A request's prompt reading names its session only to who may see that
// session, on every route that returns models (web-api-4).
func TestPrefillSessionMaskedOnModelRoutes(t *testing.T) {
	e := newUsersEnv(t)
	cm := newChatManager(t, e.srv)
	e.srv.chats = cm
	e.srv.cfg.Chat.Enabled = true
	e.ss.add("sc", e.u["cy"].ID)
	e.srv.models = newLiveModels()
	e.srv.busy = func() map[string]gateway.Busy {
		return map[string]gateway.Busy{"qwen": {BusyForS: 3, Prefill: &engine.Prefill{Model: "qwen", Session: "sc", Processed: 1, Total: 9}}}
	}
	session := func(who, path string) string {
		w := e.call(who, "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", who, path, w.Code, w.Body)
		}
		var ms []engine.ModelInfo
		if path == "/api/chats/models" {
			var out ChatModels
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			ms = out.Models
		} else {
			_ = json.Unmarshal(w.Body.Bytes(), &ms)
		}
		for _, m := range ms {
			if m.ID == "qwen" && m.Prefill != nil {
				return m.Prefill.Session
			}
		}
		t.Fatalf("%s %s: no prefill in %s", who, path, w.Body)
		return ""
	}
	for _, path := range []string{"/api/models", "/api/chats/models"} {
		if got := session("bob", path); got != "" {
			t.Errorf("bob sees cy's session %q on %s", got, path)
		}
		if got := session("cy", path); got != "sc" {
			t.Errorf("cy sees %q on %s, want her own session", got, path)
		}
	}
}
