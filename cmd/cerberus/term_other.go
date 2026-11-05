//go:build !unix

package main

// rawTerminal is a no-op here: `top` then quits on q followed by Enter.
func rawTerminal() func() { return func() {} }
