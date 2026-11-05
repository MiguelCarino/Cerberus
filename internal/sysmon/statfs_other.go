//go:build !linux && !darwin

package sysmon

func statfs(path string) (size, free, avail uint64, ok bool) { return 0, 0, 0, false }
