//go:build !windows

package platform

import "syscall"

// HangupGroup sends SIGHUP to process group pgid. Unlike Hangup it takes the
// group id directly, so it works after the leader has been reaped and never
// looks up the group of a PID that may have been reused.
func HangupGroup(pgid int) {
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGHUP)
	}
}

// TerminateGroup sends SIGTERM to process group pgid.
func TerminateGroup(pgid int) {
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}
}

// KillSession sends SIGKILL to process group sid and, where the OS lets us
// enumerate them (Linux), to every other process still in session sid:
// children an agent moved into their own process group, nohup'd servers and
// the like. Use it with the PID of a session leader started by StartPTY.
func KillSession(sid int) {
	if sid <= 0 {
		return
	}
	_ = syscall.Kill(-sid, syscall.SIGKILL)
	// A member may fork while we kill; a few passes catch its children.
	for range 3 {
		pids := sessionMembers(sid)
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
