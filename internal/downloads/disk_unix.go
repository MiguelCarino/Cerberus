//go:build unix

package downloads

import "syscall"

// oNoFollow makes an open fail on a symlink in the last path component.
const oNoFollow = syscall.O_NOFOLLOW

// freeSpace is the space available to this user under path.
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
