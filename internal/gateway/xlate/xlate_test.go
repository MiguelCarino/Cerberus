package xlate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decode(t *testing.T, r json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(r, &v); err != nil {
		t.Fatalf("%s: %v", r, err)
	}
	return v
}

func messagesOf(t *testing.T, req *Request) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(req.Fields["messages"], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseMessagesFull(t *testing.T) {
	body := `{
	  "model": "claude-sonnet-4", "max_tokens": 1024, "stream": true,
	  "system": [{"type":"text","text":"Be brief."},{"type":"text","text":"Use tools.","cache_control":{"type":"ephemeral"}}],
	  "messages": [
	    {"role":"user","content":"read a.txt"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"","signature":"` + Sign("I should read it") + `"},
	      {"type":"text","text":"Reading."},
	      {"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.txt"}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"hello"}],"is_error":true},
	      {"type":"text","text":"and now?"},
	      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
	    {"role":"system","content":"a reminder"}
	  ],
	  "tools": [{"name":"read","description":"Read a file","input_schema":{"type":"object"}},
	            {"type":"web_search_20250305","name":"web_search"}],
	  "tool_choice": {"type":"any","disable_parallel_tool_use":true},
	  "stop_sequences": ["END"], "temperature": 0.2, "top_k": 40,
	  "thinking": {"type":"enabled","budget_tokens":2000},
	  "output_config": {"effort":"low"},
	  "metadata": {"user_id":"x"}
	}`
	req, err := ParseMessages([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "claude-sonnet-4" || !req.Stream || req.Effort != "low" || req.Thinking == nil || !*req.Thinking {
		t.Errorf("request info %+v", req)
	}
	if req.ToolResults != 1 || req.ToolErrors != 1 {
		t.Errorf("tool results %d errors %d", req.ToolResults, req.ToolErrors)
	}
	msgs := messagesOf(t, req)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if want := []string{"system", "user", "assistant", "tool", "user", "user"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles %v, want %v", roles, want)
	}
	if msgs[0]["content"] != "Be brief.\n\nUse tools." {
		t.Errorf("system %q", msgs[0]["content"])
	}
	a := msgs[2]
	if a["content"] != "Reading." || a["reasoning_content"] != "I should read it" {
		t.Errorf("assistant %v (the signature must restore the thinking)", a)
	}
	tc := a["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["function"].(map[string]any)["arguments"] != `{"path":"a.txt"}` {
		t.Errorf("tool call %v", tc)
	}
	if msgs[3]["tool_call_id"] != "toolu_1" || msgs[3]["content"] != "hello" {
		t.Errorf("tool result %v", msgs[3])
	}
	parts := msgs[4]["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("user parts %v", parts)
	}
	if msgs[5]["content"] != "a reminder" {
		t.Errorf("system turn in messages → user text: %v", msgs[5])
	}
	tools := decode(t, req.Fields["tools"]).([]any)
	if len(tools) != 1 {
		t.Errorf("server tool not dropped: %v", tools)
	}
	if decode(t, req.Fields["tool_choice"]) != "required" || decode(t, req.Fields["parallel_tool_calls"]) != false {
		t.Errorf("tool_choice %s parallel %s", req.Fields["tool_choice"], req.Fields["parallel_tool_calls"])
	}
	if decode(t, req.Fields["max_tokens"]) != float64(1024) || decode(t, req.Fields["top_k"]) != float64(40) {
		t.Error("budget or sampling lost")
	}
	notes := strings.Join(req.Notes, "|")
	for _, want := range []string{"metadata", "cache_control", "web_search"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q lack %s", notes, want)
		}
	}
}

func TestParseMessagesRefusesDocuments(t *testing.T) {
	_, err := ParseMessages([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`))
	var xe *Error
	if err == nil || !errorAs(err, &xe) || xe.Status != 400 || !strings.Contains(err.Error(), "PDF") {
		t.Errorf("err %v", err)
	}
}

func errorAs(err error, xe **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*xe = e
	}
	return ok
}

func TestForeignSignatureIgnored(t *testing.T) {
	req, err := ParseMessages([]byte(`{"model":"m","messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":"","signature":"EqQBCkYIBxgCKkB..."},{"type":"text","text":"ok"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if rc, ok := messagesOf(t, req)[0]["reasoning_content"]; ok && rc != "" {
		t.Errorf("a foreign signature was decoded: %q", rc)
	}
}

func TestMessagesReply(t *testing.T) {
	body := `{"id":"x","model":"qwen","choices":[{"finish_reason":"tool_calls","message":{
		"content":"Let me check.","reasoning_content":"think",
		"tool_calls":[{"id":"call_9","type":"function","function":{"name":"read","arguments":"{\"path\":\"b\"}"}}]}}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":60}}}`
	out, err := (&Messages{Model: "claude-x"}).Reply([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["type"] != "message" || m["stop_reason"] != "tool_use" || m["model"] != "qwen" {
		t.Errorf("reply %s", out)
	}
	c := m["content"].([]any)
	if len(c) != 3 || c[0].(map[string]any)["type"] != "thinking" || c[2].(map[string]any)["input"].(map[string]any)["path"] != "b" {
		t.Errorf("content %v", c)
	}
	sig := c[0].(map[string]any)["signature"].(string)
	if txt, ok := Unsign(sig); !ok || txt != "think" {
		t.Errorf("signature %q does not carry the thinking", sig)
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != float64(40) || u["cache_read_input_tokens"] != float64(60) || u["output_tokens"] != float64(20) {
		t.Errorf("usage %v", u)
	}
}

// events splits SSE output into (name, data) pairs.
func events(t *testing.T, b []byte) [][2]string {
	t.Helper()
	var out [][2]string
	for _, block := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		var name, data string
		for _, l := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(l, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(l, "data: "); ok {
				data = v
			}
		}
		if !json.Valid([]byte(data)) {
			t.Fatalf("event %s: invalid JSON %q", name, data)
		}
		out = append(out, [2]string{name, data})
	}
	return out
}

func names(evs [][2]string) string {
	var n []string
	for _, e := range evs {
		n = append(n, e[0])
	}
	return strings.Join(n, ",")
}

var streamChunks = []string{
	`{"choices":[{"delta":{"role":"assistant"}}]}`,
	`{"choices":[{"delta":{"reasoning_content":"plan"}}]}`,
	`{"choices":[{"delta":{"content":"Hi"}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":""}}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":1}"}}]}}]}`,
	`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
}

func TestMessagesStream(t *testing.T) {
	s := (&Messages{Model: "claude-x"}).NewStream()
	var out []byte
	out = append(out, s.Start()...)
	for _, c := range streamChunks {
		out = append(out, s.Chunk([]byte(c))...)
	}
	out = append(out, s.End()...)
	out = append(out, s.End()...) // once only
	evs := events(t, out)
	want := "message_start,ping," +
		"content_block_start,content_block_delta,content_block_delta,content_block_stop," + // thinking + signature
		"content_block_start,content_block_delta,content_block_stop," + // text
		"content_block_start,content_block_delta,content_block_delta,content_block_stop," + // tool_use
		"message_delta,message_stop"
	if got := names(evs); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	var json1 strings.Builder
	for _, e := range evs {
		if strings.Contains(e[1], "input_json_delta") {
			var d struct {
				Delta struct {
					PartialJSON string `json:"partial_json"`
				}
			}
			_ = json.Unmarshal([]byte(e[1]), &d)
			json1.WriteString(d.Delta.PartialJSON)
		}
	}
	if json1.String() != `{"path":1}` {
		t.Errorf("tool input %q", json1.String())
	}
	last := evs[len(evs)-2][1]
	if !strings.Contains(last, `"stop_reason":"tool_use"`) || !strings.Contains(last, `"output_tokens":4`) {
		t.Errorf("message_delta %s", last)
	}
}

func TestMessagesStreamError(t *testing.T) {
	s := (&Messages{}).NewStream()
	out := s.Chunk([]byte(`{"error":{"message":"engine fell over"}}`))
	if !strings.Contains(string(out), "engine fell over") || !strings.Contains(string(out), "event: error") {
		t.Errorf("error event %s", out)
	}
	if len(s.End()) != 0 {
		t.Error("End after Fail wrote more")
	}
}

func TestMessagesErrorShape(t *testing.T) {
	var e map[string]any
	_ = json.Unmarshal((&Messages{}).Error(404, "no such model"), &e)
	if e["type"] != "error" || e["error"].(map[string]any)["type"] != "not_found_error" {
		t.Errorf("error %v", e)
	}
}

func TestParseResponses(t *testing.T) {
	body := `{"model":"gpt-5","instructions":"You are Codex.","stream":true,
	  "include":["reasoning.encrypted_content"], "store": false,
	  "reasoning":{"effort":"medium"},
	  "input":[
	    {"type":"message","role":"developer","content":[{"type":"input_text","text":"env: linux"}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"fix it"}]},
	    {"type":"reasoning","summary":[],"encrypted_content":"` + Sign("look first") + `"},
	    {"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":[\"ls\"]}"},
	    {"type":"function_call_output","call_id":"c1","output":"a.go"},
	    {"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin Patch"},
	    {"type":"custom_tool_call_output","call_id":"c2","output":"ok"}
	  ],
	  "tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},
	           {"type":"custom","name":"apply_patch","description":"Patch files"},
	           {"type":"web_search"}],
	  "text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}},
	  "max_output_tokens": 500}`
	req, err := ParseResponses([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Effort != "medium" || !req.EncryptedReasoning || !req.Custom["apply_patch"] || req.ToolResults != 2 {
		t.Errorf("info %+v", req)
	}
	msgs := messagesOf(t, req)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if want := []string{"system", "system", "user", "assistant", "tool", "assistant", "tool"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles %v want %v", roles, want)
	}
	if msgs[3]["reasoning_content"] != "look first" {
		t.Errorf("reasoning not carried: %v", msgs[3])
	}
	ct := msgs[5]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
	if ct != `{"input":"*** Begin Patch"}` {
		t.Errorf("custom call args %v", ct)
	}
	if len(decode(t, req.Fields["tools"]).([]any)) != 2 || decode(t, req.Fields["max_tokens"]) != float64(500) {
		t.Error("tools or budget")
	}
	if rf := decode(t, req.Fields["response_format"]).(map[string]any); rf["type"] != "json_schema" {
		t.Errorf("response_format %v", rf)
	}
}

func TestResponsesNoStore(t *testing.T) {
	_, err := ParseResponses([]byte(`{"model":"m","previous_response_id":"resp_1","input":"next"}`))
	if err == nil || !strings.Contains(err.Error(), "whole conversation") {
		t.Errorf("err %v", err)
	}
	if _, err := ParseResponses([]byte(`{"model":"m","previous_response_id":"resp_1","input":[
		{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)); err != nil {
		t.Errorf("history sent, still refused: %v", err)
	}
}

func TestResponsesReplyCustomTool(t *testing.T) {
	a := &Responses{Model: "m", Custom: map[string]bool{"apply_patch": true}}
	out, err := a.Reply([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"content":null,
		"tool_calls":[{"id":"c1","function":{"name":"apply_patch","arguments":"{\"input\":\"P\"}"}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
	}
	_ = json.Unmarshal(out, &r)
	if r.Status != "completed" || len(r.Output) != 1 || r.Output[0]["type"] != "custom_tool_call" || r.Output[0]["input"] != "P" {
		t.Errorf("reply %s", out)
	}
}

func TestResponsesStream(t *testing.T) {
	s := (&Responses{Model: "m", Encrypted: true}).NewStream()
	var out []byte
	out = append(out, s.Start()...)
	for _, c := range streamChunks {
		out = append(out, s.Chunk([]byte(c))...)
	}
	out = append(out, s.End()...)
	evs := events(t, out)
	want := "response.created,response.in_progress," +
		"response.output_item.added,response.reasoning_text.delta,response.reasoning_text.done,response.output_item.done," +
		"response.output_item.added,response.content_part.added,response.output_text.delta,response.output_text.done,response.content_part.done,response.output_item.done," +
		"response.output_item.added,response.function_call_arguments.delta,response.function_call_arguments.delta,response.function_call_arguments.done,response.output_item.done," +
		"response.completed"
	if got := names(evs); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	for i, e := range evs {
		var v struct {
			Seq int `json:"sequence_number"`
		}
		_ = json.Unmarshal([]byte(e[1]), &v)
		if v.Seq != i {
			t.Fatalf("event %d has sequence_number %d", i, v.Seq)
		}
	}
	done := evs[len(evs)-1][1]
	for _, want := range []string{`"arguments":"{\"path\":1}"`, `"input_tokens":9`, `"encrypted_content":"cerberus1:`} {
		if !strings.Contains(done, want) {
			t.Errorf("response.completed lacks %s: %s", want, done)
		}
	}
}

func TestResponsesStreamIncomplete(t *testing.T) {
	s := (&Responses{Model: "m"}).NewStream()
	s.Start()
	s.Chunk([]byte(`{"choices":[{"delta":{"content":"x"},"finish_reason":"length"}]}`))
	if out := string(s.End()); !strings.Contains(out, "response.incomplete") || !strings.Contains(out, "max_output_tokens") {
		t.Errorf("end %s", out)
	}
}

func TestSignRoundTrip(t *testing.T) {
	for _, s := range []string{"", "plain", "ünïcode\nlines"} {
		if got, ok := Unsign(Sign(s)); !ok || got != s {
			t.Errorf("%q → %q %v", s, got, ok)
		}
	}
	if _, ok := Unsign("not ours"); ok {
		t.Error("foreign signature accepted")
	}
}
