//go:build darwin

package platform

import (
	"os"
	"syscall"
	"unsafe"
)

// PTYEcho reports whether the terminal behind PTY master f has ECHO set
// (off while a program reads a password). known is false when it cannot
// be read. It uses SyscallConn, never Fd, so f stays non-blocking.
func PTYEcho(f *os.File) (on, known bool) {
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
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))
	}); err != nil || errno != 0 {
		return false, false
	}
	return t.Lflag&syscall.ECHO != 0, true
}
