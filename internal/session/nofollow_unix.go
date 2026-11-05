//go:build unix

package session

import "syscall"

// oNoFollow makes an open fail on a symlink in the last path component.
const oNoFollow = syscall.O_NOFOLLOW
