package session

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func sampleSetup() Setup {
	return Setup{
		Agent: SetupAgent{Name: "cerberus", Binary: "/usr/bin/cerberus", Command: []string{"{self}", "agent", "{task_file}"},
			EnvKeys: []string{"A"}, Version: "cerberus 1.0", Sandbox: "bwrap"},
		Model:      SetupModel{Requested: "default", ID: "qwen/qwen3-8b", Engine: "llamacpp", Path: "/m/qwen3-8b-Q4_K_M.gguf", Quant: "Q4_K_M", Ctx: 32768},
		MCPServers: []SetupMCP{{Name: "cerberus", Command: []string{"/usr/bin/cerberus", "mcp"}}},
		Workspace:  WorkspaceRepo, BaseCommit: "abc", Cerberus: "1.0 (dev)",
	}
}

func TestSetupHashStableAndSensitive(t *testing.T) {
	a := sampleSetup()
	h := a.Hash()
	if len(h) != setupHashLen {
		t.Fatalf("hash %q", h)
	}
	// The same setup read back from its indented file hashes the same.
	raw, _ := json.MarshalIndent(a, "", "  ")
	var back Setup
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Hash() != h {
		t.Fatalf("round trip changed the hash: %s vs %s", back.Hash(), h)
	}
	b := sampleSetup()
	b.Agent.Version = "cerberus 1.1"
	if b.Hash() == h {
		t.Fatal("a different version should change the hash")
	}
}

func TestDiffSetups(t *testing.T) {
	a, b := sampleSetup(), sampleSetup()
	if d := DiffSetups(a, b); len(d) != 0 {
		t.Fatalf("same setups differ: %+v", d)
	}
	b.Model.ID, b.Model.Quant, b.BaseCommit = "qwen/qwen3-27b", "", "def"
	d := DiffSetups(a, b)
	got := []string{}
	for _, x := range d {
		got = append(got, x.Field)
	}
	if strings.Join(got, ",") != "model.id,model.quantization,base_commit" {
		t.Fatalf("diff fields %v", got)
	}
	if d[0].A != "qwen/qwen3-8b" || d[0].B != "qwen/qwen3-27b" || d[1].B != "" {
		t.Fatalf("diff %+v", d)
	}
	if n := len(a.Fields()); n != 18 {
		t.Fatalf("%d fields (web/dist/assets/js/setup.js mirrors 18)", n)
	}
}

func TestQuantOf(t *testing.T) {
	for in, want := range map[string]string{
		"/models/Qwen3-8B-Q4_K_M.gguf":      "Q4_K_M",
		"/m/model.IQ3_XXS.gguf":             "IQ3_XXS",
		"qwen3:8b-q8_0":                     "Q8_0",
		"/m/gemma-3-27b-it-BF16.gguf":       "BF16",
		"qwen/qwen3-8b":                     "",
		"/m/llama-3.1-8b-instruct-f16.gguf": "F16",
	} {
		if got := quantOf("", in); got != want {
			t.Errorf("quantOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClipVersion(t *testing.T) {
	if got := clipVersion("\n  claude 2.1.0 (Claude Code)  \nmore\n"); got != "claude 2.1.0 (Claude Code)" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", 300)
	if got := clipVersion(long); len([]rune(got)) != versionMax {
		t.Fatalf("clipped to %d runes", len([]rune(got)))
	}
	if clipVersion("") != "" || clipVersion("\n\n") != "" {
		t.Fatal("empty output should be empty")
	}
}

func TestArgMasker(t *testing.T) {
	m := newArgMasker([]string{"ghp_vaultvalue123"})
	got := m.args([]string{"agent", "--api-key", "sk-live-1", "--token=abc", "API_KEY=xyz", "--model", "{model}",
		"--header", "x ghp_vaultvalue123", "--key", "{api_key}", "--path=/a/b", "--auth", "--verbose"})
	want := []string{"agent", "--api-key", redactMask, "--token=" + redactMask, "API_KEY=" + redactMask, "--model", "{model}",
		"--header", "x " + redactMask, "--key", "{api_key}", "--path=/a/b", "--auth", "--verbose"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("masked\n got %q\nwant %q", got, want)
	}
}

func TestAgentVersionCachedPerMtime(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := &Manager{ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		probeVersion: func(_ context.Context, b string) string { calls++; return "agent 1.0\nbuilt today" }}
	for range 3 {
		if v := m.agentVersion(bin); v != "agent 1.0" {
			t.Fatalf("version %q", v)
		}
	}
	if calls != 1 {
		t.Fatalf("probed %d times, want 1", calls)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	m.agentVersion(bin)
	if calls != 2 {
		t.Fatalf("a replaced binary should be probed again (calls %d)", calls)
	}
	if m.agentVersion(filepath.Join(dir, "missing")) != "" || calls != 2 {
		t.Fatal("a missing binary is not probed")
	}
}

func TestExecVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script agent")
	}
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("#!/bin/sh\n[ \"$1\" = --version ] && echo 'fake-agent 3.4.5' && exit 0\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := clipVersion(execVersion(context.Background(), ok)); got != "fake-agent 3.4.5" {
		t.Fatalf("got %q", got)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho oops\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := execVersion(context.Background(), bad); got != "" {
		t.Fatalf("a failing --version should be recorded as empty, got %q", got)
	}
}

// Secrets the vault does not hold are still hidden when their shape or
// place gives them away: secret-named URL query parameters and URL
// passwords (an MCP server url), header arguments and token shapes.
func TestArgMaskerURLsHeadersAndTokens(t *testing.T) {
	m := newArgMasker(nil)
	got := m.args([]string{
		"https://mcp.example.com/sse?api_key=sk-live-123&mode=fast",
		"https://mcp.example.com/sse?mode=fast#access_token=abc",
		"https://user:hunter2@mcp.example.com/sse",
		"curl", "-H", "Authorization: Bearer xyz", "-H", "X-Api-Key: abc123", "-H", "Accept: text/plain",
		"--note", "uses sk-abcdefghijklmnopqrstuv",
		"https://example.com/docs?page=2",
	})
	want := []string{
		"https://mcp.example.com/sse?api_key=" + redactMask + "&mode=fast",
		"https://mcp.example.com/sse?mode=fast#access_token=" + redactMask,
		"https://user:" + redactMask + "@mcp.example.com/sse",
		"curl", "-H", "Authorization: " + redactMask, "-H", "X-Api-Key: " + redactMask, "-H", "Accept: text/plain",
		"--note", "uses " + redactMask,
		"https://example.com/docs?page=2",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("masked\n got %q\nwant %q", got, want)
	}
}
