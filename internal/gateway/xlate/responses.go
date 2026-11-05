package xlate

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// OpenAI Responses API (POST /v1/responses) ⇄ Chat Completions. There is no
// response store: the client sends the whole history each turn.

type rItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// function_call / custom_tool_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
	// function_call_output / custom_tool_call_output
	Output json.RawMessage `json:"output"`
	// reasoning
	Summary          []struct{ Text string } `json:"summary"`
	EncryptedContent string                  `json:"encrypted_content"`
}

type rTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

type rRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	Tools              []rTool         `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	MaxOutputTokens    *int            `json:"max_output_tokens"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	Stream             bool            `json:"stream"`
	PreviousResponseID string          `json:"previous_response_id"`
	Include            []string        `json:"include"`
	Reasoning          *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Text *struct {
		Format *struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict *bool           `json:"strict"`
		} `json:"format"`
	} `json:"text"`
}

var ignoredResponsesFields = []string{"store", "metadata", "truncation", "service_tier", "user", "prompt_cache_key"}

// ParseResponses translates a Responses request body.
func ParseResponses(body []byte) (*Request, error) {
	var in rRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, badRequest("request body must be a Responses API JSON object: %v", err)
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(body, &top)
	out := &Request{Fields: map[string]json.RawMessage{}, Model: in.Model, Stream: in.Stream, Custom: map[string]bool{}}
	for _, k := range ignoredResponsesFields {
		if _, ok := top[k]; ok {
			out.Notes = append(out.Notes, k)
		}
	}
	for _, inc := range in.Include {
		if inc == "reasoning.encrypted_content" {
			out.EncryptedReasoning = true
		}
	}

	var items []rItem
	inp := bytes.TrimSpace(in.Input)
	switch {
	case len(inp) > 0 && inp[0] == '"':
		var s string
		_ = json.Unmarshal(inp, &s)
		items = []rItem{{Type: "message", Role: "user", Content: raw(s)}}
	case len(inp) > 0 && string(inp) != "null":
		if err := json.Unmarshal(inp, &items); err != nil {
			return nil, badRequest("input: %v", err)
		}
	}
	if in.PreviousResponseID != "" && !hasHistory(items) {
		return nil, badRequest("previous_response_id is not supported: this server keeps no responses, so send the whole conversation in input each turn")
	}

	for _, t := range in.Tools {
		if t.Type == "custom" {
			out.Custom[t.Name] = true
		}
	}

	var msgs []chatMessage
	if in.Instructions != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: in.Instructions})
	}
	pendingThink := ""
	// assistant returns the open assistant message (one is opened for
	// calls and text that follow each other).
	var cur *chatMessage
	flush := func() {
		if cur != nil {
			if len(cur.ToolCalls) > 0 && cur.Content == "" {
				cur.Content = nil
			}
			msgs = append(msgs, *cur)
			cur = nil
		}
	}
	assistant := func() *chatMessage {
		if cur == nil {
			cur = &chatMessage{Role: "assistant", Content: "", ReasoningContent: pendingThink}
			pendingThink = ""
		}
		return cur
	}
	for i, it := range items {
		typ := it.Type
		if typ == "" && it.Role != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			if it.Role == "assistant" {
				a := assistant()
				if s, _ := a.Content.(string); s != "" {
					a.Content = s + textOf(it.Content)
				} else {
					a.Content = textOf(it.Content)
				}
				continue
			}
			flush()
			parts, err := responsesParts(it.Content)
			if err != nil {
				return nil, badRequest("input[%d]: %v", i, err)
			}
			role := it.Role
			if role == "developer" {
				role = "system"
			}
			if role == "system" {
				msgs = append(msgs, chatMessage{Role: "system", Content: textOf(it.Content)})
			} else if len(parts) > 0 {
				msgs = append(msgs, chatMessage{Role: "user", Content: content(parts)})
			}
		case "reasoning":
			flush()
			pendingThink += reasoningText(it)
		case "function_call", "custom_tool_call":
			a := assistant()
			args := it.Arguments
			if typ == "custom_tool_call" {
				args = string(raw(map[string]string{"input": it.Input}))
			}
			if args == "" {
				args = "{}"
			}
			a.ToolCalls = append(a.ToolCalls, chatCall{ID: it.CallID, Type: "function", Function: chatFunc{Name: it.Name, Arguments: args}})
		case "function_call_output", "custom_tool_call_output":
			flush()
			out.ToolResults++
			msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: it.CallID, Content: outputText(it.Output)})
		default:
			out.Notes = append(out.Notes, "input item "+typ)
		}
	}
	flush()

	out.Fields["model"] = raw(in.Model)
	out.Fields["messages"] = raw(msgs)
	if in.Stream {
		out.Fields["stream"] = raw(true)
	}
	if in.MaxOutputTokens != nil {
		out.Fields["max_tokens"] = raw(*in.MaxOutputTokens)
	}
	if in.Temperature != nil {
		out.Fields["temperature"] = raw(*in.Temperature)
	}
	if in.TopP != nil {
		out.Fields["top_p"] = raw(*in.TopP)
	}
	var tools []chatTool
	for _, t := range in.Tools {
		switch t.Type {
		case "function":
			tools = append(tools, chatTool{Type: "function", Function: chatToolDef{Name: t.Name,
				Description: t.Description, Parameters: t.Parameters, Strict: t.Strict}})
		case "custom":
			tools = append(tools, chatTool{Type: "function", Function: chatToolDef{Name: t.Name,
				Description: t.Description + "\n\nPut the whole input in the \"input\" string.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`)}})
		default:
			out.Notes = append(out.Notes, "tool type "+t.Type+" (not served)")
		}
	}
	if len(tools) > 0 {
		out.Fields["tools"] = raw(tools)
		if tc := responsesToolChoice(in.ToolChoice); tc != nil {
			out.Fields["tool_choice"] = tc
		}
		if in.ParallelToolCalls != nil {
			out.Fields["parallel_tool_calls"] = raw(*in.ParallelToolCalls)
		}
	}
	if r := in.Reasoning; r != nil && r.Effort != "" {
		out.Effort = r.Effort
		if r.Effort == "none" {
			off := false
			out.Thinking = &off
		}
	}
	if t := in.Text; t != nil && t.Format != nil {
		switch t.Format.Type {
		case "json_schema":
			strict := true
			if t.Format.Strict != nil {
				strict = *t.Format.Strict
			}
			out.Fields["response_format"] = raw(map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": orStr(t.Format.Name, "output"), "schema": t.Format.Schema, "strict": strict}})
		case "json_object":
			out.Fields["response_format"] = raw(map[string]string{"type": "json_object"})
		}
	}
	return out, nil
}

func hasHistory(items []rItem) bool {
	for _, it := range items {
		switch it.Type {
		case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "reasoning":
			return true
		}
		if it.Role == "assistant" {
			return true
		}
	}
	return false
}

func reasoningText(it rItem) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(it.Content, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	if t, ok := Unsign(it.EncryptedContent); ok {
		return t
	}
	var sb strings.Builder
	for _, s := range it.Summary {
		sb.WriteString(s.Text)
	}
	return sb.String()
}

func outputText(r json.RawMessage) string {
	r = bytes.TrimSpace(r)
	if len(r) > 0 && r[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(r, &parts) == nil {
			var sb strings.Builder
			for _, p := range parts {
				sb.WriteString(p.Text)
			}
			return sb.String()
		}
	}
	return textOf(r)
}

func responsesParts(r json.RawMessage) ([]map[string]any, error) {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return nil, nil
	}
	if r[0] == '"' {
		return []map[string]any{textPart(textOf(r))}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		FileID   string `json:"file_id"`
	}
	if err := json.Unmarshal(r, &parts); err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, textPart(p.Text))
		case "input_image":
			if p.ImageURL == "" {
				return nil, badRequestErr("input_image needs image_url (file ids are not served)")
			}
			out = append(out, imagePart(p.ImageURL))
		case "input_file":
			return nil, badRequestErr("input_file is not supported: send the text")
		}
	}
	return out, nil
}

func responsesToolChoice(r json.RawMessage) json.RawMessage {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return nil
	}
	if r[0] == '"' {
		return r // auto | none | required
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(r, &tc) == nil && tc.Name != "" {
		return raw(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
	}
	return nil
}

// Responses is the Responses API for one request.
type Responses struct {
	Model     string
	Custom    map[string]bool
	Encrypted bool
}

// NewResponses returns the translator for req.
func NewResponses(req *Request) *Responses {
	return &Responses{Model: req.Model, Custom: req.Custom, Encrypted: req.EncryptedReasoning}
}

func (a *Responses) reasoningItem(id, text string) map[string]any {
	it := map[string]any{"type": "reasoning", "id": id, "summary": []any{},
		"content": []map[string]string{{"type": "reasoning_text", "text": text}}}
	if a.Encrypted {
		it["encrypted_content"] = Sign(text)
	}
	return it
}

func messageItem(id, text, status string) map[string]any {
	return map[string]any{"type": "message", "id": id, "status": status, "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func (a *Responses) callItem(id, callID, name, args, status string) map[string]any {
	if a.Custom[name] {
		var in struct {
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			in.Input = args
		}
		return map[string]any{"type": "custom_tool_call", "id": id, "call_id": callID, "name": name,
			"input": in.Input, "status": status}
	}
	return map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": name,
		"arguments": args, "status": status}
}

func responsesUsage(u *Usage) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens,
		"total_tokens":          u.PromptTokens + u.CompletionTokens,
		"input_tokens_details":  map[string]int{"cached_tokens": u.cached()},
		"output_tokens_details": map[string]int{"reasoning_tokens": u.reasoning()}}
}

func (a *Responses) envelope(id, model, status string, output []map[string]any, u *Usage, finish string) map[string]any {
	if output == nil {
		output = []map[string]any{}
	}
	r := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": model, "output": output, "usage": responsesUsage(u), "error": nil, "incomplete_details": nil}
	if finish == "length" {
		r["status"] = "incomplete"
		r["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}
	return r
}

// Reply translates a non-streamed reply.
func (a *Responses) Reply(body []byte) ([]byte, error) {
	var c Completion
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	var out []map[string]any
	finish := ""
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		finish = ch.FinishReason
		if t := orStr(ch.Message.ReasoningContent, ch.Message.Reasoning); t != "" {
			out = append(out, a.reasoningItem(newID("rs_"), t))
		}
		if t := textOf(ch.Message.Content); t != "" {
			out = append(out, messageItem(newID("msg_"), t, "completed"))
		}
		for _, tc := range ch.Message.ToolCalls {
			out = append(out, a.callItem(newID("fc_"), orStr(tc.ID, newID("call_")), tc.Function.Name, tc.Function.Arguments, "completed"))
		}
	}
	return json.Marshal(a.envelope(newID("resp_"), orStr(c.Model, a.Model), "completed", out, c.Usage, finish))
}

// Error translates an error (the OpenAI shape, which the Responses API uses).
func (a *Responses) Error(status int, msg string) []byte {
	typ := "server_error"
	if status/100 == 4 {
		typ = "invalid_request_error"
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": nil}})
	return b
}

// NewStream starts a streamed reply.
func (a *Responses) NewStream() Stream {
	return &rStream{a: a, id: newID("resp_"), model: a.Model, cur: -1, calls: map[int]int{}}
}

type rOut struct {
	kind                 string // reasoning | message | call
	id, callID, name     string
	text                 strings.Builder
	done                 bool
	outputIndex          int
	partOpen, itemOpened bool
}

type rStream struct {
	a      *Responses
	id     string
	model  string
	seq    int
	outs   []*rOut
	cur    int         // open output index, -1 = none
	calls  map[int]int // chat tool index → output index
	finish string
	usage  *Usage
	ended  bool
}

func (s *rStream) ev(b *bytes.Buffer, typ string, fields map[string]any) {
	fields["type"] = typ
	fields["sequence_number"] = s.seq
	s.seq++
	sseEvent(b, typ, fields)
}

func (s *rStream) Start() []byte {
	var b bytes.Buffer
	env := s.a.envelope(s.id, s.model, "in_progress", nil, nil, "")
	s.ev(&b, "response.created", map[string]any{"response": env})
	s.ev(&b, "response.in_progress", map[string]any{"response": env})
	return b.Bytes()
}

// Ping: the Responses API has no ping event; an SSE comment keeps the
// connection alive.
func (s *rStream) Ping() []byte { return []byte(": keep-alive\n\n") }

func (s *rStream) item(o *rOut, status string) map[string]any {
	switch o.kind {
	case "reasoning":
		return s.a.reasoningItem(o.id, o.text.String())
	case "message":
		return messageItem(o.id, o.text.String(), status)
	default:
		return s.a.callItem(o.id, o.callID, o.name, o.text.String(), status)
	}
}

func (s *rStream) closeCur(b *bytes.Buffer) {
	if s.cur < 0 {
		return
	}
	o := s.outs[s.cur]
	s.cur = -1
	if o.done {
		return
	}
	o.done = true
	text := o.text.String()
	switch o.kind {
	case "reasoning":
		s.ev(b, "response.reasoning_text.done", map[string]any{"item_id": o.id, "output_index": o.outputIndex,
			"content_index": 0, "text": text})
	case "message":
		s.ev(b, "response.output_text.done", map[string]any{"item_id": o.id, "output_index": o.outputIndex,
			"content_index": 0, "text": text})
		s.ev(b, "response.content_part.done", map[string]any{"item_id": o.id, "output_index": o.outputIndex,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})
	default:
		if s.a.Custom[o.name] {
			in := s.item(o, "completed")["input"]
			s.ev(b, "response.custom_tool_call_input.done", map[string]any{"item_id": o.id, "output_index": o.outputIndex, "input": in})
		} else {
			s.ev(b, "response.function_call_arguments.done", map[string]any{"item_id": o.id, "output_index": o.outputIndex, "arguments": text})
		}
	}
	s.ev(b, "response.output_item.done", map[string]any{"output_index": o.outputIndex, "item": s.item(o, "completed")})
}

func (s *rStream) open(b *bytes.Buffer, o *rOut) int {
	s.closeCur(b)
	o.outputIndex = len(s.outs)
	s.outs = append(s.outs, o)
	s.cur = o.outputIndex
	s.ev(b, "response.output_item.added", map[string]any{"output_index": o.outputIndex, "item": s.item(o, "in_progress")})
	if o.kind == "message" {
		s.ev(b, "response.content_part.added", map[string]any{"item_id": o.id, "output_index": o.outputIndex,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	}
	return o.outputIndex
}

func (s *rStream) curKind() string {
	if s.cur < 0 {
		return ""
	}
	return s.outs[s.cur].kind
}

func (s *rStream) Chunk(payload []byte) []byte {
	var ch Chunk
	if json.Unmarshal(payload, &ch) != nil {
		return nil
	}
	if len(ch.Error) > 0 && string(ch.Error) != "null" {
		return s.Fail(ErrorMessage(payload))
	}
	if ch.Usage != nil {
		s.usage = ch.Usage
	}
	if ch.Model != "" {
		s.model = ch.Model
	}
	var b bytes.Buffer
	for i, c := range ch.Choices {
		if i > 0 {
			break
		}
		d := c.Delta
		if t := d.ReasoningContent + d.Reasoning; t != "" {
			if s.curKind() != "reasoning" {
				s.open(&b, &rOut{kind: "reasoning", id: newID("rs_")})
			}
			o := s.outs[s.cur]
			o.text.WriteString(t)
			s.ev(&b, "response.reasoning_text.delta", map[string]any{"item_id": o.id, "output_index": o.outputIndex,
				"content_index": 0, "delta": t})
		}
		if d.Content != "" {
			if s.curKind() != "message" {
				s.open(&b, &rOut{kind: "message", id: newID("msg_")})
			}
			o := s.outs[s.cur]
			o.text.WriteString(d.Content)
			s.ev(&b, "response.output_text.delta", map[string]any{"item_id": o.id, "output_index": o.outputIndex,
				"content_index": 0, "delta": d.Content})
		}
		for _, tc := range d.ToolCalls {
			idx := len(s.calls)
			if tc.Index != nil {
				idx = *tc.Index
			}
			oi, ok := s.calls[idx]
			if !ok {
				oi = s.open(&b, &rOut{kind: "call", id: newID("fc_"), callID: orStr(tc.ID, newID("call_")), name: tc.Function.Name})
				s.calls[idx] = oi
			}
			o := s.outs[oi]
			if tc.Function.Arguments == "" || o.done {
				continue
			}
			o.text.WriteString(tc.Function.Arguments)
			if !s.a.Custom[o.name] { // a custom tool's input is only known whole
				s.ev(&b, "response.function_call_arguments.delta", map[string]any{"item_id": o.id,
					"output_index": o.outputIndex, "delta": tc.Function.Arguments})
			}
		}
		if c.FinishReason != "" {
			s.finish = c.FinishReason
		}
	}
	return b.Bytes()
}

func (s *rStream) final() []map[string]any {
	out := make([]map[string]any, 0, len(s.outs))
	for _, o := range s.outs {
		out = append(out, s.item(o, "completed"))
	}
	return out
}

func (s *rStream) End() []byte {
	if s.ended {
		return nil
	}
	s.ended = true
	var b bytes.Buffer
	s.closeCur(&b)
	env := s.a.envelope(s.id, s.model, "completed", s.final(), s.usage, s.finish)
	name := "response.completed"
	if env["status"] == "incomplete" {
		name = "response.incomplete"
	}
	s.ev(&b, name, map[string]any{"response": env})
	return b.Bytes()
}

func (s *rStream) Fail(msg string) []byte {
	if s.ended {
		return nil
	}
	s.ended = true
	var b bytes.Buffer
	env := s.a.envelope(s.id, s.model, "failed", s.final(), s.usage, "")
	env["error"] = map[string]string{"code": "server_error", "message": msg}
	s.ev(&b, "response.failed", map[string]any{"response": env})
	return b.Bytes()
}
