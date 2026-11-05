package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRotateTokenKeepsDiskEdits: rotation changes only the token fields of
// config.json, so edits made on disk while the daemon runs survive.
func TestRotateTokenKeepsDiskEdits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// The owner edits the file by hand after the daemon loaded it.
	raw, _ := os.ReadFile(p)
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["server"].(map[string]any)["allowed_hosts"] = []string{"box.lan"}
	doc["future_key"] = "kept"
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.RotateToken("newtok"); err != nil {
		t.Fatal(err)
	}
	if c.TokenSnapshot() != "newtok" || c.TokenCreatedAt().IsZero() {
		t.Fatal("in-memory token not updated")
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.Token != "newtok" {
		t.Fatalf("disk token %q", got.Server.Token)
	}
	if len(got.Server.AllowedHosts) != 1 || got.Server.AllowedHosts[0] != "box.lan" {
		t.Fatalf("hand edit lost: %v", got.Server.AllowedHosts)
	}
	raw, _ = os.ReadFile(p)
	if !strings.Contains(string(raw), "future_key") {
		t.Fatal("unknown key dropped")
	}
}

// TestRotateTokenFailureKeepsLiveToken: a write that fails leaves the
// in-memory token unchanged.
func TestRotateTokenFailureKeepsLiveToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	old := c.TokenSnapshot()
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.RotateToken("newtok"); err == nil {
		t.Fatal("rotation over a broken file should fail")
	}
	if c.TokenSnapshot() != old {
		t.Fatal("live token changed although the write failed")
	}
}
