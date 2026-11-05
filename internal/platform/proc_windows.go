//go:build windows

package platform

import (
	"os"
	"os/exec"
	"time"
)

func NewGroup(cmd *exec.Cmd) {}

// NewSession does nothing on Windows (no sessions or controlling terminals).
func NewSession(cmd *exec.Cmd) {}

func Terminate(p *os.Process, grace time.Duration, done <-chan struct{}) {
	if p != nil {
		_ = p.Kill()
	}
}

func Interrupt(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}

// Hangup does nothing on Windows; Terminate kills the process.
func Hangup(p *os.Process) {}
