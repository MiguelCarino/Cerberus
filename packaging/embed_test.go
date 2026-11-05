package packaging

import (
	"strings"
	"testing"
)

func TestUnit(t *testing.T) {
	for _, want := range []string{
		"Type=simple",
		"ExecStart=%h/.local/bin/cerberus serve",
		"Restart=on-failure",
		"Slice=app.slice",
		"WantedBy=default.target",
	} {
		if !strings.Contains(Unit, want+"\n") {
			t.Errorf("unit lacks %q", want)
		}
	}
	if strings.Contains(Unit, "MemoryMax") {
		t.Error("the daemon itself must not have a MemoryMax")
	}
}
