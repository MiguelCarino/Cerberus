package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/config"
)

type fakeOllama struct {
	mu   sync.Mutex
	gens []map[string]any
}

func (f *fakeOllama) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.9.0"}`))
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[
			{"name":"llama3.2:3b","model":"llama3.2:3b","size":2019393189,"details":{"family":"llama","families":["llama"]}},
			{"name":"llava:7b","size":4733363377,"details":{"families":["llama","clip"]}},
			{"model":"noname:latest","size":1048576}
		]}`))
	})
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"llama3.2:3b","model":"llama3.2:3b","size":3000000000,"size_vram":2147483648,"context_length":4096}]}`))
	})
	mux.HandleFunc("POST /api/generate", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.gens = append(f.gens, body)
		f.mu.Unlock()
		w.Write([]byte(`{"model":"x","done":true}`))
	})
	return mux
}

func testOllama(url string) *ollama {
	cfg := config.Defaults()
	cfg.Engines.IdleTTLSec = 600
	cfg.Engines.Ollama.URL = url
	return newOllama(cfg, quietLog())
}

func TestOllamaListLoadUnload(t *testing.T) {
	f := &fakeOllama{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	b := testOllama(srv.URL + "/")
	ctx := context.Background()
	if !b.Available() {
		t.Fatal("not available")
	}
	ms, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("models: %+v", ms)
	}
	if l := ms[0]; !l.Loaded || l.Ctx != 4096 || l.SizeMiB != 2048 || l.Vision {
		t.Errorf("llama: %+v", l)
	}
	if v := ms[1]; !v.Vision || v.Loaded {
		t.Errorf("llava: %+v", v)
	}
	if ms[2].ID != "noname:latest" {
		t.Errorf("fallback to model field: %+v", ms[2])
	}

	ep, err := b.Load(ctx, ModelInfo{ID: "llava:7b", Name: "llava:7b", Ctx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL != srv.URL+"/v1" || ep.Model != "llava:7b" || !ep.Local {
		t.Errorf("endpoint %+v", ep)
	}
	if err := b.Unload(ctx, ModelInfo{ID: "llava:7b"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.gens) != 2 {
		t.Fatalf("generate calls: %v", f.gens)
	}
	load, unload := f.gens[0], f.gens[1]
	if load["keep_alive"] != "600s" || load["model"] != "llava:7b" || load["options"].(map[string]any)["num_ctx"] != float64(8192) {
		t.Errorf("load body %v", load)
	}
	if unload["keep_alive"] != float64(0) {
		t.Errorf("unload body %v", unload)
	}
}

// countHandler counts log records (to check "log once").
type countHandler struct{ n atomic.Int32 }

func (h *countHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h *countHandler) Handle(context.Context, slog.Record) error { h.n.Add(1); return nil }
func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *countHandler) WithGroup(string) slog.Handler             { return h }

func TestOllamaUnreachableQuiet(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + l.Addr().String()
	l.Close() // nothing listens here now
	b := testOllama(url)
	h := &countHandler{}
	b.log = slog.New(h)
	for range 3 {
		if b.Available() {
			t.Fatal("available while unreachable")
		}
		b.markDown() // force a fresh probe each time
	}
	if n := h.n.Load(); n != 1 {
		t.Fatalf("logged %d times, want 1", n)
	}
	cfg := config.Defaults()
	cfg.Engines.Ollama.Enabled = false
	if newOllama(cfg, quietLog()).Available() {
		t.Fatal("available while disabled")
	}
}
