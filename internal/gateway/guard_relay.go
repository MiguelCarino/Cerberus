package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"

	"carino.systems/cerberus/internal/config"
)

// The guard's side of relaying a reply (G2). Streamed: in hold mode, from
// the first tool call piece on, the reply is buffered until its turn's
// verdicts are in (wait_ms at most); a tainted turn's side-effecting calls
// are then replaced by the held message. Either way a chunk without choices
// carrying cerberus_guard goes out ahead of [DONE]. Non-streamed: the body
// is read whole and gets the same treatment and a cerberus_guard field.

// guardStream is the per-reply state of relayStream.
type guardStream struct {
	run       *guardRun
	buffering bool
	buf       [][]byte // lines held back while buffering
	finished  bool
}

func isDoneLine(line []byte) bool {
	p, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	return ok && string(bytes.TrimSpace(p)) == "[DONE]"
}

// sseData formats one data event.
func sseData(v any) []byte {
	b, _ := json.Marshal(v)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// assembleCalls reads the tool calls out of buffered stream lines.
func assembleCalls(lines [][]byte) []heldCall {
	type acc struct {
		name, args string
	}
	m := map[int]*acc{}
	for _, l := range lines {
		p, ok := bytes.CutPrefix(bytes.TrimSpace(l), []byte("data:"))
		if !ok {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    *int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(bytes.TrimSpace(p), &ch) != nil || len(ch.Choices) == 0 {
			continue
		}
		for i, tc := range ch.Choices[0].Delta.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			a := m[idx]
			if a == nil {
				a = &acc{}
				m[idx] = a
			}
			a.name += tc.Function.Name
			a.args += tc.Function.Arguments
		}
	}
	idx := make([]int, 0, len(m))
	for i := range m {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]heldCall, 0, len(idx))
	for _, i := range idx {
		out = append(out, heldCall{name: m[i].name, args: m[i].args})
	}
	return out
}

// usageOnlyLine reports a buffered line that carries usage and no choices
// (kept when held calls are replaced, so the client still gets its usage).
func usageOnlyLine(l []byte) bool {
	p, ok := bytes.CutPrefix(bytes.TrimSpace(l), []byte("data:"))
	if !ok {
		return false
	}
	var ch struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	return json.Unmarshal(bytes.TrimSpace(p), &ch) == nil && len(ch.Choices) == 0 && len(ch.Usage) > 0 && string(ch.Usage) != "null"
}

// endGuardStream settles a streamed reply's guard: flushes or replaces what
// was buffered, then writes the cerberus_guard chunk.
func (g *Gateway) endGuardStream(write func([]byte) error, c *call, gs *guardStream) error {
	if gs.finished {
		return nil
	}
	gs.finished = true
	r := gs.run
	ready := false
	if gs.buffering || r.s.mode == config.GuardHold {
		ready = r.wait(context.Background())
	} else {
		select {
		case <-r.done:
			ready = true
		default:
		}
	}
	if gs.buffering {
		gs.buffering = false
		lines := gs.buf
		gs.buf = nil
		if !ready {
			g.gc.timedOut.Add(1)
			g.log.Warn("agent guard: verdicts not in time; tool calls went out unscreened", "model", c.rec.Model, "wait", r.s.wait)
		}
		if ready && r.tainted() {
			if held := r.decide(assembleCalls(lines)); len(held) > 0 {
				g.noteHeld(c, r, held)
				c.finish, c.toolN, c.contentN = "held", 0, 1
				for _, b := range [][]byte{
					sseData(map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0,
						"delta": map[string]string{"role": "assistant", "content": heldText(held)}}}}),
					sseData(map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0,
						"delta": map[string]any{}, "finish_reason": "stop"}}}),
				} {
					if err := write(b); err != nil {
						return err
					}
				}
				for _, l := range lines {
					if usageOnlyLine(l) {
						if err := write(append(append([]byte{}, l...), '\n')); err != nil {
							return err
						}
					}
				}
				return write(sseData(map[string]any{"object": "chat.completion.chunk", "choices": []any{}, "cerberus_guard": r.report()}))
			}
		}
		for _, l := range lines {
			if err := write(l); err != nil {
				return err
			}
		}
	}
	return write(sseData(map[string]any{"object": "chat.completion.chunk", "choices": []any{}, "cerberus_guard": r.report()}))
}

// guardBody settles a non-streamed reply: reads it whole, holds side-effecting
// calls in a tainted turn (hold mode) and adds cerberus_guard.
func (g *Gateway) guardBody(c *call, body io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxUsageScan))
	if err != nil {
		return b, err
	}
	r := c.guardRun
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return b, nil
	}
	var choices []map[string]json.RawMessage
	_ = json.Unmarshal(doc["choices"], &choices)
	var calls []heldCall
	if len(choices) > 0 {
		var msg struct {
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		_ = json.Unmarshal(choices[0]["message"], &msg)
		for _, tc := range msg.ToolCalls {
			calls = append(calls, heldCall{name: tc.Function.Name, args: tc.Function.Arguments})
		}
	}
	ready := false
	if r.s.mode == config.GuardHold && len(calls) > 0 {
		if ready = r.wait(context.Background()); !ready {
			g.gc.timedOut.Add(1)
			g.log.Warn("agent guard: verdicts not in time; tool calls went out unscreened", "model", c.rec.Model, "wait", r.s.wait)
		}
	}
	if ready && r.tainted() {
		if held := r.decide(calls); len(held) > 0 {
			g.noteHeld(c, r, held)
			choices[0]["message"] = mustJSON(map[string]any{"role": "assistant", "content": heldText(held)})
			choices[0]["finish_reason"] = mustJSON("stop")
			doc["choices"] = mustJSON(choices)
		}
	}
	doc["cerberus_guard"] = mustJSON(r.report())
	return json.Marshal(doc)
}
