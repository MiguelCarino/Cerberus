package xlate

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Anthropic Messages API (POST /v1/messages) ⇄ Chat Completions.

type mBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// image
	Source *struct {
		Type      string `json:"type"` // base64 | url
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	// thinking
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type mMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type mTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type mRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int            `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []mMessage      `json:"messages"`
	Tools         []mTool         `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	StopSequences []string        `json:"stop_sequences"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	Stream        bool            `json:"stream"`
	Thinking      *struct {
		Type         string `json:"type"` // enabled | disabled | adaptive
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
}

// ignoredMessagesFields are accepted and not used; each is named once.
var ignoredMessagesFields = []string{"metadata", "context_management", "service_tier", "container",
	"mcp_servers", "betas"}

// ParseMessages translates a Messages request body.
func ParseMessages(body []byte) (*Request, error) {
	var in mRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, badRequest("request body must be a Messages API JSON object: %v", err)
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(body, &top)
	out := &Request{Fields: map[string]json.RawMessage{}, Model: in.Model, Stream: in.Stream}
	for _, k := range ignoredMessagesFields {
		if _, ok := top[k]; ok {
			out.Notes = append(out.Notes, k)
		}
	}
	if bytes.Contains(body, []byte(`"cache_control"`)) {
		out.Notes = append(out.Notes, "cache_control (the engine's prompt cache needs no marks)")
	}

	var msgs []chatMessage
	if sys := textOf(in.System); sys != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: sys})
	}
	for i, m := range in.Messages {
		conv, err := convertMessage(m, out)
		if err != nil {
			return nil, badRequest("messages[%d]: %v", i, err)
		}
		msgs = append(msgs, conv...)
	}
	out.Fields["model"] = raw(in.Model)
	out.Fields["messages"] = raw(msgs)
	if in.MaxTokens != nil {
		out.Fields["max_tokens"] = raw(*in.MaxTokens)
	}
	if in.Stream {
		out.Fields["stream"] = raw(true)
	}
	if len(in.StopSequences) > 0 {
		out.Fields["stop"] = raw(in.StopSequences)
	}
	if in.Temperature != nil {
		out.Fields["temperature"] = raw(*in.Temperature)
	}
	if in.TopP != nil {
		out.Fields["top_p"] = raw(*in.TopP)
	}
	if in.TopK != nil {
		out.Fields["top_k"] = raw(*in.TopK)
	}

	var tools []chatTool
	for _, t := range in.Tools {
		if (t.Type != "" && t.Type != "custom") || len(t.InputSchema) == 0 {
			out.Notes = append(out.Notes, "server tool "+orStr(t.Name, t.Type)+" (not served: the client's own tools are used)")
			continue
		}
		tools = append(tools, chatTool{Type: "function", Function: chatToolDef{Name: t.Name,
			Description: t.Description, Parameters: t.InputSchema}})
	}
	if len(tools) > 0 {
		out.Fields["tools"] = raw(tools)
		if tc, par := messagesToolChoice(in.ToolChoice); tc != nil {
			out.Fields["tool_choice"] = tc
			if par != nil {
				out.Fields["parallel_tool_calls"] = raw(*par)
			}
		}
	}

	if t := in.Thinking; t != nil {
		switch t.Type {
		case "enabled":
			on := true
			out.Thinking = &on
		case "disabled":
			off := false
			out.Thinking = &off
		}
	}
	if oc := in.OutputConfig; oc != nil {
		if oc.Effort != "" {
			out.Effort = oc.Effort
		}
		if f := oc.Format; f != nil && f.Type == "json_schema" && len(f.Schema) > 0 {
			out.Fields["response_format"] = raw(map[string]any{"type": "json_schema",
				"json_schema": map[string]any{"name": "output", "schema": f.Schema, "strict": true}})
		}
	}
	return out, nil
}

// messagesToolChoice maps tool_choice; the second value is
// parallel_tool_calls when disable_parallel_tool_use was sent.
func messagesToolChoice(r json.RawMessage) (json.RawMessage, *bool) {
	if len(r) == 0 || string(r) == "null" {
		return nil, nil
	}
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(r, &tc) != nil {
		return nil, nil
	}
	var par *bool
	if tc.DisableParallelToolUse {
		f := false
		par = &f
	}
	switch tc.Type {
	case "any":
		return raw("required"), par
	case "none":
		return raw("none"), par
	case "tool":
		return raw(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}), par
	default:
		return raw("auto"), par
	}
}

func convertMessage(m mMessage, req *Request) ([]chatMessage, error) {
	var blocks []mBlock
	c := bytes.TrimSpace(m.Content)
	if len(c) > 0 && c[0] == '"' {
		var s string
		_ = json.Unmarshal(c, &s)
		blocks = []mBlock{{Type: "text", Text: s}}
	} else if len(c) > 0 && string(c) != "null" {
		if err := json.Unmarshal(c, &blocks); err != nil {
			return nil, err
		}
	}
	switch m.Role {
	case "assistant":
		return []chatMessage{assistantMessage(blocks)}, nil
	case "user", "system":
		// A system turn inside messages becomes user text (Claude Code sends
		// them; chat templates take one system prompt, at the top).
		var out []chatMessage
		var parts []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, textPart(b.Text))
				}
			case "image":
				u, err := imageURL(b)
				if err != nil {
					return nil, err
				}
				parts = append(parts, imagePart(u))
			case "tool_result":
				req.ToolResults++
				if b.IsError {
					req.ToolErrors++
				}
				text, imgs := toolResultContent(b.Content)
				out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
				for _, u := range imgs {
					parts = append(parts, imagePart(u))
				}
			case "document":
				return nil, badRequestErr("document blocks (PDF) are not supported: send the text or page images")
			case "thinking", "redacted_thinking":
			default:
				req.Notes = append(req.Notes, "content block "+b.Type)
			}
		}
		// Tool results answer the previous assistant turn, so they come first.
		if len(parts) > 0 {
			out = append(out, chatMessage{Role: "user", Content: content(parts)})
		}
		return out, nil
	default:
		return nil, badRequestErr("unknown role " + m.Role)
	}
}

func badRequestErr(msg string) error { return badRequest("%s", msg) }

func assistantMessage(blocks []mBlock) chatMessage {
	out := chatMessage{Role: "assistant"}
	var text, think strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			t := b.Thinking
			if t == "" {
				t, _ = Unsign(b.Signature) // display "omitted": the signature carries it
			}
			think.WriteString(t)
		case "tool_use":
			args := string(bytes.TrimSpace(b.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, chatCall{ID: b.ID, Type: "function",
				Function: chatFunc{Name: b.Name, Arguments: args}})
		}
	}
	out.Content = text.String()
	if len(out.ToolCalls) > 0 && text.Len() == 0 {
		out.Content = nil
	}
	out.ReasoningContent = think.String()
	return out
}

func imageURL(b mBlock) (string, error) {
	s := b.Source
	if s == nil {
		return "", badRequestErr("image block without a source")
	}
	switch s.Type {
	case "base64":
		return "data:" + s.MediaType + ";base64," + s.Data, nil
	case "url":
		return s.URL, nil
	}
	return "", badRequestErr("image source type " + s.Type + " is not supported")
}

// toolResultContent returns a tool result's text and image URLs.
func toolResultContent(r json.RawMessage) (string, []string) {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || r[0] == '"' {
		return textOf(r), nil
	}
	var blocks []mBlock
	if json.Unmarshal(r, &blocks) != nil {
		return string(r), nil
	}
	var sb strings.Builder
	var imgs []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
		case "image":
			if u, err := imageURL(b); err == nil {
				imgs = append(imgs, u)
			}
		}
	}
	return sb.String(), imgs
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Messages is the Messages API for one request.
type Messages struct{ Model string }

// NewMessages returns the translator for req.
func NewMessages(req *Request) *Messages { return &Messages{Model: req.Model} }

func stopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func messagesUsage(u *Usage) map[string]int {
	out := map[string]int{"input_tokens": 0, "output_tokens": 0}
	if u != nil {
		c := u.cached()
		out["input_tokens"] = max(u.PromptTokens-c, 0)
		out["output_tokens"] = u.CompletionTokens
		out["cache_read_input_tokens"] = c
		out["cache_creation_input_tokens"] = 0
	}
	return out
}

func toolInput(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(args)) && strings.HasPrefix(args, "{") {
		return json.RawMessage(args)
	}
	return raw(map[string]string{"arguments": args}) // the model wrote something that is not an object
}

// Reply translates a non-streamed reply.
func (a *Messages) Reply(body []byte) ([]byte, error) {
	var c Completion
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	var blocks []map[string]any
	finish := ""
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		finish = ch.FinishReason
		if t := orStr(ch.Message.ReasoningContent, ch.Message.Reasoning); t != "" {
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": t, "signature": Sign(t)})
		}
		if t := textOf(ch.Message.Content); t != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": t})
		}
		for _, tc := range ch.Message.ToolCalls {
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": orStr(tc.ID, newID("toolu_")),
				"name": tc.Function.Name, "input": toolInput(tc.Function.Arguments)})
		}
	}
	if blocks == nil {
		blocks = []map[string]any{}
	}
	return json.Marshal(map[string]any{
		"id": newID("msg_"), "type": "message", "role": "assistant", "model": orStr(c.Model, a.Model),
		"content": blocks, "stop_reason": stopReason(finish), "stop_sequence": nil, "usage": messagesUsage(c.Usage),
	})
}

// Error translates an error to the Messages error shape.
func (a *Messages) Error(status int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{
		"type": messagesErrorType(status), "message": msg}})
	return b
}

func messagesErrorType(status int) string {
	switch status {
	case 400, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// NewStream starts a streamed reply.
func (a *Messages) NewStream() Stream {
	return &mStream{model: a.Model, id: newID("msg_"), tools: map[int]int{}, cur: -1}
}

type mStream struct {
	model, id string
	next      int    // next block index
	cur       int    // open block index, -1 = none
	curType   string // thinking | text | tool_use
	think     strings.Builder
	tools     map[int]int // chat tool index → block index
	finish    string
	usage     *Usage
	ended     bool
}

func (s *mStream) Start() []byte {
	var b bytes.Buffer
	sseEvent(&b, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}})
	sseEvent(&b, "ping", map[string]string{"type": "ping"})
	return b.Bytes()
}

func (s *mStream) Ping() []byte {
	var b bytes.Buffer
	sseEvent(&b, "ping", map[string]string{"type": "ping"})
	return b.Bytes()
}

func (s *mStream) closeBlock(b *bytes.Buffer) {
	if s.cur < 0 {
		return
	}
	if s.curType == "thinking" {
		sseEvent(b, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.cur,
			"delta": map[string]string{"type": "signature_delta", "signature": Sign(s.think.String())}})
		s.think.Reset()
	}
	sseEvent(b, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.cur})
	s.cur, s.curType = -1, ""
}

func (s *mStream) open(b *bytes.Buffer, typ string, block map[string]any) int {
	s.closeBlock(b)
	s.cur, s.curType = s.next, typ
	s.next++
	sseEvent(b, "content_block_start", map[string]any{"type": "content_block_start", "index": s.cur, "content_block": block})
	return s.cur
}

func (s *mStream) Chunk(payload []byte) []byte {
	var ch Chunk
	if json.Unmarshal(payload, &ch) != nil {
		return nil
	}
	var b bytes.Buffer
	if len(ch.Error) > 0 && string(ch.Error) != "null" {
		return s.Fail(ErrorMessage(payload))
	}
	if ch.Usage != nil {
		s.usage = ch.Usage
	}
	if ch.Model != "" {
		s.model = ch.Model
	}
	for i, c := range ch.Choices {
		if i > 0 {
			break
		}
		d := c.Delta
		if t := d.ReasoningContent + d.Reasoning; t != "" {
			if s.curType != "thinking" {
				s.open(&b, "thinking", map[string]any{"type": "thinking", "thinking": ""})
			}
			s.think.WriteString(t)
			sseEvent(&b, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.cur,
				"delta": map[string]string{"type": "thinking_delta", "thinking": t}})
		}
		if d.Content != "" {
			if s.curType != "text" {
				s.open(&b, "text", map[string]any{"type": "text", "text": ""})
			}
			sseEvent(&b, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.cur,
				"delta": map[string]string{"type": "text_delta", "text": d.Content}})
		}
		for _, tc := range d.ToolCalls {
			idx := len(s.tools)
			if tc.Index != nil {
				idx = *tc.Index
			}
			bi, ok := s.tools[idx]
			if !ok {
				bi = s.open(&b, "tool_use", map[string]any{"type": "tool_use", "id": orStr(tc.ID, newID("toolu_")),
					"name": tc.Function.Name, "input": map[string]any{}})
				s.tools[idx] = bi
			}
			if tc.Function.Arguments != "" {
				if bi != s.cur {
					continue // a late piece of a closed call: blocks cannot reopen
				}
				sseEvent(&b, "content_block_delta", map[string]any{"type": "content_block_delta", "index": bi,
					"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
			}
		}
		if c.FinishReason != "" {
			s.finish = c.FinishReason
		}
	}
	return b.Bytes()
}

func (s *mStream) End() []byte {
	if s.ended {
		return nil
	}
	s.ended = true
	var b bytes.Buffer
	s.closeBlock(&b)
	sseEvent(&b, "message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(s.finish), "stop_sequence": nil}, "usage": messagesUsage(s.usage)})
	sseEvent(&b, "message_stop", map[string]string{"type": "message_stop"})
	return b.Bytes()
}

func (s *mStream) Fail(msg string) []byte {
	if s.ended {
		return nil
	}
	s.ended = true
	var b bytes.Buffer
	sseEvent(&b, "error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": msg}})
	return b.Bytes()
}
