//go:build !linux && !darwin

package platform

import "os"

// PTYEcho cannot read terminal settings on this platform: known is false,
// and callers treat the input as hidden.
func PTYEcho(f *os.File) (on, known bool) { return false, false }
