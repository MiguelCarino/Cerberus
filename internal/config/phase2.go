package config

import (
	"os"
	"path/filepath"
)

// VaultConfig selects where secrets (provider keys, tokens for agents) live.
type VaultConfig struct {
	// "auto" (default): Secret Service via secret-tool when a provider answers,
	// else the encrypted file. "secret-service" | "file" force one.
	Backend string `json:"backend"`
}

// VoiceConfig turns recorded speech into task text, on this machine only.
type VoiceConfig struct {
	Enabled bool `json:"enabled"` // default true (no-op when no backend is found)
	// "auto" (default): Atrium's voice.py if present, else whisper-cli, else
	// an OpenAI-compatible URL when set, else off. "command" | "openai" | "off".
	Backend    string   `json:"backend"`
	Command    []string `json:"command"`     // argv; {wav} = 16 kHz mono WAV path; prints text or {"text":...}
	URL        string   `json:"url"`         // OpenAI-compatible base (…/v1) with /audio/transcriptions
	Model      string   `json:"model"`       // for URL backends, e.g. "whisper-1" / "large-v3-turbo"
	Language   string   `json:"language"`    // "" = detect
	FFmpeg     string   `json:"ffmpeg"`      // default "ffmpeg"
	MaxSeconds int      `json:"max_seconds"` // default 300
}

// Price is USD per million tokens for one model.
type Price struct {
	In       float64 `json:"in"`
	Out      float64 `json:"out"`
	CachedIn float64 `json:"cached_in"`
}

// Budget caps API spend for one provider ("*" = all cloud providers).
type Budget struct {
	Provider string  `json:"provider"`
	Period   string  `json:"period"` // "day" | "month"
	SoftUSD  float64 `json:"soft_usd"`
	HardUSD  float64 `json:"hard_usd"` // 0 = no hard cap; past it the gateway refuses with 429
}

// SubscriptionConfig describes a consumer plan that Cerberus only observes:
// the sessions it runs with the plan's official CLI, and the CLI's own local
// usage records. Plan limits are not published exactly, so the limits here are
// the user's own estimates, shown as a gauge, never enforced.
type SubscriptionConfig struct {
	Name          string   `json:"name"`            // e.g. "claude"
	Kind          string   `json:"kind"`            // "claude-code" (reads its local JSONL transcripts)
	Agents        []string `json:"agents"`          // profiles that consume it, e.g. ["claude","claude-headless"]
	DataDir       string   `json:"data_dir"`        // default ~/.claude/projects
	Window5hLimit int      `json:"window_5h_limit"` // estimated tokens per rolling 5 h (0 = unknown)
	WeeklyLimit   int      `json:"weekly_limit"`    // estimated tokens per rolling 7 days (0 = unknown)
	AllSessions   bool     `json:"all_sessions"`    // count every CLI session on this machine, not only Cerberus'
}

func phase2Defaults(c *Config) {
	home, _ := os.UserHomeDir()
	c.Vault = VaultConfig{Backend: "auto"}
	c.Voice = VoiceConfig{Enabled: true, Backend: "auto", FFmpeg: "ffmpeg", MaxSeconds: 300}
	c.Sessions.RetentionDays = 30
	c.Subscriptions = []SubscriptionConfig{{
		Name: "claude", Kind: "claude-code", Agents: []string{"claude", "claude-headless"},
		DataDir: filepath.Join(home, ".claude", "projects"), AllSessions: true,
	}}
}
