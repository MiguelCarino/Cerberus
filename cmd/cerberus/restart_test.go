//go:build unix

package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestHelperServe is not a test: the restart tests run this test binary as
// `cerberus serve` (re-exec in place runs it again with the same args).
func TestHelperServe(t *testing.T) {
	if os.Getenv("CERBERUS_TEST_SERVE_CHILD") != "1" {
		t.Skip("helper process only")
	}
	os.Exit(run([]string{"serve", "-config", os.Getenv("CERBERUS_TEST_CONFIG")}, os.Stdin, os.Stdout, os.Stderr))
}

// restartFixture writes a quiet config on a free port in temp folders.
func restartFixture(t *testing.T) (env []string, base, dataDir string) {
	t.Helper()
	tmp := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg := config.Defaults()
	cfg.Server.Listen = addr
	cfg.Server.Token = "tok"
	cfg.GPU.Backend = "none"
	cfg.Engines.LlamaCpp.Enabled = false
	cfg.Engines.LMStudio.Enabled = false
	cfg.Engines.Ollama.Enabled = false
	cfg.Notify.Disabled = true
	cfg.RepoRoots = nil
	cfg.Vault.Backend = "file"
	cfg.Subscriptions = nil
	cfg.Voice.Enabled = false
	cfg.Monitor.Enabled = false
	path := filepath.Join(tmp, "config", "cerberus", "config.json")
	cfg.SetPath(path)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	env = append(cleanEnv(), "HOME="+tmp, "XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
		"XDG_DATA_HOME="+filepath.Join(tmp, "data"), "XDG_RUNTIME_DIR="+filepath.Join(tmp, "run"),
		"CERBERUS_TEST_SERVE_CHILD=1", "CERBERUS_TEST_CONFIG="+path)
	return env, "http://" + addr, filepath.Join(tmp, "data", "cerberus")
}

// cleanEnv is the environment without the variables the fixture sets (and
// without INVOCATION_ID, in case the tests themselves run under systemd).
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "INVOCATION_ID", supervisorEnv,
			"CERBERUS_TEST_SERVE_CHILD", "CERBERUS_TEST_CONFIG":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// child is one daemon process started by the test supervisor.
type child struct {
	cmd   *exec.Cmd
	ready chan struct{} // one value per "cerberus ready" log line
	done  chan error    // gets the exit once
	ended chan struct{} // closed at exit
}

func startChild(t *testing.T, env []string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServe$")
	cmd.Env = env
	pr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd, ready: make(chan struct{}, 4), done: make(chan error, 1), ended: make(chan struct{})}
	lines := make(chan struct{})
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "cerberus ready") {
				c.ready <- struct{}{}
			}
		}
	}()
	go func() { <-lines; err := cmd.Wait(); close(c.ended); c.done <- err }()
	t.Cleanup(func() {
		select {
		case <-c.ended:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM) // only our own child
		select {
		case <-c.ended:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return c
}

func (c *child) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-c.ready:
	case err := <-c.done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("daemon not ready")
	}
}

func postRestart(t *testing.T, base string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/config/restart", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restart status %d", resp.StatusCode)
	}
}

func healthy(t *testing.T, base string) {
	t.Helper()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
}

func auditCount(t *testing.T, dataDir, event string) int {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(dataDir, "audit.jsonl"))
	return strings.Count(string(raw), `"`+event+`"`)
}

// TestRestartInPlace: without systemd the daemon shuts down cleanly and
// re-executes itself: same pid, a second start, and it serves again.
func TestRestartInPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the daemon twice")
	}
	env, base, data := restartFixture(t)
	c := startChild(t, env)
	c.waitReady(t)
	healthy(t, base)
	pid := c.cmd.Process.Pid
	postRestart(t, base)
	c.waitReady(t) // the re-executed process logs ready again
	healthy(t, base)
	select {
	case err := <-c.done:
		t.Fatalf("process exited instead of re-executing: %v", err)
	default:
	}
	if c.cmd.Process.Pid != pid {
		t.Fatal("pid changed")
	}
	if n := auditCount(t, data, "serve.start"); n != 2 {
		t.Fatalf("serve.start records = %d, want 2", n)
	}
	if n := auditCount(t, data, "serve.restart"); n != 1 {
		t.Fatalf("serve.restart records = %d, want 1", n)
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("clean stop after restart: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no stop")
	}
}

// TestRestartInheritedInvocationID: INVOCATION_ID alone (inherited from a
// terminal that runs as a systemd unit) is not systemd supervising the
// daemon: it re-executes in place instead of exiting with no supervisor.
func TestRestartInheritedInvocationID(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the daemon twice")
	}
	env, base, _ := restartFixture(t)
	env = append(env, "INVOCATION_ID=inherited-from-terminal")
	c := startChild(t, env)
	c.waitReady(t)
	pid := c.cmd.Process.Pid
	postRestart(t, base)
	c.waitReady(t)
	healthy(t, base)
	select {
	case err := <-c.done:
		t.Fatalf("process exited instead of re-executing: %v", err)
	default:
	}
	if c.cmd.Process.Pid != pid {
		t.Fatal("pid changed")
	}
}

// TestRestartUnderSystemd: with the unit's marker and INVOCATION_ID set the daemon exits with
// restartExitCode and the supervisor (here the test, as systemd's
// Restart=on-failure) starts it again.
func TestRestartUnderSystemd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the daemon twice")
	}
	env, base, data := restartFixture(t)
	env = append(env, "INVOCATION_ID=test-supervisor", supervisorEnv+"=systemd")
	c := startChild(t, env)
	c.waitReady(t)
	postRestart(t, base)
	var err error
	select {
	case err = <-c.done:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not exit for its supervisor")
	}
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != restartExitCode {
		t.Fatalf("exit = %v, want code %d", err, restartExitCode)
	}
	// The supervisor restarts the unit.
	c2 := startChild(t, env)
	c2.waitReady(t)
	healthy(t, base)
	if n := auditCount(t, data, "serve.start"); n != 2 {
		t.Fatalf("serve.start records = %d, want 2", n)
	}
}
