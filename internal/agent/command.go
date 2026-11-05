package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// captureMax is how much command output is kept: the head and the tail of
// 4×outputMax each, so a long build log still shows its final error.
const captureMax = 4 * outputMax

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// runCommand runs bash -c in the working directory, in its own process group,
// with stdin closed, until it exits, times out or the run is stopped.
func (t *toolbox) runCommand(ctx context.Context, a args) (string, error) {
	command := strings.TrimSpace(a.str("command"))
	if command == "" {
		return "", errors.New("no command given")
	}
	limit := int(t.timeout / time.Second)
	if limit <= 0 {
		limit = timeoutS
	}
	if n, ok := a.int("timeout"); ok && n != 0 {
		limit = n
	}
	limit = min(timeoutMax, max(1, limit))

	argv := []string{"bash", "-c", command}
	if t.jail && t.wrap == nil {
		return "", errors.New("commands are not available here: the server has no sandbox for them")
	}
	if t.wrap != nil {
		argv = t.wrap(argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = t.cwd
	cmd.Env = append(commandEnv(os.Environ()), "TERM=dumb", "PAGER=cat", "GIT_PAGER=cat", "NO_COLOR=1")
	out := &capture{max: captureMax}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 2 * time.Second // a background grandchild must not hold the pipe open forever
	// A session of its own, not just a group: the command has no
	// controlling terminal, so it cannot write to the agent's PTY through
	// /dev/tty (and forge Cerberus markers there). Its leftovers stay in
	// that session for reap.
	platform.NewSession(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("could not start bash: %v", err)
	}
	t.started(cmd.Process.Pid)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	timer := time.NewTimer(time.Duration(limit) * time.Second)
	defer timer.Stop()
	var why string
	select {
	case <-done:
	case <-timer.C:
		why = fmt.Sprintf("stopped after %d s (timeout)", limit)
	case <-ctx.Done():
		why = "stopped by the user"
	}
	if why != "" {
		platform.Terminate(cmd.Process, 1500*time.Millisecond, done)
		<-done
	}

	text := strings.TrimRight(reANSI.ReplaceAllString(out.String(), ""), " \t\r\n")
	code := cmd.ProcessState.ExitCode()
	switch {
	case why != "":
		return "", errors.New(strings.TrimSpace(text + "\n[" + why + "]"))
	case code != 0:
		return "", errors.New(strings.TrimSpace(fmt.Sprintf("%s\n[exit code %d]", text, code)))
	case text == "":
		return "(no output)", nil
	}
	return text, nil
}

// started remembers a command's session, for reap.
func (t *toolbox) started(pid int) {
	t.cmdMu.Lock()
	t.cmdSessions = append(t.cmdSessions, pid)
	t.cmdMu.Unlock()
}

// reap ends whatever the run's commands left behind (background jobs,
// nohup'd servers): each command led a session of its own, out of reach of
// the session manager's kill of the agent's session.
func (t *toolbox) reap() {
	t.cmdMu.Lock()
	sids := t.cmdSessions
	t.cmdSessions = nil
	t.cmdMu.Unlock()
	for _, sid := range sids {
		platform.KillSession(sid)
	}
}

// commandEnv is env without the agent's own credentials and Cerberus
// variables: commands (and the repo scripts they run) have no business
// talking to the gateway or the control API as this session.
func commandEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		// CERBERUS_PREVIEW_TAG stays: it carries no authority, it only marks
		// a dev server the command starts as this session's (live previews).
		if k == "OPENAI_API_KEY" || (strings.HasPrefix(k, "CERBERUS_") && k != "CERBERUS_PREVIEW_TAG") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// capture keeps the first and the last max bytes written to it.
type capture struct {
	mu      sync.Mutex
	max     int
	head    []byte
	tail    []byte // ring buffer once full
	pos     int
	dropped bool
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := c.max - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	for len(p) > 0 {
		if len(c.tail) < c.max {
			k := min(c.max-len(c.tail), len(p))
			c.tail = append(c.tail, p[:k]...)
			p = p[k:]
			continue
		}
		c.dropped = true
		k := copy(c.tail[c.pos:], p)
		c.pos = (c.pos + k) % c.max
		p = p[k:]
	}
	return n, nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	tail := append(append([]byte(nil), c.tail[c.pos:]...), c.tail[:c.pos]...)
	s := string(c.head)
	if c.dropped {
		s += "\n[... output cut ...]\n"
	}
	return strings.ToValidUTF8(s+string(tail), "�")
}
