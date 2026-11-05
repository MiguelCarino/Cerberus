//go:build !unix

package main

import "io"

// noEcho cannot turn echo off here without a terminal library; the prompt
// still works, and piping the value on stdin keeps it off the screen.
func noEcho(io.Reader) func() { return func() {} }
