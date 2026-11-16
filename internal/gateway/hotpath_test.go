package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/engine"
)

// cacheSource is a fakeSource with the engine manager's cache-aware calls:
// ResolveCached knows "qwen" only, Resolve records every id it was asked
// for (in the real manager a miss there forces a listing), and
// LoadedEndpoint hands out the loaded model without a slot.
type cacheSource struct {
	fakeSource
	mu       sync.Mutex
	resolved []string
	block    chan struct{} // when set, Acquire waits on it (a busy slot)
	loadedEP atomic.Int32
}

func (c *cacheSource) ResolveCached(_ context.Context, id string) (engine.ModelInfo, bool) {
	if id == "qwen" {
		return engine.ModelInfo{ID: "qwen", Backend: engine.BackendLlamaCpp, Local: true, Loaded: true}, true
	}
	return engine.ModelInfo{}, false
}

func (c *cacheSource) Resolve(_ context.Context, id string) (engine.ModelInfo, error) {
	c.mu.Lock()
	c.resolved = append(c.resolved, id)
	c.mu.Unlock()
	if id == "qwen" {
		return engine.ModelInfo{ID: "qwen", Backend: engine.BackendLlamaCpp, Local: true, Loaded: true}, nil
	}
	return engine.ModelInfo{}, fmt.Errorf("%w: %s", engine.ErrUnknownModel, id)
}

func (c *cacheSource) Acquire(ctx context.Context, id string) (engine.Endpoint, func(), error) {
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return engine.Endpoint{}, nil, ctx.Err()
		}
	}
	return c.fakeSource.Acquire(ctx, id)
}

func (c *cacheSource) LoadedEndpoint(_ context.Context, id string) (engine.Endpoint, bool) {
	if id != "qwen" {
		return engine.Endpoint{}, false
	}
	c.loadedEP.Add(1)
	return engine.Endpoint{BaseURL: c.base + "/v1", Model: "backend-qwen", Local: true, Backend: engine.BackendLlamaCpp}, true
}

func (c *cacheSource) asked(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.resolved {
		if r == id {
			n++
		}
	}
	return n
}

func cacheGateway(t *testing.T, h http.HandlerFunc) (*Gateway, *cacheSource) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	src := &cacheSource{fakeSource: fakeSource{base: up.URL, local: true}}
	return newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), src
}

// A mapped vendor name (claude-*) goes through the cache and model_map:
// the forcing Resolve is never asked for it.
func TestMappedNameSkipsForcedResolve(t *testing.T) {
	gw, src := cacheGateway(t, jsonOK(okReply))
	gw.SetProviders(nil, map[string]string{"claude-*": "qwen"})
	for range 10 {
		rec := httptest.NewRecorder()
		gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/messages",
			strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":5,"messages":[]}`)))
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	if n := src.asked("claude-sonnet-4-5"); n != 0 {
		t.Errorf("the forcing Resolve was asked for the mapped name %d times", n)
	}
	// A name in the cache needs no Resolve at all; an unknown one falls
	// through to it once per request (the engine bounds its listings).
	postChat(gw, `{"model":"qwen","messages":[]}`)
	if n := src.asked("qwen"); n != 10 { // only the ten mapped requests' targets
		t.Errorf("Resolve(qwen) %d times, want 10", n)
	}
	if rec := postChat(gw, `{"model":"nope","messages":[]}`); rec.Code != 404 || src.asked("nope") != 1 {
		t.Errorf("unknown: %d, asked %d", rec.Code, src.asked("nope"))
	}
}

// count_tokens takes no request slot: it answers while the model's only
// slot is busy.
func TestCountTokensTakesNoSlot(t *testing.T) {
	gw, src := cacheGateway(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apply-template":
			_, _ = io.WriteString(w, `{"prompt":"<|user|>hello"}`)
		case "/tokenize":
			_, _ = io.WriteString(w, `{"tokens":[1,2,3,4,5]}`)
		default:
			http.NotFound(w, r)
		}
	})
	src.block = make(chan struct{}) // the slot is held by a long generation
	defer close(src.block)
	start := time.Now()
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/messages/count_tokens",
		strings.NewReader(`{"model":"qwen","messages":[{"role":"user","content":"hello"}]}`)))
	if d := time.Since(start); d > time.Second {
		t.Errorf("count_tokens took %v: it waited for a slot", d)
	}
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"input_tokens":5}` {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if src.acquired.Load() != 0 || src.loadedEP.Load() != 1 {
		t.Errorf("acquired %d, LoadedEndpoint %d: want 0, 1", src.acquired.Load(), src.loadedEP.Load())
	}
}

// A count the engine does not answer in time falls back to the estimate.
func TestCountTokensTimeoutEstimates(t *testing.T) {
	old := countTimeout
	countTimeout = 100 * time.Millisecond
	t.Cleanup(func() { countTimeout = old })
	gw, _ := cacheGateway(t, func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: the server notices a client that hung up
		// only once it has.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/messages/count_tokens",
		strings.NewReader(`{"model":"qwen","messages":[{"role":"user","content":"hello"}]}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"estimated":true`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// The gateway keeps more than Go's default two idle connections per
// upstream: two waves of four concurrent requests open four connections,
// not six.
func TestTransportReusesConnections(t *testing.T) {
	var opened atomic.Int32
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // the four overlap
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okReply)
	}))
	up.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	up.Start()
	t.Cleanup(up.Close)
	src := &fakeSource{base: up.URL, local: false}
	gw := newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wave := func() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if rec := postChat(gw, `{"model":"qwen","messages":[]}`); rec.Code != 200 {
					t.Errorf("%d %s", rec.Code, rec.Body)
				}
			}()
		}
		wg.Wait()
	}
	wave()
	wave()
	if n := opened.Load(); n != 4 {
		t.Errorf("%d connections opened for two waves of 4, want 4", n)
	}
	if tr, ok := gw.client.Transport.(*http.Transport); !ok || tr.MaxIdleConnsPerHost != maxIdlePerHost {
		t.Errorf("transport %#v", gw.client.Transport)
	}
}
