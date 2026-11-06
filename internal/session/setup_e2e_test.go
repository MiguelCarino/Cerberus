//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// TestSetupSnapshotWritten: a run start writes setup.json with the resolved
// profile (secrets masked, env keys only), the agent's version, the
// workspace and base commit, and keeps its hash on Info; a changed file is
// reported as not verified; a session without one is ErrNoSetup.
func TestSetupSnapshotWritten(t *testing.T) {
	const secret = "ghp_setupsecret0123456789"
	var probed []string
	e := newTestEnv(t, nil, func(e *testEnv) {
		withFileVault(e, map[string]string{"gh": secret})
		agent := e.script("agent.sh", "echo hi\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{agent, "--token", secret, "{task_file}"},
			Env: map[string]string{"FAKE_MODE": "value-never-recorded"}})
		e.opts = append(e.opts, WithVersion("9.9.9 (test)"), func(m *Manager) {
			m.probeVersion = func(_ context.Context, bin string) string { probed = append(probed, bin); return "fake-agent 1.2.3\n" }
		})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "say hi"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	info := e.waitState(id, StatePassed)
	if len(info.SetupHash) != setupHashLen {
		t.Fatalf("SetupHash %q", info.SetupHash)
	}
	raw, err := os.ReadFile(filepath.Join(e.m.sessDir, id, setupFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "value-never-recorded") {
		t.Fatalf("setup.json leaks a secret or an env value:\n%s", raw)
	}
	v, err := e.m.Setup(id)
	if err != nil {
		t.Fatal(err)
	}
	st := v.Setup
	if !v.Verified || v.Hash != info.SetupHash || v.Recorded != info.SetupHash {
		t.Fatalf("view %+v, info hash %s", v, info.SetupHash)
	}
	if st.Agent.Name != "fake" || st.Agent.Version != "fake-agent 1.2.3" || st.Agent.Command[2] != redactMask ||
		st.Agent.Command[3] != "{task_file}" || strings.Join(st.Agent.EnvKeys, ",") != "FAKE_MODE" {
		t.Fatalf("agent %+v", st.Agent)
	}
	if st.Workspace != WorkspaceRepo || st.BaseCommit == "" || st.BaseCommit != info.BaseCommit || st.Cerberus != "9.9.9 (test)" {
		t.Fatalf("setup %+v (base %s)", st, info.BaseCommit)
	}
	if st.Model.ID != "" || st.Model.Engine != "" || len(st.MCPServers) != 0 {
		t.Fatalf("no model and no MCP expected: %+v %+v", st.Model, st.MCPServers)
	}
	if len(probed) != 1 || probed[0] != st.Agent.Binary {
		t.Fatalf("probed %v, binary %s", probed, st.Agent.Binary)
	}

	// The hash survives a restart of the manager (it is in meta.json).
	var meta struct{ Info Info }
	mraw, _ := os.ReadFile(filepath.Join(e.m.sessDir, id, "meta.json"))
	if json.Unmarshal(mraw, &meta) != nil || meta.Info.SetupHash != info.SetupHash {
		t.Fatalf("meta.json SetupHash %q", meta.Info.SetupHash)
	}

	// An agent (or anyone) editing setup.json afterwards shows.
	st.Agent.Version = "something else"
	edited, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(e.m.sessDir, id, setupFile), edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if v2, err := e.m.Setup(id); err != nil || v2.Verified || v2.Hash == info.SetupHash {
		t.Fatalf("edited setup still verified: %+v %v", v2, err)
	}

	// A symlink in its place is refused, never followed.
	p := filepath.Join(e.m.sessDir, id, setupFile)
	_ = os.Remove(p)
	if err := os.Symlink(filepath.Join(e.m.sessDir, id, "meta.json"), p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Setup(id); err == nil {
		t.Fatal("a symlinked setup.json should not be read")
	}
	_ = os.Remove(p)
	if _, err := e.m.Setup(id); !errors.Is(err, ErrNoSetup) {
		t.Fatalf("missing setup: %v", err)
	}
	if _, err := e.m.Setup("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}
