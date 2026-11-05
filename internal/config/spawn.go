package config

// SpawnConfig limits agents that start sessions of their own (through
// `cerberus mcp` or the built-in agent's cerberus_* tools).
type SpawnConfig struct {
	Enabled         bool `json:"enabled"`          // default true
	MaxDepth        int  `json:"max_depth"`        // levels below a person's session (default 2)
	MaxChildren     int  `json:"max_children"`     // open children per session (default 4)
	MaxTotal        int  `json:"max_total"`        // open spawned sessions per tree (default 8)
	RequireApproval bool `json:"require_approval"` // children wait in the queue until a person approves (default false)
}

func spawnDefaults(c *Config) {
	c.Agent.Spawn = SpawnConfig{Enabled: true, MaxDepth: 2, MaxChildren: 4, MaxTotal: 8}
}
