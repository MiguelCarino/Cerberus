//go:build windows

package sysmon

import (
	"errors"

	"carino.systems/cerberus/internal/platform"
)

var (
	errNiceRange = errors.New("nice must be 0..19")
	errRaise     = errors.New("raising a process's priority is not allowed")
)

func sendSignal(pid int, sig string) error { return platform.ErrUnsupported }

func setNice(pid, nice int) error { return platform.ErrUnsupported }
