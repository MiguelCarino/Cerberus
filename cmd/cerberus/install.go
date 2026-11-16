package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/packaging"
)

const unitName = "cerberus.service"

// installer installs the binary and the systemd user unit. Its fields are
// the environment, so tests can point it at temp dirs and a fake systemctl.
type installer struct {
	goos      string
	exe       string // the running binary
	home      string
	configDir string // os.UserConfigDir()
	systemctl func(args ...string) error
	// query runs `systemctl --user ARGS` and returns its trimmed output
	// (the pending job of the unit, for example).
	query func(args ...string) (string, error)
	// jobWait is how long a queued start or restart may wait in systemd's
	// job queue before it is cancelled; poll is the step between looks.
	// Zero values mean 30 s and 500 ms.
	jobWait, poll time.Duration
	sleep         func(time.Duration) // nil = time.Sleep (tests pass a fake)
	out           io.Writer
	// audit records the install (the audit log keeps every state-changing
	// CLI action); nil records nothing (tests).
	audit func(event, detail string, targets ...string)
	// guard is guard.max_sessions and guard.session_memory_max, which size
	// the sessions slice; nil means the defaults (3 x 6G).
	guard func() (int, string)
}

func installCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	in, err := defaultInstaller(stdout)
	if err == nil {
		in.audit = func(event, detail string, targets ...string) { cliAudit(stderr, event, detail, targets...) }
		err = in.install()
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus install-service:", err)
		return 1
	}
	return 0
}

func defaultInstaller(out io.Writer) (*installer, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &installer{goos: runtime.GOOS, exe: exe, home: home, configDir: cfgDir, out: out,
		guard: func() (int, string) {
			c, _, err := loadConfigFile("")
			if err != nil {
				c = config.Defaults()
			}
			return c.Guard.MaxSessions, c.Guard.SessionMemoryMax
		},
		systemctl: func(args ...string) error {
			ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
			cmd.Stdout, cmd.Stderr = out, out
			return cmd.Run()
		},
		query: func(args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
			defer cancel()
			raw, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Output()
			return strings.TrimSpace(string(raw)), err
		}}, nil
}

// systemctlTimeout bounds every systemctl call install-service makes. None
// of them blocks on a job (start and restart are queued with --no-block),
// so a call that takes this long means the user manager itself is stuck.
const systemctlTimeout = 20 * time.Second

// awaitJob waits until the unit has no pending job. A start or restart that
// is still waiting in the job queue after jobWait is cancelled: on 2026-11-15
// a restart queued right after a reload never ran, and every later job of the
// user manager (agent command scopes, new terminals, timers) queued behind it
// until the manager was re-executed. Cancelling it lets the queue move again.
func (in *installer) awaitJob(what string) error {
	wait, poll, sleep := in.jobWait, in.poll, in.sleep
	if wait <= 0 {
		wait = 30 * time.Second
	}
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	job := ""
	for waited := time.Duration(0); ; waited += poll {
		var err error
		job, err = in.query("show", "-p", "Job", "--value", unitName)
		if err != nil {
			return fmt.Errorf("systemctl --user show %s: %w", unitName, err)
		}
		if job == "" || job == "0" {
			return nil
		}
		if waited >= wait {
			break
		}
		sleep(poll)
	}
	id, _, _ := strings.Cut(job, " ")
	if id != "" {
		_ = in.systemctl("cancel", id)
	}
	return fmt.Errorf("the %s of %s was still waiting in systemd's job queue after %s, so it was cancelled "+
		"to keep the queue moving; the new files are in place. Run `systemctl --user %s %s` yourself; "+
		"if that hangs too, `systemctl --user list-jobs` shows the job it waits for", what, unitName, wait, what, unitName)
}

// install copies the binary to ~/.local/bin/cerberus (unless it already is
// that file), writes the unit and the sessions slice
// (cerberus-sessions.slice, sized from the config), reloads systemd once and
// enables the service, then starts it (or restarts it when it was running an
// older binary) with --no-block and watches the job with a deadline
// (awaitJob). The slice is used once guard.systemd_slice names it.
func (in *installer) install() (err error) {
	if in.goos != "linux" {
		return errors.New("installing a service is not supported yet on " + in.goos)
	}
	unit := filepath.Join(in.configDir, "systemd", "user", unitName)
	dst := filepath.Join(in.home, ".local", "bin", "cerberus")
	defer func() {
		if in.audit == nil {
			return
		}
		detail := "installed and started the user service"
		if err != nil {
			detail = "failed: " + err.Error()
		}
		in.audit("service.install", detail, unit, dst)
	}()
	copied, err := installBinary(in.exe, dst)
	if err != nil {
		return err
	}
	if copied {
		fmt.Fprintln(in.out, "installed", dst)
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unit, []byte(packaging.Unit), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(in.out, "wrote", unit)
	n, per := 3, "6G"
	if in.guard != nil {
		n, per = in.guard()
	}
	slice := filepath.Join(filepath.Dir(unit), packaging.SessionSliceName)
	if err := os.WriteFile(slice, []byte(packaging.SessionSliceFor(n, per)), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(in.out, "wrote", slice)
	if err := in.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	wasActive := in.systemctl("is-active", "--quiet", unitName) == nil
	// --no-reload: the reload above already saw the new files, and a second
	// reload right before a restart is what preceded the stuck queue.
	if err := in.systemctl("enable", "--no-reload", unitName); err != nil {
		return fmt.Errorf("systemctl --user enable: %w", err)
	}
	// Start or restart without blocking, then watch the job with a deadline,
	// so a stuck job queue never hangs this command or the user manager.
	what := ""
	switch {
	case !wasActive:
		what = "start"
	case copied:
		what = "restart"
	}
	if what != "" {
		if err := in.systemctl(what, "--no-block", unitName); err != nil {
			return fmt.Errorf("systemctl --user %s: %w", what, err)
		}
		if err := in.awaitJob(what); err != nil {
			return err
		}
		if in.systemctl("is-active", "--quiet", unitName) != nil {
			return fmt.Errorf("%s did not stay up after the %s; `journalctl --user -u %s` says why", unitName, what, unitName)
		}
	}
	fmt.Fprintln(in.out, "cerberus is running; `cerberus url` prints the link for your phone")
	return nil
}

// installBinary copies src to dst atomically (temp file + rename, so a
// running copy is never overwritten in place). It does nothing when dst is
// already src.
func installBinary(src, dst string) (bool, error) {
	if a, err := os.Stat(src); err != nil {
		return false, err
	} else if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".cerberus-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), dst)
}
