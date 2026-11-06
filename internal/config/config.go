// Package config holds Cerberus' configuration: the types every other package
// reads, their defaults, and load/save of ~/.config/cerberus/config.json.
//
// Day phase 4 adds the schema (schema.go, schema_meta.go: every setting with
// label, help, group, level, kind and limits), Get/Set/SetAll/Reset by JSON
// path (path.go), Validate (validate.go), Clone/Diff for live reload
// (clone.go) and profile bundles, Export/Import (bundle.go).
//
// FROZEN CONTRACT: other packages are written against these types. Add fields
// only; never rename or remove one without updating every user.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// Config is the whole configuration file.
type Config struct {
	Server       ServerConfig     `json:"server"`
	GPU          GPUConfig        `json:"gpu"`
	Engines      EnginesConfig    `json:"engines"`
	Models       []ModelConfig    `json:"models"`    // explicit local models (llamacpp) and overrides for discovered ones
	Providers    []ProviderConfig `json:"providers"` // cloud / remote OpenAI-compatible APIs (API keys, never subscriptions)
	Agents       []AgentProfile   `json:"agents"`    // merged over DefaultAgents() by Name
	DefaultAgent string           `json:"default_agent"`
	Agent        AgentConfig      `json:"agent"` // the built-in agent
	Context      ContextConfig    `json:"context"`
	Repos        []RepoConfig     `json:"repos"`
	RepoRoots    []string         `json:"repo_roots"` // folders whose direct git children are offered as repos
	Sessions     SessionsConfig   `json:"sessions"`
	Guard        GuardConfig      `json:"guard"`
	Notify       NotifyConfig     `json:"notify"`
	Git          GitConfig        `json:"git"`

	// Phase 2 (see phase2.go for the types and defaults).
	Vault         VaultConfig          `json:"vault"`
	Voice         VoiceConfig          `json:"voice"`
	Prices        map[string]Price     `json:"prices"`        // model id (or "<provider>/*") → USD per million tokens
	Budgets       []Budget             `json:"budgets"`       // spend caps per provider and period
	Subscriptions []SubscriptionConfig `json:"subscriptions"` // consumer plans used through official CLIs (tracked, never proxied)

	// Monitor phase (see monitor.go).
	Monitor MonitorConfig `json:"monitor"`
	Tariff  TariffConfig  `json:"tariff"`

	// Day phase 2 (see files.go).
	Files FilesConfig `json:"files"`

	// Day phase 3 (see safety.go).
	Sandbox SandboxConfig `json:"sandbox"`

	// Day phase 5 (see chat.go).
	Chat     ChatConfig     `json:"chat"`
	Delegate DelegateConfig `json:"delegate"` // agents hand small jobs to local models
	Finetune FinetuneConfig `json:"finetune"` // examples collected for training a local model

	// Flows phase (see flows.go).
	Flows FlowsConfig `json:"flows"`

	// Evals phase (see evals.go).
	Evals EvalsConfig `json:"evals"`

	// Users and roles, Phase 4a (see users.go).
	Users UsersConfig `json:"users"`

	// Gateway phase (see gateway.go).
	Gateway GatewayConfig `json:"gateway"`

	// path is where this config was loaded from (not serialised).
	path string
}

type ServerConfig struct {
	Listen      string   `json:"listen"`        // default "127.0.0.1:7340"
	ExtraListen []string `json:"extra_listen"`  // e.g. "100.x.y.z:7340" (Tailscale)
	Token       string   `json:"token"`         // generated on first run
	LocalNoAuth bool     `json:"local_no_auth"` // /v1 from loopback needs no token (desktop clients)
	PublicURL   string   `json:"public_url"`    // used in notifications/links, e.g. "http://desk.tailnet.ts.net:7340"
	TLSCert     string   `json:"tls_cert"`      // PEM cert (e.g. from `tailscale cert`); with TLSKey, ExtraListen serve HTTPS
	TLSKey      string   `json:"tls_key"`

	// Day phase 3 (safety.go).
	AllowPublic  bool      `json:"allow_public"`           // allow listening on a public (non-private) IP
	AllowedHosts []string  `json:"allowed_hosts"`          // extra Host header values accepted (DNS-rebinding guard)
	TokenCreated time.Time `json:"token_created,omitzero"` // when Token was made (first run or rotation); doctor's token age

	// Day phase 4.
	InitDone bool `json:"init_done"` // first-run setup finished (`cerberus init` or the web welcome); the UI shows the welcome while false

	// Work previews (2026-11-03): "7350-7369" gives each open preview its own
	// origin on a port of this range, bound on the hosts of listen and
	// extra_listen; "" keeps previews under /preview/<id>/ (static only).
	PreviewPorts string `json:"preview_ports"`
}

type GPUConfig struct {
	Backend          string `json:"backend"`            // "auto" | "nvidia" | "amd" | "none"
	ActiveIntervalMs int    `json:"active_interval_ms"` // default 1000
	IdleIntervalMs   int    `json:"idle_interval_ms"`   // default 15000
	NvidiaSmi        string `json:"nvidia_smi"`         // default "nvidia-smi"
}

type EnginesConfig struct {
	DefaultModel    string `json:"default_model"`
	IdleTTLSec      int    `json:"idle_ttl_sec"`     // default 600
	LoadTimeoutSec  int    `json:"load_timeout_sec"` // default 180
	AllowOverCommit bool   `json:"allow_overcommit"`
	WaitForRoomSec  int    `json:"wait_for_room_sec"` // default 600; 0 = never wait for busy models
	// StallSec (gateway phase G5): a local request with no upstream byte for
	// this long gets its engine probed; a probe that fails twice marks the
	// engine stalled (requests fail with 503, the model is reloaded).
	// Default 120; 0 = off.
	StallSec int            `json:"stall_sec"`
	LlamaCpp LlamaCppConfig `json:"llamacpp"`
	LMStudio LMStudioConfig `json:"lmstudio"`
	Ollama   OllamaConfig   `json:"ollama"`
}

type LlamaCppConfig struct {
	Enabled   bool     `json:"enabled"`    // default true (no-op when Binary missing)
	Binary    string   `json:"binary"`     // default "llama-server"
	ModelDirs []string `json:"model_dirs"` // scanned for *.gguf
	Ctx       int      `json:"ctx"`        // default 32768
	Parallel  int      `json:"parallel"`   // default 4 (slots; concurrent agents share the model)
	Args      []string `json:"args"`       // extra args for every model
	PortBase  int      `json:"port_base"`  // default 39200
}

type LMStudioConfig struct {
	Enabled bool   `json:"enabled"`  // default true (no-op when lms missing)
	Binary  string `json:"binary"`   // default "lms" (also tries ~/.lmstudio/bin/lms)
	BaseURL string `json:"base_url"` // default "http://127.0.0.1:1234/v1"
	Ctx     int    `json:"ctx"`      // default 32768
	GPU     string `json:"gpu"`      // "" (auto split) | "max" | "off" | "0.x": passed to lms load --gpu
}

type OllamaConfig struct {
	Enabled bool   `json:"enabled"` // default true (no-op when unreachable)
	URL     string `json:"url"`     // default "http://127.0.0.1:11434"
}

// ModelConfig declares a local model or overrides a discovered one (matched by ID).
type ModelConfig struct {
	ID       string   `json:"id"`
	Backend  string   `json:"backend"` // "llamacpp" | "lmstudio" | "ollama"
	Path     string   `json:"path"`    // llamacpp: .gguf path
	Name     string   `json:"name"`    // backend-native name if different from ID
	Ctx      int      `json:"ctx"`
	Parallel int      `json:"parallel"`
	TTLSec   int      `json:"ttl_sec"`
	SizeMiB  int      `json:"size_mib"` // VRAM estimate override
	Args     []string `json:"args"`
	Vision   bool     `json:"vision"`
	Draft    string   `json:"draft"` // llamacpp: draft model path for speculative decoding (-md)
	// MaxParallel caps the requests Cerberus sends this model at once; the
	// rest wait in a FIFO queue (0 = automatic, see engine.Manager).
	MaxParallel int `json:"max_parallel"`
	// Defaults are the request settings the gateway applies when a request
	// names none of its own (gateway phase G4): agent harnesses send no
	// thinking control to a custom endpoint, so this is what they run at.
	Defaults ModelDefaults `json:"defaults,omitzero"`
}

// ModelDefaults are per-model request defaults (gateway phase G4). Zero
// values and nil pointers mean "no default".
type ModelDefaults struct {
	ReasoningEffort string   `json:"reasoning_effort,omitempty"` // none | minimal | low | medium | high | xhigh
	EnableThinking  *bool    `json:"enable_thinking,omitempty"`  // sent as chat_template_kwargs.enable_thinking
	MaxTokens       int      `json:"max_tokens,omitempty"`       // budget when the request sends none
	MaxTokensCap    int      `json:"max_tokens_cap,omitempty"`   // larger budgets (and -1) are clipped to this
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            int      `json:"top_k,omitempty"`
}

// ReasoningEfforts are the reasoning_effort values the gateway passes on.
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

type ProviderConfig struct {
	Name      string            `json:"name"`     // id prefix, e.g. "anthropic", "openai", "openrouter"
	BaseURL   string            `json:"base_url"` // ends in /v1 (or provider's OpenAI-compatible root)
	KeyEnv    string            `json:"key_env"`  // env var holding the key; else secrets.json[name]
	Models    []string          `json:"models"`   // empty = discover via GET /models
	Headers   map[string]string `json:"headers"`
	Enabled   bool              `json:"enabled"`
	KeySecret string            `json:"key_secret"` // vault secret name for the key (default: Name)
	// APIs the provider serves natively beside Chat Completions ("messages",
	// "responses"): the gateway passes those requests through untranslated.
	APIs []string `json:"apis,omitempty"`
}

// AgentProfile is how to start one kind of coding agent in a PTY.
// Command placeholders: {task} {task_file} {model} {base_url} {cwd} {session} {self} ({self} = path of the cerberus binary).
type AgentProfile struct {
	Name          string            `json:"name"`
	Command       []string          `json:"command"`
	Env           map[string]string `json:"env"`
	NeedsModel    bool              `json:"needs_model"` // shows the model picker
	Interactive   bool              `json:"interactive"` // stays open after finishing (idle → waiting)
	Description   string            `json:"description"`
	ResumeCommand []string          `json:"resume_command"` // how to continue a finished session in place (same placeholders + {resume_file})
	SecretEnv     map[string]string `json:"secret_env"`     // env var → vault secret name, injected into this agent's sessions
	Sandbox       string            `json:"sandbox"`        // "" (Sandbox.Default) | "none" | "bwrap" (day phase 3)
	SandboxRW     []string          `json:"sandbox_rw"`     // extra read-write binds for this agent under bwrap (e.g. ~/.claude)
}

type AgentConfig struct {
	Approve         string       `json:"approve"`   // "ask" (default) | "auto"
	MaxSteps        int          `json:"max_steps"` // default 60
	DenyPaths       []string     `json:"deny_paths"`
	CommandTimeoutS int          `json:"command_timeout_s"` // default 120
	Spawn           SpawnConfig  `json:"spawn"`             // agents starting sessions (spawn.go)
	Policy          PolicyConfig `json:"policy"`            // run_command guard rail (safety.go)
}

type ContextConfig struct {
	Files           []string `json:"files"`              // extra context for the built-in agent (e.g. memory index)
	MaxBytesPerFile int      `json:"max_bytes_per_file"` // default 16384
}

type RepoConfig struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Checks    []string          `json:"checks"`
	Preview   *PreviewConfig    `json:"preview,omitempty"`
	Push      bool              `json:"push"`       // default for "push" on accept
	SecretEnv map[string]string `json:"secret_env"` // env var → vault secret name, for sessions on this repo
	Sandbox   string            `json:"sandbox"`    // overrides the agent profile's sandbox for this repo (day phase 3)
}

type PreviewConfig struct {
	Dir string `json:"dir"` // relative to the worktree; "." = root
	// Command starts a preview server ("Run preview") in the worktree, e.g.
	// "npm run dev -- --port {port} --host 127.0.0.1"; {port} and $PORT
	// carry the port Cerberus picked. "" = detected from package.json.
	Command string `json:"command,omitempty"`
}

type SessionsConfig struct {
	MaxAttempts       int `json:"max_attempts"`        // default 2
	ChecksTimeoutSec  int `json:"checks_timeout_sec"`  // default 600
	IdleWaitSec       int `json:"idle_wait_sec"`       // default 20
	ScrollbackMiB     int `json:"scrollback_mib"`      // default 8
	ScrollbackTailKiB int `json:"scrollback_tail_kib"` // default 256
	DefaultCols       int `json:"default_cols"`        // default 120
	DefaultRows       int `json:"default_rows"`        // default 36
	RetentionDays     int `json:"retention_days"`      // finished sessions older than this are pruned (default 30; 0 keeps all). Repo worktrees with unaccepted work are kept.
	RecordMaxMiB      int `json:"record_max_mib"`      // asciicast recording per run, capped at this size (default 16; 0 = off)
	PreviewIdleMin    int `json:"preview_idle_min"`    // a preview server or origin nobody used this long is stopped (default 30)
}

type GuardConfig struct {
	MaxSessions      int    `json:"max_sessions"`       // default 3
	MinFreeMiB       int    `json:"min_free_mib"`       // default 4096
	SystemdSlice     string `json:"systemd_slice"`      // default "agents.slice" (used only if systemd-run exists)
	SessionMemoryMax string `json:"session_memory_max"` // default "6G"
}

type NotifyConfig struct {
	Command  []string `json:"command"` // argv with {title} {body} {urgency}; default Atrium notify if present
	NtfyURL  string   `json:"ntfy_url"`
	Disabled bool     `json:"disabled"`
}

type GitConfig struct {
	Name  string `json:"name"` // commit identity on accept; empty = repo git config
	Email string `json:"email"`
	// Accept never adds attribution trailers. There is deliberately no option for it.
}

// Defaults returns a config with every default filled in (no token).
func Defaults() *Config {
	home, _ := os.UserHomeDir()
	c := &Config{
		Server: ServerConfig{Listen: "127.0.0.1:7340", LocalNoAuth: true},
		GPU:    GPUConfig{Backend: "auto", ActiveIntervalMs: 1000, IdleIntervalMs: 15000, NvidiaSmi: "nvidia-smi"},
		Engines: EnginesConfig{
			IdleTTLSec: 600, LoadTimeoutSec: 180, WaitForRoomSec: 600, StallSec: 120,
			LlamaCpp: LlamaCppConfig{Enabled: true, Binary: "llama-server", Ctx: 32768, Parallel: 4, PortBase: 39200,
				ModelDirs: []string{filepath.Join(home, "models")}},
			LMStudio: LMStudioConfig{Enabled: true, Binary: "lms", BaseURL: "http://127.0.0.1:1234/v1", Ctx: 32768},
			Ollama:   OllamaConfig{Enabled: true, URL: "http://127.0.0.1:11434"},
		},
		DefaultAgent: "cerberus",
		Agent: AgentConfig{Approve: "ask", MaxSteps: 60, CommandTimeoutS: 120,
			DenyPaths: []string{"**/Carino-Systems-Business/**", "**/.ssh/**", "**/.gnupg/**", "**/secrets.json"}},
		Context:   ContextConfig{MaxBytesPerFile: 16384},
		RepoRoots: []string{filepath.Join(home, "Github")},
		Sessions: SessionsConfig{MaxAttempts: 2, ChecksTimeoutSec: 600, IdleWaitSec: 20, ScrollbackMiB: 8, PreviewIdleMin: 30,
			ScrollbackTailKiB: 256, DefaultCols: 120, DefaultRows: 36, RecordMaxMiB: 16},
		Guard: GuardConfig{MaxSessions: 3, MinFreeMiB: 4096, SystemdSlice: "agents.slice", SessionMemoryMax: "6G"},
	}
	if p := filepath.Join(home, ".config", "hypr", "scripts", "notify"); fileExists(p) {
		c.Notify.Command = []string{p, "{title}", "{body}"}
	}
	phase2Defaults(c)
	spawnDefaults(c)
	filesDefaults(c)
	safetyDefaults(c)
	monitorDefaults(c)
	chatDefaults(c)
	delegateDefaults(c)
	flowsDefaults(c)
	evalsDefaults(c)
	usersDefaults(c)
	return c
}

// DefaultAgents are the built-in profiles; config Agents override by Name.
func DefaultAgents() []AgentProfile {
	return []AgentProfile{
		{Name: "cerberus", Command: []string{"{self}", "agent", "--base", "{base_url}", "--model", "{model}", "--task-file", "{task_file}", "--cwd", "{cwd}", "--session", "{session}"},
			ResumeCommand: []string{"{self}", "agent", "--base", "{base_url}", "--model", "{model}", "--task-file", "{task_file}", "--cwd", "{cwd}", "--session", "{session}", "--resume", "{resume_file}"},
			NeedsModel:    true, Description: "Built-in agent: any local or API model through the gateway"},
		{Name: "claude", Command: []string{"claude", "{task}"}, Interactive: true, ResumeCommand: []string{"claude", "--continue"},
			SandboxRW:   []string{"~/.claude", "~/.claude.json"},
			Description: "Claude Code, interactive, on your own subscription"},
		{Name: "claude-headless", Command: []string{"claude", "-p", "{task}", "--permission-mode", "acceptEdits"},
			SandboxRW:   []string{"~/.claude", "~/.claude.json"},
			Description: "Claude Code, non-interactive (edits accepted, commands per your settings)"},
		{Name: "claude-local", Command: []string{"claude", "{task}"}, Interactive: true, NeedsModel: true,
			ResumeCommand: []string{"claude", "--continue"},
			Env: map[string]string{
				"ANTHROPIC_BASE_URL": "{server_url}", "ANTHROPIC_AUTH_TOKEN": "{api_key}", "ANTHROPIC_API_KEY": "",
				"ANTHROPIC_MODEL": "{model}", "ANTHROPIC_SMALL_FAST_MODEL": "{model}", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "{model}",
				"CLAUDE_CODE_MAX_OUTPUT_TOKENS": "32000", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"},
			SandboxRW:   []string{"~/.claude", "~/.claude.json"},
			Description: "Claude Code against a local or API model through the gateway (no subscription)"},
		{Name: "codex", NeedsModel: true, SandboxRW: []string{"~/.codex"},
			Command: []string{"codex", "exec", "--full-auto",
				"-c", `model_provider="cerberus"`,
				"-c", `model_providers.cerberus={name="Cerberus",base_url="{base_url}",env_key="OPENAI_API_KEY",wire_api="responses"}`,
				"-m", "{model}", "{task}"},
			Description: "Codex CLI against a local or API model through the gateway (Responses API)"},
		{Name: "opencode", Command: []string{"opencode", "run", "{task}"}, NeedsModel: true,
			Description: "OpenCode (point it at the gateway via OPENAI_BASE_URL)"},
		{Name: "aider", Command: []string{"aider", "--model", "openai/{model}", "--message", "{task}", "--yes-always"}, NeedsModel: true,
			Description: "Aider through the gateway"},
		{Name: "shell", Command: []string{"bash", "-l"}, Interactive: true, Description: "A plain shell in the worktree"},
	}
}

// AgentProfiles merges DefaultAgents with c.Agents (config wins by Name, new names appended).
func (c *Config) AgentProfiles() []AgentProfile {
	out := DefaultAgents()
	for _, a := range c.Agents {
		replaced := false
		for i := range out {
			if out[i].Name == a.Name {
				out[i] = a
				replaced = true
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out
}

// Agent finds a profile by name.
func (c *Config) AgentProfile(name string) (AgentProfile, bool) {
	for _, a := range c.AgentProfiles() {
		if a.Name == name {
			return a, true
		}
	}
	return AgentProfile{}, false
}

// IdleTTL for a model config (0 = engine default).
func (c *Config) IdleTTL(m *ModelConfig) time.Duration {
	if m != nil && m.TTLSec > 0 {
		return time.Duration(m.TTLSec) * time.Second
	}
	return time.Duration(c.Engines.IdleTTLSec) * time.Second
}

// Path returns where the config lives.
func (c *Config) Path() string { return c.path }

// DefaultPath is os.UserConfigDir()/cerberus/config.json.
func DefaultPath() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "cerberus", "config.json"), nil
}

// Dir is the folder holding config.json and secrets.json.
func (c *Config) Dir() string { return filepath.Dir(c.path) }

// Load reads path (DefaultPath if empty) over Defaults(). A missing file is
// created with defaults and a fresh token (mode 0600). A config without a
// token gets one and is saved.
func Load(path string) (*Config, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	c := Defaults()
	c.path = path
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// first run
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		c.path = path
		// Migration (day phase 4): a config written before init_done existed
		// belongs to an install that is already set up, so it skips the
		// first-run welcome. Only a freshly created config starts false.
		if !hasInitDone(raw) {
			c.Server.InitDone = true
		}
	}
	if c.Server.Token == "" || errors.Is(err, fs.ErrNotExist) {
		if c.Server.Token == "" {
			c.SetToken(NewToken())
		}
		if err := c.Save(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Save writes the config atomically with mode 0600.
func (c *Config) Save() error {
	if c.path == "" {
		p, err := DefaultPath()
		if err != nil {
			return err
		}
		c.path = p
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tokenMu.RLock() // a concurrent SetToken must not tear the marshal
	raw, err := json.MarshalIndent(c, "", "  ")
	tokenMu.RUnlock()
	if err != nil {
		return err
	}
	// A fresh temp file (0600 whatever an old file there had), renamed
	// into place: the config holds the admin token.
	return platform.WritePrivate(c.path, append(raw, '\n'))
}

// SetPath sets where Save writes (tests).
func (c *Config) SetPath(p string) { c.path = p }

// Secret returns secrets.json[name] from the config dir ("" if absent).
func (c *Config) Secret(name string) string {
	raw, err := os.ReadFile(filepath.Join(c.Dir(), "secrets.json"))
	if err != nil {
		return ""
	}
	m := map[string]string{}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m[name]
}

// NewToken returns 32 random bytes as hex.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Parse reads config file contents the way Load does (defaults first, then
// the file, then the init_done migration) without touching the file: no
// token is created and nothing is saved. The result's path is path.
func Parse(path string, raw []byte) (*Config, error) {
	c := Defaults()
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.path = path
	if !hasInitDone(raw) {
		c.Server.InitDone = true
	}
	return c, nil
}

// HasInitDone reports whether config file contents set server.init_done
// (a file without it predates day phase 4 and Load reads it as true).
func HasInitDone(raw []byte) bool { return hasInitDone(raw) }

// hasInitDone reports whether a config file sets server.init_done.
func hasInitDone(raw []byte) bool {
	var doc struct {
		Server map[string]json.RawMessage `json:"server"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	_, ok := doc.Server["init_done"]
	return ok
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ParsePortRange reads server.preview_ports ("7350-7369", or one port).
// ok is false for "" (no per-origin previews).
func ParsePortRange(s string) (lo, hi int, ok bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false, nil
	}
	a, b, found := strings.Cut(s, "-")
	if !found {
		b = a
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || lo < 1024 || hi > 65535 || lo > hi {
		return 0, 0, false, fmt.Errorf("%q is not a port range like 7350-7369 (ports 1024 to 65535)", s)
	}
	if hi-lo+1 > 200 {
		return 0, 0, false, fmt.Errorf("%q holds %d ports; at most 200", s, hi-lo+1)
	}
	return lo, hi, true, nil
}
