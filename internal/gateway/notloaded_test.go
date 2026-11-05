package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/engine"
)

// droppingSource is a local engine that unloaded its model behind Cerberus'
// back: the first request gets "No models loaded" until Forget is called,
// and Acquire then loads it again.
type droppingSource struct {
	fakeSource
	forgot atomic.Int32
}

func (d *droppingSource) Forget(id string) bool { d.forgot.Add(1); return true }

func TestNotLoadedRetriesOnce(t *testing.T) {
	var calls atomic.Int32
	src := &droppingSource{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if src.forgot.Load() == 0 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"No models loaded. Please load a model in the developer page or use the 'lms load' command."}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()
	src.base, src.local = up.URL, true
	gw := newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hi"`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if calls.Load() != 2 || src.forgot.Load() != 1 || src.acquired.Load() != 2 || src.released.Load() != 2 {
		t.Errorf("upstream calls %d, forgot %d, acquired %d, released %d; want 2,1,2,2",
			calls.Load(), src.forgot.Load(), src.acquired.Load(), src.released.Load())
	}
}

func TestNotLoadedRelayedWhenStillMissing(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"model 'qwen' not found"}}`)
	}))
	defer up.Close()
	src := &droppingSource{fakeSource: fakeSource{base: up.URL, local: true}}
	gw := newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not found") || calls.Load() != 2 {
		t.Fatalf("status %d calls %d body %s: one retry, then the engine's error", rec.Code, calls.Load(), rec.Body)
	}
}

func TestOtherBadRequestNotRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"context length exceeded"}}`)
	}))
	defer up.Close()
	src := &droppingSource{fakeSource: fakeSource{base: up.URL, local: true}}
	gw := newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[]}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "context length") || calls.Load() != 1 || src.forgot.Load() != 0 {
		t.Fatalf("status %d calls %d forgot %d body %s", rec.Code, calls.Load(), src.forgot.Load(), rec.Body)
	}
}

func TestModelNotLoadedMessages(t *testing.T) {
	for msg, want := range map[string]bool{
		"No models loaded. Please load a model in the developer page": true,
		"model 'llama3.2:3b' not found":                               true,
		"model_not_found":                                             true,
		"Model is not loaded":                                         true,
		"context length exceeded":                                     false,
		"invalid tool schema":                                         false,
		"":                                                            false,
	} {
		if got := modelNotLoaded(msg); got != want {
			t.Errorf("modelNotLoaded(%q) = %v, want %v", msg, got, want)
		}
	}
}

var _ = engine.ModelInfo{}
var _ = context.Background
