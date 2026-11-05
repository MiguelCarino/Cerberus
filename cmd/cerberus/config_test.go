//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// cliConfig writes a config in temp folders on a closed port (so no CLI
// command ever reaches the owner's daemon) and returns its path.
func cliConfig(t *testing.T, mut func(*config.Config)) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	cfg := config.Defaults()
	cfg.Server.Listen = freeAddr(t)
	cfg.Server.Token = "tok"
	cfg.RepoRoots = nil
	if mut != nil {
		mut(cfg)
	}
	p := filepath.Join(home, "config", "cerberus", "config.json")
	cfg.SetPath(p)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return p
}

func cfgCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestConfigCLIOffline(t *testing.T) {
	p := cliConfig(t, nil)
	c := "-config=" + p
	if code, out, _ := cfgCLI(t, "", "config", "path", c); code != 0 || strings.TrimSpace(out) != p {
		t.Fatalf("path %d %q", code, out)
	}
	if code, out, _ := cfgCLI(t, "", "config", "get", "engines.idle_ttl_sec", c); code != 0 || strings.TrimSpace(out) != "600" {
		t.Fatalf("get %d %q", code, out)
	}
	if code, out, _ := cfgCLI(t, "", "config", "get", c); code != 0 || strings.Contains(out, `"tok"`) {
		t.Fatalf("get all leaks the token or failed: %d", code)
	}
	if code, _, errs := cfgCLI(t, "", "config", "get", "server.token", c); code != 1 || !strings.Contains(errs, "sensitive") {
		t.Fatalf("get token %d %q", code, errs)
	}
	if code, out, errs := cfgCLI(t, "", "config", "set", "engines.idle_ttl_sec", "15m", c); code != 0 || !strings.Contains(out, "Saved engines.idle_ttl_sec") {
		t.Fatalf("set %d %q %q", code, out, errs)
	}
	if code, out, _ := cfgCLI(t, "", "config", "get", "engines.idle_ttl_sec", c); code != 0 || strings.TrimSpace(out) != "900" {
		t.Fatalf("after set %q", out)
	}
	if code, _, errs := cfgCLI(t, "", "config", "set", "guard.max_sessions", "0", c); code != 1 || !strings.Contains(errs, "guard.max_sessions") {
		t.Fatalf("invalid set %d %q", code, errs)
	}
	if code, _, _ := cfgCLI(t, "", "config", "set", "agents", `[{"name":"mine","command":["bash"]}]`, c); code != 0 {
		t.Fatal("set a list as JSON")
	}
	if code, out, _ := cfgCLI(t, "", "config", "reset", "engines.idle_ttl_sec", c); code != 0 || !strings.Contains(out, "Saved") {
		t.Fatalf("reset %d %q", code, out)
	}
	if _, out, _ := cfgCLI(t, "", "config", "get", "engines.idle_ttl_sec", c); strings.TrimSpace(out) != "600" {
		t.Fatalf("after reset %q", out)
	}
	if code, out, _ := cfgCLI(t, "", "config", "validate", c); code != 0 || !strings.Contains(out, "valid") && !strings.Contains(out, "warning") {
		t.Fatalf("validate %d %q", code, out)
	}
	if code, out, _ := cfgCLI(t, "", "config", "schema", "--basic", c); code != 0 || !strings.Contains(out, "server.listen") || strings.Contains(out, "gpu.nvidia_smi") {
		t.Fatalf("schema %d", code)
	}
	if code, out, _ := cfgCLI(t, "", "config", "schema", "--json", c); code != 0 || !json.Valid([]byte(out)) {
		t.Fatalf("schema json %d", code)
	}
	raw, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_DATA_HOME"), "cerberus", "audit.jsonl"))
	if !strings.Contains(string(raw), "config.set") || !strings.Contains(string(raw), "config.reset") {
		t.Fatalf("CLI changes not audited: %s", raw)
	}
	// A broken file fails validation.
	if err := os.WriteFile(p, []byte(`{"guard":{"max_sessions":-1},"server":{"token":"tok","listen":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := cfgCLI(t, "", "config", "validate", c); code != 1 {
		t.Fatalf("validate broken %d", code)
	}
}

func TestConfigEdit(t *testing.T) {
	p := cliConfig(t, nil)
	bin := t.TempDir()
	good := filepath.Join(bin, "good-editor")
	writeExec(t, good, "#!/bin/sh\nsed -i 's/\"idle_ttl_sec\": 600/\"idle_ttl_sec\": 99/' \"$1\"\n")
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", good)
	if code, out, errs := cfgCLI(t, "", "config", "edit", "-config="+p); code != 0 || !strings.Contains(out, "engines.idle_ttl_sec") {
		t.Fatalf("edit %d %q %q", code, out, errs)
	}
	cfg, _ := config.Load(p)
	if cfg.Engines.IdleTTLSec != 99 || cfg.Server.Token != "tok" {
		t.Fatalf("after edit ttl %d", cfg.Engines.IdleTTLSec)
	}
	before, _ := os.ReadFile(p)
	bad := filepath.Join(bin, "bad-editor")
	writeExec(t, bad, "#!/bin/sh\necho '{broken' > \"$1\"\n")
	t.Setenv("EDITOR", bad)
	code, out, errs := cfgCLI(t, "n\n", "config", "edit", "-config="+p)
	if code != 1 || !strings.Contains(errs, "not valid config JSON") || !strings.Contains(out, "Kept the old config") {
		t.Fatalf("bad edit %d %q %q", code, out, errs)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("a bad edit changed the file")
	}
}

// TestExportImportRoundTrip exports a profile and imports it on a fresh
// machine: settings travel, secrets never do.
func TestExportImportRoundTrip(t *testing.T) {
	src := cliConfig(t, func(c *config.Config) {
		c.Agents = []config.AgentProfile{{Name: "mine", Command: []string{"bash"}, Env: map[string]string{"API_TOKEN": "sk-live-123", "MODE": "x"},
			SecretEnv: map[string]string{"GH": "github"}}}
		c.Providers = []config.ProviderConfig{{Name: "acme", BaseURL: "https://api.acme.test/v1", Enabled: true, KeySecret: "acme-key",
			Headers: map[string]string{"Authorization": "Bearer sk-live-456"}}}
		c.Prices = map[string]config.Price{"acme/*": {In: 1, Out: 2}}
		c.Budgets = []config.Budget{{Provider: "acme", Period: "month", SoftUSD: 5, HardUSD: 10}}
		c.Tariff.HouseholdKWhMonth = 321
		c.Guard.MaxSessions = 7
	})
	dir := t.TempDir()
	bundle := filepath.Join(dir, "profile.json")
	if code, _, errs := cfgCLI(t, "", "export", "-config="+src, "-o", bundle); code != 0 {
		t.Fatalf("export %d %s", code, errs)
	}
	raw, _ := os.ReadFile(bundle)
	for _, leak := range []string{"sk-live", `"tok"`, "acme-key", "github"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("bundle leaks %q:\n%s", leak, raw)
		}
	}
	code, out, _ := cfgCLI(t, "", "export", "-config="+src, "--secrets-names")
	if code != 0 || !strings.Contains(out, `"acme-key"`) || !strings.Contains(out, `"github"`) || strings.Contains(out, "sk-live") {
		t.Fatalf("export with names %d:\n%s", code, out)
	}

	dst := cliConfig(t, nil) // a new machine (new temp home)
	before, _ := os.ReadFile(dst)
	code, out, _ = cfgCLI(t, "n\n", "import", bundle, "-config="+dst)
	if code != 1 || !strings.Contains(out, "agents") || !strings.Contains(out, "Nothing changed") {
		t.Fatalf("declined import %d %q", code, out)
	}
	if after, _ := os.ReadFile(dst); string(after) != string(before) {
		t.Fatal("declined import changed the file")
	}
	if code, out, errs := cfgCLI(t, "", "import", bundle, "--yes", "-config="+dst); code != 0 {
		t.Fatalf("import %d %q %q", code, out, errs)
	}
	got, err := config.Load(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Env["MODE"] != "x" || got.Agents[0].Env["API_TOKEN"] != "" ||
		len(got.Providers) != 1 || got.Providers[0].KeySecret != "" || got.Providers[0].Headers["Authorization"] != "" ||
		got.Prices["acme/*"].Out != 2 || len(got.Budgets) != 1 || got.Tariff.HouseholdKWhMonth != 321 || got.Guard.MaxSessions != 7 {
		t.Fatalf("imported %+v %+v %+v", got.Agents, got.Providers, got.Guard)
	}
	if got.Server.Token != "tok" {
		t.Fatal("import touched the token")
	}
	// Importing again changes nothing.
	if code, out, _ := cfgCLI(t, "", "import", bundle, "-config="+dst); code != 0 || !strings.Contains(out, "nothing to change") {
		t.Fatalf("re-import %d %q", code, out)
	}
}

// TestConfigCLIThroughDaemon: with the daemon running, `config set` goes
// through PATCH /api/config and applies live; `config restart` ends serve
// with errRestart.
func TestConfigCLIThroughDaemon(t *testing.T) {
	p := cliConfig(t, func(c *config.Config) {
		c.GPU.Backend = "none"
		c.Engines.LlamaCpp.Enabled, c.Engines.LMStudio.Enabled, c.Engines.Ollama.Enabled = false, false, false
		c.Notify.Disabled = true
		c.Vault.Backend = "file"
		c.Subscriptions = nil
		c.Voice.Enabled = false
		c.Monitor.Enabled = false
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan []string, 1)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- serve(ctx, func() {}, p, log, func(a []string) { ready <- a }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serve: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("not ready")
	}
	code, out, errs := cfgCLI(t, "", "config", "set", "guard.max_sessions", "5", "-config="+p)
	if code != 0 || !strings.Contains(out, "guard.max_sessions: applied now") {
		t.Fatalf("set via daemon %d %q %q", code, out, errs)
	}
	code, out, _ = cfgCLI(t, "", "config", "set", "gpu.backend", "nvidia", "-config="+p)
	if code != 0 || !strings.Contains(out, "after a restart") {
		t.Fatalf("restart setting %d %q", code, out)
	}
	if code, _, errs := cfgCLI(t, "", "config", "set", "guard.max_sessions", "0", "-config="+p); code != 1 || !strings.Contains(errs, "guard.max_sessions") {
		t.Fatalf("invalid via daemon %d %q", code, errs)
	}
	cfg, _ := config.Load(p)
	if cfg.Guard.MaxSessions != 5 || cfg.GPU.Backend != "nvidia" {
		t.Fatalf("daemon did not save: %+v %+v", cfg.Guard, cfg.GPU)
	}
	if code, _, errs := cfgCLI(t, "", "config", "restart", "-config="+p); code != 0 {
		t.Fatalf("restart %d %q", code, errs)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errRestart) {
			t.Fatalf("serve returned %v, want errRestart", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop for the restart")
	}
}

// A daemon on the configured address that refuses this file's token is
// another install (the daemon saves every rotation to its file): config set
// writes the file instead of failing.
func TestConfigSetOtherDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"missing or invalid token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := cliConfig(t, func(c *config.Config) { c.Server.Listen = strings.TrimPrefix(srv.URL, "http://") })
	c := "-config=" + p
	if code, out, errs := cfgCLI(t, "", "config", "set", "engines.idle_ttl_sec", "5m", c); code != 0 || !strings.Contains(out, "Saved") {
		t.Fatalf("set %d %q %q", code, out, errs)
	}
	if code, out, _ := cfgCLI(t, "", "config", "get", "engines.idle_ttl_sec", c); code != 0 || strings.TrimSpace(out) != "300" {
		t.Fatalf("after set %q", out)
	}
}
