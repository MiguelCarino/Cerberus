// Package packaging holds the files used to install Cerberus as a service.
// cerberus.service is the systemd user unit that `cerberus install-service`
// writes to ~/.config/systemd/user/, beside cerberus-sessions.slice, the
// slice agent sessions run in.
package packaging

import (
	_ "embed"
	"strconv"
	"strings"
)

// Unit is the systemd user unit (cerberus.service).
//
//go:embed cerberus.service
var Unit string

// SessionSliceName is the slice agent sessions run in once
// guard.systemd_slice names it.
const SessionSliceName = "cerberus-sessions.slice"

// SessionSlice is the sessions slice as shipped (MemoryMax for 3 sessions
// of 6G each); SessionSliceFor fits it to a config.
//
//go:embed cerberus-sessions.slice
var SessionSlice string

// SessionSliceFor is the sessions slice with MemoryMax = sessions x per
// (a size such as "6G") and MemoryHigh at 90% of it. A per that is not a
// plain size (a percentage, "infinity", empty) or sessions < 1 leaves both
// lines out: the slice then has no cap of its own.
func SessionSliceFor(sessions int, per string) string {
	b := MemBytes(per)
	var out []string
	for _, l := range strings.Split(SessionSlice, "\n") {
		switch {
		case strings.HasPrefix(l, "MemoryMax="):
			if b <= 0 || sessions < 1 {
				continue
			}
			l = "MemoryMax=" + strconv.FormatInt(int64(sessions)*b>>20, 10) + "M"
		case strings.HasPrefix(l, "MemoryHigh="):
			if b <= 0 || sessions < 1 {
				continue
			}
			l = "MemoryHigh=" + strconv.FormatInt(int64(sessions)*b*9/10>>20, 10) + "M"
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// MemBytes reads a systemd size: bytes, or a number with K, M, G or T
// (base 1024; a trailing B is allowed). 0 for anything else (empty,
// "infinity", a percentage).
func MemBytes(s string) int64 {
	s = strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(s)), "B")
	if s == "" {
		return 0
	}
	mul := int64(1)
	switch s[len(s)-1] {
	case 'K':
		mul = 1 << 10
	case 'M':
		mul = 1 << 20
	case 'G':
		mul = 1 << 30
	case 'T':
		mul = 1 << 40
	}
	if mul > 1 {
		s = s[:len(s)-1]
	}
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0 // "infinity", "NaN" and friends
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f > 1<<20 && mul > 1 {
		return 0
	}
	return int64(f * float64(mul))
}
