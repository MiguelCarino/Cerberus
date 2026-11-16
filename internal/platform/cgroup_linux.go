//go:build linux

package platform

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupRoomMiB is how much more memory process pid may use before its
// cgroup's memory.max stops it: the smallest (memory.max - used) over the
// cgroup and its parents that set a limit. Used is memory.current less the
// page cache (memory.stat "file"), which the kernel reclaims before it
// kills. -1 when no level sets a limit or the cgroup cannot be read
// (cgroup v1, a gone pid).
func CgroupRoomMiB(pid int) int { return cgroupRoomMiB("/proc", "/sys/fs/cgroup", pid) }

// cgroupRoomMiB is CgroupRoomMiB with the /proc and cgroup v2 roots
// injectable (tests).
func cgroupRoomMiB(procRoot, cgRoot string, pid int) int {
	if pid <= 0 {
		return -1
	}
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return -1
	}
	defer f.Close()
	rel := ""
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			rel, found = p, true
			break
		}
	}
	if !found || strings.Contains(rel, "..") {
		return -1
	}
	room := int64(-1)
	for dir := filepath.Join(cgRoot, filepath.FromSlash(rel)); ; dir = filepath.Dir(dir) {
		if r, ok := levelRoom(dir); ok && (room < 0 || r < room) {
			room = r
		}
		if dir == cgRoot || len(dir) <= len(cgRoot) {
			break
		}
	}
	if room < 0 {
		return -1
	}
	return int(room >> 20)
}

// levelRoom is one cgroup's room in bytes; ok is false when it sets no
// limit or cannot be read.
func levelRoom(dir string) (int64, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return 0, false
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil { // "max": no limit here
		return 0, false
	}
	raw, err = os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, false
	}
	used, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		for line := range strings.Lines(string(raw)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "file "); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 && n <= used {
					used -= n
				}
				break
			}
		}
	}
	return max(limit-used, 0), true
}
