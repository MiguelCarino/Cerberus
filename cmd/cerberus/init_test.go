//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/config"
)

// fixtureMachine is a pretend computer: temp home and data folders, fake
// binaries on a temp PATH (LM Studio, llama-server, nvidia-smi, claude,
// aider, tailscale with HTTPS, bwrap), an Ollama that answers, two .gguf
// files and Atrium's notify script.
func fixtureMachine(t *testing.T) (machine, string, *int) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	bin := filepath.Join(home, "bin")
	scripts := map[string]string{
		"lms":          `[ "$1" = ls ] && echo '[{"type":"llm","modelKey":"qwen3-8b"},{"type":"embedding","modelKey":"nomic-embed"}]'`,
		"llama-server": `exit 0`,
		"nvidia-smi":   `echo "NVIDIA Test GPU, 24576 MiB"`,
		"claude":       `exit 0`,
		"aider":        `exit 0`,
		"bwrap":        `exit 0`,
		"tailscale": `case "$1" in
ip) echo 100.64.1.2 ;;
status) echo '{"Self":{"DNSName":"desk.tail1234.ts.net."},"CertDomains":["desk.tail1234.ts.net"]}' ;;
cert) echo "init must never request a certificate" >&2; exit 9 ;;
esac`,
	}
	for name, body := range scripts {
		writeExec(t, filepath.Join(bin, name), "#!/bin/sh\n"+body+"\n")
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	for _, f := range []string{"models/coder-7b.gguf", "models/sub/vision-2b.gguf", "models/sub/mmproj-vision.gguf"} {
		p := filepath.Join(home, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("GGUF"), 0o644)
	}
	writeExec(t, filepath.Join(home, ".config", "hypr", "scripts", "notify"), "#!/bin/sh\n")
	installs := 0
	m := machine{goos: "linux", home: home, lookPath: exec.LookPath,
		output: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		httpGet: func(ctx context.Context, url string) ([]byte, error) {
			if strings.HasSuffix(url, "/api/tags") {
				return []byte(`{"models":[{"name":"llama3.2:3b"},{"name":"qwen3-8b"}]}`), nil
			}
			return nil, errors.New("connection refused")
		},
		stat: func(p string) (os.FileInfo, error) {
			if strings.HasPrefix(p, "/sys/") { // the fixture has no RAPL
				return nil, os.ErrNotExist
			}
			return os.Stat(p)
		},
		readFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		readable: func(string) bool { return false },
		ifaceIPs: func() []string { return []string{"192.168.1.20", "100.64.1.2"} },
		install:  func(io.Writer) error { installs++; return nil },
		doctor: func(e doctor.Env) doctor.Env {
			e.UserConfig = filepath.Join(home, ".config")
			e.Bwrap = func(context.Context) (string, error) { return "", nil }
			return e
		},
	}
	return m, home, &installs
}

// freeAddr is a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func writeExec(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInitDetect(t *testing.T) {
	m, home, _ := fixtureMachine(t)
	d := m.detect(context.Background(), config.Defaults())
	if want := []string{"qwen3-8b", "llama3.2:3b", "coder-7b", "vision-2b"}; !slices.Equal(d.Models, want) {
		t.Fatalf("models %v, want %v", d.Models, want)
	}
	if d.GPU != "NVIDIA Test GPU, 24576 MiB" || !slices.Equal(d.Agents, []string{"claude", "aider"}) {
		t.Fatalf("gpu %q agents %v", d.GPU, d.Agents)
	}
	ts := d.Tailscale
	if !ts.Found || ts.IP != "100.64.1.2" || ts.DNSName != "desk.tail1234.ts.net" || !ts.CertOK {
		t.Fatalf("tailscale %+v", ts)
	}
	if d.LANIP != "192.168.1.20" || !d.Bwrap || d.RAPL != "absent" || d.Notify != filepath.Join(home, ".config", "hypr", "scripts", "notify") {
		t.Fatalf("detected %+v", d)
	}
}

// TestInitScripted answers the six questions from stdin (with one retry).
func TestInitScripted(t *testing.T) {
	m, home, installs := fixtureMachine(t)
	cfgPath := filepath.Join(home, ".config", "cerberus", "config.json")
	stdin := strings.NewReader("2\ntailscale\ny\nmytopic\nlots\n250\ny\n")
	var out, errb bytes.Buffer
	if code := runInit(nil, stdin, &out, &errb, m); code != 0 {
		t.Fatalf("init %d: %s\n%s", code, errb.String(), out.String())
	}
	o := out.String()
	for _, want := range []string{"1/6", "2/6", "3/6", "4/6", "5/6", "6/6", "Please give a number", "Created " + cfgPath,
		"tailscale cert --cert-file", "https://desk.tail1234.ts.net:7340/?token=", "Cerberus doctor", "\u2580"} {
		if !strings.Contains(o, want) {
			t.Errorf("output lacks %q:\n%s", want, o)
		}
	}
	if *installs != 1 {
		t.Fatalf("installs %d", *installs)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Server
	if !s.InitDone || s.Token == "" || s.PublicURL != "https://desk.tail1234.ts.net:7340" ||
		!slices.Equal(s.ExtraListen, []string{"100.64.1.2:7340"}) || !slices.Contains(s.AllowedHosts, "desk.tail1234.ts.net") ||
		s.TLSCert != filepath.Join(home, ".config", "cerberus", "tls", "desk.tail1234.ts.net.crt") {
		t.Fatalf("server %+v", s)
	}
	if cfg.Engines.DefaultModel != "llama3.2:3b" || cfg.Notify.NtfyURL != "https://ntfy.sh/mytopic" ||
		cfg.Tariff.HouseholdKWhMonth != 250 || len(cfg.Notify.Command) != 3 {
		t.Fatalf("config %+v %+v %+v", cfg.Engines, cfg.Notify, cfg.Tariff)
	}
	if _, err := os.Stat(cfg.Server.TLSCert); err == nil {
		t.Fatal("init must not create a certificate")
	}
}

// TestInitYesJSON takes the defaults and prints JSON for scripts.
func TestInitYesJSON(t *testing.T) {
	m, home, installs := fixtureMachine(t)
	var out, errb bytes.Buffer
	if code := runInit([]string{"--json"}, strings.NewReader(""), &out, &errb, m); code != 0 {
		t.Fatalf("init %d: %s", code, errb.String())
	}
	var res InitResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if !res.Created || res.Answers.Model != "qwen3-8b" || res.Answers.Phone != "none" || *installs != 0 ||
		!strings.HasPrefix(res.PairURL, "http://127.0.0.1:7340/?token=") || len(res.Doctor.Checks) == 0 {
		t.Fatalf("result %+v", res)
	}
	cfg, err := config.Load(filepath.Join(home, ".config", "cerberus", "config.json"))
	if err != nil || !cfg.Server.InitDone || len(cfg.Server.ExtraListen) != 0 || cfg.Engines.DefaultModel != "qwen3-8b" {
		t.Fatalf("config %v %+v", err, cfg.Server)
	}
	// A second run keeps the token and changes nothing it was not asked.
	// (On a closed port: never the owner's real daemon.)
	cfg.Server.Listen = freeAddr(t)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	tok := cfg.Server.Token
	out.Reset()
	if code := runInit([]string{"--yes"}, strings.NewReader(""), &out, &errb, m); code != 0 {
		t.Fatalf("second init %d", code)
	}
	cfg2, _ := config.Load(cfg.Path())
	if cfg2.Server.Token != tok || !strings.Contains(out.String(), "Updated ") {
		t.Fatalf("second run: token kept %v\n%s", cfg2.Server.Token == tok, out.String())
	}
}
