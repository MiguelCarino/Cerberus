package config

import "regexp"

// EvalsConfig turns evals (an agent task judged against a criteria graph
// by a judge model; see internal/evals) on or off and bounds their runs.
// Everything applies at the next run (or rejudge).
type EvalsConfig struct {
	Enabled     bool   `json:"enabled"`      // runs may start; default true
	JudgeModel  string `json:"judge_model"`  // the judge ("" = engines.default_model); an eval's own judge_model wins
	MaxParallel int    `json:"max_parallel"` // sessions of one run at once (default 1)
	EvidenceKB  int    `json:"evidence_kb"`  // evidence text per judge call, KiB (default 64)
	// Personas are named voices for judge feedback (E2): name → instructions
	// (at most 2000 characters). "" is the built-in reviewer.
	Personas map[string]string `json:"personas"`
}

// Defaults of evals.*.
const (
	DefaultEvalMaxParallel = 1
	DefaultEvalEvidenceKB  = 64
)

func evalsDefaults(c *Config) {
	c.Evals = EvalsConfig{Enabled: true, MaxParallel: DefaultEvalMaxParallel, EvidenceKB: DefaultEvalEvidenceKB,
		Personas: map[string]string{}}
}

// MaxEvalPersonaRunes bounds one persona's instructions.
const MaxEvalPersonaRunes = 2000

var evalPersonaRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
