package packaging

import (
	"os"
	"strings"
	"testing"
)

func TestUnit(t *testing.T) {
	for _, want := range []string{
		"Type=simple",
		"ExecStart=%h/.local/bin/cerberus serve",
		"Restart=on-failure",
		"Slice=app.slice",
		"KillMode=mixed",
		"TimeoutStopSec=60",
		"OOMPolicy=continue",
		"ManagedOOMPreference=avoid",
		"WantedBy=default.target",
	} {
		if !strings.Contains(Unit, want+"\n") {
			t.Errorf("unit lacks %q", want)
		}
	}
	if strings.Contains(Unit, "MemoryMax") {
		t.Error("the daemon itself must not have a MemoryMax")
	}
	// The shutdown comment says what KillMode=mixed really does.
	if strings.Contains(Unit, "Anything left after the timeout is killed") ||
		!strings.Contains(Unit, "SIGKILLs whatever is left in this unit's") {
		t.Error("the KillMode comment does not describe mixed mode")
	}
}

// The container files give the daemon time to stop and an init to reap
// orphans (web-api-ops-9).
func TestContainerStop(t *testing.T) {
	for file, wants := range map[string][]string{
		"container/cerberus.container": {"RunInit=true", "StopTimeout=60", "TimeoutStopSec=90"},
		"container/compose.yaml":       {"init: true", "stop_grace_period: 60s"},
		"../Containerfile":             {"STOPSIGNAL SIGTERM"},
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
}

// The sessions slice (web-api-ops-3): its own caps, never an oomd kill on
// pressure, a normal CPU weight; MemoryMax follows the config.
func TestSessionSlice(t *testing.T) {
	if SessionSliceFor(3, "6G") != SessionSlice {
		t.Fatalf("the shipped slice is not the 3 x 6G one:\n%s", SessionSliceFor(3, "6G"))
	}
	for _, want := range []string{"[Slice]", "MemoryMax=18432M", "MemoryHigh=16588M", "CPUWeight=100"} {
		if !strings.Contains(SessionSlice, want+"\n") {
			t.Errorf("slice lacks %q", want)
		}
	}
	for _, l := range strings.Split(SessionSlice, "\n") {
		if strings.HasPrefix(l, "ManagedOOMMemoryPressure=") {
			t.Errorf("slice sets %q", l)
		}
	}
	s := SessionSliceFor(4, "512M")
	if !strings.Contains(s, "MemoryMax=2048M\n") || !strings.Contains(s, "MemoryHigh=1843M\n") {
		t.Errorf("4 x 512M:\n%s", s)
	}
	for _, per := range []string{"", "infinity", "50%", "lots"} {
		if s := SessionSliceFor(3, per); strings.Contains(s, "MemoryMax=") || strings.Contains(s, "MemoryHigh=") || !strings.Contains(s, "CPUWeight=100") {
			t.Errorf("%q:\n%s", per, s)
		}
	}
	for in, want := range map[string]int64{"6G": 6 << 30, "512M": 512 << 20, "1.5G": 3 << 29, "1024": 1024, "2gb": 2 << 30,
		"infinity": 0, "": 0, "10%": 0, "x": 0} {
		if got := MemBytes(in); got != want {
			t.Errorf("MemBytes(%q) = %d, want %d", in, got, want)
		}
	}
}
