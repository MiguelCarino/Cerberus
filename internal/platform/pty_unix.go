//go:build !windows

package platform

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// StartPTY starts cmd attached to a new PTY of the given size, in its own
// session (so it gets a controlling terminal and its own process group).
// The returned file is the PTY master: read output, write input.
func StartPTY(cmd *exec.Cmd, cols, rows int) (*os.File, error) {
	return pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// ResizePTY changes the terminal size of a PTY master.
func ResizePTY(f *os.File, cols, rows int) error {
	return pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}
