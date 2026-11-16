//go:build !windows

package platform

import (
	"os"
	"strconv"
	"syscall"
)

// ExitSignal names the signal that ended a process ("SIGTERM") and its
// number, or "" and 0 when it exited on its own (or ps is nil).
func ExitSignal(ps *os.ProcessState) (string, int) {
	if ps == nil {
		return "", 0
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", 0
	}
	n := int(ws.Signal())
	return signalName(n), n
}

func signalName(n int) string {
	switch syscall.Signal(n) {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return "signal " + strconv.Itoa(n)
}

// OutsideStopSignal reports whether n is a signal used to stop a process
// on purpose (SIGTERM, SIGKILL, SIGINT, SIGHUP), as a watchdog, the OOM
// killer or a user would send, rather than a crash (SIGSEGV, SIGABRT).
func OutsideStopSignal(n int) bool {
	switch syscall.Signal(n) {
	case syscall.SIGTERM, syscall.SIGKILL, syscall.SIGINT, syscall.SIGHUP:
		return true
	}
	return false
}
