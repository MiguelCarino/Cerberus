//go:build !windows

package main

import (
	"bufio"
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
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/session"
)

// TestMCPRoundTrip runs `cerberus mcp` (in-process, over pipes) for a
// parent session against a test daemon: the real API handler and session
// manager with fake agents. initialize → tools/list → spawn → wait → read,
// then the limits and the scope through the same tools.
func TestMCPRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	bin := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	parentAgent := write("parent.sh", "echo parent up\nwhile IFS= read -r l; do :; done\n")
	childAgent := write("child.sh", "echo \"child finished: $1\"\nexit 0\n")

	cfg := config.Defaults()
	cfg.Server.Token = "admin-token"
	cfg.RepoRoots, cfg.Repos = nil, nil
	cfg.Guard = config.GuardConfig{MaxSessions: 6}
	cfg.Notify = config.NotifyConfig{Disabled: true}
	cfg.Agents = []config.AgentProfile{
		{Name: "parent", Command: []string{"bash", parentAgent}},
		{Name: "child", Command: []string{"bash", childAgent, "{task}"}},
	}
	cfg.DefaultAgent = "child"
	cfg.Agent.Spawn = config.SpawnConfig{Enabled: true, MaxDepth: 1, MaxChildren: 1, MaxTotal: 4}
	log := quietLog()
	m, err := session.NewManager(cfg, nil, nil, notify.New(cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	ts := httptest.NewServer(api.New(api.Deps{Cfg: cfg, Sessions: m, Log: log, Version: "test"}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer scancel()
		m.Shutdown(sctx)
	})
	out, err := m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Agent: "parent", Task: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	parent := out[0].ID
	other, err := m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Agent: "parent", Task: "unrelated"})
	if err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	exited := make(chan int, 1)
	go func() {
		exited <- mcpCmd([]string{"--url", ts.URL, "--key", cfg.SessionAgentToken(parent), "--session", parent},
			inR, outW, io.Discard)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	p := &mcpPipe{t: t, in: inW, out: sc}

	if res := p.rpc(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})["result"].(map[string]any); res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize %v", res)
	}
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if tools := p.rpc(2, "tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != 9 {
		t.Fatalf("tools %v", tools)
	}

	text, isErr := toolText(t, p.rpc(3, "tools/call", map[string]any{"name": "cerberus_spawn",
		"arguments": map[string]any{"task": "count the files", "name": "counter"}}))
	match := regexp.MustCompile(`Started child session (\S+) \(counter\)`).FindStringSubmatch(text)
	if isErr || match == nil {
		t.Fatalf("spawn: %q %v", text, isErr)
	}
	child := match[1]
	if info, _ := m.Get(child); info.Parent != parent || info.Depth != 1 || info.Agent != "child" {
		t.Fatalf("child info %+v", info)
	}

	text, isErr = toolText(t, p.rpc(4, "tools/call", map[string]any{"name": "cerberus_wait",
		"arguments": map[string]any{"id": child, "timeout_s": 30}}))
	if isErr || !strings.Contains(text, child+" is passed (exit 0)") {
		t.Fatalf("wait: %q %v", text, isErr)
	}
	text, isErr = toolText(t, p.rpc(5, "tools/call", map[string]any{"name": "cerberus_read",
		"arguments": map[string]any{"id": child, "tail_kib": 8}}))
	if isErr || !strings.Contains(text, "child finished: count the files") || !strings.Contains(text, "is passed") {
		t.Fatalf("read: %q %v", text, isErr)
	}
	text, isErr = toolText(t, p.rpc(6, "tools/call", map[string]any{"name": "cerberus_list"}))
	if isErr || !strings.Contains(text, child+" · counter · passed · child") {
		t.Fatalf("list: %q %v", text, isErr)
	}

	// MaxChildren 1: a second spawn is refused, as a tool error.
	text, isErr = toolText(t, p.rpc(7, "tools/call", map[string]any{"name": "cerberus_spawn",
		"arguments": map[string]any{"task": "more"}}))
	if !isErr || !strings.Contains(text, "spawn refused") {
		t.Fatalf("over the limit: %q %v", text, isErr)
	}
	// Sessions outside the caller's tree do not exist for it.
	text, isErr = toolText(t, p.rpc(8, "tools/call", map[string]any{"name": "cerberus_stop",
		"arguments": map[string]any{"id": other[0].ID}}))
	if !isErr || !strings.Contains(text, "404") {
		t.Fatalf("stop another tree: %q %v", text, isErr)
	}
	if info, _ := m.Get(other[0].ID); info.State == session.StateStopped {
		t.Fatal("the unrelated session was stopped")
	}
	text, isErr = toolText(t, p.rpc(9, "tools/call", map[string]any{"name": "cerberus_read", "arguments": map[string]any{}}))
	if !isErr || !strings.Contains(text, "id is required") {
		t.Fatalf("read without id: %q %v", text, isErr)
	}

	inW.Close()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("mcp exited %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mcp did not exit on EOF")
	}
}
