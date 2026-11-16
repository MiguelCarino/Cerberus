package doctor

import (
	"strings"
	"testing"
)

func TestSessionSliceCheck(t *testing.T) {
	for _, c := range []struct {
		name, slice, show string
		want              Status
		text, fix         string
	}{
		{"no systemctl", "agents.slice", "", OK, "not checked", ""},
		{"defaults pass", "cerberus-sessions.slice",
			"ManagedOOMMemoryPressure=auto\nMemoryMax=infinity\nCPUWeight=[not set]\n", OK, "suits sessions", ""},
		{"own slice sized", "cerberus-sessions.slice",
			"ManagedOOMMemoryPressure=auto\nMemoryMax=19327352832\nCPUWeight=100\n", OK, "suits sessions", ""},
		{"shared with oomd kill", "agents.slice",
			"ManagedOOMMemoryPressure=kill\nMemoryMax=21474836480\nCPUWeight=30\n", Warn, "ManagedOOMMemoryPressure=kill", "guard.systemd_slice"},
		{"low weight", "agents.slice",
			"ManagedOOMMemoryPressure=auto\nMemoryMax=infinity\nCPUWeight=30\n", Warn, "CPUWeight is 30", "shared"},
		{"cap under sessions", "cerberus-sessions.slice",
			"ManagedOOMMemoryPressure=auto\nMemoryMax=8589934592\nCPUWeight=100\n", Warn, "under 3 sessions x 6G", "install-service again"},
		{"no slice", "", "", OK, "no slice", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.cfg.Guard.SystemdSlice = c.slice
			if c.show != "" {
				f.path["systemctl"] = true
				f.show = c.show
			}
			got := f.run()["session_slice"]
			if got.Status != c.want || !strings.Contains(got.Detail, c.text) || !strings.Contains(got.Fix, c.fix) {
				t.Fatalf("%s %q fix %q", got.Status, got.Detail, got.Fix)
			}
		})
	}
	f := newFixture(t)
	e := f.envFor()
	e.GOOS = "darwin"
	if c := sessionSliceCheck(t.Context(), Defaults(e)); c.Status != OK || !strings.Contains(c.Detail, "darwin") {
		t.Fatalf("darwin %+v", c)
	}
}
