package config

// DelegateConfig lets an agent hand small jobs to local models
// (cerberus_models, cerberus_ask): a cloud planner keeps the thinking and
// the local GPU does the bulk work.
type DelegateConfig struct {
	Enabled        bool                `json:"enabled"`         // offer cerberus_models and cerberus_ask (default true)
	Models         []string            `json:"models"`          // local models agents may use (empty: every local model)
	Tags           map[string][]string `json:"tags"`            // model → tags shown to agents, e.g. "code", "fast", "vision"
	Load           bool                `json:"load"`            // cerberus_ask may load a model that is not loaded (default false: answers "not loaded")
	MaxQueue       int                 `json:"max_queue"`       // local requests in flight before cerberus_ask answers "busy" (default 2)
	MaxTokens      int                 `json:"max_tokens"`      // longest answer of one cerberus_ask (default 1024)
	MaxFileKiB     int                 `json:"max_file_kib"`    // most of one workspace file sent with an ask (default 64)
	Guidance       string              `json:"guidance"`        // what agents should send to local models (shown with cerberus_models)
	ReferenceModel string              `json:"reference_model"` // the cloud model a delegated call is compared with for "saved" (its price), e.g. "openrouter/anthropic/claude-sonnet-4"
}

// FinetuneConfig collects examples for training a local model later (off by
// default: examples hold prompts and answers).
type FinetuneConfig struct {
	Capture      string   `json:"capture"`       // "off" (default) | "delegated" (cerberus_ask calls and their ratings) | "cloud" (also cloud chat answers through the gateway)
	Dir          string   `json:"dir"`           // where examples are kept ("" = DATA/finetune)
	MaxRowKiB    int      `json:"max_row_kib"`   // larger examples are skipped (default 256)
	TrainCommand []string `json:"train_command"` // run by `cerberus finetune train`; placeholders {dataset} {base_model} {out}
	BaseModel    string   `json:"base_model"`    // the model to train from, passed as {base_model}
}

// Finetune capture modes.
const (
	CaptureOff       = "off"
	CaptureDelegated = "delegated"
	CaptureCloud     = "cloud"
)

// DefaultGuidance is shown to agents with the local model list.
const DefaultGuidance = "Send local models the bulk, low-risk work: summaries of long files or logs, " +
	"searching and listing, boilerplate, first drafts of tests and docs, and simple rewrites. " +
	"Keep planning, review and anything risky yourself, and check what comes back before you use it."

func delegateDefaults(c *Config) {
	c.Delegate = DelegateConfig{Enabled: true, MaxQueue: 2, MaxTokens: 1024, MaxFileKiB: 64, Guidance: DefaultGuidance}
	c.Finetune = FinetuneConfig{Capture: CaptureOff, MaxRowKiB: 256}
}
