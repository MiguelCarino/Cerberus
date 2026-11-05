package config

import "testing"

// lms load --gpu takes max, off or a number from 0 to 1; NaN parses as a
// float but is none of those, and lms would refuse every load.
func TestLMStudioGPURejectsNaN(t *testing.T) {
	for _, g := range []string{"NaN", "nan", "-0.1", "Inf"} {
		c := Defaults()
		c.Engines.LMStudio.GPU = g
		if !hasProblem(c.Validate(), "engines.lmstudio.gpu", LevelError, "") {
			t.Errorf("gpu %q accepted", g)
		}
	}
}
