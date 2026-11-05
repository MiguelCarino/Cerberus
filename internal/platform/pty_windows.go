//go:build windows

package platform

import (
	"os"
	"os/exec"
)

// StartPTY is not implemented on Windows yet (ConPTY in a later phase).
func StartPTY(cmd *exec.Cmd, cols, rows int) (*os.File, error) { return nil, ErrUnsupported }

func ResizePTY(f *os.File, cols, rows int) error { return ErrUnsupported }
