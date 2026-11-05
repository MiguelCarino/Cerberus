package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestServeLifecycle starts the daemon with every engine disabled, checks
// /healthz and an authenticated call, then shuts it down while an SSE stream
// is open: shutdown must not wait for the stream.
func TestServeLifecycle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	cfg := config.Defaults()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.ExtraListen = []string{"256.0.0.1:1"} // unavailable: logged, not fatal
	cfg.Server.Token = "tok"
	cfg.GPU.Backend = "none"
	cfg.Engines.LlamaCpp.Enabled = false
	cfg.Engines.LMStudio.Enabled = false
	cfg.Engines.Ollama.Enabled = false
	cfg.Notify.Disabled = true
	cfg.RepoRoots = nil
	cfg.Vault.Backend = "file" // never the desktop keyring in tests
	cfg.Subscriptions = nil    // never the real ~/.claude
	cfg.Voice.Enabled = false
	cfgPath := filepath.Join(tmp, "config.json")
	cfg.SetPath(cfgPath)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan []string, 1)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- serve(ctx, func() {}, cfgPath, log, func(a []string) { ready <- a }) }()

	var base string
	select {
	case addrs := <-ready:
		if len(addrs) != 1 {
			t.Fatalf("addrs = %v", addrs)
		}
		base = "http://" + addrs[0]
	case err := <-done:
		t.Fatalf("serve exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve not ready")
	}

	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := get("/healthz"); r.StatusCode != 200 {
		t.Fatalf("healthz %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r := get("/api/status"); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	} else {
		var st struct {
			VaultBackend string `json:"vault_backend"`
		}
		_ = json.NewDecoder(r.Body).Decode(&st)
		r.Body.Close()
		if st.VaultBackend != "file" {
			t.Fatalf("vault_backend %q", st.VaultBackend)
		}
	}
	// The vault is wired: a secret stored through the API is listed.
	req, _ := http.NewRequest("PUT", base+"/api/secrets/OPENAI", strings.NewReader(`{"value":"sk-test-123456"}`))
	req.Header.Set("Authorization", "Bearer tok")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("put secret: %v %v", err, resp)
	} else {
		resp.Body.Close()
	}
	if r := get("/api/secrets"); r.StatusCode != 200 {
		t.Fatalf("secrets %d", r.StatusCode)
	} else {
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if !strings.Contains(string(raw), `"OPENAI"`) || strings.Contains(string(raw), "sk-test") {
			t.Fatalf("secrets = %s", raw)
		}
	}
	for _, p := range []string{"/api/usage", "/api/voice", "/api/batches", "/api/search?q=x", "/api/flows", "/api/flows/runs"} {
		if r := get(p); r.StatusCode != 200 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		} else {
			r.Body.Close()
		}
	}
	stream := get("/api/events")
	defer stream.Body.Close()

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not shut down")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("shutdown took %s", d)
	}
}

func TestWithUserBins(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.ListSeparator)
	cases := []struct{ in, want string }{
		{"/usr/bin", local + sep + "/usr/bin"},
		{local + sep + "/usr/bin", local + sep + "/usr/bin"},
		{"", local},
	}
	for _, c := range cases {
		if got := withUserBins(c.in, home); got != c.want {
			t.Errorf("withUserBins(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
