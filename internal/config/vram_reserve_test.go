package config

import (
	"encoding/json"
	"testing"
)

func TestVRAMReserveSetting(t *testing.T) {
	c := Defaults()
	if c.Engines.VRAMReserveMiB != 0 {
		t.Fatalf("default %d, want 0 (auto)", c.Engines.VRAMReserveMiB)
	}
	for _, tc := range []struct {
		v   int
		bad bool
	}{{0, false}, {-1, false}, {2048, false}, {-2, true}} {
		c := Defaults()
		c.Engines.VRAMReserveMiB = tc.v
		if got := hasProblem(c.Validate(), "engines.vram_reserve_mib", LevelError, ""); got != tc.bad {
			t.Errorf("%d: problem %v, want %v", tc.v, got, tc.bad)
		}
	}
	var e EnginesConfig
	if err := json.Unmarshal([]byte(`{"vram_reserve_mib": 1500}`), &e); err != nil || e.VRAMReserveMiB != 1500 {
		t.Fatalf("json: %v %d", err, e.VRAMReserveMiB)
	}
}
