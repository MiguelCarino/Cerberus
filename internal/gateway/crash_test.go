package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/engine"
)

// crashingSource records MarkCrashed and MarkStalledGen calls.
type crashingSource struct {
	fakeSource
	crashed atomic.Int32
	stalled atomic.Int32
}

func (c *crashingSource) MarkStalledGen(id string, gen uint64) bool {
	if !c.local {
		return false
	}
	c.stalled.Add(1)
	return true
}

func (c *crashingSource) MarkCrashed(id string) bool {
	if !c.local {
		return false
	}
	c.crashed.Add(1)
	return true
}

func crashGateway(t *testing.T, local bool, h http.HandlerFunc) (*Gateway, *crashingSource, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(up.Close)
	src := &crashingSource{fakeSource: fakeSource{base: up.URL, local: local}}
	return newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), src, &calls
}

func postChat(gw *Gateway, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/chat/completions", strings.NewReader(body)))
	return rec
}

func TestLocalEngineCrashMapsTo503(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"LM Studio 400 terminated", 400, `{"error":"terminated"}`},
		{"Channel Error", 500, `{"error":{"message":"Channel Error"}}`},
		{"502 from the engine", 502, `bad gateway`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, src, calls := crashGateway(t, true, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			rec := postChat(gw, `{"model":"qwen","messages":[]}`)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "local engine failed") ||
				!strings.Contains(rec.Body.String(), "it will be reloaded") {
				t.Fatalf("status %d body %s", rec.Code, rec.Body)
			}
			if src.crashed.Load() != 1 || calls.Load() != 1 || src.released.Load() != 1 {
				t.Fatalf("crashed %d calls %d released %d: want 1,1,1 (no gateway retry)",
					src.crashed.Load(), calls.Load(), src.released.Load())
			}
			if !engine.IsTransientText(rec.Code, rec.Body.String()) {
				t.Error("the 503 is not transient for the flows runner")
			}
		})
	}
}

func TestEngineCrashOnlyForLocal(t *testing.T) {
	gw, src, _ := crashGateway(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"terminated"}`)
	})
	rec := postChat(gw, `{"model":"qwen","messages":[]}`)
	if rec.Code != 400 || src.crashed.Load() != 0 {
		t.Fatalf("cloud: status %d crashed %d, want the 400 relayed and no mark", rec.Code, src.crashed.Load())
	}
}

func TestLocalValidationErrorRelayed(t *testing.T) {
	gw, src, _ := crashGateway(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"context length exceeded"}}`)
	})
	rec := postChat(gw, `{"model":"qwen","messages":[]}`)
	if rec.Code != 400 || src.crashed.Load() != 0 || !strings.Contains(rec.Body.String(), "context length") {
		t.Fatalf("status %d crashed %d body %s", rec.Code, src.crashed.Load(), rec.Body)
	}
}

func TestLocalEngineDownMarksCrashed(t *testing.T) {
	src := &crashingSource{fakeSource: fakeSource{base: "http://127.0.0.1:1", local: true}}
	gw := newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := postChat(gw, `{"model":"qwen","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable || src.crashed.Load() != 1 {
		t.Fatalf("status %d crashed %d body %s", rec.Code, src.crashed.Load(), rec.Body)
	}
}

func TestStreamErrorEventMarksCrashed(t *testing.T) {
	gw, src, _ := crashGateway(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		io.WriteString(w, "data: {\"error\":{\"message\":\"Channel Error\"}}\n\n")
	})
	rec := postChat(gw, `{"model":"qwen","stream":true,"messages":[]}`)
	if rec.Code != 200 || src.crashed.Load() != 1 {
		t.Fatalf("status %d crashed %d", rec.Code, src.crashed.Load())
	}
}

// The real Manager is what the gateway tells (engine tests cover MarkCrashed).
func TestManagerIsCrashMarker(t *testing.T) {
	var _ crashMarker = (*engine.Manager)(nil)
	var _ stallMarker = (*engine.Manager)(nil)
	var _ cachedResolver = (*engine.Manager)(nil)
	var _ loadedEndpointer = (*engine.Manager)(nil)
}

// causeSource is a crashingSource whose engine knows why the model ended.
type causeSource struct {
	crashingSource
	why string
}

func (c *causeSource) FailureText(id string) string { return c.why }

// The 503 names the cause the engine found (railguard's VRAM kill), and it
// stays transient for the flows runner.
func TestEngineFailedNamesCause(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"terminated"}`)
	}))
	defer up.Close()
	src := &causeSource{crashingSource: crashingSource{fakeSource: fakeSource{base: up.URL, local: true}},
		why: "local engine ended by Railguard (VRAM 97% full); reloading with a smaller GPU share"}
	gw := newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := postChat(gw, `{"model":"qwen","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), src.why) || src.crashed.Load() != 1 {
		t.Fatalf("status %d crashed %d body %s", rec.Code, src.crashed.Load(), rec.Body)
	}
	if !engine.IsTransientText(rec.Code, rec.Body.String()) || !engine.IsTransientText(0, src.why) {
		t.Error("the cause's 503 is not transient for the flows runner")
	}
}
