package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// TestHelpEveryCommand checks that each subcommand answers -h (and
// "cerberus help CMD") with its own usage line and exit 0, and that the
// overview names it.
func TestHelpEveryCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/cfg")
	t.Setenv("XDG_DATA_HOME", home+"/data")

	var overview bytes.Buffer
	if code := run([]string{"help"}, nil, &overview, &overview); code != 0 {
		t.Fatalf("help: exit %d", code)
	}
	cmds := []string{"serve", "status", "task", "url", "secret", "search", "resume", "fleet",
		"pull", "prune", "top", "energy", "rapl-setup", "pair", "devices", "token", "audit", "finetune",
		"doctor", "init", "config", "export", "import", "install-service", "mcp", "user"}
	for _, c := range cmds {
		if !strings.Contains(overview.String(), "\n  "+c) {
			t.Errorf("overview does not list %q", c)
		}
		for _, args := range [][]string{{c, "-h"}, {"help", c}} {
			var out bytes.Buffer
			if code := run(args, nil, &out, &out); code != 0 {
				t.Errorf("%v: exit %d\n%s", args, code, out.String())
			}
			if !strings.Contains(out.String(), "Usage: cerberus "+c) {
				t.Errorf("%v: no usage line:\n%s", args, out.String())
			}
		}
	}
	for _, args := range [][]string{{"config", "set", "-h"}, {"secret", "set", "--help"}} {
		var out bytes.Buffer
		if code := run(args, nil, &out, &out); code != 0 || !strings.Contains(out.String(), "Usage: cerberus "+args[0]+" "+args[1]) {
			t.Errorf("%v: exit %d\n%s", args, code, out.String())
		}
	}
	// "help CMD SUB" is "CMD SUB -h": the subcommand's own help.
	for _, args := range [][]string{{"help", "secret", "set"}, {"help", "config", "set"}, {"help", "secret", "list"}} {
		var out bytes.Buffer
		want := "Usage: cerberus " + args[1] + " " + args[2]
		if code := run(args, nil, &out, &out); code != 0 || !strings.Contains(out.String(), want) {
			t.Errorf("%v: exit %d, want %q\n%s", args, code, want, out.String())
		}
	}
	for _, args := range [][]string{{"version", "-h"}, {"version", "--help"}, {"help", "version"}} {
		var out bytes.Buffer
		if code := run(args, nil, &out, &out); code != 0 || !strings.Contains(out.String(), "Usage: cerberus version") {
			t.Errorf("%v: exit %d\n%s", args, code, out.String())
		}
	}
	// Unknown commands exit 2, also when help is asked for.
	for _, args := range [][]string{{"no-such-command"}, {"no-such-command", "-h"}, {"help", "no-such-command"}} {
		var out bytes.Buffer
		if code := run(args, nil, &out, &out); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// TestHelpNeverRuns: "cerberus help CMD ARGS…" prints help and never runs
// the command, even for commands whose flags stop at the first argument
// (before, "help task fix it" started the task "fix it -h").
func TestHelpNeverRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/cfg")
	t.Setenv("XDG_DATA_HOME", home+"/data")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cfg := config.Defaults()
	cfg.Server.Listen = strings.TrimPrefix(srv.URL, "http://")
	cfgPath := filepath.Join(home, "config.json")
	cfg.SetPath(cfgPath)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"help", "task", "-config", cfgPath, "fix", "it"},
		{"help", "resume", "-config", cfgPath, "20260101-000000-abcdef"},
		{"help", "search", "-config", cfgPath, "words"},
		{"help", "secret", "set", "NAME"},
	} {
		var out bytes.Buffer
		code := run(args, strings.NewReader(""), &out, &out)
		want := "Usage: cerberus " + args[1]
		if code != 0 || !strings.Contains(out.String(), want) {
			t.Errorf("%v: exit %d, want 0 and %q\n%s", args, code, want, out.String())
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("help reached the daemon %d times", n)
	}
}
