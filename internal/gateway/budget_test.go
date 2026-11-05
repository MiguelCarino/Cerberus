package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func jsonOK(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

const okReply = `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`

func TestBudgetAliases(t *testing.T) {
	cases := []struct {
		name, fields string
		want         int // 0 = 400
		absent       bool
	}{
		{"max_tokens", `"max_tokens":100`, 100, false},
		{"completion", `"max_completion_tokens":200`, 200, false},
		{"output", `"max_output_tokens":300`, 300, false},
		{"agree", `"max_tokens":50,"max_completion_tokens":50,"max_output_tokens":50`, 50, false},
		{"disagree", `"max_tokens":50,"max_completion_tokens":60`, 0, false},
		{"not an integer", `"max_tokens":"lots"`, 0, false},
		{"none", `"temperature":0`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, jsonOK(okReply))
			resp := h.post(t, "/chat/completions", `{"model":"qwen","messages":[],`+c.fields+`}`)
			body := readAll(t, resp.Body)
			if c.want == 0 && !c.absent {
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status %d (%s), want 400", resp.StatusCode, body)
				}
				if h.src.acquired.Load() != 0 {
					t.Error("a bad budget loaded the model")
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			for _, n := range []string{"max_completion_tokens", "max_output_tokens"} {
				if _, ok := h.up.body[n]; ok {
					t.Errorf("%s reached a local engine", n)
				}
			}
			got, ok := h.up.body["max_tokens"]
			if c.absent {
				if ok {
					t.Errorf("max_tokens = %v with no budget and no default", got)
				}
				return
			}
			if got != float64(c.want) {
				t.Errorf("max_tokens = %v, want %d", got, c.want)
			}
		})
	}
}

func TestBudgetDisagreeNamesBoth(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	resp := h.post(t, "/chat/completions", `{"model":"qwen","max_tokens":5,"max_output_tokens":9}`)
	body := readAll(t, resp.Body)
	if !strings.Contains(body, "max_tokens (5)") || !strings.Contains(body, "max_output_tokens (9)") {
		t.Errorf("error does not name both fields: %s", body)
	}
}

func TestCloudKeepsMaxCompletionTokens(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	h.src.local = false
	resp := h.post(t, "/chat/completions", `{"model":"qwen","max_completion_tokens":70,"max_tokens":70}`)
	readAll(t, resp.Body)
	if h.up.body["max_completion_tokens"] != float64(70) {
		t.Errorf("cloud body %v", h.up.body)
	}
	if _, ok := h.up.body["max_tokens"]; ok {
		t.Error("both names sent to a cloud model")
	}
}

func ptr[T any](v T) *T { return &v }

func TestModelDefaults(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	h.gw.SetModels([]config.ModelConfig{{ID: "qwen", Defaults: config.ModelDefaults{
		ReasoningEffort: "medium", EnableThinking: ptr(false), MaxTokens: 4096, MaxTokensCap: 8192,
		Temperature: ptr(0.6), TopP: ptr(0.95), TopK: 20}}})

	// A harness that sends nothing gets every default.
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","messages":[]}`).Body)
	b := h.up.body
	if b["max_tokens"] != float64(4096) || b["reasoning_effort"] != "medium" || b["temperature"] != 0.6 ||
		b["top_p"] != 0.95 || b["top_k"] != float64(20) {
		t.Errorf("defaults not applied: %v", b)
	}
	if kw, _ := b["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
		t.Errorf("enable_thinking not applied: %v", b["chat_template_kwargs"])
	}

	// What the request names wins; other kwargs are kept.
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","max_tokens":10,"temperature":0,
		"chat_template_kwargs":{"enable_thinking":true,"x":1}}`).Body)
	b = h.up.body
	if b["max_tokens"] != float64(10) || b["temperature"] != float64(0) {
		t.Errorf("request values overridden: %v", b)
	}
	if _, ok := b["reasoning_effort"]; ok {
		t.Error("reasoning_effort added to a request that named its thinking")
	}
	if kw, _ := b["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != true || kw["x"] != float64(1) {
		t.Errorf("kwargs changed: %v", b["chat_template_kwargs"])
	}

	// Over the cap, and -1, are clipped.
	for _, ask := range []string{"100000", "-1"} {
		readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","max_completion_tokens":`+ask+`}`).Body)
		if h.up.body["max_tokens"] != float64(8192) {
			t.Errorf("ask %s: max_tokens = %v, want the cap", ask, h.up.body["max_tokens"])
		}
	}

	// Defaults never touch embeddings.
	readAll(t, h.post(t, "/embeddings", `{"model":"qwen","input":"x"}`).Body)
	if _, ok := h.up.body["max_tokens"]; ok {
		t.Error("budget added to embeddings")
	}
}

func TestEnableThinkingOnlyForLocal(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	h.src.local = false
	h.gw.SetModels([]config.ModelConfig{{ID: "qwen", Defaults: config.ModelDefaults{EnableThinking: ptr(false)}}})
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen"}`).Body)
	if _, ok := h.up.body["chat_template_kwargs"]; ok {
		t.Error("chat_template_kwargs sent to a cloud model")
	}
}

func TestFinishRecorded(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"stop", `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`, "stop"},
		{"length with answer", `{"choices":[{"message":{"content":"half","reasoning_content":"..."},"finish_reason":"length"}]}`, "length"},
		{"length in thinking", `{"choices":[{"message":{"content":"","reasoning_content":"hmm"},"finish_reason":"length"}]}`, "length_in_thinking"},
		{"length with tool call", `{"choices":[{"message":{"content":null,"reasoning_content":"hmm","tool_calls":[{"id":"1"}]},"finish_reason":"length"}]}`, "length"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, jsonOK(c.body))
			readAll(t, h.post(t, "/chat/completions", `{"model":"qwen"}`).Body)
			if got := h.store.last(t).Finish; got != c.want {
				t.Errorf("Finish = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFinishInThinkingStreamed(t *testing.T) {
	const stream = `data: {"choices":[{"delta":{"reasoning_content":"let me"}}]}

data: {"choices":[{"delta":{"reasoning_content":" think"}}]}

data: {"choices":[{"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	})
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`).Body)
	if got := h.store.last(t).Finish; got != "length_in_thinking" {
		t.Errorf("Finish = %q", got)
	}
}

func TestModelsCarryLimits(t *testing.T) {
	h := newHarness(t, jsonOK(okReply))
	h.gw.SetModels([]config.ModelConfig{{ID: "qwen", Defaults: config.ModelDefaults{MaxTokens: 512, MaxTokensCap: 1024}}})
	resp, err := http.Get(h.srv.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []struct {
			ID       string      `json:"id"`
			Cerberus modelLimits `json:"cerberus"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var q modelLimits
	for _, d := range out.Data {
		if d.ID == "qwen" {
			q = d.Cerberus
		}
	}
	if q.Ctx != 8192 || q.MaxTokensDefault != 512 || q.MaxTokensCap != 1024 || len(q.ReasoningEfforts) == 0 || len(q.BudgetNames) != 3 {
		t.Errorf("limits %+v", q)
	}
}
