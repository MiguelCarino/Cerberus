package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func readConversation(t *testing.T, path string) conversation {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c conversation
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("conversation.json: %v\n%s", err, raw)
	}
	return c
}

func roles(msgs []message) string {
	var r []string
	for _, m := range msgs {
		r = append(r, m.Role)
	}
	return strings.Join(r, ",")
}

// requestRoles lists the roles of a recorded request's messages.
func requestRoles(req map[string]any) string {
	var r []string
	for _, m := range req["messages"].([]any) {
		r = append(r, m.(map[string]any)["role"].(string))
	}
	return strings.Join(r, ",")
}

func lastRequestMessage(req map[string]any) map[string]any {
	ms := req["messages"].([]any)
	return ms[len(ms)-1].(map[string]any)
}

func TestConversationWrittenAfterEachStep(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "sessions", "s-1")
	t.Setenv("CERBERUS_SESSION_DIR", sessionDir)
	path := conversationPath()
	if path != filepath.Join(sessionDir, "conversation.json") {
		t.Fatalf("path %q", path)
	}
	var afterStep1 conversation
	f, base := newFake(t,
		sse(toolChunks([2]string{"list_dir", `{"path":"."}`})...),
		func(w http.ResponseWriter, req map[string]any) {
			// The second request starts only after step 1 was saved.
			afterStep1 = readConversation(t, path)
			sse(content("finished"))(w, req)
		},
	)
	o := testOptions(t, base)
	o.SaveTo = path
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if got := roles(afterStep1.Messages); got != "system,user,assistant,tool" {
		t.Errorf("after step 1: roles %s", got)
	}
	c := readConversation(t, path)
	if c.Model != "test-model" || c.Cwd != o.Cwd {
		t.Errorf("model %q cwd %q", c.Model, c.Cwd)
	}
	if got := roles(c.Messages); got != "system,user,assistant,tool,assistant" {
		t.Fatalf("final roles %s", got)
	}
	if c.Messages[1].Content != "do the thing" || c.Messages[4].Content != "finished" ||
		c.Messages[2].ToolCalls[0].Function.Name != "list_dir" || c.Messages[3].ToolCallID != "call_0" {
		t.Errorf("messages %+v", c.Messages)
	}
	// The file matches what the model was last sent, system prompt included.
	sent := f.requests()[1]["messages"].([]any)
	if sys := sent[0].(map[string]any)["content"]; sys != c.Messages[0].Content {
		t.Errorf("system prompt differs from the one sent")
	}
	entries, _ := os.ReadDir(sessionDir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
	if st, _ := os.Stat(path); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("mode %v", st.Mode())
	}
}

func TestNoSessionDirWritesNothing(t *testing.T) {
	t.Setenv("CERBERUS_SESSION_DIR", "")
	if conversationPath() != "" {
		t.Fatal("path without CERBERUS_SESSION_DIR")
	}
}

// writeResume saves a prior conversation as an earlier run would have.
func writeResume(t *testing.T, msgs []message) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "conversation.json")
	if err := saveConversation(p, conversation{Model: "old", Cwd: "/old", Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	return p
}

var priorConversation = []message{
	{Role: "system", Content: "OLD SYSTEM PROMPT"},
	{Role: "user", Content: "add a README"},
	{Role: "assistant", Content: "Added README.md."},
}

func parseResume(t *testing.T, cwd string, extra ...string) (options, string, bool) {
	t.Helper()
	var out bytes.Buffer
	args := append([]string{"--cwd", cwd, "--approve", "auto"}, extra...)
	o, code, ok := parseFlags(args, config.Defaults(), &out)
	if !ok && code != exitSetup {
		t.Fatalf("exit %d, want %d", code, exitSetup)
	}
	return o, out.String(), ok
}

func TestResumeWithNewText(t *testing.T) {
	cwd := t.TempDir()
	taskFile := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(taskFile, []byte("now add a LICENSE\n"), 0o644)
	o, msg, ok := parseResume(t, cwd, "--task-file", taskFile, "--resume", writeResume(t, priorConversation))
	if !ok {
		t.Fatal(msg)
	}
	f, base := newFake(t, sse(content("Added LICENSE.")))
	o.Base = base
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	req := f.requests()[0]
	if got := requestRoles(req); got != "system,user,assistant,user" {
		t.Fatalf("roles %s", got)
	}
	sys := req["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "OLD SYSTEM PROMPT") || !strings.Contains(sys, cwd) {
		t.Errorf("system prompt not rebuilt: %q", sys)
	}
	if last := lastRequestMessage(req); last["content"] != "now add a LICENSE" {
		t.Errorf("last message %v", last)
	}
	if !strings.Contains(out.String(), "resumed (2 messages)") {
		t.Errorf("header:\n%s", out.String())
	}
}

func TestResumeWithoutNewText(t *testing.T) {
	cwd := t.TempDir()
	empty := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(empty, nil, 0o644)

	// The model had answered: it is told to continue.
	o, msg, ok := parseResume(t, cwd, "--task-file", empty, "--resume", writeResume(t, priorConversation))
	if !ok {
		t.Fatal(msg)
	}
	if o.Task != "" {
		t.Errorf("task %q", o.Task)
	}
	f, base := newFake(t, sse(content("ok")))
	o.Base = base
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	req := f.requests()[0]
	if got := requestRoles(req); got != "system,user,assistant,user" {
		t.Fatalf("roles %s", got)
	}
	if last := lastRequestMessage(req); last["content"] != continueText {
		t.Errorf("last message %v", last)
	}

	// Stopped mid-tool: the unanswered call is closed off and the run picks
	// up from there without an extra user turn.
	stopped := []message{
		{Role: "system", Content: "old"},
		{Role: "user", Content: "fix it"},
		{Role: "assistant", ToolCalls: []toolCall{
			{ID: "a", Type: "function", Function: functionCall{Name: "list_dir", Arguments: "{}"}},
			{ID: "b", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"x"}`}},
		}},
		{Role: "tool", ToolCallID: "a", Content: "x"},
	}
	o, msg, ok = parseResume(t, cwd, "--resume", writeResume(t, stopped))
	if !ok {
		t.Fatal(msg)
	}
	want := []message{stopped[1], stopped[2], stopped[3],
		{Role: "tool", ToolCallID: "b", Content: "(not run: the previous session ended before this call ran)"}}
	if got := initialMessages(o); !reflect.DeepEqual(got, want) {
		t.Errorf("messages %+v", got)
	}
}

func TestResumeCorruptFile(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"garbage":     "{not json",
		"no messages": `{"model":"m"}`,
		"bad role":    `{"messages":[{"role":"wizard","content":"x"}]}`,
		"orphan tool": `{"messages":[{"role":"tool","content":"x"}]}`,
		"empty":       `{"messages":[]}`,
	}
	for name, body := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		os.WriteFile(p, []byte(body), 0o600)
		_, msg, ok := parseResume(t, dir, "--resume", p)
		if ok {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(msg, "cerberus agent:") || !strings.Contains(msg, "resume") {
			t.Errorf("%s: unclear error %q", name, msg)
		}
	}
	_, msg, ok := parseResume(t, dir, "--resume", filepath.Join(dir, "missing.json"))
	if ok || !strings.Contains(msg, "resume file") {
		t.Errorf("missing file: ok=%v %q", ok, msg)
	}

	// Through Main the exit code is 1.
	old := loadConfig
	t.Cleanup(func() { loadConfig = old })
	loadConfig = config.Defaults
	p := filepath.Join(dir, "garbage.json")
	var out bytes.Buffer
	if code := Main([]string{"--cwd", dir, "--resume", p, "go"}, strings.NewReader(""), &out); code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, out.String())
	}
}
