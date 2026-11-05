package gateway

import (
	"net/http"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/records"
)

func withRecords(t *testing.T, h *harness, text string) *records.Store {
	t.Helper()
	s, err := records.Open(records.Config{Enabled: true, Text: text, Dir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h.gw.records = s
	return s
}

func onlyLine(t *testing.T, s *records.Store) records.Line {
	t.Helper()
	ls, err := s.List(records.Filter{})
	if err != nil || len(ls) != 1 {
		t.Fatalf("lines %v %v", ls, err)
	}
	return ls[0]
}

func TestRecordShapeAndCalls(t *testing.T) {
	const stream = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_A","function":{"name":"read","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":4}}}

data: [DONE]

`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	})
	s := withRecords(t, h, records.TextNone)
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true,"temperature":0,
		"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:,"}}]}],
		"tools":[{"type":"function","function":{"name":"read"}}],"response_format":{"type":"json_object"}}`).Body)
	l := onlyLine(t, s)
	if l.API != "chat" || !l.Stream || l.Messages != 2 || l.Tools != 1 || l.Images != 1 || l.Sampled || !l.Schema ||
		l.PromptTokens != 50 || l.CompletionTokens != 9 || l.ReasoningTokens != 4 || l.Finish != "tool_calls" || l.Client != "test" {
		t.Errorf("line %+v", l)
	}
	if len(l.Calls) != 1 || l.Calls[0].ID != "call_A" || l.Calls[0].Name != "read" {
		t.Errorf("calls %+v", l.Calls)
	}
	// The next turn answers call_A: matched; an unknown id is not.
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","messages":[{"role":"tool","tool_call_id":"call_A","content":"ok"},
		{"role":"tool","tool_call_id":"call_Z","content":"?"}]}`).Body)
	ls, _ := s.List(records.Filter{})
	if ls[0].Results != 2 || ls[0].Matched != 1 {
		t.Errorf("results %d matched %d", ls[0].Results, ls[0].Matched)
	}
}

func TestRecordText(t *testing.T) {
	h := newHarness(t, jsonOK(`{"choices":[{"message":{"content":"the answer","reasoning_content":"thinking"},"finish_reason":"stop"}]}`))
	s := withRecords(t, h, records.TextUser)
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","messages":[{"role":"system","content":"secret prompt"},
		{"role":"user","content":"question"},{"role":"tool","tool_call_id":"x","content":"tool output"}]}`).Body)
	ref := strings.Split(onlyLine(t, s).Ref, "/")
	l, err := s.Get(ref[0], ref[1], 0)
	if err != nil {
		t.Fatal(err)
	}
	if l.Text == nil || l.Text.Reply != "the answer" || !strings.Contains(l.Text.Messages, "user: question") {
		t.Fatalf("text %+v", l.Text)
	}
	if l.Text.System != "" || l.Text.Reasoning != "" || strings.Contains(l.Text.Messages, "tool output") {
		t.Errorf("level user kept system, reasoning or tool output: %+v", l.Text)
	}
}

func TestRecordErrorsAndMessagesAPI(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	s := withRecords(t, h, records.TextNone)
	readAll(t, h.post(t, "/chat/completions", `{"model":"nope"}`).Body)
	if l := onlyLine(t, s); l.Finish != "error" || l.Status != 404 {
		t.Errorf("unknown model line %+v", l)
	}
	readAll(t, h.post(t, "/messages", `{"model":"qwen","max_tokens":9,"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"boom","is_error":true}]}]}`).Body)
	ls, _ := s.List(records.Filter{})
	if ls[0].API != "messages" || ls[0].Results != 1 || ls[0].ResultErrors != 1 || ls[0].Budget != 9 {
		t.Errorf("messages line %+v", ls[0])
	}
}
