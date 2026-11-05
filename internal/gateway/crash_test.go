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

// crashingSource records MarkCrashed calls.
type crashingSource struct {
	fakeSource
	crashed atomic.Int32
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
}
