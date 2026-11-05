//go:build unix

package main

import (
	"os"
	"os/exec"
	"strings"
)

// rawTerminal puts stdin in cbreak mode without echo (via stty, no extra
// dependency) so `top` reads single keys; the returned func restores it.
func rawTerminal() func() {
	save := exec.Command("stty", "-g")
	save.Stdin = os.Stdin
	state, err := save.Output()
	if err != nil {
		return func() {}
	}
	set := exec.Command("stty", "-icanon", "-echo", "min", "1")
	set.Stdin = os.Stdin
	if set.Run() != nil {
		return func() {}
	}
	return func() {
		r := exec.Command("stty", strings.TrimSpace(string(state)))
		r.Stdin = os.Stdin
		_ = r.Run()
	}
}
