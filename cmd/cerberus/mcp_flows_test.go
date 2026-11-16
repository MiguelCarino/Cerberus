package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"testing"

	"carino.systems/cerberus/internal/agent"
)

// The flow tools are offered to an MCP client (cerberus mcp --client)
// only, and a call reaches the caller like any other tool.
func TestMCPFlowToolsForClientsOnly(t *testing.T) {
	names := func(p *mcpPipe) []string {
		var out []string
		for _, raw := range p.rpc(1, "tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any) {
			out = append(out, raw.(map[string]any)["name"].(string))
		}
		return out
	}
	session := startMCP(t, &fakeCaller{})
	for _, n := range names(session) {
		if agent.IsFlowTool(n) {
			t.Fatalf("a session's server offers %s", n)
		}
	}
	if m := session.rpc(2, "tools/call", map[string]any{"name": agent.ToolFlows, "arguments": map[string]any{}}); m["error"] == nil {
		t.Fatalf("a session's server ran %s: %v", agent.ToolFlows, m)
	}

	f := &fakeCaller{}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := newMCPServer(f, outW)
	srv.extra = agent.FlowTools()
	done := make(chan error, 1)
	go func() { done <- srv.serve(context.Background(), inR); outW.Close() }()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	client := &mcpPipe{t: t, in: inW, out: sc, done: done}
	t.Cleanup(func() {
		inW.Close()
		go func() { _, _ = io.Copy(io.Discard, outR) }()
		<-done
	})
	got := names(client)
	want := []string{agent.ToolFlows, agent.ToolRunFlow, agent.ToolFlowRun}
	if len(got) != 9+len(want) {
		t.Fatalf("client tools %v", got)
	}
	for i, n := range want {
		if got[9+i] != n {
			t.Fatalf("client tools %v", got)
		}
	}
	m := client.rpc(2, "tools/call", map[string]any{"name": agent.ToolRunFlow, "arguments": map[string]any{"flow": "fl-x"}})
	if text, isErr := toolText(t, m); isErr || text == "" {
		t.Fatalf("run_flow call: %v", m)
	}
}

// cerberus mcp takes the session token from CERBERUS_AGENT_TOKEN first
// (Codex keeps OPENAI_API_KEY for its own provider).
func TestAgentTokenFromEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai")
	t.Setenv("CERBERUS_AGENT_TOKEN", "")
	os.Unsetenv("CERBERUS_AGENT_TOKEN")
	if got := agentTokenFromEnv(); got != "openai" {
		t.Fatalf("fallback %q", got)
	}
	t.Setenv("CERBERUS_AGENT_TOKEN", "cerb")
	if got := agentTokenFromEnv(); got != "cerb" {
		t.Fatalf("got %q", got)
	}
}
