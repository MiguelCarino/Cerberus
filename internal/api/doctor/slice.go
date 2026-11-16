package doctor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/packaging"
)

// ---------- the sessions slice ----------

// sessionSliceCheck reads the properties of the slice sessions run in
// (guard.systemd_slice) and warns when it would hurt them: systemd-oomd
// ends a scope of a slice with ManagedOOMMemoryPressure=kill under
// pressure (a whole session, the agent mid-task), a MemoryMax under
// max_sessions x session_memory_max starves the sessions, and a CPU weight
// under 100 puts them behind other work. A slice with no unit file runs
// with systemd's defaults, which pass.
func sessionSliceCheck(ctx context.Context, e Env) Check {
	const id, name = "session_slice", "Sessions slice"
	if e.GOOS != "linux" {
		return ok(id, name, "not used on "+e.GOOS)
	}
	slice := e.Cfg.Guard.SystemdSlice
	if slice == "" {
		return ok(id, name, "no slice configured (see the systemd check)")
	}
	if _, err := e.LookPath("systemctl"); err != nil {
		return ok(id, name, "systemctl not found: "+slice+" not checked")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := e.Output(ctx, "systemctl", "--user", "show", slice,
		"-p", "ManagedOOMMemoryPressure", "-p", "MemoryMax", "-p", "CPUWeight")
	if err != nil {
		return ok(id, name, "could not read "+slice+" (no user manager?): not checked")
	}
	props := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, found := strings.Cut(strings.TrimSpace(line), "="); found {
			props[k] = v
		}
	}
	var probs []string
	if props["ManagedOOMMemoryPressure"] == "kill" {
		probs = append(probs, "systemd-oomd ends a session's whole scope when the slice is under memory pressure (ManagedOOMMemoryPressure=kill)")
	}
	per := packaging.MemBytes(e.Cfg.Guard.SessionMemoryMax)
	n := max(e.Cfg.Guard.MaxSessions, 1)
	if mm, err := strconv.ParseInt(props["MemoryMax"], 10, 64); err == nil && mm > 0 && per > 0 && mm < int64(n)*per {
		probs = append(probs, fmt.Sprintf("MemoryMax is %s, under %d sessions x %s", gibText(mm), n, e.Cfg.Guard.SessionMemoryMax))
	}
	if w, err := strconv.Atoi(props["CPUWeight"]); err == nil && w < 100 {
		probs = append(probs, fmt.Sprintf("CPUWeight is %d, so sessions get less CPU than other work", w))
	}
	if len(probs) == 0 {
		return ok(id, name, slice+" suits sessions")
	}
	fix := "This slice may be shared with other work or deprioritised. Run cerberus install-service to write " +
		packaging.SessionSliceName + " (sized from guard.max_sessions and guard.session_memory_max), then set guard.systemd_slice to \"" +
		packaging.SessionSliceName + "\""
	if slice == packaging.SessionSliceName {
		fix = "Run cerberus install-service again to rewrite " + packaging.SessionSliceName + " for the current guard settings, or edit its unit file"
	}
	return warn(id, name, slice+": "+strings.Join(probs, "; "), fix)
}

// gibText is a byte count in GiB with one decimal.
func gibText(n int64) string {
	return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GiB"
}
