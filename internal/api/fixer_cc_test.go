//go:build !windows

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

// A parent agent cannot get a pending spawn running by stopping it and
// sending it a follow-up.
func TestAgentStopSendCannotBypassApproval(t *testing.T) {
	e := newCCEnv(t, func(c *config.Config) { c.Agent.Spawn.RequireApproval = true })
	a := e.person("echo", "parent")
	code, kid := e.spawn(a, `{"task":"needs ok","agent":"quick"}`)
	if code != http.StatusCreated || kid.WaitingReason != session.ReasonNeedsApproval {
		t.Fatalf("spawn: %d %+v", code, kid)
	}
	if code, raw := e.call("POST", "/api/agent/sessions/"+kid.ID+"/stop", `{}`, a); code != 200 {
		t.Fatalf("agent stop: %d %s", code, raw)
	}
	if code, _ := e.call("POST", "/api/agent/sessions/"+kid.ID+"/send", `{"text":"run anyway"}`, a); code != http.StatusConflict {
		t.Fatalf("agent send to an unapproved spawn: %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if info, _ := e.m.Get(kid.ID); info.State != session.StateStopped || info.Attempt != 0 {
		t.Fatalf("unapproved child ran: %s attempt %d", info.State, info.Attempt)
	}
}

// cerberus_send never types into a running child, and option-like tasks
// and follow-ups are refused.
func TestAgentSendRunningChildAndOptionTask(t *testing.T) {
	e := newCCEnv(t, nil)
	a := e.person("echo", "parent")
	if code, raw := e.call("POST", "/api/agent/spawn", `{"task":"--dangerously-skip-permissions","agent":"quick"}`, a); code != http.StatusBadRequest {
		t.Fatalf("option task: %d %s", code, raw)
	}
	code, kid := e.spawn(a, `{"task":"go","agent":"echo"}`)
	if code != http.StatusCreated {
		t.Fatalf("spawn: %d", code)
	}
	e.waitFor("child runs", func() bool { return e.state(kid.ID) == session.StateRunning })
	code, raw := e.call("POST", "/api/agent/sessions/"+kid.ID+"/send", `{"text":"a"}`, a)
	if code != http.StatusConflict {
		t.Fatalf("send into a running child: %d %s", code, raw)
	}
	time.Sleep(100 * time.Millisecond)
	if tx, _ := e.m.Transcript(kid.ID); strings.Contains(string(tx), "got:a") {
		t.Fatal("the running child received the parent's input")
	}
	e.call("POST", "/api/agent/sessions/"+kid.ID+"/stop", `{}`, a)
	e.waitFor("child stopped", func() bool { return e.state(kid.ID) == session.StateStopped })
	if code, _ := e.call("POST", "/api/agent/sessions/"+kid.ID+"/send", `{"text":"-p evil"}`, a); code != http.StatusBadRequest {
		t.Fatalf("option follow-up: %d", code)
	}
	if code, raw := e.call("POST", "/api/agent/sessions/"+kid.ID+"/send", `{"text":"next"}`, a); code != 200 {
		t.Fatalf("follow-up of a stopped child: %d %s", code, raw)
	}
	e.call("POST", "/api/sessions/"+kid.ID+"/stop", `{}`, "admin")
	e.call("POST", "/api/sessions/"+a+"/stop", `{}`, "admin")
}

func TestStatusMemoryEnforced(t *testing.T) {
	e := newCCEnv(t, func(c *config.Config) { c.Guard.SessionMemoryMax = "6G"; c.Guard.SystemdSlice = "" })
	code, raw := e.call("GET", "/api/status", "", "admin")
	if code != 200 || !strings.Contains(string(raw), `"memory_enforced":false`) || !strings.Contains(string(raw), `"session_memory_max":"6G"`) {
		t.Fatalf("status: %d %s", code, raw)
	}
}
