//go:build !windows

package subs

import (
	"errors"
	"os"
	"syscall"
)

// openRegular opens path for reading only if it is a regular file. It never
// follows a symlink (swapped in after the directory scan) and never blocks
// on a FIFO.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("not a regular file")
	}
	return f, nil
}
