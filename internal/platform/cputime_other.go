//go:build !linux

package platform

import "time"

// ProcessCPUTime is unknown off Linux.
func ProcessCPUTime(pid int) (time.Duration, bool) { return 0, false }
