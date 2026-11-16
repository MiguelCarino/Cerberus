//go:build linux

package platform

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// userHZ is the kernel's USER_HZ, the unit of /proc/<pid>/stat times. It
// is 100 on every Linux build Cerberus targets; without cgo it cannot be
// asked for, and only differences between two readings are used.
const userHZ = 100

// ProcessCPUTime is the user plus system CPU time process pid has used so
// far. ok is false when it cannot be read (a gone pid).
func ProcessCPUTime(pid int) (time.Duration, bool) { return processCPUTime("/proc", pid) }

func processCPUTime(procRoot string, pid int) (time.Duration, bool) {
	if pid <= 0 {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	// The command name (field 2) is in parentheses and may hold spaces:
	// count fields from the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); utime and stime are fields 14 and 15.
	if len(f) < 13 {
		return 0, false
	}
	ut, err1 := strconv.ParseUint(f[11], 10, 64)
	st, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return time.Duration(ut+st) * time.Second / userHZ, true
}
