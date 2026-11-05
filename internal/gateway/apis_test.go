package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
)

func TestMessagesStreamedEndToEnd(t *testing.T) {
	h := newHarness(t, sseHandler(true, nil))
	resp := h.post(t, "/messages", `{"model":"qwen","max_tokens":64,"stream":true,
		"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"event: message_start", `"text":"Hel"`, `"text":"lo"`, `"stop_reason":"end_turn"`,
		`"input_tokens":7`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "chat.completion") || strings.Contains(body, "[DONE]") {
		t.Errorf("chat completions leaked:\n%s", body)
	}
	// Upstream got a chat completion with the thinking switch for a local engine.
	if h.up.path != "/v1/chat/completions" || h.up.body["max_tokens"] != float64(64) {
		t.Errorf("upstream %s %v", h.up.path, h.up.body)
	}
	if kw, _ := h.up.body["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
		t.Errorf("thinking switch not sent: %v", h.up.body)
	}
	rec := h.store.last(t)
	if rec.API != "messages" || rec.PromptTokens != 7 || rec.CompletionTokens != 5 || rec.Finish != "stop" {
		t.Errorf("record %+v", rec)
	}
}

func TestMessagesNonStreamed(t *testing.T) {
	h := newHarness(t, jsonOK(`{"choices":[{"message":{"content":"hi there"},"finish_reason":"length"}],
		"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	resp := h.post(t, "/messages", `{"model":"qwen","max_tokens":2,"messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(body, `"stop_reason":"max_tokens"`) || !strings.Contains(body, `"text":"hi there"`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func TestMessagesErrorsInMessagesShape(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	resp := h.post(t, "/messages", `{"model":"nope","max_tokens":5,"messages":[]}`)
	body := readAll(t, resp.Body)
	if resp.StatusCode != 404 || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "not_found_error") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	resp = h.post(t, "/messages", `{"model":"qwen","messages":[{"role":"user","content":[{"type":"document"}]}]}`)
	if body = readAll(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "invalid_request_error") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	// An upstream error is translated too.
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"context too long","type":"invalid_request_error"}}`)
	})
	resp = h2.post(t, "/messages", `{"model":"qwen","max_tokens":5,"messages":[]}`)
	if body = readAll(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "context too long") || !strings.Contains(body, `"type":"error"`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func TestModelMap(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	h.gw.SetProviders(nil, map[string]string{"claude-*": "qwen", "claude-opus-*": "full"})
	resp := h.post(t, "/messages", `{"model":"claude-sonnet-4-5","max_tokens":5,"messages":[]}`)
	if body := readAll(t, resp.Body); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if h.up.body["model"] != "backend-qwen" {
		t.Errorf("upstream model %v", h.up.body["model"])
	}
	// The longer pattern wins.
	resp = h.post(t, "/messages", `{"model":"claude-opus-4","max_tokens":5,"messages":[]}`)
	if readAll(t, resp.Body); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("claude-opus-* should map to the full model: %d", resp.StatusCode)
	}
}

func TestResponsesStreamedEndToEnd(t *testing.T) {
	h := newHarness(t, sseHandler(true, nil))
	resp := h.post(t, "/responses", `{"model":"qwen","stream":true,"input":"hi","reasoning":{"effort":"low"},"max_output_tokens":99}`)
	body := readAll(t, resp.Body)
	for _, want := range []string{"event: response.created", `"delta":"Hel"`, "event: response.completed", `"text":"Hello"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %s:\n%s", want, body)
		}
	}
	if h.up.body["reasoning_effort"] != "low" || h.up.body["max_tokens"] != float64(99) {
		t.Errorf("upstream %v", h.up.body)
	}
	if rec := h.store.last(t); rec.API != "responses" {
		t.Errorf("record API %q", rec.API)
	}
}

func TestResponsesNoStoreIs400(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	resp := h.post(t, "/responses", `{"model":"qwen","previous_response_id":"resp_x","input":"more"}`)
	if body := readAll(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "whole conversation") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	if h.src.acquired.Load() != 0 {
		t.Error("model loaded for a refused request")
	}
}

func TestNativePassThrough(t *testing.T) {
	const native = `{"id":"msg_1","type":"message","content":[{"type":"text","text":"native"}],
		"usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":4}}`
	h := newHarness(t, jsonOK(native))
	h.src.local = false
	h.src.headers = map[string]string{"Authorization": "Bearer sk-ant-key"}
	h.gw.SetProviders([]config.ProviderConfig{{Name: "llamacpp", APIs: []string{"messages"}}}, nil)
	resp := h.post(t, "/messages", `{"model":"qwen","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"metadata":{"x":1}}`)
	body := readAll(t, resp.Body)
	if strings.TrimSpace(body) != native {
		t.Errorf("body changed: %s", body)
	}
	if h.up.path != "/v1/messages" || h.up.body["model"] != "backend-qwen" || h.up.body["metadata"] == nil {
		t.Errorf("upstream %s %v", h.up.path, h.up.body)
	}
	if h.up.headers.Get("X-Api-Key") != "sk-ant-key" || h.up.headers.Get("Authorization") != "" ||
		h.up.headers.Get("Anthropic-Version") == "" {
		t.Errorf("headers %v", h.up.headers)
	}
	rec := h.store.last(t)
	if rec.PromptTokens != 15 || rec.CompletionTokens != 3 || rec.CachedTokens != 4 {
		t.Errorf("native usage %+v", rec)
	}
}

func TestNativeStreamKeepsUsageEvents(t *testing.T) {
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	})
	h.src.local = false
	h.gw.SetProviders([]config.ProviderConfig{{Name: "llamacpp", APIs: []string{"messages"}}}, nil)
	resp := h.post(t, "/messages", `{"model":"qwen","max_tokens":5,"stream":true,"messages":[]}`)
	if body := readAll(t, resp.Body); body != stream {
		t.Errorf("native stream changed:\n%s", body)
	}
	if rec := h.store.last(t); rec.PromptTokens != 20 || rec.CompletionTokens != 6 || rec.TTFTMs < 0 {
		t.Errorf("record %+v", rec)
	}
}

func TestCountTokensEstimate(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	resp := h.post(t, "/messages/count_tokens", `{"model":"qwen","messages":[{"role":"user","content":"hello world"}]}`)
	body := readAll(t, resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(body, `"input_tokens"`) || !strings.Contains(body, `"estimated":true`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func TestCountTokensFromEngine(t *testing.T) {
	var tokenized atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apply-template":
			_, _ = io.WriteString(w, `{"prompt":"<|user|>hello"}`)
		case "/tokenize":
			tokenized.Store(true)
			_, _ = io.WriteString(w, `{"tokens":[1,2,3,4,5]}`)
		default:
			http.NotFound(w, r)
		}
	})
	n, err := llamaCount(t.Context(), http.DefaultClient,
		engine.Endpoint{BaseURL: strings.TrimSuffix(h.src.base, "/") + "/v1"}, map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"hello"}]`)})
	if err != nil || n != 5 || !tokenized.Load() {
		t.Errorf("n=%d err=%v", n, err)
	}
}

func TestTranslatedStreamPings(t *testing.T) {
	old := pingEvery
	pingEvery = 30 * time.Millisecond
	t.Cleanup(func() { pingEvery = old })
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release // a long prompt read: nothing for a while
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	resp := h.post(t, "/messages", `{"model":"qwen","max_tokens":5,"stream":true,"messages":[]}`)
	body := readAll(t, resp.Body)
	if strings.Count(body, "event: ping") < 2 {
		t.Errorf("no keepalive pings during a quiet upstream:\n%s", body)
	}
}
