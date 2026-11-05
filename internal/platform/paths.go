// Package platform hides OS differences: data paths, process groups, memory
// and PTYs. Linux is the supported target; darwin builds and works where the
// primitives exist; windows compiles with stubs (ErrUnsupported) for now.
package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// ErrUnsupported is returned by features not implemented on this OS yet.
var ErrUnsupported = errors.New("not supported on " + runtime.GOOS + " yet")

// DataDir is where sessions, worktrees, logs and usage live (created 0700).
func DataDir() (string, error) {
	var d string
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		d = filepath.Join(home, "Library", "Application Support", "Cerberus")
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		d = filepath.Join(base, "Cerberus")
	default:
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "share")
		}
		d = filepath.Join(base, "cerberus")
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}
