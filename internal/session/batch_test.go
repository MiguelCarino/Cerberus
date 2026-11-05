//go:build !windows

package session

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

// newRepo makes a git repo with one commit on main.
func newRepo(t *testing.T, path string) {
	t.Helper()
	gitT(t, filepath.Dir(path), "init", "-q", "-b", "main", path)
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("# "+filepath.Base(path)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, path, "add", "-A")
	gitT(t, path, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "initial")
}

func TestFleetBatch(t *testing.T) {
	var fleet string
	var repos []string
	e := newTestEnv(t, nil, func(e *testEnv) {
		fleet = filepath.Join(e.home, "fleet")
		if err := os.MkdirAll(fleet, 0o755); err != nil {
			t.Fatal(err)
		}
		for i, name := range []string{"alpha", "beta", "gamma"} {
			p := filepath.Join(fleet, name)
			newRepo(t, p)
			repos = append(repos, p)
			checks := []string{"test -f result.txt"}
			if i == 2 {
				checks = []string{"echo gamma check fails; false"}
			}
			e.cfg.Repos = append(e.cfg.Repos, config.RepoConfig{Name: name, Path: p, Checks: checks})
		}
		// A file only alpha and beta have, for the finder.
		for _, p := range repos[:2] {
			if err := os.WriteFile(filepath.Join(p, "carino-navbar.js"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			gitT(t, p, "add", "-A")
			gitT(t, p, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "navbar")
		}
		// Not a repo, and a repo protected by deny_paths: never offered.
		if err := os.MkdirAll(filepath.Join(fleet, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		newRepo(t, filepath.Join(fleet, "Carino-Systems-Business"))
		agent := e.script("fleet.sh", "echo \"$(cat \"$1\")\" > result.txt\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "fleet", Command: []string{"bash", agent, "{task_file}"}})
	})
	ctx := context.Background()

	// Finder: has-file glob, name glob, dirty flag.
	if err := os.WriteFile(filepath.Join(repos[2], "local.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	all := e.m.FindRepos(RepoSelector{Roots: []string{fleet}})
	if len(all) != 3 || all[0].Name != "alpha" || all[2].Name != "gamma" || !all[2].Dirty || all[0].Dirty {
		t.Fatalf("all repos %+v", all)
	}
	if got := e.m.FindRepos(RepoSelector{Roots: []string{fleet}, HasFile: "carino-*.js"}); len(got) != 2 || got[1].Name != "beta" {
		t.Fatalf("has-file %+v", got)
	}
	if got := e.m.FindRepos(RepoSelector{Roots: []string{fleet}, NameGlob: "G*"}); len(got) != 1 || got[0].Name != "gamma" {
		t.Fatalf("name glob %+v", got)
	}
	if got := e.m.FindRepos(RepoSelector{Roots: []string{fleet}, HasFile: "../*"}); len(got) != 0 {
		t.Fatalf("has-file outside the repo must match nothing: %+v", got)
	}
	if err := os.Remove(filepath.Join(repos[2], "local.txt")); err != nil {
		t.Fatal(err)
	}

	// Validation happens before anything is created.
	if _, _, err := e.m.CreateBatch(ctx, BatchSpec{Repos: []string{repos[0], filepath.Join(fleet, "notes")}, Task: "x"}); err == nil {
		t.Fatal("a non-repo must fail the whole batch")
	}
	if n := len(e.m.List()); n != 0 {
		t.Fatalf("a failed batch created %d sessions", n)
	}

	bi, infos, err := e.m.CreateBatch(ctx, BatchSpec{Repos: repos, Task: "Update {name} at {repo}", MaxAttempts: 1, Title: "Fleet update"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 || len(bi.Sessions) != 3 {
		t.Fatalf("batch %+v", bi)
	}
	for i, info := range infos {
		name := filepath.Base(repos[i])
		if info.Group != bi.ID || info.Task != "Update "+name+" at "+repos[i] || info.Title != "Fleet update · "+name {
			t.Fatalf("session %+v", info)
		}
	}
	e.waitState(infos[0].ID, StatePassed)
	e.waitState(infos[1].ID, StatePassed)
	e.waitState(infos[2].ID, StateFailed)
	if bs := e.m.Batches(); len(bs) != 1 || bs[0].Counts[StatePassed] != 2 || bs[0].Counts[StateFailed] != 1 {
		t.Fatalf("batches %+v", bs)
	}

	res := e.m.AcceptBatch(ctx, bi.ID, AcceptOpts{})
	if len(res) != 3 {
		t.Fatalf("results %+v", res)
	}
	for i, r := range res[:2] {
		if r.Result != ResultAccepted || r.Commit == "" || r.Repo != repos[i] {
			t.Fatalf("result %d %+v", i, r)
		}
		if head := gitT(t, repos[i], "rev-parse", "refs/heads/main"); head != r.Commit {
			t.Fatalf("%s main is %s, want fast-forward to %s", repos[i], head, r.Commit)
		}
		if msg := gitT(t, repos[i], "log", "-1", "--format=%s"); msg != "Fleet update" {
			t.Fatalf("commit message %q", msg)
		}
		raw, err := os.ReadFile(filepath.Join(repos[i], "result.txt"))
		if err != nil || !strings.Contains(string(raw), "Update "+filepath.Base(repos[i])) {
			t.Fatalf("main checkout lacks the work: %q %v", raw, err)
		}
	}
	if r := res[2]; r.Result != ResultSkipped || !strings.Contains(r.Detail, StateFailed) || r.Commit != "" {
		t.Fatalf("gamma result %+v", r)
	}
	for _, p := range repos {
		if refs := gitT(t, p, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/main" {
			t.Fatalf("%s has branches %q; only main may exist", p, refs)
		}
	}

	// Batches persist across a restart.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m2, err := NewManager(e.cfg, nil, nil, notify.New(e.cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	if b2, s2, ok := m2.Batch(bi.ID); !ok || len(s2) != 3 || b2.Title != "Fleet update" || b2.Counts[StateAccepted] != 2 {
		t.Fatalf("reloaded batch %+v %d %v", b2, len(s2), ok)
	}

	res = e.m.DiscardBatch(bi.ID)
	if res[0].Result != ResultSkipped || res[1].Result != ResultSkipped || res[2].Result != ResultDiscarded {
		t.Fatalf("discard results %+v", res)
	}
	if info, _ := e.m.Get(infos[2].ID); info.State != StateDiscarded {
		t.Fatalf("gamma state %s", info.State)
	}
	if _, _, ok := e.m.Batch("missing"); ok || len(e.m.AcceptBatch(ctx, "missing", AcceptOpts{})) != 0 {
		t.Fatal("unknown batch")
	}
}
