package trajectory

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/records"
	"carino.systems/cerberus/internal/session"
)

var t0 = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func mustBuild(t *testing.T, in Input) (Trajectory, []byte) {
	t.Helper()
	tr, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if ps := atifProblems(t, raw); len(ps) != 0 {
		t.Fatalf("not valid ATIF:\n%s\n%s", strings.Join(ps, "\n"), raw)
	}
	return tr, raw
}

func sources(tr Trajectory) string {
	var b []string
	for _, s := range tr.Steps {
		b = append(b, s.Source)
	}
	return strings.Join(b, ",")
}

func cx(s Step) map[string]any { m, _ := s.Extra["cerberus"].(map[string]any); return m }

func TestBuildTextAll(t *testing.T) {
	sys := "You are a coding agent."
	task := "user: Fix the bug"
	lines := []records.Line{
		// Given out of order: Build sorts by time.
		{Time: at(20), API: "chat", Model: "qwen", Session: "s1", Client: "opencode", Messages: 4, Tools: 3, Results: 1,
			PromptTokens: 200, CachedTokens: 100, CompletionTokens: 30, Finish: "tool_calls", Status: 200,
			Calls: []records.Call{{ID: "c2", Name: "bash"}, {ID: "c3", Name: "edit"}},
			Text:  &records.Text{System: sys, Messages: task + "\nassistant: Looking\ntool: package main\nfunc main() {}", Reply: ""}},
		{Time: at(1), API: "chat", Model: "qwen", Session: "s1", Client: "opencode", Messages: 2, Tools: 3,
			PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 5, Finish: "tool_calls", Status: 200, Effort: "low",
			Calls: []records.Call{{ID: "c1", Name: "read"}},
			Text:  &records.Text{System: sys, Messages: task, Reply: "Looking", Reasoning: "read main.go first"}},
		// A side call (another system prompt): its own thread.
		{Time: at(5), API: "chat", Model: "small", Session: "s1", Client: "opencode", Messages: 2, Finish: "stop", Status: 200,
			Text: &records.Text{System: "Write a title.", Messages: task, Reply: "Bug fix"}},
		{Time: at(40), API: "chat", Model: "qwen", Session: "s1", Client: "opencode", Messages: 7, Tools: 3, Results: 3,
			PromptTokens: 300, CompletionTokens: 10, Finish: "stop", Status: 200,
			Guard: &records.Guard{Mode: "watch", Scores: []float64{0.1, 0.9}, Flagged: 1},
			Text: &records.Text{System: sys, Messages: task + "\nassistant: Looking\ntool: package main\nfunc main() {}" +
				"\ntool: ok\ntool: edited\nuser: also add a test", Reply: "Done."}},
	}
	setup := &session.SetupView{Hash: "abcdefabcdef", Recorded: "abcdefabcdef", Verified: true, Setup: session.Setup{
		Agent:     session.SetupAgent{Name: "opencode", Command: []string{"opencode", "run", "{task}"}, Version: "opencode 0.9.1", Sandbox: "bwrap"},
		Model:     session.SetupModel{ID: "qwen", Engine: "llamacpp", Quant: "Q4_K_M"},
		Workspace: "scratch", Cerberus: "1.2.3"}}
	info := session.Info{ID: "s1", Agent: "opencode", Model: "default", Task: "Fix the bug", Workspace: "scratch",
		State: "done", Created: at(0), RunStarted: at(0), RunEnded: at(50), SetupHash: "abcdefabcdef"}
	tr, raw := mustBuild(t, Input{Session: info, Setup: setup, Lines: lines, TextLevel: records.TextAll, Cerberus: "1.2.3", Now: at(60)})

	if tr.SchemaVersion != "ATIF-v1.8" || tr.SessionID != "s1" {
		t.Fatalf("root %+v", tr)
	}
	// system, user, agent (c1) | side: system, user, agent | agent (c2,c3) | user, agent
	if got := sources(tr); got != "system,user,agent,system,user,agent,agent,user,agent" {
		t.Fatalf("sources %s", got)
	}
	s := tr.Steps
	if s[0].Message != sys || s[1].Message != "Fix the bug" || s[2].Message != "Looking" ||
		s[2].ReasoningContent != "read main.go first" || s[2].ReasoningEffort != "low" {
		t.Fatalf("first turn %+v", s[:3])
	}
	if s[2].Timestamp != "2026-11-05T10:00:01.000Z" || s[2].ModelName != "qwen" || *s[2].LLMCallCount != 1 {
		t.Fatalf("agent step %+v", s[2])
	}
	if m := s[2].Metrics; *m.PromptTokens != 100 || *m.CompletionTokens != 20 || m.Extra["reasoning_tokens"] != 5 {
		t.Fatalf("metrics %+v", m)
	}
	// The first call's result, with its multi-line text, lands on the step that made it.
	if o := s[2].Observation; o == nil || len(o.Results) != 1 || o.Results[0].SourceCallID != "c1" ||
		o.Results[0].Content != "package main\nfunc main() {}" {
		t.Fatalf("observation of c1: %+v", s[2].Observation)
	}
	if cx(s[4])["conversation"] != "new" || s[5].Message != "Bug fix" || cx(s[5])["thread"] != 2 {
		t.Fatalf("side thread %+v %+v", s[4], s[5])
	}
	if o := s[6].Observation; o == nil || len(o.Results) != 2 || o.Results[0].SourceCallID != "c2" ||
		o.Results[1].Content != "edited" || o.Results[0].Extra != nil {
		t.Fatalf("observation of c2/c3 (chat API: no error flags): %+v", s[6].Observation)
	}
	if s[7].Message != "also add a test" || s[8].Message != "Done." {
		t.Fatalf("last turn %+v %+v", s[7], s[8])
	}
	ann, _ := s[8].Extra["annotations"].([]any)
	if len(ann) != 1 || ann[0].(map[string]any)["kind"] != "agent_guard" || ann[0].(map[string]any)["flagged"] != 1 {
		t.Fatalf("guard annotation %+v", s[8].Extra)
	}
	fm := tr.FinalMetrics
	if *fm.TotalPromptTokens != 600 || *fm.TotalCompletionTokens != 60 || *fm.TotalCachedTokens != 100 || *fm.TotalSteps != 9 ||
		fm.Extra["model_calls"] != 4 || fm.Extra["guard_flagged"] != 1 {
		t.Fatalf("final %+v", fm)
	}
	if tr.Agent.Name != "opencode" || tr.Agent.Version != "opencode 0.9.1" || tr.Agent.ModelName != "qwen" ||
		tr.Agent.Extra["setup_hash"] != "abcdefabcdef" || tr.Agent.Extra["setup_verified"] != true {
		t.Fatalf("agent %+v", tr.Agent)
	}
	if !strings.Contains(string(raw), `"quantization":"Q4_K_M"`) || !strings.Contains(string(raw), `"spec":"harbor-framework/harbor`) {
		t.Fatalf("setup metadata or spec missing: %s", raw)
	}
}

func TestBuildNoText(t *testing.T) {
	lines := []records.Line{
		{Time: at(1), API: "messages", Model: "m", Session: "s2", Messages: 1, Calls: []records.Call{{Name: "Read"}, {Name: "Grep"}}},
		{Time: at(2), API: "messages", Model: "m", Session: "s2", Messages: 3, Results: 2, ResultErrors: 2,
			Calls: []records.Call{{ID: "toolu_1", Name: "Bash"}}},
		{Time: at(3), API: "messages", Model: "m", Session: "s2", Messages: 5, Results: 3, ResultErrors: 2},
	}
	info := session.Info{ID: "s2", Agent: "claude", Model: "m", Task: "List the files", Created: at(0)}
	tr, _ := mustBuild(t, Input{Session: info, Lines: lines, TextLevel: records.TextNone})
	if got := sources(tr); got != "user,agent,agent,agent" {
		t.Fatalf("sources %s", got)
	}
	if tr.Steps[0].Message != "List the files" || cx(tr.Steps[0])["from"] != "session task" {
		t.Fatalf("task step %+v", tr.Steps[0])
	}
	a := tr.Steps[1]
	if a.ToolCalls[0].ToolCallID != "call-2-1" || a.ToolCalls[1].ToolCallID != "call-2-2" || a.ToolCalls[0].Arguments == nil {
		t.Fatalf("generated ids %+v", a.ToolCalls)
	}
	// Both results errors (Messages API counts them): flagged per result, no text.
	if o := a.Observation; o == nil || len(o.Results) != 2 || o.Results[1].SourceCallID != "call-2-2" ||
		o.Results[0].Extra["is_error"] != true || o.Results[0].Content != "" {
		t.Fatalf("observation %+v", a.Observation)
	}
	if o := tr.Steps[2].Observation; o == nil || len(o.Results) != 1 || o.Results[0].Extra["is_error"] != false {
		t.Fatalf("second observation %+v", tr.Steps[2].Observation)
	}
	if tr.Agent.Name != "claude" || tr.Agent.Version != "unknown" || tr.Agent.ModelName != "m" || tr.Agent.Extra != nil {
		t.Fatalf("agent without setup %+v", tr.Agent)
	}
	if !strings.Contains(tr.Notes, "No conversation text") {
		t.Fatalf("notes %q", tr.Notes)
	}
}

func TestBuildUnmatchedResultsAndClipped(t *testing.T) {
	lines := []records.Line{
		{Time: at(1), API: "chat", Model: "m", Session: "s3", Messages: 1, Calls: []records.Call{{ID: "a", Name: "x"}},
			Text: &records.Text{Messages: "user: go", Reply: ""}},
		// Two results for one call: not one-to-one, so a system step holds them.
		{Time: at(2), API: "chat", Model: "m", Session: "s3", Messages: 4, Results: 2,
			Text: &records.Text{Messages: "user: go\ntool: one\ntool: two", Reply: "ok"}},
		// A clipped conversation: no user step is made from its cut tail.
		{Time: at(3), API: "chat", Model: "m", Session: "s3", Messages: 6, Results: 2,
			Text: &records.Text{Messages: "user: go\ntool: one\ntool: two\nassistant: ok\nuser: more " + clippedMark, Reply: "fine"}},
	}
	tr, _ := mustBuild(t, Input{Session: session.Info{ID: "s3", Created: at(0)}, Lines: lines, TextLevel: records.TextAll,
		Partial: "The start is gone.", More: true})
	if got := sources(tr); got != "user,agent,system,agent,agent" {
		t.Fatalf("sources %s", got)
	}
	o := tr.Steps[2].Observation
	if o == nil || len(o.Results) != 2 || o.Results[0].SourceCallID != "" || o.Results[1].Content != "two" {
		t.Fatalf("system observation %+v", o)
	}
	if cx(tr.Steps[4])["input_clipped"] != true {
		t.Fatalf("clipped flag %+v", tr.Steps[4].Extra)
	}
	if !strings.Contains(tr.Notes, "The start is gone.") || !strings.Contains(tr.Notes, "record cap") {
		t.Fatalf("notes %q", tr.Notes)
	}
	rec := tr.Extra["cerberus"].(map[string]any)["records"].(map[string]any)
	if rec["more"] != true || rec["partial"] != "The start is gone." {
		t.Fatalf("records extra %+v", rec)
	}
}

func TestBuildNoLines(t *testing.T) {
	if _, err := Build(Input{Session: session.Info{ID: "s"}}); !errors.Is(err, ErrNoLines) {
		t.Fatal(err)
	}
}

func TestSegments(t *testing.T) {
	got := segments("user: a\nb\nassistant: c\ntool:\nuser: d")
	want := []seg{{"user", "a\nb"}, {"assistant", "c"}, {"tool", ""}, {"user", "d"}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%d: %+v want %+v", i, got[i], want[i])
		}
	}
	if s := segments("free text\nuser: x"); len(s) != 2 || s[0].role != "user" || s[0].text != "free text" {
		t.Fatalf("leading text %+v", s)
	}
}
