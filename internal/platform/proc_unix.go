//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// NewGroup makes cmd start in its own process group (so the whole tree can be signalled).
func NewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// NewSession makes cmd start as the leader of a new session (setsid): it
// has no controlling terminal, so it cannot open /dev/tty to write to the
// terminal of whoever started it, and it leads its own process group, so
// Terminate still reaches its tree. Children that outlive it stay in that
// session: KillSession(pid) ends them.
func NewSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = false // setsid already makes a group; setpgid after it fails
	cmd.SysProcAttr.Setsid = true
}

// Terminate sends SIGTERM to the process group of p, then SIGKILL after grace
// if it is still alive. It returns immediately; done (may be nil) is closed by
// the caller's Wait goroutine to signal exit.
func Terminate(p *os.Process, grace time.Duration, done <-chan struct{}) {
	if p == nil {
		return
	}
	pgid, err := syscall.Getpgid(p.Pid)
	if err != nil || pgid <= 0 {
		_ = p.Signal(syscall.SIGTERM)
		pgid = 0
	} else {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}
	go func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-done:
			return
		case <-t.C:
		}
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = p.Kill()
		}
	}()
}

// Interrupt sends SIGINT to the process group of p.
func Interrupt(p *os.Process) {
	if p == nil {
		return
	}
	if pgid, err := syscall.Getpgid(p.Pid); err == nil && pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGINT)
		return
	}
	_ = p.Signal(syscall.SIGINT)
}

// Hangup sends SIGHUP to the process group of p: the signal a terminal sends
// when it closes. Interactive shells ignore SIGTERM but exit on SIGHUP, so a
// PTY session is hung up before Terminate.
func Hangup(p *os.Process) {
	if p == nil {
		return
	}
	if pgid, err := syscall.Getpgid(p.Pid); err == nil && pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGHUP)
		return
	}
	_ = p.Signal(syscall.SIGHUP)
}
