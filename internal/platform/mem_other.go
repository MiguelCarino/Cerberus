//go:build !linux

package platform

// MemAvailableMiB is unknown on this OS for now (-1 = guard allows).
func MemAvailableMiB() int { return -1 }
