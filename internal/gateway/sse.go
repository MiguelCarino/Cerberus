package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/usage"
)

// usageJSON is the OpenAI usage object, or (a native pass-through, G1) the
// Anthropic and Responses ones, which name the counts input and output.
type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	Details          *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`

	CompletionDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CacheRead    int `json:"cache_read_input_tokens"` // Anthropic: not in input_tokens
	InputDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"` // Responses: included in input_tokens
}

// apply sets the counts the object carries; a count it lacks (Anthropic's
// message_delta has no input count) leaves the record's as it was.
func (u *usageJSON) apply(r *usage.Record) {
	if p := u.PromptTokens + u.InputTokens + u.CacheRead; p > 0 {
		r.PromptTokens = p
	}
	if n := u.CompletionTokens + u.OutputTokens; n > 0 {
		r.CompletionTokens = n
	}
	if d := u.CompletionDetails; d != nil && d.ReasoningTokens > 0 {
		r.ReasoningTokens = d.ReasoningTokens
	} else if d := u.OutputDetails; d != nil && d.ReasoningTokens > 0 {
		r.ReasoningTokens = d.ReasoningTokens
	}
	switch {
	case u.Details != nil:
		r.CachedTokens = u.Details.CachedTokens
	case u.InputDetails != nil:
		r.CachedTokens = u.InputDetails.CachedTokens
	case u.CacheRead > 0:
		r.CachedTokens = u.CacheRead
	}
}

// chunk is the part of a streamed completion chunk the gateway inspects.
type chunk struct {
	Choices []struct {
		Text  string `json:"text"` // legacy /completions
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageJSON      `json:"usage"`
	Error json.RawMessage `json:"error"` // an engine's in-stream error event

	PromptProgress json.RawMessage `json:"prompt_progress"` // llama.cpp, with return_progress (G5)

	// A native pass-through's events (G1): Anthropic's "message_start"
	// (message.usage), "message_delta" (usage) and content deltas; the
	// Responses API's "response.*" events (response.usage at the end).
	Type     string                      `json:"type"`
	Message  *struct{ Usage *usageJSON } `json:"message"`
	Response *struct{ Usage *usageJSON } `json:"response"`
}

// nativeContent reports a native event that carries generated output.
func (ch *chunk) nativeContent() bool {
	return ch.Type == "content_block_delta" ||
		(strings.HasPrefix(ch.Type, "response.") && strings.HasSuffix(ch.Type, ".delta"))
}

// hasContent reports whether the chunk carries generated output.
func (ch *chunk) hasContent() bool {
	for _, c := range ch.Choices {
		d := c.Delta
		if c.Text != "" || d.Content != "" || d.ReasoningContent != "" || d.Reasoning != "" ||
			(len(d.ToolCalls) > 0 && string(d.ToolCalls) != "null") {
			return true
		}
	}
	return false
}

// relayStream copies an SSE body line by line, flushing after each line. It
// notes the first content chunk (TTFT), counts content deltas and reads the
// usage chunk, which it drops when only the gateway asked for it.
func (g *Gateway) relayStream(w http.ResponseWriter, body io.Reader, c *call, clientUsage bool) error {
	rc := http.NewResponseController(w)
	br := bufio.NewReaderSize(body, 16<<10)
	var buf []byte
	skipBlank := false // the blank line ending a dropped event
	write := func(b []byte) error {
		if _, werr := w.Write(b); werr != nil {
			return &clientWriteError{werr}
		}
		_ = rc.Flush()
		return nil
	}
	gs := &guardStream{run: c.guardRun} // G2
	for {
		line, err := readLine(br, buf[:0])
		buf = line
		if len(line) > 0 {
			drop, done := false, false
			if blank := len(bytes.TrimRight(line, "\r\n")) == 0; blank {
				drop, skipBlank = skipBlank, false
			} else {
				skipBlank = false
				calls := c.toolN
				if usageOnly, progressOnly := g.inspect(line, c); (usageOnly && !clientUsage) || progressOnly {
					drop, skipBlank = true, true
				}
				done = isDoneLine(line)
				if gs.run != nil && gs.run.s.mode == config.GuardHold && c.toolN > calls {
					gs.buffering = true // a tool call: it waits for the turn's verdicts
				}
			}
			if done && gs.run != nil {
				if werr := g.endGuardStream(write, c, gs); werr != nil {
					return werr
				}
			}
			if !drop {
				if gs.buffering {
					gs.buf = append(gs.buf, append([]byte(nil), line...))
				} else if werr := write(line); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if gs.run != nil && !gs.finished && errors.Is(err, io.EOF) {
				if werr := g.endGuardStream(write, c, gs); werr != nil {
					return werr
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// inspect parses a "data:" line and updates c. It reports whether the line
// is a usage-only chunk (no choices), which may be dropped, and whether it is
// a progress frame the gateway asked for and the client did not (G5).
func (g *Gateway) inspect(line []byte, c *call) (usageOnly, progressOnly bool) {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return false, false
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return false, false // e.g. [DONE]
	}
	var ch chunk
	if json.Unmarshal(payload, &ch) != nil {
		return false, false
	}
	if c.dropProgress && len(ch.PromptProgress) > 0 && string(ch.PromptProgress) != "null" &&
		!ch.hasContent() && ch.Usage == nil && len(ch.Error) == 0 {
		return false, true
	}
	if len(ch.Error) > 0 && string(ch.Error) != "null" && c.rec.Error == "" {
		c.rec.Error = errorMessage(payload)
	}
	if ch.Type != "" { // a native event: count it, never drop it
		if ch.nativeContent() {
			if c.first.IsZero() {
				c.first = time.Now()
			}
			c.deltas++
		}
		for _, u := range []*usageJSON{ch.Usage, msgUsage(ch.Message), msgUsage(ch.Response)} {
			if u != nil {
				u.apply(&c.rec)
			}
		}
		return false, false
	}
	if ch.hasContent() {
		if c.first.IsZero() {
			c.first = time.Now()
		}
		c.deltas++
	}
	for i, cc := range ch.Choices {
		if i > 0 {
			break // the record follows the first choice
		}
		d := cc.Delta
		if cc.Text != "" || d.Content != "" {
			c.contentN++
		}
		if d.ReasoningContent != "" || d.Reasoning != "" {
			c.reasoningN++
			c.addReason(d.ReasoningContent + d.Reasoning)
		}
		if len(d.ToolCalls) > 0 && string(d.ToolCalls) != "null" {
			c.toolN++
			if c.shape != nil {
				c.noteCalls(d.ToolCalls)
			}
		}
		if cc.FinishReason != "" {
			c.finish = cc.FinishReason
		}
	}
	if c.capture || c.keepText {
		for _, ch := range ch.Choices {
			c.addText(ch.Delta.Content)
		}
	}
	if ch.Usage != nil {
		ch.Usage.apply(&c.rec)
		return len(ch.Choices) == 0, false
	}
	return false, false
}

// readLine reads one line including its '\n' into buf, growing it for
// lines longer than the reader's buffer.
func readLine(br *bufio.Reader, buf []byte) ([]byte, error) {
	for {
		part, err := br.ReadSlice('\n')
		buf = append(buf, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return buf, err
		}
	}
}

func msgUsage(m *struct{ Usage *usageJSON }) *usageJSON {
	if m == nil {
		return nil
	}
	return m.Usage
}
