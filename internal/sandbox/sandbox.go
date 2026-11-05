// Package sandbox runs agent sessions under bubblewrap (bwrap) on Linux.
//
// A sandboxed session sees the whole filesystem read-only (--ro-bind / /)
// with a fresh /dev, /proc and an empty /tmp. Only its workspace, its
// session folder and the profile's or config's read-write extras are
// writable. Sensitive home folders (HideHome: ~/.ssh, ~/.gnupg, Cerberus'
// own config, keyrings, browser profiles …) are replaced by empty tmpfs
// mounts, and the network is unshared when Network is false (which also
// cuts the agent off from the gateway, so local models through the gateway
// need Network true).
//
// The sandbox is composed inside the systemd-run scope: scope outside,
// bwrap inside, the agent's command last. When bwrap is wanted but missing
// (or unusable, e.g. user namespaces are disabled) the session manager
// falls back to no sandbox with a feed and audit warning, or refuses to
// start the session when Sandbox.Strict is set.
//
// A confined spec (a non-admin user's session or chat command) also puts
// an empty tmpfs over the whole home folder, so nothing of the admin's home
// (other repos, ~/.config/gh, ~/.aws, shell history …) is readable; only
// KeepRO, the read-only extras (sandbox.extra_ro: toolchains under the
// home), the profile's own SandboxRW, the workspace and the session folder
// are bound back. The agent's executable, when it lives under the home, is
// bound back read-only by Wrap.
//
// What it does not do: it is not a network firewall (with Network true the
// agent reaches the LAN and abstract Unix sockets), it does not limit CPU
// or memory (the systemd scope does), and anything readable on the host
// that is not hidden stays readable.
package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
)

// Modes.
const (
	ModeNone  = "none"
	ModeBwrap = "bwrap"
)

// Normalize maps a configured mode to ModeNone, ModeBwrap or "" (inherit).
// Unknown words fail closed: anything that is not empty or an explicit
// "off" means bwrap.
func Normalize(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		return ""
	case ModeNone, "off", "false", "no":
		return ModeNone
	}
	return ModeBwrap
}

// Resolve picks a session's mode: the repo's override, else the profile's,
// else the config default, else none.
func Resolve(cfg config.SandboxConfig, profile config.AgentProfile, repo string) string {
	for _, m := range []string{repo, profile.Sandbox, cfg.Default} {
		if n := Normalize(m); n != "" {
			return n
		}
	}
	return ModeNone
}

// Spec is everything one bwrap invocation needs. Paths may start with "~"
// (expanded against Home); relative paths are ignored.
type Spec struct {
	Home         string
	Workspace    string   // bound read-write at the same path
	SessionDir   string   // bound read-write at the same path
	Network      bool     // false: --unshare-net
	ReadOnlyHome bool     // false: the home folder is bound read-write
	ExtraRO      []string // extra read-only binds (useful under a hidden folder)
	ExtraRW      []string // extra read-write binds (config and profile)
	HideHome     []string // replaced with an empty tmpfs (a file is masked with /dev/null)
	Hide         []string // more hidden paths (Cerberus' data folder, the user runtime folder …)
	Protect      []string // bound read-only last, over anything writable (e.g. a config snapshot in the session folder)
	// Confined is a non-admin user's session (Phase 4a): the home folder is
	// replaced by an empty tmpfs (whatever ReadOnlyHome says) and only
	// KeepRO, ExtraRO, ExtraRW, the workspace and the session folder are
	// bound back. Confine also drops the global extra_rw binds.
	Confined bool
	// KeepRO are paths bound back read-only into a confined home (the
	// agent's executable, a shared repo's git folder). Ignored otherwise:
	// without Confined the whole filesystem is readable anyway.
	KeepRO []string
}

// Confine returns s for a non-admin user's session: Confined and a
// read-only home, and of the writable extras only the agent profile's own
// SandboxRW (the global sandbox.extra_rw is dropped). Read-only extras and
// hidden folders stay as configured.
func (s Spec) Confine(profile config.AgentProfile) Spec {
	s.Confined = true
	s.ReadOnlyHome = true
	s.ExtraRW = slices.Clone(profile.SandboxRW)
	return s
}

// FromConfig fills a Spec's policy fields from the config defaults and the
// profile's SandboxRW. The caller sets Workspace, SessionDir and Hide.
func FromConfig(cfg config.SandboxConfig, profile config.AgentProfile, home string) Spec {
	return Spec{
		Home:         home,
		Network:      cfg.Network,
		ReadOnlyHome: cfg.ReadOnlyHome,
		ExtraRO:      slices.Clone(cfg.ExtraRO),
		ExtraRW:      append(slices.Clone(cfg.ExtraRW), profile.SandboxRW...),
		HideHome:     slices.Clone(cfg.HideHome),
	}
}

// Expand turns "~" and "~/x" into paths under home and cleans the result.
// It returns "" for a relative path or an unexpandable "~user".
func Expand(p, home string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return ""
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		if home == "" {
			return ""
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

// statFn is os.Stat; tests swap it to build argv for paths that need not
// exist.
var statFn = os.Stat

// Args returns the bwrap argv (bwrap first, ending in "--") for s. Binds of
// paths that do not exist are left out, as bwrap would fail on them.
// Later mounts win over earlier ones, so the order is: the read-only root,
// /dev, /proc, /tmp, the home folder (if writable), extras, hidden paths,
// the workspace and session folder, then the protected files.
func Args(bwrap string, s Spec) []string {
	a := []string{bwrap,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	seenTmp := map[string]bool{"/tmp": true}
	exists := func(p string) (os.FileInfo, bool) {
		if p == "" {
			return nil, false
		}
		st, err := statFn(p)
		return st, err == nil
	}
	bind := func(flag string, raw string) {
		p := Expand(raw, s.Home)
		if _, ok := exists(p); ok && p != "/" {
			a = append(a, flag, p, p)
		}
	}
	if !s.ReadOnlyHome && !s.Confined && s.Home != "" {
		bind("--bind", s.Home)
	}
	if home := Expand(s.Home, ""); s.Confined && home != "" && home != "/" {
		// Deny by default: nothing of the home is visible but what is
		// bound back below.
		seenTmp[home] = true
		a = append(a, "--tmpfs", home)
		for _, p := range s.KeepRO {
			bind("--ro-bind", p)
		}
	}
	for _, p := range s.ExtraRO {
		bind("--ro-bind", p)
	}
	for _, p := range s.ExtraRW {
		bind("--bind", p)
	}
	for _, raw := range append(slices.Clone(s.HideHome), s.Hide...) {
		p := Expand(raw, s.Home)
		st, ok := exists(p)
		if !ok || p == "/" || seenTmp[p] {
			continue
		}
		if st.IsDir() {
			seenTmp[p] = true
			a = append(a, "--tmpfs", p)
		} else {
			a = append(a, "--ro-bind", os.DevNull, p) // a file cannot hold a tmpfs
		}
	}
	bind("--bind", s.Workspace)
	bind("--bind", s.SessionDir)
	for _, p := range s.Protect {
		bind("--ro-bind", p)
	}
	if !s.Network {
		a = append(a, "--unshare-net")
	}
	// A pid namespace makes the agent's whole tree die with bwrap: with
	// --new-session the agent leaves the PTY's process group, so a stop
	// reaches it only through bwrap.
	a = append(a, "--unshare-pid", "--die-with-parent", "--new-session")
	if s.Workspace != "" {
		if p := Expand(s.Workspace, s.Home); p != "" {
			a = append(a, "--chdir", p)
		}
	}
	return append(a, "--")
}

// Wrap returns argv run inside bwrap. In a confined spec the command's
// executable is bound back read-only when it lives under the hidden home.
func Wrap(bwrap string, s Spec, argv []string) []string {
	if s.Confined && len(argv) > 0 {
		s.KeepRO = append(slices.Clone(s.KeepRO), executablePaths(argv[0], s.Home)...)
	}
	return append(Args(bwrap, s), argv...)
}

// lookPath is exec.LookPath; tests swap it.
var lookPath = exec.LookPath

// executablePaths is the program name resolved on the host, and its
// symlink target, for those under home.
func executablePaths(name, home string) []string {
	home = Expand(home, "")
	if home == "" {
		return nil
	}
	p, err := lookPath(name)
	if err != nil {
		return nil
	}
	if p, err = filepath.Abs(p); err != nil {
		return nil
	}
	var out []string
	for _, c := range []string{p, evalSymlinks(p)} {
		if c != "" && strings.HasPrefix(c, home+string(filepath.Separator)) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func evalSymlinks(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return r
}

var (
	probeMu      sync.Mutex
	probeCache   = map[string]error{}
	probeRunning = map[string]chan struct{}{}
	probeFn      = Probe
)

// probeTimeout bounds one probe; a probe cut short by it is not cached.
var probeTimeout = 10 * time.Second

// Available returns the bwrap binary if sandboxing can work here: Linux,
// bwrap on PATH, and a trivial sandbox starts (user namespaces may be
// disabled). Only a definite result is cached per binary path for the
// life of the process: the probe runs detached from the caller's context
// (so an aborted HTTP request cannot poison it), concurrent callers share
// one run without holding a lock across it, and a timed-out probe is
// retried by the next caller.
func Available(ctx context.Context) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("bwrap sandboxing is Linux only")
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return "", errors.New("bwrap is not installed (dnf install bubblewrap / apt install bubblewrap)")
	}
	return bin, probeOnce(ctx, bin)
}

// probeOnce runs (or joins) the probe for bin. The caller's ctx only ends
// the caller's wait, never the probe itself.
func probeOnce(ctx context.Context, bin string) error {
	for {
		probeMu.Lock()
		if err, ok := probeCache[bin]; ok {
			probeMu.Unlock()
			return err
		}
		if ch, ok := probeRunning[bin]; ok {
			probeMu.Unlock()
			select {
			case <-ch:
				probeMu.Lock()
				err, ok := probeCache[bin]
				probeMu.Unlock()
				if ok {
					return err
				}
				// The shared run was inconclusive; try again.
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		ch := make(chan struct{})
		probeRunning[bin] = ch
		probeMu.Unlock()

		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
		err := probeFn(pctx, bin)
		definite := pctx.Err() == nil
		cancel()

		probeMu.Lock()
		if definite {
			probeCache[bin] = err
		}
		delete(probeRunning, bin)
		close(ch)
		probeMu.Unlock()
		return err
	}
}

// Probe runs `bwrap --ro-bind / / --dev /dev --proc /proc --unshare-pid
// --die-with-parent true` and reports why it failed, if it did.
func Probe(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--unshare-pid", "--die-with-parent", "--", "true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return errors.New("bwrap cannot start a sandbox: " + msg)
	}
	return nil
}
