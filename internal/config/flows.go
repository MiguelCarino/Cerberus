package config

// FlowsConfig turns Flows (saved recipes of model calls and agent sessions,
// run by hand or on a schedule; see internal/flows) on or off and bounds
// how many runs execute at once. Both apply live.
type FlowsConfig struct {
	Enabled bool `json:"enabled"`  // runs may start (by hand, API, CLI or schedule); default true
	MaxRuns int  `json:"max_runs"` // runs executing at once; more wait in a queue (default 2)
}

// DefaultFlowMaxRuns is flows.max_runs when unset.
const DefaultFlowMaxRuns = 2

func flowsDefaults(c *Config) {
	c.Flows = FlowsConfig{Enabled: true, MaxRuns: DefaultFlowMaxRuns}
}
