//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/session"
)

// TestMCPClientRoundTrip pairs an MCP client with `cerberus pair --mcp`
// against a test daemon, then runs `cerberus mcp --client` (in-process,
// over pipes) with nothing but the config: the URL and the token come from
// there. The client spawns, lists, waits for and reads its own session and
// cannot touch a person's.
func TestMCPClientRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	isolate(t)
	bin := t.TempDir()
	child := filepath.Join(bin, "child.sh")
	if err := os.WriteFile(child, []byte("#!/bin/bash\necho \"done: $1\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Server.Token = "admin-token"
	cfg.RepoRoots, cfg.Repos = nil, nil
	cfg.Guard = config.GuardConfig{MaxSessions: 4}
	cfg.Notify = config.NotifyConfig{Disabled: true}
	cfg.Agents = []config.AgentProfile{{Name: "child", Command: []string{"bash", child, "{task}"}}}
	cfg.DefaultAgent = "child"
	cfg.Agent.Spawn = config.SpawnConfig{Enabled: true, MaxDepth: 1, MaxChildren: 2, MaxTotal: 4}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg.SetPath(cfgPath)

	log := quietLog()
	m, err := session.NewManager(cfg, nil, nil, notify.New(cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := devices.Open(cfg.Dir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	ts := httptest.NewServer(api.New(api.Deps{Cfg: cfg, Sessions: m, Devices: ds, Log: log, Version: "test"}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer scancel()
		m.Shutdown(sctx)
	})
	cfg.Server.Listen = strings.TrimPrefix(ts.URL, "http://")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	person, err := m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Task: "a person's"})
	if err != nil {
		t.Fatal(err)
	}

	// Pair: through the daemon, the token saved 0600, the command printed.
	var out, errb bytes.Buffer
	if code := run([]string{"pair", "--mcp", "--name", "claude-code", "-config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("pair --mcp: %d %s", code, errb.String())
	}
	tokPath := filepath.Join(cfg.Dir(), "mcp-clients", "claude-code.token")
	st, err := os.Stat(tokPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", st, err)
	}
	raw, _ := os.ReadFile(tokPath)
	if !strings.HasPrefix(string(raw), devices.ClientPrefix) {
		t.Fatalf("token %q", raw)
	}
	if o := out.String(); !strings.Contains(o, "claude mcp add --scope user cerberus -- ") ||
		!strings.Contains(o, "mcp --client claude-code --config "+cfgPath) || strings.Contains(o, string(raw[:12])) {
		t.Fatalf("pair output (the token must not be printed):\n%s", o)
	}
	if list := ds.List(); len(list) != 1 || list[0].Scope != devices.ScopeMCP {
		t.Fatalf("devices %+v", list)
	}
	out.Reset()
	if code := run([]string{"devices", "-config", cfgPath}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "(MCP client)") {
		t.Fatalf("devices list: %d %s", code, out.String())
	}

	// A client with no token says how to get one.
	if code := mcpCmd([]string{"--client", "nobody", "-config", cfgPath}, strings.NewReader(""), io.Discard, &errb); code != 1 ||
		!strings.Contains(errb.String(), "cerberus pair --mcp --name nobody") {
		t.Fatalf("no token: %d %s", code, errb.String())
	}

	// The OPENAI_API_KEY and CERBERUS_URL of the agent's own environment
	// are not taken for the client's.
	t.Setenv("OPENAI_API_KEY", "sk-someone-elses")
	t.Setenv("CERBERUS_URL", "http://127.0.0.1:1")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	exited := make(chan int, 1)
	go func() {
		exited <- mcpCmd([]string{"--client", "claude-code", "-config", cfgPath}, inR, outW, io.Discard)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	p := &mcpPipe{t: t, in: inW, out: sc}

	res := p.rpc(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})["result"].(map[string]any)
	if !strings.Contains(res["instructions"].(string), "cerberus_ask") {
		t.Fatalf("client instructions %v", res["instructions"])
	}
	text, isErr := toolText(t, p.rpc(2, "tools/call", map[string]any{"name": "cerberus_spawn",
		"arguments": map[string]any{"task": "count the files", "name": "counter"}}))
	match := regexp.MustCompile(`Started child session (\S+) \(counter\)`).FindStringSubmatch(text)
	if isErr || match == nil {
		t.Fatalf("spawn: %q %v", text, isErr)
	}
	id := match[1]
	if info, _ := m.Get(id); !strings.HasPrefix(info.Client, "mcp:claude-code#") || info.Parent != "" {
		t.Fatalf("client session %+v", info)
	}
	text, isErr = toolText(t, p.rpc(3, "tools/call", map[string]any{"name": "cerberus_wait",
		"arguments": map[string]any{"id": id, "timeout_s": 30}}))
	if isErr || !strings.Contains(text, id+" is passed (exit 0)") {
		t.Fatalf("wait: %q %v", text, isErr)
	}
	text, isErr = toolText(t, p.rpc(4, "tools/call", map[string]any{"name": "cerberus_read",
		"arguments": map[string]any{"id": id}}))
	if isErr || !strings.Contains(text, "done: count the files") {
		t.Fatalf("read: %q %v", text, isErr)
	}
	text, isErr = toolText(t, p.rpc(5, "tools/call", map[string]any{"name": "cerberus_list"}))
	if isErr || !strings.Contains(text, id) || strings.Contains(text, person[0].ID) {
		t.Fatalf("list: %q %v", text, isErr)
	}
	text, isErr = toolText(t, p.rpc(6, "tools/call", map[string]any{"name": "cerberus_stop",
		"arguments": map[string]any{"id": person[0].ID}}))
	if !isErr || !strings.Contains(text, "404") {
		t.Fatalf("stop a person's session: %q %v", text, isErr)
	}
	inW.Close()
	if code := <-exited; code != 0 {
		t.Fatalf("exit %d", code)
	}
}
