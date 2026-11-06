package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/session"
)

func TestSessionsSetupPrint(t *testing.T) {
	a := session.SetupView{Hash: "aaaaaaaaaaaa", Recorded: "aaaaaaaaaaaa", Verified: true, Setup: session.Setup{
		Agent:     session.SetupAgent{Name: "claude", Command: []string{"claude", "-p", "{task}"}, Version: "2.1.0", Sandbox: "bwrap"},
		Workspace: session.WorkspaceScratch, Cerberus: "1.0"}}
	var out bytes.Buffer
	printSetup(&out, "s1", a, false)
	for _, want := range []string{"Setup of s1 · aaaaaaaaaaaa", "agent.version", "2.1.0", "claude -p {task}"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "model.id") {
		t.Fatalf("empty fields should be left out:\n%s", out.String())
	}

	b := a
	b.Hash, b.Recorded, b.Verified = "bbbbbbbbbbbb", "cccccccccccc", false
	b.Setup.Agent.Version = "2.2.0"
	out.Reset()
	printSetupDiff(&out, []string{"s1", "s2"}, []session.SetupView{a, b}, false)
	s := out.String()
	if !strings.Contains(s, "agent.version") || !strings.Contains(s, "2.2.0") || strings.Contains(s, "agent.name") ||
		!strings.Contains(s, "changed since the run started") {
		t.Fatalf("diff output:\n%s", s)
	}
	out.Reset()
	printSetupDiff(&out, []string{"s1", "s1"}, []session.SetupView{a, a}, false)
	if !strings.Contains(out.String(), "The same setup.") {
		t.Fatalf("same output:\n%s", out.String())
	}
}

// TestSessionsTrajectory: the command streams the daemon's answer to
// stdout, passes --format through, and prints the 409 message.
func TestSessionsTrajectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/sessions/s1/trajectory":
			_, _ = io.WriteString(w, "format="+r.URL.Query().Get("format")+"\n")
		default:
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"request records are off","reason":"records_off"}`)
		}
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/cfg")
	t.Setenv("XDG_DATA_HOME", home+"/data")
	cfgPath := filepath.Join(home, "config.json")
	listen := strings.TrimPrefix(srv.URL, "http://")
	if err := os.WriteFile(cfgPath, []byte(`{"server":{"listen":"`+listen+`","token":"t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"sessions", "trajectory", "-config", cfgPath, "s1"}, nil, &out, &errb); code != 0 || out.String() != "format=atif\n" {
		t.Fatalf("atif: exit %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"sessions", "trajectory", "-config", cfgPath, "--format", "jsonl", "s1"}, nil, &out, &errb); code != 0 || out.String() != "format=jsonl\n" {
		t.Fatalf("jsonl: exit %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"sessions", "trajectory", "-config", cfgPath, "s2"}, nil, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "request records are off") || out.Len() != 0 {
		t.Fatalf("409: exit %d %q %q", code, out.String(), errb.String())
	}
	if code := run([]string{"sessions", "trajectory", "-config", cfgPath, "--format", "xml", "s1"}, nil, &out, &errb); code != 2 {
		t.Fatalf("bad format: exit %d", code)
	}
}
