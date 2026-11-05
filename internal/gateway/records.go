package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"carino.systems/cerberus/internal/records"
)

// Gateway phase G3: request records. The gateway fills one records.Line per
// request from the request's shape, the reply as it streams and the usage
// record.

// WithRecords keeps a request record per request in store (nil = off).
func WithRecords(store *records.Store) Option {
	return func(g *Gateway) { g.records = store }
}

// issuedCalls remembers the tool call ids this gateway returned, so a tool
// result sent back can be matched to one of them (bounded: a ring).
type issuedCalls struct {
	mu   sync.Mutex
	set  map[string]bool
	ring []string
	next int
}

const issuedCap = 4096

func (s *issuedCalls) add(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set == nil {
		s.set, s.ring = map[string]bool{}, make([]string, issuedCap)
	}
	if s.set[id] {
		return
	}
	if old := s.ring[s.next]; old != "" {
		delete(s.set, old)
	}
	s.ring[s.next] = id
	s.next = (s.next + 1) % issuedCap
	s.set[id] = true
}

func (s *issuedCalls) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set[id]
}

// reqShape is what a record says about the request.
type reqShape struct {
	messages, tools, images int
	thinking                *bool
	effort                  string
	sampled, schema         bool
	budget                  int
	results, matched        int
	resultErrors            int
	text                    *records.Text
}

type shapeMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

// shapeOf reads a Chat Completions request (as sent upstream) for its record.
// level is the records text level: the text is rendered only when kept.
func (g *Gateway) shapeOf(req *request, level string) reqShape {
	f := req.fields
	var sh reqShape
	var msgs []shapeMessage
	_ = json.Unmarshal(f["messages"], &msgs)
	if len(msgs) == 0 {
		if p, ok := f["prompt"]; ok { // /completions
			msgs = []shapeMessage{{Role: "user", Content: p}}
		}
	}
	sh.messages = len(msgs)
	var tools []json.RawMessage
	_ = json.Unmarshal(f["tools"], &tools)
	sh.tools = len(tools)
	var sys, conv strings.Builder
	for _, m := range msgs {
		sh.images += countImages(m.Content)
		if m.Role == "tool" {
			sh.results++
			if g.issued.has(m.ToolCallID) {
				sh.matched++
			}
		}
		if level == "" || level == records.TextNone {
			continue
		}
		switch m.Role {
		case "system", "developer":
			sys.WriteString(contentText(m.Content))
			sys.WriteString("\n")
		case "tool":
			if level == records.TextAll {
				conv.WriteString("tool: " + contentText(m.Content) + "\n")
			}
		default:
			if t := contentText(m.Content); t != "" {
				conv.WriteString(m.Role + ": " + t + "\n")
			}
		}
	}
	if level != "" && level != records.TextNone {
		sh.text = &records.Text{System: strings.TrimSpace(sys.String()), Messages: strings.TrimSpace(conv.String())}
	}
	sh.resultErrors = req.resultErrors
	if e, ok := f["reasoning_effort"]; ok {
		_ = json.Unmarshal(e, &sh.effort)
	}
	if kw, ok := f["chat_template_kwargs"]; ok {
		var k struct {
			EnableThinking *bool `json:"enable_thinking"`
		}
		if json.Unmarshal(kw, &k) == nil {
			sh.thinking = k.EnableThinking
		}
	}
	if sh.thinking == nil && req.thinking != nil {
		sh.thinking = req.thinking
	}
	sh.sampled = true
	if t, ok := f["temperature"]; ok {
		var v float64
		if json.Unmarshal(t, &v) == nil && v == 0 {
			sh.sampled = false
		}
	}
	if rf, ok := f["response_format"]; ok {
		var v struct {
			Type string `json:"type"`
		}
		sh.schema = json.Unmarshal(rf, &v) == nil && (v.Type == "json_schema" || v.Type == "json_object")
	}
	for _, n := range budgetNames {
		if b, ok := f[n]; ok {
			_ = json.Unmarshal(b, &sh.budget)
			break
		}
	}
	return sh
}

// countImages counts the image parts of a message content.
func countImages(r json.RawMessage) int {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || r[0] != '[' {
		return 0
	}
	var parts []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(r, &parts) != nil {
		return 0
	}
	n := 0
	for _, p := range parts {
		switch p.Type {
		case "image_url", "image", "input_image":
			n++
		}
	}
	return n
}

// contentText is a message content's text (a string, or the text parts).
func contentText(r json.RawMessage) string {
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
		return string(r)
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			sb.WriteString(p.Text)
		case "image_url":
			sb.WriteString("[image]")
		}
	}
	return sb.String()
}

// toolDelta is a tool call piece in a reply.
type toolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// noteCalls adds a reply's tool calls (streamed pieces or whole calls).
func (c *call) noteCalls(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var ds []toolDelta
	if json.Unmarshal(raw, &ds) != nil {
		return
	}
	for i, d := range ds {
		idx := i
		if d.Index != nil {
			idx = *d.Index
		}
		if c.calls == nil {
			c.calls = map[int]*records.Call{}
		}
		k := c.calls[idx]
		if k == nil {
			k = &records.Call{}
			c.calls[idx] = k
		}
		if d.ID != "" {
			k.ID = d.ID
		}
		k.Name += d.Function.Name
	}
}

// addReason keeps reasoning text for a record at text level all.
func (c *call) addReason(s string) {
	if s == "" || !c.keepReason || c.reason.Len() >= maxCaptureText {
		return
	}
	if room := maxCaptureText - c.reason.Len(); len(s) > room {
		s = s[:room]
	}
	c.reason.WriteString(s)
}

// record writes c's request record.
func (g *Gateway) record(c *call) {
	if g.records == nil || c.shape == nil {
		return
	}
	sh := c.shape
	l := records.Line{Time: c.start, API: c.rec.API, Stream: c.stream, Model: c.rec.Model, Backend: c.rec.Backend,
		Provider: c.rec.Provider, Session: c.rec.SessionID, User: c.rec.User, Client: c.family,
		Messages: sh.messages, Tools: sh.tools, Images: sh.images, Thinking: sh.thinking, Effort: sh.effort,
		Sampled: sh.sampled, Schema: sh.schema, Budget: sh.budget,
		PromptTokens: c.rec.PromptTokens, CachedTokens: c.rec.CachedTokens, CompletionTokens: c.rec.CompletionTokens,
		ReasoningTokens: c.rec.ReasoningTokens,
		Finish:          c.rec.Finish, Status: c.rec.Status, Error: c.rec.Error, TTFTMs: c.rec.TTFTMs, DurationMs: c.rec.DurationMs,
		Results: sh.results, ResultErrors: sh.resultErrors, Matched: sh.matched, Guard: c.guard}
	switch {
	case c.rec.Status == statusClientClosed:
		l.Finish = "disconnect"
	case c.rec.Error != "" || c.rec.Status >= 400:
		if l.Finish == "" {
			l.Finish = "error"
		}
	}
	for i := 0; i < len(c.calls); i++ {
		if k := c.calls[i]; k != nil {
			l.Calls = append(l.Calls, *k)
		}
	}
	if sh.text != nil {
		t := *sh.text
		t.Reply = c.text.String()
		t.Reasoning = c.reason.String()
		l.Text = &t
	}
	g.records.Add(l)
}

// startRecord sets up c's request record from req (called again once the
// request is shaped for its endpoint, so the record says what went upstream).
func (g *Gateway) startRecord(c *call, r *http.Request, req *request) {
	if g.records == nil {
		return
	}
	lvl := g.records.TextLevel()
	sh := g.shapeOf(req, lvl)
	c.shape, c.stream = &sh, req.stream
	c.family = records.ClientFamily(r.Header.Get("X-Cerberus-Client"), r.Header.Get("User-Agent"))
	c.keepText = lvl == records.TextUser || lvl == records.TextAll
	c.keepReason = lvl == records.TextAll
}
