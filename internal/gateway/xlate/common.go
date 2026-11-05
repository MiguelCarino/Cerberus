// Package xlate translates the Anthropic Messages API and the OpenAI
// Responses API to and from Chat Completions (gateway phase G1), so a model
// that only speaks Chat Completions (llama.cpp, LM Studio, Ollama, most API
// providers) can serve Claude Code, Codex and the vendor SDKs.
//
// It is pure: request bodies in, request bodies out; reply bodies and
// streamed chunks in, the client API's bodies and SSE events out. The
// gateway does the proxying, usage, energy and budgets as for any request.
package xlate

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Request is a translated request: the Chat Completions fields plus what the
// gateway still has to decide once it knows the endpoint.
type Request struct {
	Fields map[string]json.RawMessage // the Chat Completions body (model is the client's name)
	Model  string                     // the client's model name
	Stream bool

	// Thinking is the client's thinking switch (nil = not said). The gateway
	// sends it as chat_template_kwargs.enable_thinking to local engines only.
	Thinking *bool
	// Effort is reasoning_effort ("" = not said).
	Effort string
	// Notes name fields that were accepted but not used (logged once).
	Notes []string
	// Custom are the Responses API's custom (freeform) tools, which go to
	// the model as functions taking one string, "input".
	Custom map[string]bool
	// EncryptedReasoning: the client asked for reasoning.encrypted_content.
	EncryptedReasoning bool
	// ToolResults count the tool results the request sends back, and how
	// many the client marked as errors (request records, G3).
	ToolResults, ToolErrors int
}

// Error is a request the translator refuses; the gateway answers Status.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func badRequest(format string, a ...any) *Error {
	return &Error{Status: 400, Msg: fmt.Sprintf(format, a...)}
}

// sigPrefix marks a thinking signature (or Responses encrypted_content) that
// Cerberus wrote: the base64 of the reasoning text. A signature from another
// server (a real Anthropic one) does not carry it and is not decoded.
const sigPrefix = "cerberus1:"

// Sign returns the signature that carries text through a client round trip.
func Sign(text string) string {
	return sigPrefix + base64.StdEncoding.EncodeToString([]byte(text))
}

// Unsign returns the text a Cerberus signature carries.
func Unsign(sig string) (string, bool) {
	rest, ok := strings.CutPrefix(sig, sigPrefix)
	if !ok {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // plain values only
	}
	return b
}

// textOf reads a JSON string, or the text of an array of {type, text} parts.
func textOf(r json.RawMessage) string {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return ""
	}
	if r[0] == '"' {
		var s string
		_ = json.Unmarshal(r, &s)
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(r, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// sseEvent formats one named SSE event.
func sseEvent(buf *bytes.Buffer, name string, v any) {
	buf.WriteString("event: ")
	buf.WriteString(name)
	buf.WriteString("\ndata: ")
	b, _ := json.Marshal(v)
	buf.Write(b)
	buf.WriteString("\n\n")
}

// chatMessage is an outgoing Chat Completions message.
type chatMessage struct {
	Role             string     `json:"role"`
	Content          any        `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []chatCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type chatCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function chatFunc `json:"function"`
}

type chatFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string      `json:"type"`
	Function chatToolDef `json:"function"`
}

type chatToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// imagePart is a Chat Completions image content part.
func imagePart(url string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}}
}

func textPart(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// content returns parts as a plain string when they are all text, which every
// engine's chat template takes, else as the parts array.
func content(parts []map[string]any) any {
	var sb strings.Builder
	for _, p := range parts {
		if p["type"] != "text" {
			return parts
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(p["text"].(string))
	}
	return sb.String()
}

// Completion is the part of a non-streamed Chat Completions reply the
// translators read.
type Completion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			ToolCalls        []chatCall      `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// Usage is the Chat Completions usage object.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *Usage) cached() int {
	if u == nil || u.PromptDetails == nil {
		return 0
	}
	return u.PromptDetails.CachedTokens
}

func (u *Usage) reasoning() int {
	if u == nil || u.CompletionDetails == nil {
		return 0
	}
	return u.CompletionDetails.ReasoningTokens
}

// Chunk is one streamed Chat Completions chunk.
type Chunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage          `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// ErrorMessage reads error.message (or a string error) from an OpenAI-style
// error body, else the body itself.
func ErrorMessage(b []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
		var s string
		if json.Unmarshal(e.Error, &s) == nil && s != "" {
			return s
		}
	}
	return strings.TrimSpace(string(b))
}

// Stream turns a Chat Completions SSE stream into the client API's events.
type Stream interface {
	// Start is written before anything else (message_start, response.created).
	Start() []byte
	// Chunk translates one chunk's JSON payload.
	Chunk(payload []byte) []byte
	// End closes open blocks and ends the reply; it is called once.
	End() []byte
	// Fail ends the stream with an error event.
	Fail(msg string) []byte
	// Ping is a keepalive event for a quiet upstream.
	Ping() []byte
}

// API is one translated client API.
type API interface {
	// Reply translates a non-streamed 2xx Chat Completions body.
	Reply(body []byte) ([]byte, error)
	// Error translates an error (an OpenAI-style body, or the gateway's own).
	Error(status int, msg string) []byte
	// NewStream starts translating a streamed reply.
	NewStream() Stream
}
