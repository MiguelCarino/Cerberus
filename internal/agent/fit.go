package agent

// Context hygiene and retries (2026-11-15 hardening). A long run keeps
// every tool result, so its history can outgrow a local model's window.
// Before each request the engine estimates the request's size; above
// fitShare of the model's context it replaces old tool results with a
// short stub, oldest first, in the request only (conversation.json keeps
// the full history, so a resume elides again from the source). The
// tool_call/tool pairs stay whole: a result's content changes, its message
// stays. When the server still says the context overflowed, the engine
// elides harder (every result but the newest turn's) and sends the
// request once more.
//
// A transient engine failure (the gateway's 502/503 while a local engine
// reloads, a dropped connection) before any reply text is retried up to
// len(retryWaits) times.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	modelengine "carino.systems/cerberus/internal/engine"
)

const (
	// fitShare is the share of the model's context a request may fill
	// before old tool results are elided.
	fitShare = 0.70
	// fitKeepTurns is how many of the newest tool-calling turns keep their
	// results whole (one after an overflow).
	fitKeepTurns = 4
	// bytesPerToken is the rough size estimate (bytes / 3.5).
	bytesPerToken = 3.5
	// imageTokens is what one image counts for in the estimate.
	imageTokens = 1000
	// stubMin: a result shorter than this is left alone (a stub saves nothing).
	stubMin = 200
)

// retryWaits are the pauses before each retry of a transient engine
// failure (a variable so tests shorten them).
var retryWaits = []time.Duration{2 * time.Second, 6 * time.Second}

// reOverflow matches a server's "the prompt does not fit" error:
// llama-server ("exceeds the available context size"), LM Studio and
// OpenAI-style servers ("maximum context length", context_length_exceeded),
// Anthropic-style "prompt is too long".
var reOverflow = regexp.MustCompile(`(?i)context.?(size|length|window|limit)|context_length|exceed\w*.{0,40}context|too long|too many tokens|n_ctx`)

// contextOverflow reports whether se says the request did not fit the
// model's context.
func contextOverflow(se *ServerError) bool {
	switch se.Status {
	case 0, http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusInternalServerError:
		return reOverflow.MatchString(se.Message)
	}
	return false
}

// transientServer reports whether se is a temporary engine failure worth
// a retry (the engine's own rule: 502/503 unless the model never fits, a
// crash text, a dropped connection; never a limit or another 4xx).
func transientServer(se *ServerError) bool {
	return modelengine.IsTransientText(se.Status, se.Message)
}

// sleepCtx waits d; false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// modelContext asks the server's /models for the model's context window in
// tokens (Cerberus' cerberus.ctx, else context_length); 0 when it is not
// known.
func (c *client) modelContext(ctx context.Context, model string) int {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/models", nil)
	if err != nil {
		return 0
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var list struct {
		Data []struct {
			ID       string `json:"id"`
			Ctx      int    `json:"context_length"`
			Cerberus struct {
				Ctx int `json:"ctx"`
			} `json:"cerberus"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list) != nil {
		return 0
	}
	for _, m := range list.Data {
		if m.ID == model {
			if m.Cerberus.Ctx > 0 {
				return m.Cerberus.Ctx
			}
			return max(0, m.Ctx)
		}
	}
	return 0
}

// msgBytes is a message's share of the estimate.
func msgBytes(m Message) int {
	n := len(m.Content) + len(m.Role) + len(m.ToolCallID) + 16
	for _, c := range m.ToolCalls {
		n += len(c.ID) + len(c.Function.Name) + len(c.Function.Arguments) + 32
	}
	return n + len(m.Images)*int(imageTokens*bytesPerToken)
}

// estimateTokens is a request's rough size: message bytes plus the tool
// specs' bytes, over bytesPerToken.
func estimateTokens(msgs []Message, specBytes int) int {
	n := specBytes
	for _, m := range msgs {
		n += msgBytes(m)
	}
	return int(float64(n) / bytesPerToken)
}

// elidedStub is what an elided tool result says instead.
func elidedStub(tool string, n int) string {
	if tool == "" {
		tool = "the tool"
	}
	return fmt.Sprintf("(output of %s elided to save context: %d chars; run it again if you need it)", tool, n)
}

// fitMessages returns msgs (a request's messages, system first) with old
// tool results elided until the estimate fits fitShare of ctxTokens.
// hard elides every result except the newest tool-calling turn's, whatever
// the estimate (the server said it did not fit). Only the copy changes:
// msgs' elements are values, the caller's history is untouched. elided is
// the number of results replaced.
func fitMessages(msgs []Message, specBytes, ctxTokens int, hard bool) (out []Message, elided, tokens int) {
	tokens = estimateTokens(msgs, specBytes)
	limit := int(fitShare * float64(ctxTokens))
	if !hard && (ctxTokens <= 0 || tokens <= limit) {
		return msgs, 0, tokens
	}
	keep := fitKeepTurns
	if hard {
		keep = 1
	}
	// turn[i] is the tool-calling assistant turn a tool result follows
	// (1 = the oldest); the newest `keep` turns' results stay whole. By
	// position, not by id: some servers reuse call ids across turns.
	turn := make([]int, len(msgs))
	turns := 0
	for i, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			turns++
		case m.Role == "tool":
			turn[i] = turns
		}
	}
	out = append([]Message(nil), msgs...)
	var turnCalls []toolCall
	for i := range out {
		m := out[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			turnCalls = m.ToolCalls
		}
		if !hard && tokens <= limit {
			break
		}
		if m.Role != "tool" || turn[i] == 0 || turn[i] > turns-keep || len(m.Content) < stubMin {
			continue
		}
		name := ""
		for _, c := range turnCalls {
			if c.ID == m.ToolCallID {
				name = c.Function.Name
			}
		}
		stub := elidedStub(name, len([]rune(m.Content)))
		tokens -= int(float64(len(m.Content)-len(stub)) / bytesPerToken)
		out[i].Content = stub
		elided++
	}
	return out, elided, tokens
}

// fit applies fitMessages to a request and notes when more results were
// elided than before.
func (e *engine) fit(body *chatRequest) {
	specBytes := 0
	if body.Tools != nil {
		if raw, err := json.Marshal(body.Tools); err == nil {
			specBytes = len(raw)
		}
	}
	msgs, n, tokens := fitMessages(body.Messages, specBytes, e.ctxTokens, e.squeezed)
	body.Messages = msgs
	if n > e.elided {
		what := fmt.Sprintf("%d old tool results were shortened to fit the model's context", n)
		if n == 1 {
			what = "1 old tool result was shortened to fit the model's context"
		}
		if e.ctxTokens > 0 {
			what += fmt.Sprintf(" (about %d of %d tokens)", tokens, e.ctxTokens)
		}
		e.emit(Event{Type: EvNote, Text: what})
		e.elided = n
	}
}
