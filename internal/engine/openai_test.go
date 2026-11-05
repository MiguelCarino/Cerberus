package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func TestOpenAIDiscoveryAndHeaders(t *testing.T) {
	var hits atomic.Int32
	var auth, extra atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		extra.Store(r.Header.Get("X-Title"))
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-x"},{"id":"vendor/model-y"},{"id":""}]}`))
	}))
	defer srv.Close()

	cfg := config.Defaults()
	t.Setenv("CERBERUS_TEST_KEY", "env-key")
	p := config.ProviderConfig{Name: "router", BaseURL: srv.URL + "/v1/", KeyEnv: "CERBERUS_TEST_KEY",
		Headers: map[string]string{"X-Title": "Cerberus"}, Enabled: true}
	b := newOpenAI(cfg, p, quietLog())
	ctx := context.Background()

	ms, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "router/gpt-x" || ms[1].ID != "router/vendor/model-y" || ms[0].Local || ms[0].Backend != "openai" {
		t.Fatalf("models %+v", ms)
	}
	if auth.Load() != "Bearer env-key" || extra.Load() != "Cerberus" {
		t.Errorf("headers: auth %v extra %v", auth.Load(), extra.Load())
	}
	if _, err := b.List(ctx); err != nil || hits.Load() != 1 {
		t.Errorf("discovery not cached: hits %d err %v", hits.Load(), err)
	}

	ep, err := b.Load(ctx, ms[1])
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL != srv.URL+"/v1" || ep.Model != "vendor/model-y" || ep.Local || ep.Headers["Authorization"] != "Bearer env-key" || ep.Headers["X-Title"] != "Cerberus" {
		t.Errorf("endpoint %+v", ep)
	}
	if err := b.Unload(ctx, ms[1]); err != nil {
		t.Fatal(err)
	}
	if p.Headers["Authorization"] != "" {
		t.Error("config headers were mutated")
	}
}

func TestOpenAIKeyFromSecretsAndConfiguredModels(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.SetPath(filepath.Join(dir, "config.json"))
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"anthropic":"secret-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := config.ProviderConfig{Name: "anthropic", BaseURL: "http://127.0.0.1:1/v1", KeyEnv: "CERBERUS_UNSET_KEY",
		Models: []string{"claude-x"}, Enabled: true}
	b := newOpenAI(cfg, p, quietLog())
	ms, err := b.List(context.Background()) // configured: no network
	if err != nil || len(ms) != 1 || ms[0].ID != "anthropic/claude-x" {
		t.Fatalf("models %+v err %v", ms, err)
	}
	if ep := b.Endpoint(ModelInfo{ID: "anthropic/claude-x"}); ep.Model != "claude-x" || ep.Headers["Authorization"] != "Bearer secret-key" {
		t.Errorf("endpoint %+v", ep)
	}
}

func TestOpenAIExplicitAuthorizationWins(t *testing.T) {
	cfg := config.Defaults()
	t.Setenv("CERBERUS_TEST_KEY", "env-key")
	p := config.ProviderConfig{Name: "x", BaseURL: "http://h/v1", KeyEnv: "CERBERUS_TEST_KEY", Enabled: true,
		Headers: map[string]string{"authorization": "Token custom"}}
	h := newOpenAI(cfg, p, quietLog()).Endpoint(ModelInfo{ID: "x/m", Name: "m"}).Headers
	if len(h) != 1 || h["authorization"] != "Token custom" {
		t.Errorf("headers %v", h)
	}
}

func TestOpenAIDiscoveryFailureKeepsCache(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	b := newOpenAI(config.Defaults(), config.ProviderConfig{Name: "p", BaseURL: srv.URL, Enabled: true}, quietLog())
	if _, err := b.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	b.fetched = b.fetched.Add(-discoveryTTL) // expire
	ms, err := b.List(context.Background())
	if err != nil || len(ms) != 1 {
		t.Fatalf("stale list not kept: %+v %v", ms, err)
	}
	fresh := newOpenAI(config.Defaults(), config.ProviderConfig{Name: "p", BaseURL: srv.URL, Enabled: true}, quietLog())
	if _, err := fresh.List(context.Background()); err == nil {
		t.Fatal("want error with no cache")
	}
}

func TestOpenAIKeyOrderWithSecrets(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.SetPath(filepath.Join(dir, "config.json"))
	cfg.Engines.LlamaCpp.Enabled, cfg.Engines.LMStudio.Enabled, cfg.Engines.Ollama.Enabled = false, false, false
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"prov":"file-key","named":"file-key2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []config.ProviderConfig{
		{Name: "prov", BaseURL: "http://h/v1", KeyEnv: "CERBERUS_TEST_ORDER_KEY", Enabled: true},
		{Name: "named", BaseURL: "http://h/v1", KeySecret: "custom", Enabled: true},
	}
	vault := map[string]string{"prov": "vault-key", "custom": "vault-custom"}
	var asked []string
	m := NewManager(cfg, nil, quietLog(), WithSecrets(func(n string) string { asked = append(asked, n); return vault[n] }))
	auth := func(i int) string { return m.backends[i].(*openAI).headers()["Authorization"] }

	t.Setenv("CERBERUS_TEST_ORDER_KEY", "env-key")
	if got := auth(0); got != "Bearer env-key" {
		t.Errorf("env should win: %q", got)
	}
	t.Setenv("CERBERUS_TEST_ORDER_KEY", "")
	if got := auth(0); got != "Bearer vault-key" {
		t.Errorf("vault by Name: %q", got)
	}
	if got := auth(1); got != "Bearer vault-custom" {
		t.Errorf("vault by KeySecret: %q", got)
	}
	delete(vault, "prov")
	if got := auth(0); got != "Bearer file-key" {
		t.Errorf("secrets.json fallback: %q", got)
	}
	if len(asked) == 0 || asked[len(asked)-1] != "prov" {
		t.Errorf("lookups %v", asked)
	}
}
