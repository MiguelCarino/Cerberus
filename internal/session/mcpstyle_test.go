//go:build !windows

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// fakeAgentBin writes a script named name into dir that records its
// argv (one per line), then "--env--" and the variables it was given.
func fakeAgentBin(t *testing.T, dir, name, out string) {
	t.Helper()
	script := "#!/bin/bash\n{ printf '%s\\n' \"$@\"; echo --env--; " +
		"echo \"TOKEN=$CERBERUS_AGENT_TOKEN\"; echo \"OPENCODE_CONFIG=$OPENCODE_CONFIG\"; } > '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMCPStyleCodexAndOpencode(t *testing.T) {
	// An inherited token (a dev daemon started inside an agent session)
	// never reaches an agent: every session gets its own (flows-agent-7).
	t.Setenv(agentTokenEnv, "stale-inherited-token")
	var codexOut, openOut string
	e := newTestEnv(t, nil, func(e *testEnv) {
		dir := filepath.Join(e.home, "agentbin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		codexOut, openOut = filepath.Join(e.home, "codex.out"), filepath.Join(e.home, "opencode.out")
		fakeAgentBin(t, dir, "codex", codexOut)
		fakeAgentBin(t, dir, "opencode", openOut)
		fakeAgentBin(t, dir, "aider", filepath.Join(e.home, "aider.out"))
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		e.profiles(
			config.AgentProfile{Name: "codex", Command: []string{"codex", "exec", "{task}"}},
			config.AgentProfile{Name: "opencode", Command: []string{"opencode", "run", "{task}"}},
			config.AgentProfile{Name: "aider", Command: []string{"aider", "--message", "{task}"}},
			config.AgentProfile{Name: "codex-off", Command: []string{"codex", "exec", "{task}"}, MCPStyle: config.MCPStyleNone},
		)
		spawnOn(e, 2, 4, 8, false)
	})

	id := e.scratch("codex", func(t *TaskSpec) { t.Task = "fix it" })
	e.waitState(id, StatePassed)
	raw, _ := os.ReadFile(codexOut)
	argv, env, _ := strings.Cut(string(raw), "--env--\n")
	token := e.cfg.SessionAgentToken(id)
	if token == "" {
		t.Fatal("no session token in this env")
	}
	lines := strings.Split(strings.TrimSpace(argv), "\n")
	if len(lines) != 8 || lines[0] != "-c" || !strings.HasPrefix(lines[1], "mcp_servers.cerberus.command=\"") ||
		lines[3] != `mcp_servers.cerberus.args=["mcp"]` ||
		lines[5] != `mcp_servers.cerberus.env_vars=["CERBERUS_AGENT_TOKEN","CERBERUS_URL","CERBERUS_SESSION"]` ||
		lines[6] != "exec" || lines[7] != "fix it" {
		t.Fatalf("codex argv %q", lines)
	}
	if strings.Contains(argv, token) {
		t.Fatal("the session token is in codex's argv")
	}
	if !strings.Contains(env, "TOKEN="+token+"\n") {
		t.Fatalf("codex env %q", env)
	}

	id = e.scratch("opencode", func(t *TaskSpec) { t.Task = "fix it" })
	e.waitState(id, StatePassed)
	raw, _ = os.ReadFile(openOut)
	argv, env, _ = strings.Cut(string(raw), "--env--\n")
	path := filepath.Join(e.m.sessDir, id, opencodeConfigName)
	token = e.cfg.SessionAgentToken(id)
	if strings.TrimSpace(argv) != "run\nfix it" || strings.Contains(argv, token) {
		t.Fatalf("opencode argv %q", argv)
	}
	if !strings.Contains(env, "OPENCODE_CONFIG="+path+"\n") || !strings.Contains(env, "TOKEN="+token+"\n") {
		t.Fatalf("opencode env %q", env)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("the session token is in opencode.json")
	}
	var cfg struct {
		MCP map[string]struct {
			Type        string
			Command     []string
			Enabled     bool
			Environment map[string]string
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg.MCP["cerberus"]
	if srv.Type != "local" || !srv.Enabled || len(srv.Command) != 2 || srv.Command[1] != "mcp" ||
		srv.Environment["CERBERUS_SESSION"] != id || srv.Environment["CERBERUS_AGENT_TOKEN"] != "{env:CERBERUS_AGENT_TOKEN}" {
		t.Fatalf("opencode.json %s", data)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("opencode.json mode %v", st.Mode())
	}

	// aider has no MCP client; mcp_style none turns it off.
	for _, name := range []string{"aider", "codex-off"} {
		id = e.scratch(name, func(t *TaskSpec) { t.Task = "fix it" })
		e.waitState(id, StatePassed)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.home, "aider.out")); strings.Contains(string(raw), "mcp") ||
		strings.Contains(string(raw), "OPENCODE_CONFIG=/") {
		t.Fatalf("aider got MCP: %q", raw)
	}
	if raw, _ := os.ReadFile(codexOut); strings.Contains(string(raw), "mcp_servers") {
		t.Fatalf("codex-off got MCP: %q", raw)
	}
}

func TestMCPStyleDefaults(t *testing.T) {
	for _, c := range []struct {
		p    config.AgentProfile
		want string
	}{
		{config.AgentProfile{Command: []string{"claude", "{task}"}}, config.MCPStyleClaude},
		{config.AgentProfile{Command: []string{"/usr/bin/codex", "exec"}}, config.MCPStyleCodex},
		{config.AgentProfile{Command: []string{"opencode", "run"}}, config.MCPStyleOpencode},
		{config.AgentProfile{Command: []string{"aider"}}, config.MCPStyleNone},
		{config.AgentProfile{Command: []string{"my-wrapper"}, MCPStyle: "codex"}, config.MCPStyleCodex},
		{config.AgentProfile{Command: []string{"claude"}, MCPStyle: "none"}, config.MCPStyleNone},
		{config.AgentProfile{Command: []string{"claude"}, MCPStyle: "bogus"}, config.MCPStyleClaude},
	} {
		if got := mcpStyle(c.p); got != c.want {
			t.Errorf("mcpStyle(%v) = %q, want %q", c.p.Command, got, c.want)
		}
	}
}

// The -c flags go right after the codex program, not after a wrapper in
// front of it, and nowhere when there is no codex program (flows-agent-3).
func TestCodexFlagsAfterTheCodexProgram(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want int
	}{
		{[]string{"codex", "exec", "x"}, 0},
		{[]string{"/usr/local/bin/codex", "exec"}, 0},
		{[]string{`C:\tools\codex.exe`, "exec"}, 0},
		{[]string{"npx", "-y", "@openai/codex", "exec", "{task}"}, 2},
		{[]string{"bash", "-lc", "codex exec x"}, -1},
		{[]string{"my-wrapper", "run"}, -1},
	} {
		if got := codexIndex(c.argv); got != c.want {
			t.Errorf("codexIndex(%q) = %d, want %d", c.argv, got, c.want)
		}
	}
	e := newTestEnv(t, nil, func(e *testEnv) { spawnOn(e, 2, 4, 8, false) })
	p := config.AgentProfile{Name: "npx", MCPStyle: config.MCPStyleCodex, Command: []string{"npx", "-y", "@openai/codex", "exec", "{task}"}}
	w, err := e.m.mcpArgs(p, []string{"npx", "-y", "@openai/codex", "exec", "go"}, t.TempDir(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.argv) != 11 || w.argv[0] != "npx" || w.argv[1] != "-y" || w.argv[2] != "@openai/codex" || w.argv[3] != "-c" ||
		w.argv[9] != "exec" || w.argv[10] != "go" {
		t.Fatalf("argv %q", w.argv)
	}
	p.Command = []string{"bash", "-lc", "codex exec {task}"}
	w, err = e.m.mcpArgs(p, []string{"bash", "-lc", "codex exec go"}, t.TempDir(), "s1")
	if err != nil || len(w.argv) != 3 || len(w.flags) != 0 {
		t.Fatalf("bash wrapper: argv %q flags %q err %v", w.argv, w.flags, err)
	}
}
