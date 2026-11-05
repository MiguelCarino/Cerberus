//go:build windows

package platform

// HangupGroup does nothing on Windows (no process groups or PTYs yet).
func HangupGroup(pgid int) {}

// TerminateGroup does nothing on Windows (no process groups or PTYs yet).
func TerminateGroup(pgid int) {}

// KillSession does nothing on Windows (no sessions or PTYs yet).
func KillSession(sid int) {}
