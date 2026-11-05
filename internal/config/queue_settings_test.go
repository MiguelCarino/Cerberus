package config

import "testing"

func TestQueueSettings(t *testing.T) {
	c := Defaults()
	if c.Engines.WaitForRoomSec != 600 || c.Engines.LMStudio.GPU != "" {
		t.Fatalf("defaults: wait %d gpu %q", c.Engines.WaitForRoomSec, c.Engines.LMStudio.GPU)
	}
	for _, tc := range []struct {
		gpu string
		bad bool
	}{{"", false}, {"max", false}, {"off", false}, {"0.75", false}, {"1", false}, {"1.5", true}, {"half", true}} {
		c := Defaults()
		c.Engines.LMStudio.GPU = tc.gpu
		if got := hasProblem(c.Validate(), "engines.lmstudio.gpu", LevelError, ""); got != tc.bad {
			t.Errorf("gpu %q: problem %v, want %v", tc.gpu, got, tc.bad)
		}
	}
	c = Defaults()
	c.Models = []ModelConfig{{ID: "m", MaxParallel: -1}}
	if !hasProblem(c.Validate(), "models[0].max_parallel", LevelError, "") {
		t.Error("negative max_parallel accepted")
	}
}
