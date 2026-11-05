package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/gateway"
)

// fakeOllama is a local engine (Ollama's HTTP API) that behaves like the
// 2026-11-03 LM Studio failure: it serves one request at a time, and a
// second concurrent request kills it ("Channel Error"). It can also crash
// once on a chosen task ("terminated"), as llama.cpp did.
type fakeOllama struct {
	srv       *httptest.Server
	crashTask string // the item task whose first call crashes (empty: never)

	mu        sync.Mutex
	loaded    bool
	loads     int
	unloads   int
	crashed   bool // the one crash happened
	calls     map[string]int
	active    int
	maxActive int
	overlaps  int // second concurrent requests (the engine would have died)
}

func newFakeOllama(t *testing.T, crashTask string) *fakeOllama {
	f := &fakeOllama{crashTask: crashTask, calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOllama) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/version":
		_, _ = io.WriteString(w, `{"version":"0.9.0"}`)
	case "/api/tags":
		_, _ = io.WriteString(w, `{"models":[{"name":"qwen","size":1048576000}]}`)
	case "/api/ps":
		f.mu.Lock()
		loaded := f.loaded
		f.mu.Unlock()
		if loaded {
			_, _ = io.WriteString(w, `{"models":[{"name":"qwen","size_vram":1048576000}]}`)
		} else {
			_, _ = io.WriteString(w, `{"models":[]}`)
		}
	case "/api/generate":
		var body struct {
			KeepAlive any `json:"keep_alive"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if n, ok := body.KeepAlive.(float64); ok && n == 0 {
			f.loaded = false
			f.unloads++
		} else {
			f.loaded = true
			f.loads++
		}
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"done":true}`)
	case "/v1/chat/completions":
		f.chat(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOllama) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	task := ""
	if n := len(req.Messages); n > 0 {
		task = req.Messages[n-1].Content
	}
	f.mu.Lock()
	f.calls[task]++
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	overlap := f.active > 1
	if overlap {
		f.overlaps++
	}
	crash := task == f.crashTask && !f.crashed
	if crash {
		f.crashed = true
		f.loaded = false // the engine died with the model
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	switch {
	case overlap:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"Channel Error"}`)
		return
	case crash:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"terminated"}`)
		return
	}
	reply := "done " + task
	if task == "plan" {
		reply = `["a","b","c"]`
	} else {
		time.Sleep(40 * time.Millisecond) // long enough for siblings to pile up
	}
	out, _ := json.Marshal(map[string]any{
		"model":   "qwen",
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}},
		"usage":   map[string]int{"prompt_tokens": 3, "completion_tokens": 2},
	})
	_, _ = w.Write(out)
}

func (f *fakeOllama) snapshot() (loads, unloads, maxActive, overlaps int, calls map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make(map[string]int, len(f.calls))
	for k, v := range f.calls {
		c[k] = v
	}
	return f.loads, f.unloads, f.maxActive, f.overlaps, c
}

// e2eFlow wires a flow engine to the real gateway and engine Manager, with
// the fake Ollama as the only engine.
func e2eFlow(t *testing.T, fo *fakeOllama) (*flows.Engine, *engine.Manager) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.Enabled = false
	cfg.Engines.LMStudio.Enabled = false
	cfg.Engines.Ollama.Enabled = true
	cfg.Engines.Ollama.URL = fo.srv.URL
	cfg.Providers = nil
	log := slog.New(slog.DiscardHandler)
	eng := engine.NewManager(cfg, nil, log)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		eng.Shutdown(ctx)
	})
	gw := gateway.New(eng, nil, nil, log)
	host := NewFlowHost(FlowHostDeps{Config: func() *config.Config { return cfg }, Gateway: gw.Handler(),
		Models: eng.Models})
	o := host.Options(t.TempDir())
	o.Log = log
	fe, err := flows.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	fe.Start()
	t.Cleanup(func() { _ = fe.Close() })
	return fe, eng
}

// runFan runs a plan prompt and a fan-out of its three items, and returns
// the finished run and every "waiting" text the run page could have shown
// meanwhile (polled from GetRun, as the page does on each flowrun event).
func runFan(t *testing.T, fe *flows.Engine) (flows.RunDetail, []string) {
	t.Helper()
	f, _, err := fe.Create("test", flows.Flow{Name: "Fan", Limits: flows.Limits{MaxParallel: 3}, Steps: []flows.Step{
		{ID: "p", Kind: flows.KindPrompt, JSON: true, Model: "qwen", Template: "plan"},
		{ID: "f", Kind: flows.KindFanOut, From: "p", DefaultModel: "qwen", Template: "{item.task}"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := fe.RunFlow("test", f.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	var waits []string
	stop := make(chan struct{})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for {
			if d, err := fe.GetRun(r.ID); err == nil {
				if d.Run.Waiting != "" {
					waits = append(waits, d.Run.Waiting)
				}
				for _, it := range d.Live {
					if it.Waiting != "" {
						waits = append(waits, it.Waiting)
					}
				}
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = fe.Wait(ctx, r.ID)
	close(stop)
	<-polled
	if err != nil {
		t.Fatal(err)
	}
	d, err := fe.GetRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, waits
}

func fanItems(t *testing.T, d flows.RunDetail) []flows.ItemRecord {
	t.Helper()
	for _, s := range d.Steps {
		if s.ID == "f" {
			return s.Items
		}
	}
	t.Fatalf("no fan-out record in %+v", d.Steps)
	return nil
}

// The 2026-11-03 failure, end to end without a GPU: a fan-out of three
// prompt items to a one-request-at-a-time local model. The engine's queue
// runs them one at a time (no "Channel Error"), and the flow shows them
// waiting meanwhile.
func TestE2EFanOutQueuesOnLocalModel(t *testing.T) {
	fo := newFakeOllama(t, "")
	fe, eng := e2eFlow(t, fo)
	d, waits := runFan(t, fe)
	if d.Run.State != flows.StatePassed {
		t.Fatalf("run %s: %q", d.Run.State, d.Run.Reason)
	}
	items := fanItems(t, d)
	if len(items) != 3 {
		t.Fatalf("items %+v", items)
	}
	for _, it := range items {
		if it.State != flows.StepPassed || it.Retries != 0 {
			t.Fatalf("item %+v", it)
		}
	}
	loads, _, maxActive, overlaps, calls := fo.snapshot()
	if maxActive != 1 || overlaps != 0 {
		t.Fatalf("engine saw %d concurrent requests (%d overlaps), want one at a time", maxActive, overlaps)
	}
	if loads != 1 || calls["a"] != 1 || calls["b"] != 1 || calls["c"] != 1 {
		t.Fatalf("loads %d calls %v", loads, calls)
	}
	for _, m := range eng.Models(context.Background()) {
		if m.ID == "qwen" && (m.MaxParallel != 1 || m.InFlight != 0 || m.Waiting != 0) {
			t.Fatalf("model after run %+v", m)
		}
	}
	sawWait := false
	for _, w := range waits {
		if strings.Contains(w, "waiting for qwen") {
			sawWait = true
		}
	}
	if !sawWait {
		t.Fatalf("the run never showed an item waiting for the model: %q", waits)
	}
}

// The engine dies under one item ("terminated"): the gateway marks the
// model crashed, the flow retries that item once, the engine reloads the
// model and the run passes with its siblings untouched.
func TestE2EFanOutCrashRetriesAndReloads(t *testing.T) {
	fo := newFakeOllama(t, "b")
	fe, _ := e2eFlow(t, fo)
	start := time.Now()
	d, _ := runFan(t, fe)
	if d.Run.State != flows.StatePassed {
		t.Fatalf("run %s: %q", d.Run.State, d.Run.Reason)
	}
	var crashed atomic.Bool
	for _, it := range fanItems(t, d) {
		if it.State != flows.StepPassed {
			t.Fatalf("item %+v", it)
		}
		switch it.Title {
		case "b":
			if it.Retries != 1 || !strings.Contains(it.RetryError, "terminated") || it.Output != "done b" {
				t.Fatalf("item b %+v", it)
			}
			crashed.Store(true)
		default:
			if it.Retries != 0 {
				t.Fatalf("sibling retried %+v", it)
			}
		}
	}
	if !crashed.Load() {
		t.Fatal("no item b")
	}
	loads, unloads, maxActive, overlaps, calls := fo.snapshot()
	if loads != 2 || unloads < 1 {
		t.Fatalf("loads %d unloads %d: want the crashed model reloaded (2 loads, an unload first)", loads, unloads)
	}
	if maxActive != 1 || overlaps != 0 || calls["b"] != 2 || calls["a"] != 1 || calls["c"] != 1 {
		t.Fatalf("max %d overlaps %d calls %v", maxActive, overlaps, calls)
	}
	if el := time.Since(start); el > 8*time.Second {
		t.Fatalf("took %s", el)
	}
}
