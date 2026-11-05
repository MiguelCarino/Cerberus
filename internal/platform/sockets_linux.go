//go:build linux

package platform

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
)

// SocketScan reports whether ListenSockets can find which processes listen
// where (Linux: /proc). Elsewhere live preview servers are not detected.
const SocketScan = true

// Procs lists every process with its parent and session, from /proc.
func Procs() []Proc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]Proc, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		i := bytes.LastIndexByte(raw, ')')
		if i < 0 {
			continue
		}
		f := bytes.Fields(raw[i+1:]) // state ppid pgrp session ...
		if len(f) < 4 {
			continue
		}
		ppid, _ := strconv.Atoi(string(f[1]))
		sid, _ := strconv.Atoi(string(f[3]))
		out = append(out, Proc{PID: pid, PPID: ppid, SID: sid})
	}
	return out
}

// ProcEnv returns the value of the environment variable name in process
// pid ("" when unset or unreadable: only our own user's processes are).
func ProcEnv(pid int, name string) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil || len(raw) == 0 {
		return ""
	}
	prefix := []byte(name + "=")
	for entry := range bytes.SplitSeq(raw, []byte{0}) {
		if v, ok := bytes.CutPrefix(entry, prefix); ok {
			return string(v)
		}
	}
	return ""
}

// ProcUnits returns the cgroup path segments of process pid (systemd
// units such as "cerberus-ab12-1-ff00.scope"), from /proc/PID/cgroup.
func ProcUnits(pid int) []string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if _, path, ok := strings.Cut(line, "::"); ok {
			out = append(out, strings.Split(path, "/")...)
		}
	}
	return out
}

// ListenSockets returns the TCP sockets in LISTEN state, bound to loopback
// or to every interface, that are open in one of pids, as seen from this
// process's network namespace (a sandbox without network has its own, and
// its servers are unreachable from here anyway).
func ListenSockets(pids []int) []ListenSock {
	if len(pids) == 0 {
		return nil
	}
	listening := map[string]ListenSock{} // socket inode -> socket
	for _, f := range []struct {
		path string
		v6   bool
	}{{"/proc/self/net/tcp", false}, {"/proc/self/net/tcp6", true}} {
		readListening(f.path, f.v6, listening)
	}
	if len(listening) == 0 {
		return nil
	}
	seen := map[int]bool{}
	var out []ListenSock
	for _, pid := range pids {
		dir := "/proc/" + strconv.Itoa(pid) + "/fd"
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(dir + "/" + fd.Name())
			if err != nil {
				continue
			}
			ino, ok := strings.CutPrefix(link, "socket:[")
			if !ok {
				continue
			}
			ino = strings.TrimSuffix(ino, "]")
			if s, ok := listening[ino]; ok {
				key := s.Port
				if s.V6 {
					key = -key
				}
				if !seen[key] {
					seen[key] = true
					out = append(out, s)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b ListenSock) int { return a.Port - b.Port })
	return out
}

// readListening adds the loopback or wildcard LISTEN sockets of one
// /proc/net/tcp{,6} table to into, keyed by inode.
func readListening(path string, v6 bool, into map[string]ListenSock) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" { // 0A = TCP_LISTEN
			continue
		}
		hexIP, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		ip := procIP(hexIP)
		if ip == nil {
			continue
		}
		var s ListenSock
		switch {
		case ip.IsLoopback():
			s = ListenSock{Port: int(port), Loopback: true, V6: v6 && ip.To4() == nil}
		case ip.IsUnspecified():
			s = ListenSock{Port: int(port), V6: v6}
		default:
			continue // bound to one LAN address: not something to proxy
		}
		into[fields[9]] = s
	}
}

// procIP decodes /proc/net/tcp's address: 32-bit words in host (little
// endian on every Linux we run on) byte order.
func procIP(h string) net.IP {
	raw, err := hex.DecodeString(h)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil
	}
	ip := make(net.IP, len(raw))
	for w := 0; w < len(raw); w += 4 {
		ip[w], ip[w+1], ip[w+2], ip[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
	}
	return ip
}
