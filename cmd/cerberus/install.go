package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

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
	out       io.Writer
	// audit records the install (the audit log keeps every state-changing
	// CLI action); nil records nothing (tests).
	audit func(event, detail string, targets ...string)
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
		systemctl: func(args ...string) error {
			cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
			cmd.Stdout, cmd.Stderr = out, out
			return cmd.Run()
		}}, nil
}

// install copies the binary to ~/.local/bin/cerberus (unless it already is
// that file), writes the unit, reloads systemd and enables and starts the
// service, restarting it when it was running an older binary.
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
	if err := in.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	wasActive := in.systemctl("is-active", "--quiet", unitName) == nil
	if err := in.systemctl("enable", "--now", unitName); err != nil {
		return fmt.Errorf("systemctl --user enable --now: %w", err)
	}
	if copied && wasActive {
		if err := in.systemctl("restart", unitName); err != nil {
			return fmt.Errorf("systemctl --user restart: %w", err)
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
