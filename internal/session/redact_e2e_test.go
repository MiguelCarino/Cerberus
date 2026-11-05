//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// A PEM-like secret printed through the real PTY (LF becomes CRLF) and a
// value split by grep-style colours are masked live, in the scrollback and
// in the transcript.
func TestMultiLineAndColouredSecretsThroughPTY(t *testing.T) {
	const pem = "-----BEGIN TEST KEY-----\nMIIEabcdefghijklmnop\nqrstuvwxyz0123456789\n-----END TEST KEY-----"
	const tok = "sk-proj-AbC123xyz"
	e := newTestEnv(t, nil, func(e *testEnv) {
		withFileVault(e, map[string]string{"deploy-key": pem, "tok": tok})
		agent := e.script("leaky.sh", `printf '%s\n' "$DEPLOY_KEY"
printf 'K=\033[01;31m\033[Ksk-\033[m\033[Kproj-AbC123xyz\n'
echo finished
exit 0
`)
		e.profiles(config.AgentProfile{Name: "leaky", Command: []string{"bash", agent},
			SecretEnv: map[string]string{"DEPLOY_KEY": "deploy-key", "TOK": "tok"}})
	})
	id := e.createOne("leak")
	e.waitState(id, StatePassed)
	sb, _ := os.ReadFile(filepath.Join(e.m.sessDir, id, "scrollback.log"))
	tx, err := e.m.Transcript(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sb), "finished") {
		t.Fatalf("no output: %q", sb)
	}
	for name, got := range map[string]string{"scrollback": string(sb), "transcript": string(tx)} {
		for _, leak := range []string{"MIIEabcdef", "qrstuvwxyz", "AbC123xyz"} {
			if strings.Contains(got, leak) {
				t.Fatalf("%s leaks %q: %q", name, leak, got)
			}
		}
	}
}

// errAfterRelease is a context that reports cancellation the first time it
// sees sessions[0] released by Prune, after marking it busy again the way an
// Accept starting right then would.
type errAfterRelease struct {
	context.Context
	m    *Manager
	s    *session
	once sync.Once
	hit  bool
}

func (c *errAfterRelease) Err() error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if !c.s.op && !c.hit {
		c.once.Do(func() { c.s.op, c.hit = true, true })
	}
	if c.hit {
		return context.Canceled
	}
	return nil
}

// A cancelled Prune must not clear op on a session it already released
// (an Accept may hold it by then).
func TestPruneCancelLeavesReleasedSessions(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		dirty := e.script("dirty.sh", "echo work > work.txt\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "dirty", Command: []string{"bash", dirty}})
	})
	a, b := e.createOne("a"), e.createOne("b")
	e.waitState(a, StatePassed)
	e.waitState(b, StatePassed)
	e.m.mu.Lock()
	for _, id := range []string{a, b} {
		e.m.sessions[id].info.Updated = time.Now().Add(-40 * 24 * time.Hour)
	}
	first := e.m.sessions[e.m.order[0]]
	e.m.mu.Unlock()
	ctx := &errAfterRelease{Context: context.Background(), m: e.m, s: first}
	if _, err := e.m.Prune(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("prune err %v", err)
	}
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if !first.op {
		t.Fatal("Prune cleared op on a session it had already released")
	}
	for _, id := range e.m.order {
		if s := e.m.sessions[id]; s != first && s.op {
			t.Fatalf("session %s still held after the cancelled prune", id)
		}
	}
}
