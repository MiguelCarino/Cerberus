//go:build linux || darwin

package sysmon

import "syscall"

func statfs(path string) (size, free, avail uint64, ok bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0, 0, false
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bfree * bs, st.Bavail * bs, true
}
