//go:build linux

package platform

import (
	"os"
	"syscall"
	"unsafe"
)

// PTYEcho reports whether the terminal behind PTY master f has ECHO set
// (off while a program reads a password). known is false when it cannot
// be read. On Linux, TCGETS on a master reads its slave's settings. It
// uses SyscallConn, never Fd, so f stays non-blocking.
func PTYEcho(f *os.File) (on, known bool) {
	return ptyLflag(f, syscall.TCGETS, func(t *syscall.Termios) bool { return t.Lflag&syscall.ECHO != 0 })
}

func ptyLflag(f *os.File, req uintptr, test func(*syscall.Termios) bool) (on, known bool) {
	if f == nil {
		return false, false
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return false, false
	}
	var t syscall.Termios
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(&t)))
	}); err != nil || errno != 0 {
		return false, false
	}
	return test(&t), true
}
