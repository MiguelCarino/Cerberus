//go:build linux

package platform

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// MemAvailableMiB reads MemAvailable from /proc/meminfo (-1 if unknown).
func MemAvailableMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.Atoi(fields[1])
				if err == nil {
					return kb / 1024
				}
			}
		}
	}
	return -1
}
