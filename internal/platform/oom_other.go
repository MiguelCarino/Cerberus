//go:build !linux

package platform

// SetOOMScoreAdj does nothing off Linux.
func SetOOMScoreAdj(pid, adj int) error { return nil }

// CgroupUnit is unknown off Linux.
func CgroupUnit(pid int) string { return "" }

// ProcessAlive is unknown off Linux: it reports true, so callers keep
// their old behaviour.
func ProcessAlive(pid int) bool { return pid > 0 }

// ProcessName is unknown off Linux: ok is true with empty names, so callers
// keep their old behaviour.
func ProcessName(pid int) (comm, exe string, ok bool) { return "", "", pid > 0 }
