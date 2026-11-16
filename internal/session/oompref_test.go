//go:build !windows

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// Session scopes ask systemd-oomd to pick another cgroup first
// (web-api-ops-3), but only on a systemd that knows the property: an older
// one would refuse it and the scope would not start.
func TestScopeOOMPreference(t *testing.T) {
	for _, c := range []struct {
		version string
		want    bool
	}{
		{"systemd 255 (255.4-1.fc40)\n+PAM +AUDIT", true},
		{"systemd 248 (248)", true},
		{"systemd 245 (245.4-4ubuntu3)", false},
		{"", false},
	} {
		t.Run(c.version, func(t *testing.T) {
			var calls string
			e := newTestEnv(t, nil, func(e *testEnv) {
				calls = fakeSystemdRun(e, "while [ \"$1\" != -- ]; do shift; done; shift\nexec \"$@\"\n")
				sc := "#!/bin/bash\n[ \"$1\" = --version ] && printf '%s\\n' '" + c.version + "'\nexit 0\n"
				if err := os.WriteFile(filepath.Join(e.home, "fakebin", "systemctl"), []byte(sc), 0o755); err != nil {
					t.Fatal(err)
				}
				e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo scoped"}})
			})
			id := e.scratch("ok", nil)
			e.waitState(id, StatePassed)
			raw, _ := os.ReadFile(calls)
			if got := strings.Contains(string(raw), "-p ManagedOOMPreference=avoid"); got != c.want {
				t.Fatalf("avoid %v, want %v:\n%s", got, c.want, raw)
			}
			if !strings.Contains(string(raw), "MemoryMax=6G") {
				t.Fatalf("calls:\n%s", raw)
			}
		})
	}
}

func TestSystemdVersion(t *testing.T) {
	for in, want := range map[string]int{"systemd 256 (256.7)\n+PAM": 256, "systemd 248": 248, "nope 3": 0, "": 0, "systemd x": 0} {
		if got := systemdVersion(in); got != want {
			t.Errorf("systemdVersion(%q) = %d, want %d", in, got, want)
		}
	}
}
