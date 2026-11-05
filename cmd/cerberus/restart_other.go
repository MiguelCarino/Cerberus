//go:build !unix

package main

import "errors"

// execSelf is not possible here: the caller exits instead.
func execSelf() error { return errors.New("re-exec in place is not supported on this system") }
