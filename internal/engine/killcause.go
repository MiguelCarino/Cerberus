package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/railguard"
)

// Engine deaths and their causes (SCOPE "Railguard contract and engine
// hardening (2026-11-15)").
//
// A model's engine can end without Cerberus asking: railguard ends the
// biggest VRAM user when the card stays past its line, the kernel or
// systemd-oomd end it for RAM, LM Studio's own TTL or another client
// unloads it, or it crashes. Each such event is noticed once per load
// (from a llama.cpp exit, a failed request or a listing that lost the
// model), named, logged once at Warn, sent as a notification of Kind
// "engine" and kept as ModelInfo.LastFailure. When railguard's history
// shows it ended the engine for VRAM, the model's next loads get a smaller
// GPU share (vramCut) instead of the same plan that was just killed.

// Failure is why a model's engine last ended outside Cerberus' control.
type Failure struct {
	Time time.Time
	// Cause is a short code: railguard-vram, railguard-memory,
	// railguard-game, signal, crash, exit, vanished or stalled (the
	// gateway's stall watchdog gave up on it).
	Cause string
	Text  string // one plain sentence for people
}

// exitInfo is how a llama-server ended.
type exitInfo struct {
	Time   time.Time
	PID    int
	Code   int    // exit code (-1 when ended by a signal)
	Signal string // "SIGTERM"; "" when it exited on its own
	SigNum int
	ByUs   bool // Cerberus stopped it (Unload, a failed load, shutdown)
}

// outside reports whether the process was stopped on purpose by someone
// else (a watchdog, the OOM killer, a user) rather than crashing.
func (e exitInfo) outside() bool { return !e.ByUs && platform.OutsideStopSignal(e.SigNum) }

const (
	vramCutStep  = 1024 // MiB taken off the GPU budget per railguard VRAM kill
	vramCutMax   = 4096
	vramCutDecay = time.Hour // a step comes back after this long with no new kill

	// The window around an engine's death in which a railguard history
	// line is taken to be its cause. Railguard writes the line when it
	// acts, which can be a little after the request failed.
	attributeBefore = 15 * time.Second
	attributeAfter  = 5 * time.Second

	// A failure whose cause was not found yet is looked up again this
	// much later: railguard may still have been writing its line.
	reattributeAfter = 6 * time.Second
	reattributeUntil = time.Minute

	// failureRecent is how long the gateway names the cause in its 503.
	failureRecent = time.Minute
)

// engineProcNames are the process names an engine's death shows under in
// railguard's history.
var engineProcNames = []string{"llama-server", "llmster"}

// readHistory is the real railguard history reader.
func readHistory(from, to time.Time) []railguard.Entry {
	return railguard.History(railguard.Dir(), from, to)
}

// ownEngineUnits is the systemd unit a backend's engines run in: Cerberus'
// own for llama.cpp (its llama-server children), LM Studio's daemon's for
// LM Studio (read from /proc/<llmster pid>/cgroup), none for others.
func ownEngineUnits(backend string) []string {
	var u string
	switch backend {
	case BackendLlamaCpp:
		u = platform.CgroupUnit(0)
	case BackendLMStudio:
		if pid := llmsterPID(); pid > 0 {
			u = platform.CgroupUnit(pid)
		}
	}
	if u == "" {
		return nil
	}
	return []string{u}
}

// vanishedWindow caps how far back a vanished model's death is looked for:
// the previous listing may be minutes old.
const vanishedWindow = time.Minute

// attribute looks for railguard's line about the death at at of an engine
// of backend: within -15 s..+5 s, kind vram, memory or game-vram, and
//   - when the engine's pid is known, that pid and nothing else;
//   - else (LM Studio's models, whose process Cerberus does not know) an
//     engine process name in this backend's unit: LM Studio's daemon's unit
//     and railguard's managed_units, less Cerberus' own unit unless the
//     daemon runs there.
//
// A llama.cpp model with no known pid is never matched by unit: Cerberus'
// unit holds all of its llama-servers, and a line about one of them must
// not be blamed on another. Conservative on purpose: a real CUDA crash
// must not be blamed on railguard.
func (m *Manager) attribute(at time.Time, pid int, backend string) (railguard.Entry, bool) {
	return m.attributeSince(time.Time{}, at, pid, backend)
}

// attributeSince is attribute with the window widened back to from (a
// model that vanished some time between two listings), at most
// vanishedWindow. A zero from is at - 15 s.
func (m *Manager) attributeSince(from, at time.Time, pid int, backend string) (railguard.Entry, bool) {
	if m.history == nil {
		return railguard.Entry{}, false
	}
	if from.IsZero() || from.After(at.Add(-attributeBefore)) {
		from = at.Add(-attributeBefore)
	}
	if from.Before(at.Add(-vanishedWindow)) {
		from = at.Add(-vanishedWindow)
	}
	if pid <= 0 && backend == BackendLlamaCpp {
		return railguard.Entry{}, false
	}
	entries := m.history(from, at.Add(attributeAfter))
	if len(entries) == 0 {
		return railguard.Entry{}, false
	}
	var units []string
	if pid <= 0 {
		managed := railguard.Defaults().ManagedUnits
		if m.railguard != nil {
			rg, _ := m.railguard()
			managed = rg.ManagedUnits
		}
		var own, mine []string
		if m.engineUnits != nil {
			own = m.engineUnits(BackendLlamaCpp)
			mine = m.engineUnits(backend)
		}
		units = append(units, mine...)
		for _, u := range managed {
			if !slices.Contains(own, u) || slices.Contains(mine, u) {
				units = append(units, u)
			}
		}
	}
	var best railguard.Entry
	found := false
	for _, e := range entries {
		switch e.Kind {
		case "vram", "memory", "game-vram":
		default:
			continue
		}
		match := pid > 0 && e.PID == pid
		if pid <= 0 {
			match = slices.Contains(engineProcNames, e.Name) && e.Unit != "" && slices.Contains(units, e.Unit)
		}
		if !match {
			continue
		}
		// The closest line to the death wins.
		if !found || absDur(e.Time.Sub(at)) < absDur(best.Time.Sub(at)) {
			best, found = e, true
		}
	}
	return best, found
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// railguardFailure turns a railguard history line into a Failure.
func railguardFailure(e railguard.Entry, at time.Time) Failure {
	why := strings.TrimSpace(e.Why)
	switch e.Kind {
	case "vram":
		if why == "" {
			why = "VRAM full"
		}
		return Failure{Time: at, Cause: "railguard-vram", Text: "ended by Railguard: " + why}
	case "game-vram":
		if why == "" {
			why = "VRAM needed for a game"
		}
		return Failure{Time: at, Cause: "railguard-game", Text: "ended by Railguard for a game: " + why}
	}
	if why == "" {
		why = "RAM full"
	}
	return Failure{Time: at, Cause: "railguard-memory", Text: "ended by Railguard: " + why}
}

// exitFailure names a llama-server exit that was not Cerberus' doing.
func exitFailure(e exitInfo) Failure {
	switch {
	case e.outside():
		return Failure{Time: e.Time, Cause: "signal",
			Text: "killed by " + e.Signal + " from outside Cerberus (a memory watchdog, the OOM killer or a user)"}
	case e.Signal != "":
		return Failure{Time: e.Time, Cause: "crash", Text: "llama-server crashed (" + e.Signal + ")"}
	}
	return Failure{Time: e.Time, Cause: "exit", Text: fmt.Sprintf("llama-server exited with code %d", e.Code)}
}

// vanishedFailure is a model the engine no longer lists although Cerberus
// loaded it and its TTL has not passed.
func vanishedFailure(at time.Time) Failure {
	return Failure{Time: at, Cause: "vanished",
		Text: "unloaded outside Cerberus (possibly LM Studio's TTL, Atrium game mode or another client)"}
}

// failureEvent is one engine death to record.
type failureEvent struct {
	st    *modelState
	gen   uint64 // the load it ended (st.loadSeq at the time)
	f     Failure
	crash bool // a real crash: one slot off the limit (crashCut)
	cut   bool // a railguard VRAM kill: a smaller GPU share next time (vramCut)
	// pending: the cause may still show up in railguard's history.
	pending bool
	pid     int
	from    time.Time // the death happened between from and f.Time (zero: at f.Time)
}

// recordFailureLocked applies ev once per load: it marks the model not
// loaded, records LastFailure and the cuts. It reports whether ev was new
// and the limit before and after. Caller holds m.mu.
func (m *Manager) recordFailureLocked(ev *failureEvent) (fresh bool, before, after int) {
	st := ev.st
	before = m.limitLocked(st)
	// One record per death: a failure of the same load while the model is
	// still not loaded again is the same event (several requests failing
	// from one crash, or the exit hook and a failed request both seeing it).
	if st.lastFail != nil && st.failGen == ev.gen && !st.loaded {
		return false, before, before
	}
	st.failGen = ev.gen
	f := ev.f
	st.lastFail = &f
	st.failPending = ev.pending
	st.failPID = ev.pid
	st.failFrom = ev.from
	st.failCrashCut = false
	if ev.crash && !st.crashed && before > 1 {
		st.crashCut++
		st.failCrashCut = true
	}
	// Whatever the engine left (LM Studio may still list the model, in a
	// bad state) is unloaded before the next load.
	st.crashed = true
	if ev.cut {
		m.addVRAMCutLocked(st)
	}
	if st.loaded {
		st.setLoadedLocked(false)
		m.broadcastLocked()
		m.loadEvents.Add(1)
	}
	m.changed() // LastFailure
	return true, before, m.limitLocked(st)
}

// addVRAMCutLocked takes one more step off st's GPU budget (max 4096 MiB).
func (m *Manager) addVRAMCutLocked(st *modelState) {
	cur := st.cutLocked(m.now())
	st.vramCut = min(cur+vramCutStep, vramCutMax)
	st.cutAt = m.now()
}

// cutLocked is the GPU budget cut now in force: one step comes back for
// each hour with no new railguard VRAM kill. Caller holds m.mu.
func (st *modelState) cutLocked(now time.Time) int {
	if st.vramCut <= 0 {
		return 0
	}
	if steps := int(now.Sub(st.cutAt) / vramCutDecay); steps > 0 {
		st.vramCut = max(st.vramCut-steps*vramCutStep, 0)
		st.cutAt = st.cutAt.Add(time.Duration(steps) * vramCutDecay)
	}
	return st.vramCut
}

// announce logs one Warn line and sends one notification for a freshly
// recorded failure. Call it without m.mu.
func (m *Manager) announce(id string, ev *failureEvent, before, after, cut int) {
	attrs := []any{"model", id, "cause", ev.f.Cause, "why", ev.f.Text}
	if ev.crash && after < before {
		attrs = append(attrs, "max_parallel_before", before, "max_parallel", after)
	}
	if cut > 0 {
		attrs = append(attrs, "gpu_budget_cut_mib", cut)
	}
	m.log.Warn("local model ended outside Cerberus; it reloads on the next request", attrs...)
	if m.notify == nil {
		return
	}
	body := id + ": " + ev.f.Text + ". It reloads on the next request"
	if cut > 0 {
		body += " with a smaller GPU share"
	}
	m.notify(notify.Message{Title: "Local model ended", Body: body + ".", Urgency: "normal",
		Kind: "engine", Level: "warn", Link: "#/models"})
}

// noteFailure records ev under m.mu, then announces it and, when its cause
// may still appear in railguard's history, looks again a little later.
func (m *Manager) noteFailure(ev *failureEvent) bool {
	m.mu.Lock()
	fresh, before, after := m.recordFailureLocked(ev)
	cut := ev.st.cutLocked(m.now())
	id := ev.st.info.ID
	m.mu.Unlock()
	if !fresh {
		return false
	}
	m.announce(id, ev, before, after, cut)
	if ev.pending && m.history != nil {
		st, gen := ev.st, ev.gen
		time.AfterFunc(m.reattributeDelay(), func() { m.reattribute(st, gen) })
	}
	return true
}

func (m *Manager) reattributeDelay() time.Duration {
	if m.reattributeEvery > 0 {
		return m.reattributeEvery
	}
	return reattributeAfter
}

// reattribute looks for railguard's line about a failure recorded without
// a cause. When it is there now, the failure is renamed, a crash slot cut
// it caused is given back, and a VRAM kill cuts the GPU share. It changes
// nothing once the model was loaded again or a minute has passed.
func (m *Manager) reattribute(st *modelState, gen uint64) {
	m.mu.Lock()
	if st.lastFail == nil || st.failGen != gen || !st.failPending {
		m.mu.Unlock()
		return
	}
	at, pid, from, backend := st.lastFail.Time, st.failPID, st.failFrom, st.info.Backend
	m.mu.Unlock()
	if m.now().Sub(at) > reattributeUntil {
		return
	}
	e, ok := m.attributeSince(from, at, pid, backend)
	if !ok {
		return
	}
	f := railguardFailure(e, e.Time)
	m.mu.Lock()
	if st.lastFail == nil || st.failGen != gen || !st.failPending {
		m.mu.Unlock()
		return
	}
	st.failPending = false
	st.lastFail = &f
	if st.failCrashCut && st.crashCut > 0 {
		st.crashCut-- // railguard's kill, not the engine's fault
		st.failCrashCut = false
	}
	if e.Kind == "vram" {
		m.addVRAMCutLocked(st)
	}
	cut := st.cutLocked(m.now())
	id := st.info.ID
	m.mu.Unlock()
	m.announce(id, &failureEvent{f: f, cut: e.Kind == "vram"}, 0, 0, cut)
}

// classify builds the failure event for a model whose engine ended at at.
// pid is the llama-server's (0: unknown); exit is its exit when known.
func (m *Manager) classify(st *modelState, gen uint64, at time.Time, pid int, exit *exitInfo, fallback Failure, crash bool) *failureEvent {
	return m.classifySince(time.Time{}, st, gen, at, pid, exit, fallback, crash)
}

// classifySince is classify for a death some time between from and at.
func (m *Manager) classifySince(from time.Time, st *modelState, gen uint64, at time.Time, pid int, exit *exitInfo, fallback Failure, crash bool) *failureEvent {
	m.mu.Lock()
	backend := st.info.Backend
	m.mu.Unlock()
	if e, ok := m.attributeSince(from, at, pid, backend); ok {
		return &failureEvent{st: st, gen: gen, f: railguardFailure(e, e.Time), cut: e.Kind == "vram", pid: pid}
	}
	if exit != nil {
		f := exitFailure(*exit)
		// A stop from outside is not the engine's fault: no slot cut. A
		// crash (SIGSEGV, SIGABRT, an exit code) is.
		return &failureEvent{st: st, gen: gen, f: f, crash: !exit.outside(), pending: exit.outside(), pid: pid}
	}
	return &failureEvent{st: st, gen: gen, f: fallback, crash: crash, pending: true, pid: pid, from: from}
}

// engineExited is the llama.cpp backend's hook: a llama-server Cerberus did
// not stop has ended.
func (m *Manager) engineExited(id string, e exitInfo) {
	m.mu.Lock()
	st := m.states[id]
	if st == nil || !st.loaded || !st.info.Local {
		m.mu.Unlock()
		return // a load that failed reports itself
	}
	gen := st.loadSeq
	m.mu.Unlock()
	m.noteFailure(m.classify(st, gen, e.Time, e.PID, &e, Failure{}, false))
}

// FailureText is the gateway's wording for a model whose engine just ended
// outside Cerberus (within the last minute), or "" when there is nothing
// better to say than its generic text. It matches engine.IsTransientText.
func (m *Manager) FailureText(id string) string {
	st := m.match(id)
	if st == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f := st.lastFail
	if f == nil || m.now().Sub(f.Time) > failureRecent {
		return ""
	}
	switch f.Cause {
	case "railguard-vram":
		why := strings.TrimPrefix(f.Text, "ended by Railguard: ")
		return "local engine ended by Railguard (" + why + "); reloading with a smaller GPU share"
	case "railguard-memory", "railguard-game", "signal":
		return "local engine " + f.Text + "; it will be reloaded"
	}
	return ""
}
