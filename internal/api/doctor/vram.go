package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/railguard"
)

// ---------- VRAM reserve ----------

// railguardDir is railguard's state folder as e sees it:
// $XDG_STATE_HOME/hypr, else ~/.local/state/hypr.
func railguardDir(e Env) string {
	if e.RailguardDir != "" {
		return e.RailguardDir
	}
	if base := e.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "hypr")
	}
	if e.Home == "" {
		return ""
	}
	return filepath.Join(e.Home, ".local", "state", "hypr")
}

// nvidiaTotalMiB sums memory.total over the cards nvidia-smi reports (-1
// unknown: no NVIDIA reader, or it failed).
func nvidiaTotalMiB(ctx context.Context, e Env) int {
	if e.Cfg == nil || e.Output == nil {
		return -1
	}
	switch strings.ToLower(strings.TrimSpace(e.Cfg.GPU.Backend)) {
	case "none", "amd":
		return -1
	}
	smi := orDefault(e.Cfg.GPU.NvidiaSmi, "nvidia-smi")
	out, err := e.Output(ctx, smi, "--query-gpu=memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return -1
	}
	total := 0
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil || n <= 0 {
			return -1
		}
		total += n
	}
	if total == 0 {
		return -1
	}
	return total
}

// vramReserveCheck compares engines.vram_reserve_mib with the card and,
// when railguard is present, with its VRAM line: planned use past the line
// is what gets a model server ended mid-request. It only reads
// railguard's settings.
func vramReserveCheck(ctx context.Context, e Env) Check {
	const id, name = "vram_reserve", "VRAM reserve"
	en := e.Cfg.Engines
	if !en.LlamaCpp.Enabled && !en.LMStudio.Enabled && !en.Ollama.Enabled {
		return ok(id, name, "no local engine is on")
	}
	rg, present := railguard.Read(railguardDir(e))
	watch := present && rg.VRAM
	total := -1
	if e.VRAMTotalMiB != nil {
		total = e.VRAMTotalMiB(ctx)
	}
	res := engine.VRAMReserve(en.VRAMReserveMiB, total, rg, present)
	const fixAuto = "set engines.vram_reserve_mib to 0 (auto)"
	if res.Source == "off" {
		if watch {
			return warn(id, name, fmt.Sprintf("off: loads may fill the card, and railguard ends the model server when VRAM stays above %g%%", rg.VRAMPct),
				fixAuto+", or add the model server to railguard's never_kill list")
		}
		return ok(id, name, "off (engines.vram_reserve_mib is -1)")
	}
	if total <= 0 {
		return ok(id, name, "the total VRAM is not known, so it is not checked")
	}
	planned := total - res.MiB
	if watch {
		line := int(float64(total) * rg.VRAMPct / 100)
		if planned > line {
			need := total - line + 768
			return warn(id, name, fmt.Sprintf("keeps %d MiB free, so loads may fill the card to %d MiB; railguard acts above %g%% (%d MiB)",
				res.MiB, planned, rg.VRAMPct, line),
				fmt.Sprintf("%s or set it to at least %d, or add the model server to railguard's never_kill list", fixAuto, need))
		}
		return ok(id, name, fmt.Sprintf("keeps %d MiB of %d MiB free (%s); loads stay under railguard's line at %g%% (%d MiB)",
			res.MiB, total, res.Source, rg.VRAMPct, line))
	}
	if res.MiB*100 < total*3 {
		return warn(id, name, fmt.Sprintf("keeps only %d MiB of %d MiB free (under 3%%); the desktop may run out of VRAM", res.MiB, total), fixAuto)
	}
	return ok(id, name, fmt.Sprintf("keeps %d MiB of %d MiB free (%s)", res.MiB, total, res.Source))
}
