package engine

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/railguard"
)

// Shedding VRAM before railguard has to kill (SCOPE "Railguard contract
// and engine hardening (2026-11-15)").
//
// Railguard, before it ends a managed engine for VRAM, writes its ask to
// $XDG_STATE_HOME/hypr/railguard-pressure.json ({"v":1, "time",
// "resource":"vram", "used_mib", "total_mib", "target_mib", "act_at",
// "managed_units"}) and waits managed_grace_s. While a local model is
// loaded, Cerberus looks for that file every 2 s (never while nothing is
// loaded), ignores one older than 60 s or of another version, and unloads
// idle local models, least recently used first, until the card is at or
// under target_mib. It never touches a busy model: if that is not enough,
// railguard decides, so its protection stays.
//
// Proactive shed: while local requests run (the GPU monitor polls fast),
// when Cerberus' own reading sits at or above railguard's line minus 1%
// for 6 s, it does the same shed down to total - reserve without being
// asked. It is off when the VRAM reserve is off (-1) or railguard's VRAM
// watch is off.

const (
	pressureInterval = 2 * time.Second
	proactiveHold    = 6 * time.Second
)

// readPressure is the real pressure file reader.
func readPressure(now time.Time) (railguard.Pressure, bool) {
	return railguard.ReadPressure(railguard.Dir(), now)
}

// anyLocalLoaded reports whether a local model is loaded.
func (m *Manager) anyLocalLoaded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.states {
		if st.info.Local && st.loaded {
			return true
		}
	}
	return false
}

// pressureLoop runs pressureTick every 2 s while a local model is loaded
// and blocks with no timer otherwise (a load wakes it).
func (m *Manager) pressureLoop(ctx context.Context) {
	every := m.pressureEvery
	if every <= 0 {
		every = pressureInterval
	}
	var t *time.Ticker
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	for {
		loaded := m.anyLocalLoaded()
		var tc <-chan time.Time
		switch {
		case loaded && t == nil:
			t = time.NewTicker(every)
		case !loaded && t != nil:
			t.Stop()
			t = nil
			m.hotSince = time.Time{}
		}
		if t != nil {
			tc = t.C
		}
		select {
		case <-ctx.Done():
			return
		case <-m.pwake:
		case <-tc:
			m.pressureTick(ctx)
		}
	}
}

// pressureTick answers a railguard ask, or sheds on its own when the card
// has sat at the line. Only the pressure loop calls it.
func (m *Manager) pressureTick(ctx context.Context) {
	now := m.now()
	if m.pressure != nil {
		if p, ok := m.pressure(now); ok {
			m.hotSince = time.Time{}
			m.answerAsk(ctx, p)
			return
		}
	}
	m.proactive(ctx, now)
}

// usedMiB is the VRAM in use now (-1 unknown), read afresh.
func (m *Manager) usedMiB(ctx context.Context) int {
	m.primeTotal()
	total := int(m.totalMiB.Load())
	if total <= 0 {
		return -1
	}
	free := m.rawFree(ctx)
	if free < 0 {
		return -1
	}
	return total - free
}

// answerAsk sheds idle models until VRAM use is at or under the ask's
// target.
func (m *Manager) answerAsk(ctx context.Context, p railguard.Pressure) {
	first := p.ActAt != m.askSeen
	m.askSeen = p.ActAt
	used := m.usedMiB(ctx)
	if used < 0 {
		used = p.UsedMiB // our reader knows nothing: trust railguard's figure
	}
	if used <= p.TargetMiB {
		return
	}
	unloaded, freed := m.shed(ctx, p.TargetMiB, used)
	switch {
	case len(unloaded) > 0:
		msg := fmt.Sprintf("unloaded %s (%d MiB)", strings.Join(unloaded, ", "), freed)
		m.log.Warn("railguard asked for VRAM; "+msg, "target_mib", p.TargetMiB, "used_mib", used)
		m.send(notify.Message{Title: "Railguard asked for VRAM", Body: "Unloaded " + strings.Join(unloaded, ", ") +
			fmt.Sprintf(" (%d MiB).", freed), Urgency: "low", Kind: "engine", Level: "info", Link: "#/models"})
	case first:
		m.log.Warn("railguard asked for VRAM; no idle local model to unload", "target_mib", p.TargetMiB, "used_mib", used,
			"act_in_s", math.Round(p.ActAt-float64(m.now().Unix())))
	}
}

// proactive sheds when the card has been at railguard's line minus 1% for
// 6 s while local requests run.
func (m *Manager) proactive(ctx context.Context, now time.Time) {
	if !m.active.Load() {
		m.hotSince = time.Time{}
		return
	}
	rg, present := railguard.Defaults(), false
	if m.railguard != nil {
		rg, present = m.railguard()
	}
	res := m.reserve()
	total := int(m.totalMiB.Load())
	if (present && !rg.VRAM) || res.MiB <= 0 || total <= 0 {
		m.hotSince = time.Time{}
		return
	}
	free := m.vram.FreeVRAMMiB() // the fast poll keeps it fresh while active
	if free < 0 {
		return
	}
	used := total - free
	if float64(used)*100 < float64(total)*(rg.VRAMPct-1) {
		m.hotSince = time.Time{}
		return
	}
	if m.hotSince.IsZero() {
		m.hotSince = now
		return
	}
	if now.Sub(m.hotSince) < proactiveHold {
		return
	}
	m.hotSince = time.Time{}
	target := total - res.MiB
	unloaded, freed := m.shed(ctx, target, used)
	if len(unloaded) == 0 {
		m.log.Info("VRAM at railguard's line; no idle local model to unload", "used_mib", used, "line_pct", rg.VRAMPct)
		return
	}
	msg := fmt.Sprintf("unloaded %s (%d MiB)", strings.Join(unloaded, ", "), freed)
	m.log.Warn("VRAM sat at railguard's line; "+msg, "used_mib", used, "target_mib", target, "line_pct", rg.VRAMPct)
	m.send(notify.Message{Title: "VRAM almost full", Body: "Unloaded " + strings.Join(unloaded, ", ") +
		fmt.Sprintf(" (%d MiB) before railguard had to act.", freed), Urgency: "low", Kind: "engine", Level: "info", Link: "#/models"})
}

// shed unloads idle local models Cerberus loaded, least recently used
// first, until VRAM use is at or under target. used is the reading the
// decision was made on. It never touches a busy model, nor a model another
// client of the engine loaded (llm-chat, the LM Studio app): Cerberus
// cannot see that model's requests, so it may be mid-answer; railguard
// deals with it. It returns the models unloaded and their size.
func (m *Manager) shed(ctx context.Context, target, used int) ([]string, int) {
	var out []string
	freed := 0
	for _, st := range m.idleLocal(nil) {
		if used >= 0 && used <= target {
			break
		}
		m.mu.Lock()
		ours := st.ours
		m.mu.Unlock()
		if !ours {
			continue
		}
		size, ok := m.tryUnload(ctx, st, false)
		if !ok {
			continue
		}
		m.mu.Lock()
		out = append(out, st.info.ID)
		m.mu.Unlock()
		freed += size
		if now := m.usedMiB(ctx); now >= 0 {
			used = now
		} else if used >= 0 {
			used -= size
		}
	}
	if len(out) > 0 {
		m.updateActive()
	}
	return out, freed
}

func (m *Manager) send(msg notify.Message) {
	if m.notify != nil {
		m.notify(msg)
	}
}
