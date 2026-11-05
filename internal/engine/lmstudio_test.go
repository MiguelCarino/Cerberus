package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// fakeLMS replays recorded `lms` JSON and records every command.
type fakeLMS struct {
	t      *testing.T
	ls, ps []byte
	onCall func(args []string)

	mu    sync.Mutex
	calls [][]string
}

func (f *fakeLMS) run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(args)
	}
	switch strings.Join(args, " ") {
	case "ls --json":
		return f.ls, nil
	case "ps --json":
		return f.ps, nil
	}
	return []byte("ok"), nil
}

func (f *fakeLMS) called(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return c
		}
	}
	return nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testLMStudio(t *testing.T, base string) (*lmStudio, *fakeLMS) {
	cfg := config.Defaults()
	cfg.Engines.IdleTTLSec = 600
	cfg.Engines.LMStudio.BaseURL = base
	cfg.Engines.LMStudio.Ctx = 32768
	cfg.Models = []config.ModelConfig{{ID: "glm-4.7-flash", TTLSec: 90}}
	f := &fakeLMS{t: t, ls: fixture(t, "lms_ls.json"), ps: fixture(t, "lms_ps.json")}
	b := newLMStudio(cfg, quietLog())
	b.binary = func() string { return "/fake/lms" }
	b.run = f.run
	// Never the real LM Studio folders: a test sets its own.
	none := t.TempDir()
	b.roots = func() []string { return []string{filepath.Join(none, "models")} }
	b.hub = func() string { return filepath.Join(none, "hub", "models") }
	return b, f
}

func TestLMStudioList(t *testing.T) {
	b, _ := testLMStudio(t, "http://127.0.0.1:1/v1")
	ms, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	if len(ms) != 4 { // the entry with neither modelKey nor path is dropped
		t.Fatalf("got %d models: %+v", len(ms), ms)
	}
	q := byID["qwen/qwen3.8-27b"]
	if !q.Vision || q.SizeMiB != 16921 || q.Ctx != 32768 || q.Loaded || !q.Local || q.Backend != "lmstudio" {
		t.Errorf("qwen: %+v", q)
	}
	if g := byID["glm-4.7-flash"]; g.Ctx != 16384 || g.Vision {
		t.Errorf("glm ctx should clip to maxContextLength: %+v", g)
	}
	e := byID["text-embedding-nomic-embed-text-v1.5"]
	if !e.Loaded || e.Ctx != 2048 || e.LastUsed.IsZero() || e.Name != "text-embedding-nomic-embed-text-v1.5" {
		t.Errorf("embedding (loaded): %+v", e)
	}
	if bare := byID["someone/bare-model.gguf"]; bare.SizeMiB != 0 || bare.Ctx != 32768 {
		t.Errorf("bare: %+v", bare)
	}
}

func TestParseLMSTolerant(t *testing.T) {
	for _, raw := range []string{
		"noise before\n[{\"modelKey\":\"a\"}]",
		`{"models":[{"modelKey":"a"}]}`,
	} {
		ms, err := parseLMS([]byte(raw))
		if err != nil || len(ms) != 1 || ms[0].key() != "a" {
			t.Errorf("parseLMS(%q) = %+v, %v", raw, ms, err)
		}
	}
	if _, err := parseLMS([]byte("not json")); err == nil {
		t.Error("want error for garbage")
	}
}

func TestLMStudioLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	b, f := testLMStudio(t, srv.URL+"/v1")
	ctx := context.Background()

	ep, err := b.Load(ctx, ModelInfo{ID: "glm-4.7-flash", Ctx: 16384})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"load", "glm-4.7-flash", "--context-length", "16384", "--ttl", "90", "-y", "--identifier", "glm-4.7-flash"}
	if got := f.called("load"); !slices.Equal(got, want) {
		t.Fatalf("load args = %v, want %v", got, want)
	}
	if ep.BaseURL != srv.URL+"/v1" || ep.Model != "glm-4.7-flash" || !ep.Local {
		t.Errorf("endpoint %+v", ep)
	}
	if f.called("server start") != nil {
		t.Error("server started although it was up")
	}

	// Already loaded (per ps): no second lms load.
	f.calls = nil
	if _, err := b.Load(ctx, ModelInfo{ID: "text-embedding-nomic-embed-text-v1.5"}); err != nil {
		t.Fatal(err)
	}
	if f.called("load") != nil {
		t.Error("loaded a model that ps reports as loaded")
	}

	if err := b.Unload(ctx, ModelInfo{ID: "glm-4.7-flash", Name: "glm-4.7-flash"}); err != nil {
		t.Fatal(err)
	}
	if got := f.called("unload"); !slices.Equal(got, []string{"unload", "glm-4.7-flash"}) {
		t.Errorf("unload args = %v", got)
	}
}

func TestLMStudioStartsServer(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	b, f := testLMStudio(t, srv.URL+"/v1")
	f.onCall = func(args []string) {
		if strings.Join(args, " ") == "server start" {
			up.Store(true)
		}
	}
	if _, err := b.Load(context.Background(), ModelInfo{ID: "qwen/qwen3.8-27b", Ctx: 8192}); err != nil {
		t.Fatal(err)
	}
	if f.called("server start") == nil || f.called("load") == nil {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestLMStudioAvailable(t *testing.T) {
	b, _ := testLMStudio(t, "")
	if !b.Available() {
		t.Fatal("want available")
	}
	b.binary = func() string { return "" }
	if b.Available() {
		t.Fatal("available without lms")
	}
}
