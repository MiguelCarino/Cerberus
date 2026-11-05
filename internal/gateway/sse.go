package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"carino.systems/cerberus/internal/usage"
)

// usageJSON is the OpenAI usage object.
type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	Details          *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *usageJSON) apply(r *usage.Record) {
	r.PromptTokens = u.PromptTokens
	r.CompletionTokens = u.CompletionTokens
	if u.Details != nil {
		r.CachedTokens = u.Details.CachedTokens
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
	} `json:"choices"`
	Usage *usageJSON      `json:"usage"`
	Error json.RawMessage `json:"error"` // an engine's in-stream error event
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
	for {
		line, err := readLine(br, buf[:0])
		buf = line
		if len(line) > 0 {
			drop := false
			if blank := len(bytes.TrimRight(line, "\r\n")) == 0; blank {
				drop, skipBlank = skipBlank, false
			} else {
				skipBlank = false
				if g.inspect(line, c) && !clientUsage {
					drop, skipBlank = true, true
				}
			}
			if !drop {
				if _, werr := w.Write(line); werr != nil {
					return &clientWriteError{werr}
				}
				_ = rc.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// inspect parses a "data:" line and updates c. It reports whether the line
// is a usage-only chunk (no choices), which may be dropped.
func (g *Gateway) inspect(line []byte, c *call) bool {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return false
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return false // e.g. [DONE]
	}
	var ch chunk
	if json.Unmarshal(payload, &ch) != nil {
		return false
	}
	if len(ch.Error) > 0 && string(ch.Error) != "null" && c.rec.Error == "" {
		c.rec.Error = errorMessage(payload)
	}
	if ch.hasContent() {
		if c.first.IsZero() {
			c.first = time.Now()
		}
		c.deltas++
	}
	if c.capture {
		for _, ch := range ch.Choices {
			c.addText(ch.Delta.Content)
		}
	}
	if ch.Usage != nil {
		ch.Usage.apply(&c.rec)
		return len(ch.Choices) == 0
	}
	return false
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
