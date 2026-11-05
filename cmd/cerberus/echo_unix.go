//go:build unix

package main

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
)

// noEcho turns terminal echo off for a secret prompt (via stty, so no extra
// dependency) and returns the function that turns it back on. An interrupt
// while echo is off restores it before exiting. Where stty is missing the
// input simply echoes.
func noEcho(in io.Reader) func() {
	f, ok := in.(*os.File)
	if !ok {
		return func() {}
	}
	stty := func(arg string) error {
		cmd := exec.Command("stty", arg)
		cmd.Stdin = f
		return cmd.Run()
	}
	if stty("-echo") != nil {
		return func() {}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			_ = stty("echo")
			os.Exit(130)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		_ = stty("echo")
	}
}
