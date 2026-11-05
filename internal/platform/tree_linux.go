//go:build linux

package platform

import (
	"bytes"
	"os"
	"strconv"
)

// sessionMembers lists the PIDs whose session id is sid, from /proc.
func sessionMembers(sid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if s, ok := procSession(pid); ok && s == sid {
			out = append(out, pid)
		}
	}
	return out
}

// procSession reads field 6 (session) of /proc/PID/stat. The command name
// may hold spaces or parentheses, so fields are counted after the last ")".
func procSession(pid int) (int, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, false
	}
	f := bytes.Fields(raw[i+1:]) // state ppid pgrp session ...
	if len(f) < 4 {
		return 0, false
	}
	sid, err := strconv.Atoi(string(f[3]))
	return sid, err == nil
}
