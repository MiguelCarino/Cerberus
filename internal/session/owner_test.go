//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

// A confined (non-admin) session never runs without bwrap, whatever the
// sandbox settings say, and its owner is kept on the session.
func TestConfinedNeedsSandbox(t *testing.T) {
	swapAvailable(t, func(context.Context) (string, error) { return "", errors.New("bwrap is not installed") })
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.cfg.Sandbox.Default = "none" // not even asked for
		e.profiles(config.AgentProfile{Name: "plain", Command: []string{"bash", "-c", "touch should-not-exist"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Task: "t", Agent: "plain", Owner: "u-bob", Confined: true})
	if err != nil {
		t.Fatal(err)
	}
	if infos[0].Owner != "u-bob" || !infos[0].Confined {
		t.Fatalf("info %+v", infos[0])
	}
	info := e.waitState(infos[0].ID, StateError)
	if !strings.Contains(info.Error, "bubblewrap") {
		t.Errorf("error %q", info.Error)
	}
	if _, err := os.Stat(filepath.Join(info.Worktree, "should-not-exist")); err == nil {
		t.Error("a confined agent ran without its sandbox")
	}
	// The owner survives a reload from meta.json.
	if got, ok := e.m.Get(info.ID); !ok || got.Owner != "u-bob" || !got.Confined {
		t.Fatalf("get %+v", got)
	}
}

// A confined session's plan forces the confined spec (read-only home, no
// global extra_rw).
func TestConfinedPlan(t *testing.T) {
	swapAvailable(t, func(context.Context) (string, error) { return "/usr/bin/bwrap", nil })
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.cfg.Sandbox.ReadOnlyHome = false
		e.cfg.Sandbox.ExtraRW = []string{"/global/rw"}
	})
	p := config.AgentProfile{Name: "x", SandboxRW: []string{"/profile/rw"}}
	pl, err := e.m.planSandbox(context.Background(), Info{ID: "s", Confined: true, Worktree: t.TempDir()}, p, t.TempDir(), nil)
	if err != nil || pl.bin == "" {
		t.Fatalf("plan %+v %v", pl, err)
	}
	if !pl.spec.Confined || !pl.spec.ReadOnlyHome || len(pl.spec.ExtraRW) != 1 || pl.spec.ExtraRW[0] != "/profile/rw" {
		t.Fatalf("spec %+v", pl.spec)
	}
	// An admin session with sandbox none is not sandboxed at all.
	if pl, _ := e.m.planSandbox(context.Background(), Info{ID: "s"}, p, t.TempDir(), nil); pl.bin != "" {
		t.Fatal("an unconfined session got bwrap")
	}
}

// Search filters by stored owner ids.
func TestSearchOwners(t *testing.T) {
	q := SearchQuery{Owners: []string{"u-a"}}
	if !q.matchesFilters(Info{Owner: "u-a"}) || q.matchesFilters(Info{Owner: "u-b"}) {
		t.Fatal("owner filter")
	}
	if !(SearchQuery{}).matchesFilters(Info{Owner: "u-b"}) {
		t.Fatal("no filter keeps all")
	}
}

// meta.json sits in the session folder, which the session's own sandbox
// can write: an agent that rewrites it (Confined false, another owner, the
// home folder as its worktree) changes nothing after a reload, because the
// guard sidecar outside the folder wins.
func TestGuardSidecarWinsOverMeta(t *testing.T) {
	swapAvailable(t, func(context.Context) (string, error) { return "", errors.New("bwrap is not installed") })
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "plain", Command: []string{"true"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Task: "t", Agent: "plain", Owner: "u-bob", Confined: true})
	if err != nil {
		t.Fatal(err)
	}
	info := e.waitState(infos[0].ID, StateError)
	metaPath := filepath.Join(e.m.sessDir, info.ID, "meta.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		t.Fatal(err)
	}
	in := md["info"].(map[string]any)
	in["Confined"], in["Owner"], in["Worktree"], in["Workspace"] = false, "u-admin", homeOf(e), WorkspaceFolder
	raw, _ = json.Marshal(md)
	if err := os.WriteFile(metaPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m2, err := NewManager(e.cfg, nil, nil, notify.New(e.cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Shutdown(context.Background())
	back, ok := m2.Get(info.ID)
	if !ok || !back.Confined || back.Owner != "u-bob" || back.Worktree != info.Worktree || back.Workspace != info.Workspace {
		t.Fatalf("an edited meta.json was trusted: %+v", back)
	}
}

// The owner's current role confines a run even when the stored session
// says otherwise (WithConfine).
func TestConfineByOwnerRole(t *testing.T) {
	swapAvailable(t, func(context.Context) (string, error) { return "/usr/bin/bwrap", nil })
	e := newTestEnv(t, nil, func(*testEnv) {})
	e.m.confineOwner = func(owner string) bool { return owner == "u-bob" }
	pl, err := e.m.planSandbox(context.Background(), Info{ID: "s", Owner: "u-bob", Worktree: t.TempDir()}, config.AgentProfile{}, t.TempDir(), nil)
	if err != nil || pl.bin == "" || !pl.spec.Confined {
		t.Fatalf("plan for a non-admin owner: %+v %v", pl, err)
	}
	if pl, _ := e.m.planSandbox(context.Background(), Info{ID: "s", Owner: "u-ana"}, config.AgentProfile{}, t.TempDir(), nil); pl.bin != "" {
		t.Fatal("an admin's session got confined")
	}
}
