//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/files/fsafe"
)

// stage writes files into a new staging folder and returns its id.
func stage(t *testing.T, m *Manager, files map[string]string) string {
	t.Helper()
	id := "0123456789abcdef" + newID()[16:22]
	dir := filepath.Join(m.UploadsDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestRepoSessionInboxExcludedAndStagingDelivered(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("agent.sh", "ls inbox > seen.txt\ncp \"$1\" task-copy.txt\n")
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{agent, "{task_file}"}})
	})
	id := stage(t, e.m, map[string]string{"photo.png": "png", "run.sh": "#!/bin/sh\n"})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "use the photo", Staging: id})
	if err != nil {
		t.Fatal(err)
	}
	info := e.waitState(infos[0].ID, StatePassed)
	wt := info.Worktree
	seen, _ := os.ReadFile(filepath.Join(wt, "seen.txt"))
	if !strings.Contains(string(seen), "photo.png") || !strings.Contains(string(seen), "run.sh") {
		t.Fatalf("agent saw %q in its inbox", seen)
	}
	st, err := os.Stat(filepath.Join(wt, "inbox", "run.sh"))
	if err != nil || st.Mode().Perm()&0o111 != 0 {
		t.Fatalf("run.sh mode %v %v: exec bit must be dropped", st.Mode(), err)
	}
	task, _ := os.ReadFile(filepath.Join(wt, "task-copy.txt"))
	if !strings.Contains(string(task), "Files added: inbox/photo.png, inbox/run.sh") {
		t.Fatalf("task without the files mention: %q", task)
	}
	if info.Task != "use the photo" {
		t.Fatalf("Info.Task changed: %q", info.Task)
	}
	// The exclude entry sits in the file git reads for the worktree (the
	// common git dir), once, and keeps the inbox out of status.
	exclude := filepath.Join(e.repo, ".git", "info", "exclude")
	raw, _ := os.ReadFile(exclude)
	if strings.Count(string(raw), "\n/inbox/\n") != 1 {
		t.Fatalf("exclude:\n%s", raw)
	}
	status := gitT(t, wt, "status", "--porcelain", "--untracked-files=all")
	if strings.Contains(status, "inbox") {
		t.Fatalf("inbox shows in git status:\n%s", status)
	}
	if _, _, err := e.m.Inbox(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(exclude)
	if strings.Count(string(raw), "/inbox/") != 1 {
		t.Fatalf("exclude entry added twice:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(e.m.UploadsDir(), id)); !os.IsNotExist(err) {
		t.Fatal("staging folder left behind")
	}
	if !feedHas(e.m, info.ID, FeedFile, "attached 2 files: photo.png (3 B), run.sh (10 B)") {
		t.Fatalf("feed: %+v", e.m.Feed(time.Time{}, 0))
	}
}

func TestStagingFanOutCopiesAndUnknownStaging(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "read -r x"}})
		e.cfg.Sessions.IdleWaitSec = 0
	})
	id := stage(t, e.m, map[string]string{"a.txt": "A"})
	infos, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceScratch, Task: "t", Count: 3, Staging: id})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range infos {
		raw, err := os.ReadFile(filepath.Join(in.Worktree, "inbox", "a.txt"))
		if err != nil || string(raw) != "A" {
			t.Fatalf("%s: %q %v", in.ID, raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.m.UploadsDir(), id)); !os.IsNotExist(err) {
		t.Fatal("staging folder left behind")
	}
	for _, bad := range []string{id, "../../etc", "zz"} {
		if _, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceScratch, Task: "t", Staging: bad}); err == nil {
			t.Fatalf("staging %q accepted", bad)
		}
	}
}

func TestFollowUpMentionsAddedFiles(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("agent.sh", "cp \"$1\" \"$HOME/last-task.txt\"\n")
		e.profiles(
			config.AgentProfile{Name: "other", Command: []string{agent, "{task_file}"}},
			config.AgentProfile{Name: "builtin", Command: []string{"{self}", "agent", "--task-file", "{task_file}"}},
		)
	})
	id := e.scratch("other", nil)
	e.waitState(id, StatePassed)
	e.m.NoteFiles(id, []string{"inbox/a.png", "inbox/b.txt", "inbox/a.png"})
	if err := e.m.FollowUp(context.Background(), id, "look at them"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second run", func() bool {
		i, _ := e.m.Get(id)
		return i.Attempt == 2 && i.State == StatePassed
	})
	raw, _ := os.ReadFile(filepath.Join(e.home, "last-task.txt"))
	if string(raw) != "look at them\n\nFiles added: inbox/a.png, inbox/b.txt\n" {
		t.Fatalf("follow-up task %q", raw)
	}
	e.m.mu.Lock()
	pending := len(e.m.sessions[id].pendingFiles)
	e.m.mu.Unlock()
	if pending != 0 {
		t.Fatal("mentioned files still pending")
	}
	if !feedHas(e.m, id, FeedFollowUp, "look at them") || feedHas(e.m, id, FeedFollowUp, "Files added") {
		t.Fatal("feed should record the user's text only")
	}

	// The built-in agent finds new inbox files itself: no mention.
	e.m.mu.Lock()
	s := e.m.sessions[id]
	s.info.Agent = "builtin"
	e.m.mu.Unlock()
	e.m.NoteFiles(id, []string{"inbox/c.png"})
	e.m.mu.Lock()
	mention := e.m.followUpMentionLocked(s)
	e.m.mu.Unlock()
	if mention != "" {
		t.Fatalf("built-in agent mention %q", mention)
	}
}

func TestNoUserBusFallbackKeepsTranscriptClean(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		fakeSystemdRun(e, "echo 'Failed to connect to user scope bus via local transport: No such file or directory' >&2\nexit 1\n")
		e.profiles(config.AgentProfile{Name: "ok", Command: []string{"bash", "-c", "echo agent ran"}})
	})
	id := e.scratch("ok", nil)
	e.waitState(id, StatePassed)
	tx, _ := e.m.Transcript(id)
	if strings.Contains(strings.ToLower(string(tx)), "failed to connect") {
		t.Fatalf("systemd-run's error reached the transcript:\n%s", tx)
	}
	if !strings.Contains(string(tx), "no systemd user bus") || !strings.Contains(string(tx), "agent ran") {
		t.Fatalf("transcript %q", tx)
	}
}

func TestMayBeScopeFailure(t *testing.T) {
	for in, want := range map[string]bool{
		"":                                      true,
		"Fai":                                   true,
		"Failed to connect to b":                true,
		"Failed to connect to bus: x\r\nFailed": true,
		"Failed to connect\r\nhello":            false,
		"hello":                                 false,
		"\x1b[?25l":                             false,
	} {
		if got := mayBeScopeFailure([]byte(in)); got != want {
			t.Errorf("mayBeScopeFailure(%q) = %v", in, got)
		}
	}
}

// stagedNames lists a staging folder.
func stagedNames(t *testing.T, m *Manager, id string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(m.UploadsDir(), id))
	if err != nil {
		t.Fatalf("staging %s: %v", id, err)
	}
	var out []string
	for _, e := range ents {
		if e.Name() != fsafe.PrivateList { // the held folder's private list
			out = append(out, e.Name())
		}
	}
	return out
}

func TestFailedCreateReturnsAttachmentsToStaging(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{"true"}})
	})
	folder := filepath.Join(e.home, "work")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	// "inbox" is a file: delivering into it fails after the staging folder
	// was taken.
	if err := os.WriteFile(filepath.Join(folder, "inbox"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := stage(t, e.m, map[string]string{"a.txt": "A", "b.txt": "B"})
	if _, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceFolder, Dir: folder, Task: "t", Staging: id}); err == nil {
		t.Fatal("delivery into a file accepted")
	}
	if got := strings.Join(stagedNames(t, e.m, id), ","); got != "a.txt,b.txt" {
		t.Fatalf("staging after the failed Create: %q", got)
	}
	if st, _ := os.Stat(filepath.Join(e.m.UploadsDir(), id)); st.Mode().Perm() != 0o700 {
		t.Fatalf("staging mode %v", st.Mode())
	}
	// Fixed, the retry gets every file.
	if err := os.Remove(filepath.Join(folder, "inbox")); err != nil {
		t.Fatal(err)
	}
	infos, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceFolder, Dir: folder, Task: "t", Staging: id})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(infos[0].Worktree, "inbox", n)); err != nil {
			t.Fatalf("%s missing after the retry: %v", n, err)
		}
	}
}

func TestCreateClaimsStagingAndFanOutCopies(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "wait", Interactive: true, Command: []string{"bash", "-c", "read -r x"}})
		e.cfg.Sessions.IdleWaitSec = 0
	})
	id := stage(t, e.m, map[string]string{"a.txt": "A", "b.txt": "B"})
	// A Create already handing these attachments out: a second one fails
	// before touching any file.
	release, err := e.m.claimStaging(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceScratch, Task: "t", Staging: id}); err == nil ||
		!strings.Contains(err.Error(), "already being handed") {
		t.Fatalf("second Create: %v", err)
	}
	if got := strings.Join(stagedNames(t, e.m, id), ","); got != "a.txt,b.txt" {
		t.Fatalf("staging touched: %q", got)
	}
	release()
	infos, err := e.m.Create(context.Background(), TaskSpec{Workspace: WorkspaceScratch, Task: "t", Count: 2, Staging: id})
	if err != nil {
		t.Fatal(err)
	}
	// Each session has its own copy, never a shared inode.
	a, err1 := os.Stat(filepath.Join(infos[0].Worktree, "inbox", "a.txt"))
	b, err2 := os.Stat(filepath.Join(infos[1].Worktree, "inbox", "a.txt"))
	if err1 != nil || err2 != nil || os.SameFile(a, b) {
		t.Fatalf("fan-out files share an inode (%v %v)", err1, err2)
	}
}

func TestFilesMentionStripsControlCharacters(t *testing.T) {
	got := filesMention([]string{"evil\rrm -rf ~/x.png", "a\x1b[2J/b‮.txt"})
	if strings.ContainsAny(got, "\r\n\x1b‮") || got != "Files added: evil?rm -rf ~/x.png, a?[2J/b?.txt" {
		t.Fatalf("mention %q", got)
	}
}

// An upload placing a file into a staging folder keeps a Create's claim
// waiting until it is done; one that comes while the claim holds is refused.
func TestPlaceStagingCoordinatesWithClaim(t *testing.T) {
	e := newTestEnv(t, nil, func(*testEnv) {})
	id := stage(t, e.m, map[string]string{"a.txt": "A"})
	place, err := e.m.PlaceStaging(id)
	if err != nil {
		t.Fatal(err)
	}
	claimed := make(chan func(), 1)
	go func() {
		release, err := e.m.claimStaging(id)
		if err != nil {
			t.Error(err)
			release = func() {}
		}
		claimed <- release
	}()
	select {
	case <-claimed:
		t.Fatal("claim did not wait for the upload placing a file")
	case <-time.After(50 * time.Millisecond):
	}
	place()
	release := <-claimed
	if _, err := e.m.PlaceStaging(id); !errors.Is(err, ErrStagingTaken) {
		t.Fatalf("upload during a claim: %v", err)
	}
	release()
	again, err := e.m.PlaceStaging(id)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
