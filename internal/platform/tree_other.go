//go:build !linux && !windows

package platform

// sessionMembers cannot enumerate a session here; KillSession falls back to
// the session's own process group.
func sessionMembers(sid int) []int { return nil }
