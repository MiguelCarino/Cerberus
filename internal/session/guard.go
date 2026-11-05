package session

import "fmt"

// guardState is what the resource guard knows when deciding whether a queued
// session may start. Unknown amounts are negative.
type guardState struct {
	Running     int // sessions holding a slot (starting, running, waiting, checking…)
	MaxSessions int // <= 0: no limit
	MemAvailMiB int // -1 = unknown
	MinFreeMiB  int
	LocalModel  bool // the session uses a local model
	ModelLoaded bool
	FreeVRAMMiB int // -1 = unknown
	NeedVRAMMiB int // the model's estimate; <= 0 = unknown
}

// agentMemMiB is what an admitted agent is assumed to take from available
// memory before the next measurement sees it.
const agentMemMiB = 512

// reserve accounts for a session just admitted: agents load their model only
// at their first request, so the next admission in the same pass would
// otherwise be judged against the same free VRAM and memory reading. newModel says
// whether the session's model is not yet counted.
func (g *guardState) reserve(newModel bool) {
	if g.MemAvailMiB >= 0 {
		g.MemAvailMiB = max(g.MemAvailMiB-agentMemMiB, 0)
	}
	if newModel && g.LocalModel && !g.ModelLoaded && g.FreeVRAMMiB >= 0 && g.NeedVRAMMiB > 0 {
		g.FreeVRAMMiB = max(g.FreeVRAMMiB-g.NeedVRAMMiB, 0)
	}
}

// decide reports whether a session may start now and, if not, why.
func (g guardState) decide() (bool, string) {
	if g.MaxSessions > 0 && g.Running >= g.MaxSessions {
		return false, fmt.Sprintf("%d of %d sessions running", g.Running, g.MaxSessions)
	}
	if g.MemAvailMiB >= 0 && g.MemAvailMiB < g.MinFreeMiB {
		return false, fmt.Sprintf("low memory: %d MiB available, %d MiB needed", g.MemAvailMiB, g.MinFreeMiB)
	}
	if g.LocalModel && !g.ModelLoaded && g.FreeVRAMMiB >= 0 && g.NeedVRAMMiB > 0 && g.FreeVRAMMiB < g.NeedVRAMMiB {
		return false, fmt.Sprintf("waiting for VRAM: %d MiB free, model needs about %d MiB", g.FreeVRAMMiB, g.NeedVRAMMiB)
	}
	return true, ""
}
