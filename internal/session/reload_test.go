//go:build !windows

package session

import (
	"context"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestReloadGuardAndProfiles proves live reload by behaviour: a session
// queued by guard.max_sessions starts once Reload raises the limit, and a
// profile added by Reload can start a session without a restart.
func TestReloadGuardAndProfiles(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "sleepy", Command: []string{"bash", e.script("sleepy.sh", "exec sleep 60\n")}})
		e.cfg.Guard.MaxSessions = 1
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "fan", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(infos[0].ID, StateRunning)
	time.Sleep(100 * time.Millisecond)
	if q, _ := e.m.Get(infos[1].ID); q.State != StateQueued {
		t.Fatalf("second session %s, want queued", q.State)
	}

	if _, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "x", Agent: "later"}); err == nil {
		t.Fatal("unknown profile accepted before reload")
	}

	next := e.cfg.Clone()
	next.Guard.MaxSessions = 3
	next.Agents = append(next.Agents, config.AgentProfile{Name: "later", Command: []string{"bash", e.script("later.sh", "exec sleep 60\n")}})
	e.m.Reload(next)
	e.waitState(infos[1].ID, StateRunning) // admitted by the new limit

	later, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "x", Agent: "later"})
	if err != nil {
		t.Fatalf("reloaded profile: %v", err)
	}
	e.waitState(later[0].ID, StateRunning)
	for _, id := range []string{infos[0].ID, infos[1].ID, later[0].ID} {
		_ = e.m.Stop(id)
		e.waitState(id, StateStopped)
	}
	e.m.Reload(nil)
}
