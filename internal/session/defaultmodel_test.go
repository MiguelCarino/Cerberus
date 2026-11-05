package session

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/notify"
)

// newModelManager is a session Manager (scheduler not running) over an
// engine whose only backend is an API provider listing models.
func newModelManager(t *testing.T, models ...string) *Manager {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	cfg := config.Defaults()
	cfg.RepoRoots = nil
	cfg.Notify = config.NotifyConfig{Disabled: true}
	cfg.Engines.DefaultModel = ""
	cfg.Engines.LlamaCpp.Enabled, cfg.Engines.LMStudio.Enabled, cfg.Engines.Ollama.Enabled = false, false, false
	cfg.Providers = []config.ProviderConfig{{Name: "p", BaseURL: "http://127.0.0.1:1/v1", Models: models, Enabled: true}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewManager(cfg, engine.NewManager(cfg, nil, log), nil, notify.New(cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	})
	return m
}

// A fresh install (engines.default_model unset) with one model: a task for
// the built-in agent with no model picked gets that model, as chat's
// "default" does; with several models it is still refused, saying why.
func TestNormalizeImplicitDefaultModel(t *testing.T) {
	m := newModelManager(t, "solo")
	got, err := m.normalize(context.Background(), TaskSpec{Task: "fix x", Agent: "cerberus", Workspace: WorkspaceScratch})
	if err != nil || got.Model != "p/solo" {
		t.Fatalf("one model: %q %v", got.Model, err)
	}

	m = newModelManager(t, "a", "b")
	_, err = m.normalize(context.Background(), TaskSpec{Task: "fix x", Agent: "cerberus", Workspace: WorkspaceScratch})
	if err == nil || !strings.Contains(err.Error(), "engines.default_model") {
		t.Fatalf("two models: %v", err)
	}
}
