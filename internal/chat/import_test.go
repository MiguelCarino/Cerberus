package chat

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/agent"
)

func TestAtriumImport(t *testing.T) {
	e := newEnv(t, "http://127.0.0.1:1/v1", nil)
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "hypr", "llm-chat", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "chat-20261020-101500-abc123"
	src := `{"id":"` + id + `","title":"notes question","cwd":"` + e.home + `","model":"qwen3","created":1760000000.5,` +
		`"updated":1760000100.25,"messages":[{"role":"user","content":[{"type":"text","text":"[image]"},` +
		`{"type":"text","text":"what is this"}]},{"role":"assistant","content":"","tool_calls":[{"id":"t1",` +
		`"type":"function","function":{"name":"read_file","arguments":"{}"}}]},{"role":"assistant","content":"a cat"}]}`
	p := filepath.Join(dir, id+".json")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "not-a-session.json"), []byte(`{}`), 0o600)
	os.WriteFile(filepath.Join(dir, "chat-20261020-101501-ffffff.json"), []byte(`{"title":"broken"}`), 0o600)

	list := e.m.AtriumSessions()
	if len(list) != 1 || list[0].ID != id || list[0].Title != "notes question" || list[0].Messages != 3 || list[0].Imported != "" {
		t.Fatalf("list %+v", list)
	}
	open, err := e.m.AtriumGet(id)
	if err != nil || len(open.Messages) != 3 || open.Messages[0].Content != "[image]\nwhat is this" {
		t.Fatalf("open %+v %v", open, err)
	}
	if _, err := e.m.AtriumGet("../../etc/passwd"); err != ErrNotFound {
		t.Errorf("bad id: %v", err)
	}
	c, err := e.m.ContinueAtrium(id)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == id || c.Source != "atrium:"+id || c.Title != "notes question" || c.Cwd != e.home || c.Model != "qwen3" || c.Created != 1760000000.5 {
		t.Errorf("copy %+v", c)
	}
	// The dangling tool call is closed so the next request is valid.
	if len(c.Messages) != 4 || c.Messages[2].Role != "tool" || c.Messages[2].ToolCallID != "t1" {
		t.Errorf("messages %+v", c.Messages)
	}
	if again, _ := e.m.ContinueAtrium(id); again.ID != c.ID {
		t.Errorf("continued twice: %s vs %s", again.ID, c.ID)
	}
	if l := e.m.AtriumSessions(); l[0].Imported != c.ID {
		t.Errorf("imported mark %+v", l)
	}
	if raw, _ := os.ReadFile(p); !bytes.Equal(raw, []byte(src)) {
		t.Error("Atrium's file was changed")
	}
	if v, err := e.m.Get(c.ID); err != nil || len(v.Chat.Messages) != 4 {
		t.Errorf("copy on disk %+v %v", v, err)
	}
	e.cfg.Chat.AtriumImport = false
	if l := e.m.AtriumSessions(); len(l) != 0 {
		t.Errorf("listed with import off: %+v", l)
	}
}

func TestRules(t *testing.T) {
	cwd := t.TempDir()
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	cases := []struct {
		rule Rule
		tool string
		data map[string]any
		want bool
	}{
		{newRule("Bash", "npm test"), "Bash", bash("npm test"), true},
		{newRule("Bash", "npm test"), "Bash", bash("npm test --watch"), false},
		{newRule("Bash", "npm run:*"), "Bash", bash("npm run build"), true},
		{newRule("Bash", "npm run:*"), "Bash", bash("npm run build && rm -rf x"), false},
		{newRule("Bash", "npm run:*"), "Bash", bash("npm runner"), false},
		{newRule("Bash", "npm run:*"), "Bash", bash("echo $(npm run x)"), false},
		{newRule("Bash", ""), "Bash", bash("anything"), true},
		{newRule("Bash", ""), "Read", map[string]any{"file_path": "/x"}, false},
		{newRule("Read", "dir:/srv/notes"), "Read", map[string]any{"file_path": "/srv/notes/a/b.md"}, true},
		{newRule("Read", "dir:/srv/notes"), "Read", map[string]any{"file_path": "/srv/notes-other/b.md"}, false},
		{newRule("Read", "dir:/srv/notes"), "Read", map[string]any{"file_path": "/srv/notes/../etc"}, false},
		{newRule("Edit", "/src/**"), "Edit", map[string]any{"file_path": filepath.Join(cwd, "src", "a.go")}, true},
		{newRule("Edit", "//etc/**"), "Edit", map[string]any{"file_path": "/etc/hosts"}, true},
		{newRule("Edit", "*.go"), "Edit", map[string]any{"file_path": "a.go"}, true},
		{newRule("WebFetch", "domain:example.com"), "WebFetch", map[string]any{"url": "https://docs.example.com/x"}, true},
		{newRule("WebFetch", "domain:example.com"), "WebFetch", map[string]any{"url": "https://badexample.com/"}, false},
	}
	for i, c := range cases {
		if got := ruleMatches(c.rule, c.tool, c.data, cwd); got != c.want {
			t.Errorf("case %d: %+v on %s %v = %v", i, c.rule, c.tool, c.data, got)
		}
	}
	// The rule "this session" adds.
	r, wire, _ := sessionRule(agent.AskRequest{Tool: "Bash", Input: bash("go test ./...")}, cwd)
	if r.Content != "go test ./..." || r.Text != "Bash(go test ./...)" || wire[0]["destination"] != "session" {
		t.Errorf("bash rule %+v %v", r, wire)
	}
	r, _, _ = sessionRule(agent.AskRequest{Tool: "Read", Input: map[string]any{"file_path": "/srv/notes/a.md"}}, cwd)
	if r.Content != "dir:/srv/notes" {
		t.Errorf("read rule %+v", r)
	}
	r, _, _ = sessionRule(agent.AskRequest{Tool: "LS", Input: map[string]any{"path": cwd}}, cwd)
	if r.Content != "dir:"+cwd {
		t.Errorf("ls rule %+v", r)
	}
	r, wire, _ = sessionRule(agent.AskRequest{Tool: "Bash", Input: bash("npm run x"), Suggestions: []map[string]any{
		{"type": "addRules", "behavior": "allow", "destination": "localSettings",
			"rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "npm run:*"}}}}}, cwd)
	if r.Content != "npm run:*" || wire[0]["destination"] != "session" {
		t.Errorf("suggested rule %+v %v", r, wire)
	}
	r, _, _ = sessionRule(agent.AskRequest{Tool: "Read", Input: map[string]any{"file_path": "/a/b"}, Suggestions: []map[string]any{
		{"type": "addDirectories", "directories": []any{"/a"}}}}, cwd)
	if r.Content != "dir:/a" {
		t.Errorf("directory suggestion %+v", r)
	}
	// A relative path is the chat folder's: "this file's folder", never "any".
	r, wire, ok := sessionRule(agent.AskRequest{Tool: "Write", Native: true, Input: map[string]any{"path": "notes.txt"}}, cwd)
	if !ok || r.Content != "dir:"+cwd || !strings.Contains(fmt.Sprint(wire), filepath.ToSlash(cwd)+"/**") {
		t.Errorf("relative write rule %+v %v %v", r, wire, ok)
	}
	if r, _, ok := sessionRule(agent.AskRequest{Tool: "Read", Native: true, Input: map[string]any{"path": "../../etc/x"}}, cwd); !ok ||
		r.Content != "dir:"+filepath.Clean(filepath.Join(cwd, "../../etc")) {
		t.Errorf("relative read outside %+v %v", r, ok)
	}
	if _, _, ok := sessionRule(agent.AskRequest{Tool: "Write", Input: map[string]any{"path": "x"}}, ""); ok {
		t.Error("a relative path with no folder must not make a rule")
	}
	if _, _, ok := sessionRule(agent.AskRequest{Tool: "Edit", Input: map[string]any{}}, cwd); ok {
		t.Error("an Edit with no path must not make an any-rule")
	}
	r, _, _ = sessionRule(agent.AskRequest{Tool: "cerberus_spawn", Input: map[string]any{"task": "x"}}, cwd)
	if r.Content != "" || r.Text != "cerberus_spawn (any)" {
		t.Errorf("tool rule %+v", r)
	}
}

func TestPrimitives(t *testing.T) {
	if !IsID(newID(e0())) || IsID("chat-1-2-3") || IsID("../chat-20260101-000000-abcdef") {
		t.Error("ids")
	}
	if BackendOf("Claude:opus") != BackendClaude || BackendOf("claude") != BackendClaude || BackendOf("claude-3-haiku") != BackendNative ||
		BackendOf("anthropic/claude-sonnet") != BackendNative {
		t.Error("backends")
	}
	if claudeModel("claude") != "" || claudeModel("claude:default") != "" || claudeModel("claude:sonnet") != "sonnet" {
		t.Error("claude models")
	}
	if cliMode("ask") != "default" || cliMode("acceptEdits") != "acceptEdits" || cliMode("plan") != "plan" {
		t.Error("cli modes")
	}
	media, data, ok := splitDataURL("data:image/png;base64,AAAA")
	if !ok || media != "image/png" || data != "AAAA" {
		t.Error("data url")
	}
	if _, _, ok := splitDataURL("data:image/png,AAAA"); ok {
		t.Error("non-base64 data url accepted")
	}
	if s := snippet("alpha beta gamma delta", "gamma"); s != "alpha beta gamma delta" {
		t.Errorf("snippet %q", s)
	}
}

func e0() time.Time { return time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC) }
