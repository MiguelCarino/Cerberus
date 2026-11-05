package energy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// raplState reads the CPU package energy counter (intel-rapl:0, which AMD Zen
// also exposes) and turns deltas into watts, handling wrap-around.
type raplState struct {
	mode    string // "auto" | "rapl" | "model"
	path    string
	maxUJ   uint64
	mu      sync.Mutex
	prevUJ  uint64
	prevAt  time.Time
	checked time.Time
	ok      bool
}

func newRAPL(sys, mode string) *raplState {
	if mode == "" {
		mode = "auto"
	}
	dir := filepath.Join(sys, "class/powercap/intel-rapl:0")
	r := &raplState{mode: mode, path: filepath.Join(dir, "energy_uj")}
	r.maxUJ, _ = strconv.ParseUint(strings.TrimSpace(readFile(filepath.Join(dir, "max_energy_range_uj"))), 10, 64)
	return r
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func (r *raplState) read() (uint64, bool) {
	b, err := os.ReadFile(r.path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

// readable re-checks at most once a minute (the permission may be granted
// while Cerberus runs: `cerberus rapl-setup`).
func (r *raplState) readable() bool {
	if r.mode == "model" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) > time.Minute {
		_, r.ok = r.read()
		r.checked = time.Now()
	}
	return r.ok
}

// watts returns package watts since the previous call (false on the first
// call, when unreadable, or in model mode).
func (r *raplState) watts(now time.Time) (float64, bool) {
	if !r.readable() {
		return 0, false
	}
	v, ok := r.read()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ok {
		r.ok = false
		return 0, false
	}
	prev, prevAt := r.prevUJ, r.prevAt
	r.prevUJ, r.prevAt = v, now
	if prevAt.IsZero() {
		return 0, false
	}
	dt := now.Sub(prevAt).Seconds()
	if dt <= 0 {
		return 0, false
	}
	var d uint64
	switch {
	case v >= prev:
		d = v - prev
	case r.maxUJ > 0:
		d = r.maxUJ - prev + v
	default:
		return 0, false
	}
	return float64(d) / 1e6 / dt, true
}
