//go:build !linux

package platform

// CgroupRoomMiB is unknown off Linux (-1: no cgroup limit applies).
func CgroupRoomMiB(pid int) int { return -1 }
