package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds every git call; push can be slow, so it is generous.
const gitTimeout = 5 * time.Minute

// errNoChanges is returned by Accept when the worktree adds nothing to main.
var errNoChanges = errors.New("nothing to accept: the worktree has no changes against main")

// forbiddenGit lists subcommands that can create refs under refs/heads. The
// fleet rule is that Cerberus never creates branches; this is a safety net.
var forbiddenGit = map[string]bool{"branch": true, "checkout": true, "switch": true}

// runGit runs git -C dir args… with prompts disabled and returns stdout.
// The error carries git's stderr.
func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	if sub := args[firstSub(args)]; forbiddenGit[sub] {
		return "", fmt.Errorf("git %s is not allowed (Cerberus never creates branches)", sub)
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append(append([]string{"-C", dir}, hostGitSafety...), args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("git %s: %w: %s", args[firstSub(args)], err, msg)
	}
	return stdout.String(), nil
}

// hostGitSafety goes in front of every host git call. Cerberus runs git
// on the host, outside any sandbox, in repos an agent may have written to
// (a sandboxed folder session can `git init` its folder or edit the .git of
// repos under it), so git must not run code the repo carries: no hooks
// (post-checkout fires on `worktree add`) and no fsmonitor command. The
// command line beats every config file. Other command-running keys are
// refused up front by checkRepoConfig.
var hostGitSafety = []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}

// repoCommandKeys matches config keys whose value git runs as a command
// (keys are lower-cased by git; subsection names keep their case).
var repoCommandKeys = `^(core\.(sshcommand|gitproxy|askpass|alternaterefscommand)|` +
	`filter\..*\.(clean|smudge|process)|diff\.external|diff\..*\.(textconv|command)|` +
	`merge\..*\.driver|sequence\.editor|gpg\.program|gpg\..*\.program|` +
	`uploadpack\.packobjectshook|credential\.helper|credential\..*\.helper|include\.path|includeif\..*\.path)$`

// checkRepoConfig refuses a repo whose own config (.git/config, or a file
// it includes) names a command git would run on the host for Cerberus: an
// agent could have written it. Global and system config are the owner's
// and are trusted. Git LFS filters and plain credential helper names
// (git-credential-<name> on PATH) are allowed.
func checkRepoConfig(ctx context.Context, repo string) error {
	out, err := runGit(ctx, repo, nil, "config", "--show-scope", "-z", "--get-regexp", repoCommandKeys)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil // no matching key
		}
		return fmt.Errorf("read the git config of %s: %w", repo, err)
	}
	// -z with --show-scope prints "scope\x00key\nvalue\x00" per entry, so
	// after the split scope and key/value alternate.
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		scope := fields[i]
		key, val, _ := strings.Cut(fields[i+1], "\n")
		if scope != "local" && scope != "worktree" {
			continue
		}
		if repoKeyAllowed(key, val) {
			continue
		}
		return fmt.Errorf("%s: its .git/config sets %s, a command git would run outside the sandbox; Cerberus refuses repos whose own config runs commands (an agent could have written it). Move the setting to your global git config or remove it", repo, key)
	}
	return nil
}

// repoKeyAllowed lets through the command-running repo keys that are
// common and harmless: Git LFS and named credential helpers.
func repoKeyAllowed(key, val string) bool {
	lk := strings.ToLower(key)
	if strings.HasPrefix(lk, "filter.lfs.") && (strings.HasPrefix(val, "git-lfs ") || val == "") {
		return true
	}
	if strings.HasPrefix(lk, "credential.") && strings.HasSuffix(lk, ".helper") || lk == "credential.helper" {
		return val == "" || !strings.ContainsAny(val, "!/\\ ")
	}
	return false
}

// firstSub is the index of the subcommand, skipping "-c key=value" pairs.
func firstSub(args []string) int {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return i
	}
	return 0
}

func gitLine(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGit(ctx, dir, nil, args...)
	return strings.TrimSpace(out), err
}

// mainBranch is the branch the main checkout has checked out. Accept requires
// it to be main or master.
func mainBranch(ctx context.Context, repo string) (string, error) {
	b, err := gitLine(ctx, repo, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || b == "" {
		return "", fmt.Errorf("the main checkout of %s is not on a branch (detached HEAD); check out main first", repo)
	}
	if b != "main" && b != "master" {
		return "", fmt.Errorf("the main checkout of %s is on %q, not main; check out main first", repo, b)
	}
	return b, nil
}

// mainHead is the commit new worktrees start from: main, else master, else HEAD.
func mainHead(ctx context.Context, repo string) (string, error) {
	for _, ref := range []string{"refs/heads/main", "refs/heads/master", "HEAD"} {
		if sha, err := gitLine(ctx, repo, "rev-parse", "--verify", "-q", ref+"^{commit}"); err == nil && sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%s has no commits on main", repo)
}

// addWorktree creates a detached worktree at path on commit base.
func addWorktree(ctx context.Context, repo, path, base string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, err := runGit(ctx, repo, nil, "worktree", "add", "--detach", path, base)
	return err
}

// removeWorktree deletes the worktree, falling back to deleting the folder
// and pruning git's record of it.
func removeWorktree(ctx context.Context, repo, path string) error {
	if path == "" {
		return nil
	}
	if _, err := runGit(ctx, repo, nil, "worktree", "remove", "--force", path); err == nil {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	_, err := runGit(ctx, repo, nil, "worktree", "prune")
	return err
}

// hasStaged reports whether the index differs from HEAD.
func hasStaged(ctx context.Context, wt string) (bool, error) {
	_, err := runGit(ctx, wt, nil, "diff", "--cached", "--quiet", "--no-ext-diff", "--no-textconv")
	if err == nil {
		return false, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

// diffWorktree is the unified diff of the worktree (including untracked,
// non-ignored files) against base. It stages into a throwaway copy of the
// worktree's index so the real index is untouched.
func diffWorktree(ctx context.Context, wt, base, scratchDir string) (string, error) {
	idx, err := gitLine(ctx, wt, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(idx) {
		idx = filepath.Join(wt, idx)
	}
	tmp, err := os.CreateTemp(scratchDir, "diff-*.index")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if raw, err := os.ReadFile(idx); err == nil {
		_, err = tmp.Write(raw) // reuse stat info so unchanged files are not rehashed
		if err != nil {
			tmp.Close()
			return "", err
		}
	}
	tmp.Close()
	env := []string{"GIT_INDEX_FILE=" + tmpPath}
	if fi, _ := os.Stat(tmpPath); fi == nil || fi.Size() == 0 {
		os.Remove(tmpPath) // git wants no file rather than an empty one
		if _, err := runGit(ctx, wt, env, "read-tree", base); err != nil {
			return "", err
		}
	}
	if _, err := runGit(ctx, wt, env, "add", "-A"); err != nil {
		return "", err
	}
	return runGit(ctx, wt, env, "diff", "--cached", "--no-color", "--no-ext-diff", "--no-textconv", base)
}

// identityArgs are the -c options for the commit identity from cfg.Git.
func (m *Manager) identityArgs() []string {
	var args []string
	if n := m.conf().Git.Name; n != "" {
		args = append(args, "-c", "user.name="+n)
	}
	if e := m.conf().Git.Email; e != "" {
		args = append(args, "-c", "user.email="+e)
	}
	return args
}

func (m *Manager) commit(ctx context.Context, wt, msg string) error {
	args := append(m.identityArgs(), "commit", "-q", "-m", msg)
	_, err := runGit(ctx, wt, nil, args...)
	return err
}

// squashOntoMain turns the worktree's work into one commit on top of the
// repo's current main and returns its sha. The worktree stays detached. Once
// the rebase has succeeded it also returns base, the main commit the
// worktree now builds on (even when a later step fails).
func (m *Manager) squashOntoMain(ctx context.Context, repo, wt, branch, msg string) (sha, base string, err error) {
	if err := requireDetached(ctx, wt); err != nil {
		return "", "", err
	}
	if _, err := runGit(ctx, wt, nil, "add", "-A"); err != nil {
		return "", "", err
	}
	if staged, err := hasStaged(ctx, wt); err != nil {
		return "", "", err
	} else if staged {
		if err := m.commit(ctx, wt, msg); err != nil {
			return "", "", err
		}
	}
	mainSHA, err := gitLine(ctx, repo, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", "", err
	}
	// The rebase replays commits, so it needs the committer identity too.
	if _, err := runGit(ctx, wt, nil, append(m.identityArgs(), "rebase", "-q", mainSHA)...); err != nil {
		_, _ = runGit(ctx, wt, nil, "rebase", "--abort")
		return "", "", fmt.Errorf("rebase onto %s failed (the worktree is unchanged): %w", branch, err)
	}
	if _, err := runGit(ctx, wt, nil, "reset", "-q", "--soft", mainSHA); err != nil {
		return "", mainSHA, err
	}
	staged, err := hasStaged(ctx, wt)
	if err != nil {
		return "", mainSHA, err
	}
	if !staged {
		return "", mainSHA, errNoChanges
	}
	if err := m.commit(ctx, wt, msg); err != nil {
		return "", mainSHA, err
	}
	sha, err = gitLine(ctx, wt, "rev-parse", "HEAD")
	return sha, mainSHA, err
}

// requireDetached refuses a worktree whose HEAD is on a branch: an agent
// switched to one (worktrees share refs/heads with the repo), and squashing
// would rebase and move that branch. Cerberus never moves branches.
func requireDetached(ctx context.Context, wt string) error {
	b, err := gitLine(ctx, wt, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil || b == "" {
		return nil // detached (symbolic-ref exits 1)
	}
	return fmt.Errorf("the session's worktree is on branch %q, which the agent created; Cerberus never moves branches. "+
		"Detach it (git -C %s checkout --detach), delete the branch if unwanted, and accept again", b, wt)
}

// pushOnly pushes branch only when that publishes exactly sha: origin's copy
// must be sha's parent. Otherwise the user's own unpushed commits would go out
// with it unseen, so nothing is pushed and the error lists what would have.
func pushOnly(ctx context.Context, repo, branch, sha string) error {
	rng := "refs/remotes/origin/" + branch + ".." + sha
	out, err := gitLine(ctx, repo, "rev-list", rng)
	if err != nil {
		return fmt.Errorf("not pushed: cannot compare with origin/%s (fetch first): %w", branch, err)
	}
	switch commits := strings.Fields(out); {
	case len(commits) == 0:
		return nil // origin already has it
	case len(commits) > 1 || commits[0] != sha:
		list, _ := gitLine(ctx, repo, "log", "--oneline", "--no-decorate", rng)
		return fmt.Errorf("not pushed: %s has %d other unpushed commit(s) that would go out too:\n%s",
			branch, len(commits)-1, list)
	}
	_, err = runGit(ctx, repo, nil, "push", "origin", sha+":refs/heads/"+branch)
	return err
}
