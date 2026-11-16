package engine

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/railguard"
)

// VRAM reserve and the shared load plan (SCOPE "Railguard contract and
// engine hardening (2026-11-15)").
//
// Atrium's railguard ends the compute process holding the most VRAM when
// the card stays above its line (vram_pct, 97% by default) for vram_s
// seconds. A model server is that process. So every load leaves a reserve
// free: makeRoom evicts and waits until the model fits in free - reserve,
// the GPU split is computed against the same budget, and after the load an
// idle model is unloaded when the card went past the reserve anyway.
//
// makeRoom, overcommit and the split all use one number for what the model
// needs (planNeedMiB): the weights with headroom plus the KV cache for the
// context and slots the engine will really allocate, or the footprint
// measured on an earlier load of the same shape (footprint.go).

const (
	railguardMarginMiB = 768  // kept free below railguard's line
	genericReserveMiB  = 1024 // the reserve with no railguard: at least this
	genericReservePct  = 4    // ... or this share of the card
	ollamaDefaultCtx   = 4096 // Ollama's num_ctx when the model sets none
)

// Reserve is the VRAM a load leaves free, and where the number came from.
type Reserve struct {
	MiB int
	// Source is "off", "fixed", "railguard", "default" or "unknown" (auto
	// with the total VRAM not known yet).
	Source string
	// LinePct is railguard's VRAM line when Source is "railguard".
	LinePct float64
}

// VRAMReserve works out engines.vram_reserve_mib (setting): -1 is off, N > 0
// is N MiB, 0 is auto. Auto is ceil(total × (100 - vram_pct) / 100) + 768
// MiB when railguard is present with its VRAM watch on, else max(1024 MiB,
// 4% of total). Auto with the total unknown is 0: the GPU reader knows the
// free VRAM only when it knows the total, so there is nothing to plan.
func VRAMReserve(setting, totalMiB int, rg railguard.Settings, railguardPresent bool) Reserve {
	switch {
	case setting < 0:
		return Reserve{Source: "off"}
	case setting > 0:
		return Reserve{MiB: setting, Source: "fixed"}
	case totalMiB <= 0:
		return Reserve{Source: "unknown"}
	case railguardPresent && rg.VRAM:
		below := int(math.Ceil(float64(totalMiB) * (100 - rg.VRAMPct) / 100))
		return Reserve{MiB: below + railguardMarginMiB, Source: "railguard", LinePct: rg.VRAMPct}
	}
	pct := int(math.Ceil(float64(totalMiB) * genericReservePct / 100))
	return Reserve{MiB: max(genericReserveMiB, pct), Source: "default"}
}

// readRailguard is the real railguard settings reader.
func readRailguard() (railguard.Settings, bool) { return railguard.Read(railguard.Dir()) }

// VRAMReserveNow is the reserve in effect, for the UI (step web-api). It
// may read railguard's settings file; call it without holding locks.
func (m *Manager) VRAMReserveNow() Reserve { return m.reserve() }

// reserve is the live reserve. Call it without m.mu (it may read files).
func (m *Manager) reserve() Reserve {
	m.primeTotal()
	rg, present := railguard.Defaults(), false
	if m.railguard != nil {
		rg, present = m.railguard()
	}
	return VRAMReserve(m.conf().Engines.VRAMReserveMiB, int(m.totalMiB.Load()), rg, present)
}

// planShape is the context and KV slots the engine will allocate for info
// loaded with the queue limit limit. llama.cpp's -c is shared by all slots,
// so it counts once; LM Studio allocates ctx for each --parallel slot;
// Ollama counts once, with its default context when the model sets none.
func planShape(info ModelInfo, limit int) (ctx, slots int) {
	ctx, slots = max(info.Ctx, 0), 1
	switch info.Backend {
	case BackendLMStudio:
		slots = limit
		if slots <= 0 {
			slots = lmsDefaultParallel
		}
	case BackendOllama:
		if ctx == 0 {
			ctx = ollamaDefaultCtx
		}
	}
	return ctx, slots
}

// planNeedMiB is the VRAM info needs loaded with the queue limit limit: the
// footprint learned on an earlier load of the same shape, else the
// estimate. makeRoom, overcommit and the GPU split all use it.
func (m *Manager) planNeedMiB(info ModelInfo, limit int) int {
	ctx, slots := planShape(info, limit)
	if m.footprints != nil {
		if n, ok := m.footprints.need(footprintKey(info.Backend, info.ID, ctx, slots)); ok {
			return n
		}
	}
	return estimateNeedMiB(info.SizeMiB, ctx, slots)
}

// cgroupRoom is the memory LM Studio's daemon may still take before its
// cgroup limit (lmstudio.service MemoryMax) ends it, or -1 when no limit
// is known. Other engines: -1 (llama.cpp children share Cerberus' own
// cgroup, which sets no limit today; Ollama runs elsewhere).
func cgroupRoom(info ModelInfo) int {
	if info.Backend != BackendLMStudio {
		return -1
	}
	pid := llmsterPID()
	if pid <= 0 {
		return -1
	}
	return platform.CgroupRoomMiB(pid)
}

// llmsterPID is the pid LM Studio's daemon (llmster) wrote in its lock
// file, or 0 when there is none or that pid is not llmster now. A SIGKILL
// or a reboot leaves the lock behind, and its pid may since belong to any
// other process: daemonUp, cgroupRoom and ownEngineUnits must not take
// that process for the daemon.
func llmsterPID() int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	raw, err := os.ReadFile(filepath.Join(home, ".lmstudio", ".internal", "llmster-pid.lock"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	comm, exe, ok := procName(pid)
	if !isLLMster(comm, exe, ok) {
		return 0
	}
	return pid
}

// procName is platform.ProcessName (a test sets a fake).
var procName = platform.ProcessName

// isLLMster reports whether a process with this command name and
// executable is LM Studio's daemon. Off Linux both are unknown ("") and
// the lock is trusted as before.
func isLLMster(comm, exe string, ok bool) bool {
	switch {
	case !ok:
		return false
	case comm == "" && exe == "":
		return true // unknown: off Linux
	case strings.HasPrefix(comm, "llmster"):
		return true
	}
	exe = filepath.ToSlash(exe)
	return strings.Contains(exe, "/.lmstudio/") && strings.Contains(filepath.Base(exe), "llmster")
}

// rawFree refreshes the GPU reading and returns the free VRAM (-1 unknown).
func (m *Manager) rawFree(ctx context.Context) int {
	m.vram.Refresh(ctx)
	return m.vram.FreeVRAMMiB()
}

// guardAfterLoad runs after target loaded: when the card has less than the
// reserve free (the estimate was low, or something else grew), idle local
// models are unloaded, least recently used first, until the reserve is
// free again. Busy models are never touched. free is the reading just taken.
func (m *Manager) guardAfterLoad(ctx context.Context, target *modelState, free, reserve int) {
	if free < 0 || reserve <= 0 || free >= reserve {
		return
	}
	m.mu.Lock()
	id := target.info.ID
	m.mu.Unlock()
	for _, v := range m.idleLocal(target) {
		size, ok := m.tryUnload(ctx, v, false)
		if !ok {
			continue
		}
		m.mu.Lock()
		vid := v.info.ID
		m.mu.Unlock()
		m.log.Info("unloaded an idle model to keep the VRAM reserve free",
			"model", vid, "after_loading", id, "size_mib", size, "free_vram_mib", free, "reserve_mib", reserve)
		if free = m.rawFree(ctx); free < 0 || free >= reserve {
			return
		}
	}
	m.log.Warn("VRAM is past the reserve after a load and no idle model is left to unload",
		"model", id, "free_vram_mib", free, "reserve_mib", reserve)
}
