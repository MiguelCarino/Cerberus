//go:build !windows

package sysmon

import (
	"errors"
	"syscall"
)

var (
	errNiceRange = errors.New("nice must be 0..19")
	errRaise     = errors.New("raising a process's priority needs root; only lowering it is allowed")
	errSignal    = errors.New(`signal must be "term", "kill", "stop" or "cont"`)
)

func sendSignal(pid int, sig string) error {
	var s syscall.Signal
	switch sig {
	case "term":
		s = syscall.SIGTERM
	case "kill":
		s = syscall.SIGKILL
	case "stop":
		s = syscall.SIGSTOP
	case "cont":
		s = syscall.SIGCONT
	default:
		return errSignal
	}
	return syscall.Kill(pid, s)
}

func setNice(pid, nice int) error { return syscall.Setpriority(syscall.PRIO_PROCESS, pid, nice) }
