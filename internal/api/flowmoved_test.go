package api

import (
	"context"
	"errors"
	"testing"

	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/users"
)

// A removed user's flows move to an admin but do not run (neither on
// schedule nor their session steps) until an admin re-saves them: their
// steps were written by the removed, untrusted account and would otherwise
// run unconfined with the admin's rights.
func TestRemovedUsersFlowsDoNotRun(t *testing.T) {
	t.Cleanup(users.SetCostForTest(1000))
	st, err := users.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ana, err := st.Add("ana", "", users.RoleAdmin, "a long password")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.Add("bob", "", users.RoleUser, "a long password")
	if err != nil {
		t.Fatal(err)
	}
	h := &FlowHost{d: FlowHostDeps{Users: st}}
	if !h.ownerActive(bob.ID) || !h.ownerActive("") || !h.ownerActive(ana.ID) {
		t.Fatal("live owners inactive")
	}
	if _, err := st.Remove(bob.ID, ana.ID); err != nil {
		t.Fatal(err)
	}
	if st.Owner(bob.ID) != ana.ID {
		t.Fatal("bob's data did not move to ana")
	}
	if h.ownerActive(bob.ID) {
		t.Fatal("a removed user's flow still runs on schedule")
	}
	if !h.ownerActive("") || !h.ownerActive(ana.ID) {
		t.Fatal("legacy or admin owner inactive")
	}
	_, err = flowSessionRunner{h}.Start(context.Background(), flows.SessionRequest{Task: "x",
		Workspace: flows.WorkspaceFolder, Folder: "/home", Owner: bob.ID})
	if !errors.Is(err, errMovedFlow) {
		t.Fatalf("session step of a removed user's flow: %v", err)
	}
}

// A user with no agents list may start a session without naming an agent
// although the server's default_agent is another profile: they get the
// built-in agent (or the first one they may use).
func TestConfineDefaultsToAllowedAgent(t *testing.T) {
	t.Cleanup(users.SetCostForTest(1000))
	defer func(old func(context.Context) error) { sandboxCheck = old }(sandboxCheck)
	sandboxCheck = func(context.Context) error { return nil }
	st, err := users.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("ana", "", users.RoleAdmin, "a long password"); err != nil {
		t.Fatal(err)
	}
	bob, err := st.Add("bob", "", users.RoleUser, "a long password")
	if err != nil {
		t.Fatal(err)
	}
	spec := session.TaskSpec{Task: "x", Owner: bob.ID}
	if ce := confineTask(context.Background(), st, "coder", &spec); ce != nil || spec.Agent != users.DefaultAgent || !spec.Confined {
		t.Fatalf("no agent named: %v %+v", ce, spec)
	}
	spec = session.TaskSpec{Task: "x", Owner: bob.ID, Agent: "coder"}
	if ce := confineTask(context.Background(), st, "coder", &spec); ce == nil || ce.code != "agent" {
		t.Fatalf("a profile bob may not use: %v", ce)
	}
}
