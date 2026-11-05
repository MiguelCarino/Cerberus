package config

// Gateway phase (G1 to G6, docs/SCOPE.md "Gateway phase specification").

// GatewayConfig holds the gateway's own settings.
type GatewayConfig struct {
	// ModelMap maps model names clients hard-code (glob, e.g. "claude-*",
	// "gpt-*") to a Cerberus model id, used when the name matches no model.
	ModelMap map[string]string `json:"model_map"`

	// Records keeps a line per gateway request (G3). Off by default.
	Records RecordsConfig `json:"records"`

	// Guard screens tool results for text aimed at the agent (G2). Off by
	// default; not a security boundary.
	Guard AgentGuardConfig `json:"guard"`
}

// Guard modes.
const (
	GuardOff   = "off"
	GuardWatch = "watch" // score and report; no reply changes
	GuardHold  = "hold"  // as watch, and hold side-effecting tool calls in a flagged turn
)

// AgentGuardConfig is the agent guard (gateway phase G2).
type AgentGuardConfig struct {
	Mode            string   `json:"mode"`              // off (default) | watch | hold
	Model           string   `json:"model"`             // a local classifier model id (Qwen3Guard-Gen output format)
	Threshold       float64  `json:"threshold"`         // flag score, default 0.5
	WaitMs          int      `json:"wait_ms"`           // longest a tool call waits for verdicts, default 10000
	WindowTokens    int      `json:"window_tokens"`     // long results are read in windows of this size, default 512
	SideEffectTools []string `json:"side_effect_tools"` // extra globs of tools that act outside the conversation
	ReadOnlyTools   []string `json:"read_only_tools"`   // extra globs of tools that only read
}

// RecordsConfig is the request-records setting (gateway phase G3).
type RecordsConfig struct {
	Enabled    bool   `json:"enabled"`
	Text       string `json:"text"`        // "none" (default) | "user" | "all"
	Dir        string `json:"dir"`         // "" = <data>/records
	RetainDays int    `json:"retain_days"` // default 14
	MaxMiB     int    `json:"max_mib"`     // default 1024
}

// Provider APIs a provider can declare it serves natively (ProviderConfig.APIs).
const (
	APIMessages  = "messages"  // Anthropic Messages API at base_url + /messages
	APIResponses = "responses" // OpenAI Responses API at base_url + /responses
)
