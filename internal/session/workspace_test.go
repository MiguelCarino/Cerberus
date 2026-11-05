//go:build !windows

package session

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

// writer is a fake agent that writes out.txt in its working directory.
func writerEnv(t *testing.T) *testEnv {
	return newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "writer", Command: []string{"bash", e.script("w.sh", "echo hi > out.txt\n")}})
	})
}

func homeOf(e *testEnv) string { return filepath.Dir(e.repo) }

func TestFolderSessionWorksInPlaceAndDiscardKeepsFolder(t *testing.T) {
	e := writerEnv(t)
	folder := filepath.Join(homeOf(e), "notes")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "keep.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	infos, err := e.m.Create(ctx, TaskSpec{Workspace: WorkspaceFolder, Dir: folder, Task: "write"})
	if err != nil {
		t.Fatal(err)
	}
	info := infos[0]
	if info.Workspace != WorkspaceFolder || info.Worktree != folder || info.Dir != folder || info.Repo != "" {
		t.Fatalf("folder session %+v", info)
	}
	info = e.waitState(info.ID, StatePassed)
	if raw, err := os.ReadFile(filepath.Join(folder, "out.txt")); err != nil || string(raw) != "hi\n" {
		t.Fatalf("agent did not work in place: %q %v", raw, err)
	}
	if _, err := e.m.Diff(info.ID); err == nil {
		t.Fatal("folder session has a diff")
	}
	if _, err := e.m.Accept(ctx, info.ID, AcceptOpts{}); err == nil {
		t.Fatal("folder session accepted")
	}
	if err := e.m.Discard(info.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"keep.md", "out.txt"} {
		if _, err := os.Stat(filepath.Join(folder, f)); err != nil {
			t.Fatalf("discard touched the folder: %s: %v", f, err)
		}
	}
	if got, _ := e.m.Get(info.ID); got.State != StateDiscarded {
		t.Fatalf("state %s", got.State)
	}
}

func TestFolderThatIsARepoBecomesRepoSession(t *testing.T) {
	e := writerEnv(t)
	infos, err := e.m.Create(context.Background(), TaskSpec{Dir: e.repo, Task: "write"})
	if err != nil {
		t.Fatal(err)
	}
	if infos[0].Workspace != WorkspaceRepo || infos[0].Repo != e.repo {
		t.Fatalf("%+v", infos[0])
	}
	info := e.waitState(infos[0].ID, StatePassed)
	if info.Worktree == e.repo || !strings.HasPrefix(info.Worktree, e.m.wtDir) {
		t.Fatalf("repo session should use a worktree, got %q", info.Worktree)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "out.txt")); err == nil {
		t.Fatal("repo session wrote into the main checkout")
	}
}

func TestScratchSessionSaveArchiveDiscard(t *testing.T) {
	e := writerEnv(t)
	ctx := context.Background()
	infos, err := e.m.Create(ctx, TaskSpec{Task: "write"}) // nothing chosen: scratch
	if err != nil {
		t.Fatal(err)
	}
	info := infos[0]
	ws := filepath.Join(e.m.sessDir, info.ID, "workspace")
	if info.Workspace != WorkspaceScratch || info.Worktree != ws || info.Dir != "" {
		t.Fatalf("scratch session %+v", info)
	}
	e.waitState(info.ID, StatePassed)

	dst := filepath.Join(homeOf(e), "saved", "deep")
	if n, err := e.m.SaveTo(info.ID, dst, false); err != nil || n != 1 {
		t.Fatalf("save: %d %v", n, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dst, "out.txt")); string(raw) != "hi\n" {
		t.Fatalf("saved %q", raw)
	}
	if _, err := e.m.SaveTo(info.ID, dst, false); err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("second save without overwrite: %v", err)
	}
	if _, err := e.m.SaveTo(info.ID, dst, true); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if !feedHas(e.m, info.ID, FeedUpdate, "saved (overwrite) 1 files to ") {
		t.Fatal("save not in the feed")
	}
	if _, err := e.m.SaveTo(info.ID, homeOf(e), true); err == nil {
		t.Fatal("saved into the home folder itself")
	}
	if _, err := e.m.SaveTo(info.ID, filepath.Join(t.TempDir(), "x"), false); err == nil {
		t.Fatal("saved outside home")
	}

	var buf bytes.Buffer
	if err := e.m.Archive(info.ID, &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "out.txt" {
		t.Fatalf("zip %v %+v", err, zr)
	}

	if err := e.m.Discard(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("scratch workspace not deleted")
	}
	if _, err := os.Stat(filepath.Join(dst, "out.txt")); err != nil {
		t.Fatal("discard removed the saved copy")
	}
	if err := e.m.Archive(info.ID, &buf); err == nil {
		t.Fatal("archive of a discarded session")
	}
}

func TestSaveAndArchiveOnlyForScratch(t *testing.T) {
	e := writerEnv(t)
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "write"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(infos[0].ID, StatePassed)
	if _, err := e.m.SaveTo(infos[0].ID, filepath.Join(homeOf(e), "x"), false); err == nil {
		t.Fatal("repo session saved")
	}
	if err := e.m.Archive(infos[0].ID, &bytes.Buffer{}); err == nil {
		t.Fatal("repo session archived")
	}
}

func TestWorkspaceValidation(t *testing.T) {
	e := writerEnv(t)
	ctx := context.Background()
	home := homeOf(e)
	denied := filepath.Join(home, "Carino-Systems-Business")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]TaskSpec{
		"home itself":    {Workspace: WorkspaceFolder, Dir: home},
		"outside home":   {Workspace: WorkspaceFolder, Dir: t.TempDir()},
		"data dir":       {Workspace: WorkspaceFolder, Dir: filepath.Dir(e.m.sessDir)},
		"inside data":    {Workspace: WorkspaceFolder, Dir: e.m.sessDir},
		"denied":         {Workspace: WorkspaceFolder, Dir: denied},
		"relative":       {Workspace: WorkspaceFolder, Dir: "notes"},
		"missing":        {Workspace: WorkspaceFolder, Dir: filepath.Join(home, "nope")},
		"folder no dir":  {Workspace: WorkspaceFolder},
		"bad kind":       {Workspace: "cloud", Dir: e.repo},
		"repo not a git": {Workspace: WorkspaceRepo, Dir: filepath.Join(home, "data")},
	}
	for name, spec := range cases {
		spec.Task = "t"
		if _, err := e.m.Create(ctx, spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A repo anywhere under home is allowed now, not only under repo_roots.
	nested := filepath.Join(home, "a", "b", "proj")
	gitT(t, home, "init", "-q", "-b", "main", nested)
	if _, err := e.m.validateRepo(ctx, nested); err != nil {
		t.Fatalf("nested repo under home: %v", err)
	}
}

func TestBrowse(t *testing.T) {
	e := writerEnv(t)
	home := homeOf(e)
	for _, d := range []string{"Zeta", "alpha", ".hidden", "Carino-Systems-Business"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	l, err := e.m.Browse("", false)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != home || l.Parent != "" || !l.Home {
		t.Fatalf("listing %+v", l)
	}
	var names []string
	git := map[string]bool{}
	for _, en := range l.Entries {
		names = append(names, en.Name)
		git[en.Name] = en.Git
	}
	// The denied folder, the dot-folder, the file and the link out of home
	// are left out ("data" holds XDG_DATA_HOME, so it is listed but a
	// session may not use it).
	if got := strings.Join(names, ","); got != "alpha,data,repo,Zeta" {
		t.Fatalf("entries %s", got)
	}
	if !git["repo"] || git["alpha"] {
		t.Fatalf("git marks %+v", git)
	}
	l, err = e.m.Browse("", true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, en := range l.Entries {
		found = found || en.Name == ".hidden"
	}
	if !found {
		t.Fatal("hidden=1 does not show dot-folders")
	}
	sub, err := e.m.Browse(filepath.Join(home, "alpha"), false)
	if err != nil || sub.Parent != home || sub.Home {
		t.Fatalf("sub %+v %v", sub, err)
	}
	if _, err := e.m.Browse(t.TempDir(), false); err == nil {
		t.Fatal("browsed outside home")
	}
	if _, err := e.m.Browse(filepath.Join(home, "Carino-Systems-Business"), false); err == nil {
		t.Fatal("browsed a denied folder")
	}
}

func TestOldMetaLoadsAsRepoSession(t *testing.T) {
	id := "20250101-000000-abcdef"
	var repo string
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "true"}})
		repo = e.repo
		dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "cerberus", "sessions", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(meta{Info: Info{ID: id, State: StatePassed, Repo: e.repo}, Task: "t", MaxAttempts: 1})
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	info, ok := e.m.Get(id)
	if !ok || info.Workspace != WorkspaceRepo || info.Dir != repo {
		t.Fatalf("%+v", info)
	}
}

// TestCopyAndZipRefuseEscapingSymlink: a listed file that the agent swaps for
// a symlink leaving the workspace (after listFiles ran) is neither copied
// nor zipped, and a symlink in the destination does not redirect a write.
func TestCopyAndZipRefuseEscapingSymlink(t *testing.T) {
	base := t.TempDir()
	src, dst, outside := filepath.Join(base, "ws"), filepath.Join(base, "dst"), filepath.Join(base, "secret")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "x")); err != nil {
		t.Fatal(err)
	}
	srcRoot, _ := os.OpenRoot(src)
	defer srcRoot.Close()
	dstRoot, _ := os.OpenRoot(dst)
	defer dstRoot.Close()
	if err := copyFile(srcRoot, dstRoot, "x"); err == nil {
		t.Fatal("copyFile followed a symlink out of the workspace")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := addZip(zw, srcRoot, "x"); err == nil {
		t.Fatal("addZip followed a symlink out of the workspace")
	}
	// Destination: a planted symlink must not let a copy overwrite outside.
	if err := os.WriteFile(filepath.Join(src, "y"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "y")); err != nil {
		t.Fatal(err)
	}
	_ = copyFile(srcRoot, dstRoot, "y")
	if b, _ := os.ReadFile(outside); string(b) != "TOP SECRET" {
		t.Fatalf("write followed a destination symlink: %q", b)
	}
}
