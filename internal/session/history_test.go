//go:build !windows

package session

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/vault"
)

// withFileVault points the config at a temp config dir and opens a file
// vault there (never the desktop keyring), holding secrets.
func withFileVault(e *testEnv, secrets map[string]string) {
	e.t.Helper()
	e.cfg.SetPath(filepath.Join(e.home, "config", "cerberus", "config.json"))
	e.cfg.Vault.Backend = vault.BackendFile
	v, err := vault.Open(e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, val := range secrets {
		if err := v.Set(k, val, ""); err != nil {
			e.t.Fatal(err)
		}
	}
	e.opts = append(e.opts, WithVault(v))
}

func TestSecretsInjectedAndRedactedLive(t *testing.T) {
	const secret = "sk-live-0123456789abcdef"
	e := newTestEnv(t, nil, func(e *testEnv) {
		withFileVault(e, map[string]string{"api-token": secret})
		agent := e.script("leaky.sh", `printf 'value=%s\n' "$MY_TOKEN"
printf 'split=%s' "${MY_TOKEN:0:9}"
sleep 0.3
printf '%s done\n' "${MY_TOKEN:9}"
printf '\033[31m%s\033[0m\n' "$MY_TOKEN"
exit 0
`)
		e.profiles(
			config.AgentProfile{Name: "leaky", Command: []string{"bash", agent}, SecretEnv: map[string]string{"MY_TOKEN": "api-token"}},
			config.AgentProfile{Name: "needy", Command: []string{"bash", agent}, SecretEnv: map[string]string{"OTHER": "missing-secret"}},
		)
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "leak it"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	backlog, ch, detach, err := e.m.Attach(id)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	live := string(backlog)
	deadline := time.After(waitLong)
	for !strings.Contains(live, "done") || !strings.Contains(live, "\x1b[0m") {
		select {
		case b, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed; got %q", live)
			}
			live += string(b)
		case <-deadline:
			t.Fatalf("no output; got %q", live)
		}
	}
	e.waitState(id, StatePassed)
	sb, _ := os.ReadFile(filepath.Join(e.m.sessDir, id, "scrollback.log"))
	tx, err := e.m.Transcript(id)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"live": live, "scrollback": string(sb), "transcript": string(tx)} {
		if strings.Contains(got, secret) || strings.Contains(got, secret[9:]) {
			t.Fatalf("%s leaks the secret: %q", name, got)
		}
		if !strings.Contains(got, "value="+redactMask) || !strings.Contains(got, "split="+redactMask+" done") {
			t.Fatalf("%s is not redacted as expected: %q", name, got)
		}
	}
	if !strings.Contains(string(tx), "\n"+redactMask+"\n") || strings.Contains(string(tx), "\x1b") {
		t.Fatalf("transcript should be plain text: %q", tx)
	}

	// A secret the vault lacks stops the session with an error naming it.
	infos, err = e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "needs a secret", Agent: "needy"})
	if err != nil {
		t.Fatal(err)
	}
	info := e.waitState(infos[0].ID, StateError)
	if !strings.Contains(info.Error, "missing-secret") || !strings.Contains(info.Error, "OTHER") {
		t.Fatalf("error should name the secret: %q", info.Error)
	}
}

func TestSecretWithoutVaultIsAnError(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.profiles(config.AgentProfile{Name: "needy", Command: []string{"bash", "-c", "true"},
			SecretEnv: map[string]string{"TOKEN": "tok"}})
	})
	info := e.waitState(e.createOne("x"), StateError)
	if !strings.Contains(info.Error, `"tok"`) {
		t.Fatalf("error %q", info.Error)
	}
}

func TestSearch(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("echoer.sh", `printf '\033[32mcolour\033[0m output for: '; cat "$1"
printf 'progress 10%%\rprogress 100%%\n'
exit 0
`)
		e.profiles(config.AgentProfile{Name: "echoer", Command: []string{"bash", agent, "{task_file}"}})
	})
	ids := []string{e.createOne("Alpha navbar tweak"), e.createOne("Beta fonts cleanup"), e.createOne("Gamma: fix it\nthe navbar is broken")}
	for _, id := range ids {
		e.waitState(id, StatePassed)
	}

	hitIDs := func(h []SearchHit) []string {
		var out []string
		for _, x := range h {
			out = append(out, x.Session.ID)
		}
		slices.Sort(out)
		return out
	}
	want := []string{ids[0], ids[2]}
	slices.Sort(want)
	if got := hitIDs(e.m.Search(SearchQuery{Text: "NAVBAR"})); !slices.Equal(got, want) {
		t.Fatalf("navbar hits %v", got)
	}
	if got := hitIDs(e.m.Search(SearchQuery{Text: "navbar broken"})); !slices.Equal(got, []string{ids[2]}) {
		t.Fatalf("all words must match: %v", got)
	}
	// "colour" is only in the transcripts, without its escape codes.
	hits := e.m.Search(SearchQuery{Text: "colour output"})
	if len(hits) != 3 {
		t.Fatalf("transcript hits %d", len(hits))
	}
	for _, h := range hits {
		if len(h.Snippets) == 0 || len(h.Snippets) > maxSnippets || !strings.Contains(h.Snippets[0], "colour output for") ||
			strings.Contains(h.Snippets[0], "\x1b") {
			t.Fatalf("snippets %q", h.Snippets)
		}
	}
	// Title matches outrank transcript-only matches.
	hits = e.m.Search(SearchQuery{Text: "beta"})
	if len(hits) != 1 || hits[0].Session.ID != ids[1] || hits[0].Score < 15 {
		t.Fatalf("beta hits %+v", hits)
	}
	tx, _ := e.m.Transcript(ids[0])
	if !strings.Contains(string(tx), "progress 100%\n") || strings.Contains(string(tx), "progress 10%\n") {
		t.Fatalf("\\r overwrite not applied: %q", tx)
	}
	// Filters and limits.
	if n := len(e.m.Search(SearchQuery{Text: "navbar", State: StateFailed})); n != 0 {
		t.Fatalf("state filter: %d", n)
	}
	if n := len(e.m.Search(SearchQuery{Agent: "echoer", Repo: "repo"})); n != 3 {
		t.Fatalf("agent+repo filter: %d", n)
	}
	if n := len(e.m.Search(SearchQuery{Repo: "/elsewhere"})); n != 0 {
		t.Fatalf("repo filter: %d", n)
	}
	if n := len(e.m.Search(SearchQuery{Limit: 2})); n != 2 {
		t.Fatalf("limit: %d", n)
	}
	if n := len(e.m.Search(SearchQuery{Since: time.Now().Add(time.Hour)})); n != 0 {
		t.Fatalf("since: %d", n)
	}
}

func TestResumeFailedSession(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("resumer.sh", `{ echo "args:$*"; echo "dir:$CERBERUS_SESSION_DIR"; echo "cwd:$PWD"; } >> "$CERBERUS_SESSION_DIR/calls.log"
if [ "$1" = "--resume" ]; then echo resumed > resumed.txt; exit 0; fi
echo partial > partial.txt
exit 1
`)
		e.profiles(config.AgentProfile{Name: "resumer", Command: []string{"bash", agent, "{task_file}"},
			ResumeCommand: []string{"bash", agent, "--resume", "{resume_file}", "{task_file}"}})
	})
	id := e.createOne("do the thing")
	first := e.waitState(id, StateFailed)
	if err := e.m.Resume(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	info := e.waitState(id, StatePassed)
	if info.Attempt != 2 || info.Worktree != first.Worktree {
		t.Fatalf("resume should be attempt 2 in the same worktree: %+v", info)
	}
	for _, f := range []string{"partial.txt", "resumed.txt"} {
		if _, err := os.Stat(filepath.Join(info.Worktree, f)); err != nil {
			t.Fatalf("%s missing from the worktree: %v", f, err)
		}
	}
	dir := filepath.Join(e.m.sessDir, id)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	want := "args:--resume " + filepath.Join(dir, conversationFile) + " " + filepath.Join(dir, "task.md")
	if !strings.Contains(string(calls), want) || !strings.Contains(string(calls), "dir:"+dir+"\n") {
		t.Fatalf("calls:\n%s\nwant %q", calls, want)
	}
	task, _ := os.ReadFile(filepath.Join(dir, "task.md"))
	if strings.TrimSpace(string(task)) != resumeDefaultTask {
		t.Fatalf("task.md %q", task)
	}

	if err := e.m.Resume(context.Background(), "nope", ""); err != ErrNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := e.m.Accept(context.Background(), id, AcceptOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Resume(context.Background(), id, "more"); err == nil || !strings.Contains(err.Error(), "accepted") {
		t.Fatalf("accepted session must be refused: %v", err)
	}
}

func TestPruneKeepsUnacceptedWork(t *testing.T) {
	var plain string
	var forgotMu sync.Mutex
	var forgot []string
	e := newTestEnv(t, nil, func(e *testEnv) {
		e.opts = append(e.opts, WithForget(func(id string) { forgotMu.Lock(); forgot = append(forgot, id); forgotMu.Unlock() }))
		dirty := e.script("dirty.sh", "echo work > work.txt\nexit 0\n")
		committed := e.script("commit.sh", "echo c > c.txt && git add c.txt && git -c user.name=A -c user.email=a@example.com commit -qm c\nexit 0\n")
		e.profiles(
			config.AgentProfile{Name: "noop", Command: []string{"bash", "-c", "exit 0"}},
			config.AgentProfile{Name: "dirty", Command: []string{"bash", dirty}},
			config.AgentProfile{Name: "commit", Command: []string{"bash", committed}},
		)
		plain = filepath.Join(e.home, "plain")
		if err := os.MkdirAll(plain, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plain, "keep.txt"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	mk := func(ts TaskSpec) string {
		infos, err := e.m.Create(ctx, ts)
		if err != nil {
			t.Fatal(err)
		}
		return infos[0].ID
	}
	clean := mk(TaskSpec{Repo: e.repo, Task: "clean", Agent: "noop"})
	dirty := mk(TaskSpec{Repo: e.repo, Task: "dirty", Agent: "dirty"})
	ahead := mk(TaskSpec{Repo: e.repo, Task: "ahead", Agent: "commit"})
	folder := mk(TaskSpec{Dir: plain, Task: "folder", Agent: "noop"})
	fresh := mk(TaskSpec{Repo: e.repo, Task: "fresh", Agent: "noop"})
	infos := map[string]Info{}
	for _, id := range []string{clean, dirty, ahead, folder, fresh} {
		infos[id] = e.waitState(id, StatePassed)
	}
	// Age everything but "fresh" past the retention period.
	e.m.mu.Lock()
	for _, id := range []string{clean, dirty, ahead, folder} {
		e.m.sessions[id].info.Updated = time.Now().Add(-40 * 24 * time.Hour)
	}
	e.m.mu.Unlock()

	rep, err := e.m.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, k := range rep.Kept {
		kept = append(kept, k.ID)
		if k.Reason == "" {
			t.Fatalf("kept without a reason: %+v", k)
		}
	}
	slices.Sort(kept)
	wantKept := []string{dirty, ahead}
	slices.Sort(wantKept)
	if !slices.Equal(kept, wantKept) || len(rep.Errors) != 0 {
		t.Fatalf("report %+v", rep)
	}
	forgotMu.Lock()
	slices.Sort(forgot)
	wantForgot := []string{clean, folder}
	slices.Sort(wantForgot)
	if !slices.Equal(forgot, wantForgot) {
		t.Fatalf("forget hook got %v, want %v", forgot, wantForgot)
	}
	forgotMu.Unlock()
	for _, id := range []string{clean, folder} {
		if _, ok := e.m.Get(id); ok {
			t.Fatalf("%s should be pruned", id)
		}
		if _, err := os.Stat(filepath.Join(e.m.sessDir, id)); !os.IsNotExist(err) {
			t.Fatalf("%s session folder still there", id)
		}
	}
	if _, err := os.Stat(infos[clean].Worktree); !os.IsNotExist(err) {
		t.Fatal("the clean worktree should be removed")
	}
	for _, id := range []string{dirty, ahead} {
		if _, err := os.Stat(infos[id].Worktree); err != nil {
			t.Fatalf("worktree with unaccepted work removed: %v", err)
		}
	}
	if _, ok := e.m.Get(fresh); !ok {
		t.Fatal("a recent session was pruned")
	}
	if raw, err := os.ReadFile(filepath.Join(plain, "keep.txt")); err != nil || string(raw) != "mine" {
		t.Fatal("a folder session's folder must never be touched")
	}
	// The kept worktrees stay usable: resume still works there.
	if err := e.m.Resume(ctx, dirty, ""); err != nil {
		t.Fatal(err)
	}
	e.waitState(dirty, StatePassed)

	// Retention 0 keeps everything.
	e.cfg.Sessions.RetentionDays = 0
	if rep, _ := e.m.Prune(ctx); len(rep.Removed)+len(rep.Kept) != 0 {
		t.Fatalf("retention 0 pruned: %+v", rep)
	}
}
