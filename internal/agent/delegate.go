package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Delegation tools: an agent (often a cloud model) lists the local models
// and hands them small jobs. They go through the daemon's agent scope like
// the cerberus_* session tools.
const (
	ToolModels = "cerberus_models"
	ToolAsk    = "cerberus_ask"
	ToolRate   = "cerberus_rate"
)

// askCallTimeout bounds one cerberus_ask (a load plus a long answer).
const askCallTimeout = 5 * time.Minute

// DelegateModel is one local model as cerberus_models shows it.
type DelegateModel struct {
	ID       string   `json:"id"`
	Backend  string   `json:"backend"`
	Loaded   bool     `json:"loaded"`
	Ctx      int      `json:"ctx,omitempty"`
	TokPerS  float64  `json:"tok_per_s,omitempty"` // measured average, when there is history
	Tags     []string `json:"tags,omitempty"`
	ParamsGB float64  `json:"size_gb,omitempty"`
}

// DelegateModels is GET /api/agent/models.
type DelegateModels struct {
	Guidance string          `json:"guidance"`
	Load     bool            `json:"load"` // asks may load a model that is not loaded
	Models   []DelegateModel `json:"models"`
}

// DelegateAsk is POST /api/agent/ask.
type DelegateAsk struct {
	Model     string   `json:"model"` // a local model id, or "local" for the best loaded one
	Prompt    string   `json:"prompt"`
	System    string   `json:"system,omitempty"`
	Files     []string `json:"files,omitempty"` // workspace paths whose text goes with the prompt
	MaxTokens int      `json:"max_tokens,omitempty"`
}

// DelegateResult is its answer.
type DelegateResult struct {
	ID               string  `json:"id"` // rate it with cerberus_rate
	Model            string  `json:"model"`
	Text             string  `json:"text"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Seconds          float64 `json:"seconds"`
	TokPerS          float64 `json:"tok_per_s,omitempty"`
	EnergyJ          float64 `json:"energy_j,omitempty"`
	SavedUSD         float64 `json:"saved_usd,omitempty"` // what the reference cloud model would have cost
	Truncated        bool    `json:"truncated,omitempty"` // stopped at max_tokens
}

// DelegateRate is POST /api/agent/rate.
type DelegateRate struct {
	ID     string `json:"id"`
	Good   bool   `json:"good"`
	Better string `json:"better,omitempty"`
	Note   string `json:"note,omitempty"`
}

// DelegateTools are the definitions of the delegation tools.
func DelegateTools() []SpawnTool {
	return []SpawnTool{
		{ToolModels, "List the local models you can hand small jobs to with cerberus_ask: whether each is " +
			"loaded, its context size, measured speed and tags, plus guidance on what to send them. Local " +
			"models cost no API money; use them for bulk, low-risk work and check what they return.",
			schema(map[string]any{})},
		{ToolAsk, "Send one self-contained job to a local model and get its answer (no tools on its side). " +
			"Good for summaries of long files or logs, searching or listing, boilerplate, first drafts and " +
			"simple rewrites. If the model is busy or not loaded you are told at once: then do the job yourself.",
			schema(map[string]any{
				"model":  prop("string", `A model id from cerberus_models, or "local" for the best loaded one.`),
				"prompt": prop("string", "The whole job: what to do and what to return. The model sees nothing else."),
				"system": prop("string", "An optional system instruction."),
				"files": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Workspace files (relative paths) to include as text, at most 8."},
				"max_tokens": map[string]any{"type": "integer", "minimum": 16, "maximum": 32768,
					"description": "The longest answer you want (default from the settings)."},
			}, "model", "prompt")},
		{ToolRate, "Say whether a cerberus_ask answer was good, and give the better answer when it was not. " +
			"Ratings teach the local model later (when collecting examples is on).",
			schema(map[string]any{
				"id":     prop("string", "The id cerberus_ask returned."),
				"good":   map[string]any{"type": "boolean", "description": "true when the answer was right and usable as it is."},
				"better": prop("string", "What the answer should have been (optional)."),
				"note":   prop("string", "A short reason (optional)."),
			}, "id", "good")},
	}
}

// isDelegateTool reports whether name is a delegation tool.
func isDelegateTool(name string) bool {
	switch name {
	case ToolModels, ToolAsk, ToolRate:
		return true
	}
	return false
}

// callDelegate runs one delegation tool through the daemon.
func (c *SpawnClient) callDelegate(ctx context.Context, name string, ar args) (string, error) {
	switch name {
	case ToolModels:
		var res DelegateModels
		if err := c.do(ctx, http.MethodGet, "/api/agent/models", nil, &res, spawnCallTimeout); err != nil {
			return "", err
		}
		return FormatDelegateModels(res), nil
	case ToolAsk:
		req := DelegateAsk{Model: strings.TrimSpace(ar.str("model")), Prompt: ar.str("prompt"), System: ar.str("system")}
		if strings.TrimSpace(req.Prompt) == "" {
			return "", errors.New("prompt is required")
		}
		if req.Model == "" {
			req.Model = "local"
		}
		if n, ok := ar.int("max_tokens"); ok {
			req.MaxTokens = n
		}
		if fs, ok := ar["files"].([]any); ok {
			for _, f := range fs {
				if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
					req.Files = append(req.Files, s)
				}
			}
		}
		if c.local != nil && len(req.Files) > 0 {
			text, err := c.local.read(req.Files)
			if err != nil {
				return "", err
			}
			req.Prompt += text
			req.Files = nil
		}
		var res DelegateResult
		err := c.do(ctx, http.MethodPost, "/api/agent/ask", req, &res, askCallTimeout)
		var de *daemonError
		if errors.As(err, &de) && (de.status == http.StatusConflict || de.status == http.StatusTooManyRequests) {
			// Busy or not loaded is an answer, not a failure: the agent goes on.
			return "Not available now: " + de.msg + ". Do this job yourself.", nil
		}
		if err != nil {
			return "", err
		}
		return FormatAskResult(res), nil
	case ToolRate:
		id := strings.TrimSpace(ar.str("id"))
		if id == "" {
			return "", errors.New("id is required")
		}
		good, _ := ar["good"].(bool)
		req := DelegateRate{ID: id, Good: good, Better: ar.str("better"), Note: ar.str("note")}
		var res struct {
			Kept bool `json:"kept"`
		}
		if err := c.do(ctx, http.MethodPost, "/api/agent/rate", req, &res, spawnCallTimeout); err != nil {
			return "", err
		}
		if !res.Kept {
			return "Noted (collecting training examples is off, so the rating is not kept).", nil
		}
		return "Rating kept for " + id + ".", nil
	}
	return "", fmt.Errorf("there is no tool %q", name)
}

// FormatDelegateModels is cerberus_models' text for the model.
func FormatDelegateModels(res DelegateModels) string {
	var b strings.Builder
	if len(res.Models) == 0 {
		b.WriteString("No local models are available for delegation. Do the work yourself.")
		return b.String()
	}
	b.WriteString("Local models (id · loaded · context · speed · tags):\n")
	for _, m := range res.Models {
		state := "not loaded"
		if m.Loaded {
			state = "loaded"
		}
		fmt.Fprintf(&b, "- %s · %s", m.ID, state)
		if m.Ctx > 0 {
			fmt.Fprintf(&b, " · %dk ctx", m.Ctx/1024)
		}
		if m.TokPerS > 0 {
			fmt.Fprintf(&b, " · ~%.0f tok/s", m.TokPerS)
		}
		if len(m.Tags) > 0 {
			b.WriteString(" · " + strings.Join(m.Tags, ", "))
		}
		b.WriteByte('\n')
	}
	if !res.Load {
		b.WriteString("Only loaded models answer; a model that is not loaded says so at once.\n")
	}
	if res.Guidance != "" {
		b.WriteString("Guidance: " + res.Guidance)
	}
	return strings.TrimRight(b.String(), "\n")
}

// FormatAskResult is cerberus_ask's text for the model.
func FormatAskResult(r DelegateResult) string {
	head := fmt.Sprintf("[%s · id %s · %d+%d tokens · %.1f s", r.Model, r.ID, r.PromptTokens, r.CompletionTokens, r.Seconds)
	if r.Truncated {
		head += " · cut at max_tokens"
	}
	head += "]"
	return head + "\n" + r.Text + "\n\nCheck it before you use it; rate it with cerberus_rate."
}
