//go:build windows

package subs

import (
	"errors"
	"os"
)

// openRegular opens path for reading only if it is a regular file.
func openRegular(path string) (*os.File, error) {
	if st, err := os.Lstat(path); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path)
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
