//go:build unix

package main

import (
	"os"
	"syscall"
)

// execSelf replaces this process with a fresh copy of the same binary.
func execSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
