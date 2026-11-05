package config

// ChatConfig controls the web LLM chat (day phase 5): conversations run
// server-side with the built-in agent's tools, or through the claude CLI.
type ChatConfig struct {
	Enabled         bool   `json:"enabled"`           // the Chat view and /api/chats (default true)
	QuickModel      string `json:"quick_model"`       // model for quick answers ("" = the default model)
	DefaultMode     string `json:"default_mode"`      // ask | auto | acceptEdits | plan (default "ask")
	MaxSteps        int    `json:"max_steps"`         // tool rounds per message (default 12)
	ResultClip      int    `json:"result_clip"`       // characters of one tool result the model gets (default 12000)
	CommandTimeoutS int    `json:"command_timeout_s"` // run_command's default timeout (default 120, at most 600)
	AtriumImport    bool   `json:"atrium_import"`     // list Atrium's desktop chat sessions for import (default true)
}

// ChatModes are the permission modes a chat can run in.
var ChatModes = []string{"ask", "auto", "acceptEdits", "plan"}

func chatDefaults(c *Config) {
	c.Chat = ChatConfig{Enabled: true, DefaultMode: "ask", MaxSteps: 12, ResultClip: 12000,
		CommandTimeoutS: 120, AtriumImport: true}
}
