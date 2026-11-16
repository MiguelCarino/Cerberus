//go:build windows

package platform

import "os"

// ExitSignal is always "" on Windows: processes do not end by signals.
func ExitSignal(ps *os.ProcessState) (string, int) { return "", 0 }

// OutsideStopSignal is always false on Windows.
func OutsideStopSignal(n int) bool { return false }
