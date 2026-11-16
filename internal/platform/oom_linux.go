//go:build linux

package platform

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// SetOOMScoreAdj sets the kernel OOM score adjustment of pid (-1000 to
// 1000). Raising it needs no privilege, so a child can be made the kernel's
// first pick over the daemon that started it. Errors are the caller's to
// ignore.
func SetOOMScoreAdj(pid, adj int) error {
	return os.WriteFile(filepath.Join("/proc", strconv.Itoa(pid), "oom_score_adj"),
		[]byte(strconv.Itoa(adj)), 0o644)
}

// CgroupUnit is the last part of process pid's cgroup v2 path, which under
// systemd is its unit ("cerberus.service", "run-p1-i2.scope"). pid 0 means
// this process. "" when it cannot be read (cgroup v1, a gone pid).
func CgroupUnit(pid int) string { return cgroupUnit("/proc", pid) }

func cgroupUnit(procRoot string, pid int) string {
	who := "self"
	if pid > 0 {
		who = strconv.Itoa(pid)
	} else if pid < 0 {
		return ""
	}
	f, err := os.Open(filepath.Join(procRoot, who, "cgroup"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			if base := path.Base(p); base != "/" && base != "." {
				return base
			}
			return ""
		}
	}
	return ""
}

// ProcessAlive reports whether a process with this pid exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	return err == nil
}

// ProcessName is process pid's command name (/proc/<pid>/comm) and its
// executable's path, "" for what cannot be read (a gone pid, another
// user's exe). ok is false when the process does not exist.
func ProcessName(pid int) (comm, exe string, ok bool) {
	if pid <= 0 {
		return "", "", false
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	raw, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return "", "", false
	}
	exe, _ = os.Readlink(filepath.Join(dir, "exe"))
	return strings.TrimSpace(string(raw)), exe, true
}
