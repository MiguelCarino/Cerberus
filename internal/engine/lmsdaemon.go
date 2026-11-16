package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// LM Studio's daemon outside cerberus.service (SCOPE "Railguard contract
// and engine hardening (2026-11-15)").
//
// Any `lms` call starts LM Studio's daemon (llmster) when it is not
// running, inside the caller's cgroup, and the daemon stays there. Started
// from cerberus.service, a restart of Cerberus then SIGKILLs it (KillMode
// mixed kills what is left in the unit once the daemon exits); started
// from an agent's command scope, every model runs under that scope's
// limits. So before an lms call finds the daemon down, Cerberus starts it
// somewhere else, by engines.lmstudio.unit:
//
//   - "" or "auto": lmstudio.service when that user unit exists, else
//     `systemd-run --user --scope --collect --unit=cerberus-lmstudio
//     --slice=app.slice lms daemon up`, else the old way;
//   - "none": the old way (lms starts it in Cerberus' own cgroup);
//   - a unit name: that user unit, else the old way.
//
// Placement runs under its own deadline (lmsPlaceTimeout), not the lms
// call's: lmstudio.service may take a minute or more to start. A failed
// placement is not remembered for the run: the call goes on the old way,
// and the next call that finds the daemon down tries again, at most once
// per lmsPlaceRetry. On Shutdown Cerberus runs `lms daemon down` (after
// unloading what it loaded) only when llmster really runs in Cerberus' own
// cgroup, so the daemon is not SIGKILLed and a daemon in lmstudio.service,
// which other clients use, is left alone. Off Linux nothing changes.

const (
	lmsDefaultUnit  = "lmstudio.service"
	lmsScopeUnit    = "cerberus-lmstudio"
	lmsDaemonWait   = 20 * time.Second
	lmsDaemonPollMs = 200
	// lmsPlaceTimeout bounds one placement: lmstudio.service may take up
	// to its TimeoutStartSec (120 s, ExecStartPost waits for the GPU) to
	// start, then the daemon has lmsDaemonWait to come up.
	lmsPlaceTimeout = 120*time.Second + lmsDaemonWait
	// lmsPlaceRetry is how soon a failed placement is tried again.
	lmsPlaceRetry = time.Minute
)

// lmsDaemon is the daemon placement state of one lmStudio backend.
type lmsDaemon struct {
	// Injectable for tests. alive and inOwnCgroup are nil off Linux (no
	// placement, never `lms daemon down`).
	alive       func() bool
	inOwnCgroup func() bool // llmster runs in Cerberus' own cgroup (the old way)
	sys         func(ctx context.Context, name string, args ...string) ([]byte, error)
	canScope    func() bool
	now         func() time.Time

	failedAt time.Time       // the last placement that failed (retried after lmsPlaceRetry)
	ours     map[string]bool // identifiers Cerberus loaded (unloaded on Shutdown)
}

// newLMSDaemon is the real placement for this OS.
func newLMSDaemon() lmsDaemon {
	d := lmsDaemon{sys: runCommand, now: time.Now, ours: map[string]bool{}}
	if runtime.GOOS != "linux" || testing.Testing() {
		// A test binary never starts systemd units (a test sets fakes).
		return d
	}
	d.alive = func() bool {
		pid := llmsterPID()
		return pid > 0 && platform.ProcessAlive(pid)
	}
	d.inOwnCgroup = func() bool {
		pid := llmsterPID()
		own := platform.CgroupUnit(0)
		return pid > 0 && own != "" && platform.CgroupUnit(pid) == own
	}
	d.canScope = func() bool {
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return false
		}
		if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
			return true
		}
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(rt, "bus"))
		return err == nil
	}
	return d
}

// daemonUp reports whether LM Studio's daemon runs (true when unknown).
func (b *lmStudio) daemonUp() bool {
	return b.daemon.alive == nil || b.daemon.alive()
}

// ensureDaemon starts LM Studio's daemon outside Cerberus' cgroup when it
// is down (see the top of this file). Called before every lms call except
// `lms daemon ...`, with the call's parent context (placement keeps its own
// deadline); it costs one file read when the daemon runs.
func (b *lmStudio) ensureDaemon(ctx context.Context, bin string) {
	if b.daemonUp() {
		return
	}
	b.daemonMu.Lock()
	defer b.daemonMu.Unlock()
	if b.daemonUp() {
		return
	}
	mode := strings.TrimSpace(b.lc.Unit)
	if mode == "" {
		mode = "auto"
	}
	if mode == "none" {
		return // the old way, on purpose
	}
	now := b.daemonNow()
	if !b.daemon.failedAt.IsZero() && now.Sub(b.daemon.failedAt) < lmsPlaceRetry {
		return // failed a moment ago: this call goes the old way
	}
	// A slow unit start must not fail because the lms call's own timeout
	// ran out, nor stop half-way because the caller went away.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lmsPlaceTimeout)
	defer cancel()
	if b.placeDaemon(pctx, mode, bin) {
		b.daemon.failedAt = time.Time{}
		return
	}
	b.daemon.failedAt = now
	b.log.Info("LM Studio's daemon starts inside Cerberus' own service this time; placement is tried again later",
		"retry_after", lmsPlaceRetry.String())
}

// placeDaemon tries the unit, then the scope. Caller holds daemonMu.
func (b *lmStudio) placeDaemon(ctx context.Context, mode, bin string) bool {
	unit := mode
	if mode == "auto" {
		unit = lmsDefaultUnit
	}
	if b.unitExists(ctx, unit) {
		if _, err := b.daemon.sys(ctx, "systemctl", "--user", "start", unit); err != nil {
			b.log.Warn("could not start LM Studio's unit", "unit", unit, "err", err)
		} else if b.waitDaemon(ctx) {
			b.log.Info("started LM Studio's daemon in its own unit", "unit", unit)
			return true
		} else {
			b.log.Warn("LM Studio's unit started but its daemon did not come up", "unit", unit)
		}
	}
	if b.daemon.canScope != nil && b.daemon.canScope() {
		_, err := b.daemon.sys(ctx, "systemd-run", "--user", "--scope", "--collect", "--quiet",
			"--unit="+lmsScopeUnit, "--slice=app.slice", bin, "daemon", "up")
		if err == nil && b.waitDaemon(ctx) {
			b.log.Info("started LM Studio's daemon in its own scope", "scope", lmsScopeUnit+".scope")
			return true
		}
		b.log.Warn("could not start LM Studio's daemon in its own scope", "err", err)
	}
	return false
}

func (b *lmStudio) daemonNow() time.Time {
	if b.daemon.now != nil {
		return b.daemon.now()
	}
	return time.Now()
}

// unitExists reports whether a user unit is installed.
func (b *lmStudio) unitExists(ctx context.Context, unit string) bool {
	out, err := b.daemon.sys(ctx, "systemctl", "--user", "show", "-p", "LoadState", "--value", unit)
	return err == nil && strings.TrimSpace(string(out)) == "loaded"
}

// waitDaemon waits for the daemon to come up.
func (b *lmStudio) waitDaemon(ctx context.Context) bool {
	wait, cancel := context.WithTimeout(ctx, lmsDaemonWait)
	defer cancel()
	t := time.NewTicker(lmsDaemonPollMs * time.Millisecond)
	defer t.Stop()
	for !b.daemonUp() {
		select {
		case <-wait.Done():
			return false
		case <-t.C:
		}
	}
	return true
}

// noteLoaded and noteUnloaded keep the identifiers Cerberus loaded.
func (b *lmStudio) noteLoaded(id string) {
	b.daemonMu.Lock()
	b.daemon.ours[id] = true
	b.daemonMu.Unlock()
}

func (b *lmStudio) noteUnloaded(id string) {
	b.daemonMu.Lock()
	delete(b.daemon.ours, id)
	b.daemonMu.Unlock()
}

// Shutdown unloads the models Cerberus loaded in LM Studio, then stops the
// daemon when it runs in Cerberus' own cgroup, all within ctx
// (shutdownGrace). It never starts the daemon: when it is down there is
// nothing to unload.
func (b *lmStudio) Shutdown(ctx context.Context) {
	b.daemonMu.Lock()
	ids := make([]string, 0, len(b.daemon.ours))
	for id := range b.daemon.ours {
		ids = append(ids, id)
	}
	b.daemonMu.Unlock()
	slices.Sort(ids)
	bin := b.binary()
	if bin == "" || !b.daemonUp() {
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		c, cancel := context.WithTimeout(ctx, lmsCmdTimeout)
		_, err := b.run(c, bin, "unload", id)
		cancel()
		if err != nil {
			b.log.Warn("unload on shutdown failed", "backend", BackendLMStudio, "model", id, "err", err)
			continue
		}
		b.noteUnloaded(id)
		b.log.Info("model unloaded on shutdown", "backend", BackendLMStudio, "model", id)
	}
	if b.daemon.inOwnCgroup != nil && b.daemon.inOwnCgroup() && ctx.Err() == nil {
		c, cancel := context.WithTimeout(ctx, lmsCmdTimeout)
		_, err := b.run(c, bin, "daemon", "down")
		cancel()
		if err != nil {
			b.log.Warn("could not stop LM Studio's daemon", "err", err)
			return
		}
		b.log.Info("stopped LM Studio's daemon, which ran in Cerberus' own cgroup")
	}
}
