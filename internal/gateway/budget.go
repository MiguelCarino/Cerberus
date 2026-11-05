package gateway

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"

	"carino.systems/cerberus/internal/config"
)

// Gateway phase G4: one token budget under three names, per-model request
// defaults and a cap.

// budgetNames are the three names clients use for one reply budget:
// Chat Completions (max_tokens, max_completion_tokens) and the Responses
// API's max_output_tokens.
var budgetNames = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// budget reads the reply budget. It reports whether one was sent, and an
// error a client gets as a 400: a value that is not an integer, or two names
// with different values (never a guess at which was meant).
func (req *request) budget() (n int, set bool, err error) {
	first := ""
	for _, name := range budgetNames {
		raw, ok := req.fields[name]
		if !ok || string(raw) == "null" {
			continue
		}
		var v int
		if json.Unmarshal(raw, &v) != nil {
			return 0, false, fmt.Errorf("%s must be an integer", name)
		}
		if set && v != n {
			return 0, false, fmt.Errorf("%s (%d) and %s (%d) disagree: send one budget, or the same value under each name", first, n, name, v)
		}
		if !set {
			n, set, first = v, true, name
		}
	}
	return n, set, nil
}

// setBudget writes the budget under the one name the upstream reads:
// max_tokens for local engines and for /completions; for a cloud chat call
// the client's own name when it was max_completion_tokens (some models take
// only that), else max_tokens.
func (req *request) setBudget(n int, local bool, path string) {
	name := "max_tokens"
	if !local && path == "/chat/completions" {
		if _, ok := req.fields["max_completion_tokens"]; ok {
			name = "max_completion_tokens"
		}
	}
	for _, b := range budgetNames {
		delete(req.fields, b)
	}
	req.fields[name] = json.RawMessage(strconv.Itoa(n))
}

// shape applies G4 to a generation request once the endpoint is known: the
// budget under the upstream's name, then d's defaults for whatever the
// request left unset, then d's cap. It returns a note when the cap clipped
// the budget (logged once per client and model by the caller).
func (req *request) shape(local bool, path string, d config.ModelDefaults) (clipped string) {
	n, set, _ := req.budget() // checked before acquire
	if !set && d.MaxTokens > 0 {
		n, set = d.MaxTokens, true
	}
	if set && d.MaxTokensCap > 0 && (n > d.MaxTokensCap || n < 0) {
		clipped = fmt.Sprintf("token budget %d clipped to the model's cap %d", n, d.MaxTokensCap)
		n = d.MaxTokensCap
	}
	if set {
		req.setBudget(n, local, path)
	}
	if !thinkingNamed(req.fields) {
		if d.ReasoningEffort != "" {
			req.fields["reasoning_effort"] = mustJSON(d.ReasoningEffort)
		}
		if d.EnableThinking != nil && local {
			req.mergeKwarg("enable_thinking", *d.EnableThinking)
		}
	}
	setIfAbsent(req.fields, "temperature", d.Temperature)
	setIfAbsent(req.fields, "top_p", d.TopP)
	if d.TopK > 0 {
		if _, ok := req.fields["top_k"]; !ok {
			req.fields["top_k"] = json.RawMessage(strconv.Itoa(d.TopK))
		}
	}
	return clipped
}

// thinkingNamed reports whether a request already says how to think, in any
// of the forms clients use.
func thinkingNamed(f map[string]json.RawMessage) bool {
	for _, k := range []string{"reasoning_effort", "reasoning", "thinking", "enable_thinking"} {
		if _, ok := f[k]; ok {
			return true
		}
	}
	if raw, ok := f["chat_template_kwargs"]; ok {
		var kw map[string]json.RawMessage
		if json.Unmarshal(raw, &kw) == nil {
			if _, ok := kw["enable_thinking"]; ok {
				return true
			}
		}
	}
	return false
}

// mergeKwarg sets one key of chat_template_kwargs, keeping the others.
func (req *request) mergeKwarg(key string, v any) {
	kw := map[string]json.RawMessage{}
	if raw, ok := req.fields["chat_template_kwargs"]; ok {
		_ = json.Unmarshal(raw, &kw)
		if kw == nil {
			kw = map[string]json.RawMessage{}
		}
	}
	kw = maps.Clone(kw)
	kw[key] = mustJSON(v)
	req.fields["chat_template_kwargs"] = mustJSON(kw)
}

func setIfAbsent(f map[string]json.RawMessage, key string, v *float64) {
	if v == nil {
		return
	}
	if _, ok := f[key]; !ok {
		f[key] = mustJSON(*v)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain values are marshalled here
	}
	return b
}

// SetModels takes the per-model request defaults from cfg's model entries
// (copied; Reload calls it again with a new config).
func (g *Gateway) SetModels(models []config.ModelConfig) {
	m := map[string]config.ModelDefaults{}
	for _, mc := range models {
		if mc.ID != "" && mc.Defaults != (config.ModelDefaults{}) {
			m[mc.ID] = mc.Defaults
		}
	}
	g.priceMu.Lock()
	g.defaults = m
	g.priceMu.Unlock()
}

// defaultsFor returns the defaults of a resolved model id.
func (g *Gateway) defaultsFor(id string) config.ModelDefaults {
	g.priceMu.Lock()
	defer g.priceMu.Unlock()
	return g.defaults[id]
}

// noteOnce logs msg once per key (a client and model), so a client that
// always asks for more than the cap does not fill the log.
func (g *Gateway) noteOnce(key, msg string, args ...any) {
	g.priceMu.Lock()
	seen := g.noted[key]
	if !seen {
		if g.noted == nil {
			g.noted = map[string]bool{}
		}
		if len(g.noted) < 1024 {
			g.noted[key] = true
		}
	}
	g.priceMu.Unlock()
	if !seen {
		g.log.Info(msg, args...)
	}
}

// finishOf names how a reply ended for the usage record: the upstream's
// finish_reason, or "length_in_thinking" when the budget ran out while the
// model was still thinking (reasoning came, no answer and no tool call).
func finishOf(c *call) string {
	if c.finish == "length" && c.reasoningN > 0 && c.contentN == 0 && c.toolN == 0 {
		return "length_in_thinking"
	}
	return c.finish
}
