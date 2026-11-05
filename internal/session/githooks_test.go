//go:build !windows

package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestHostGitRunsNoRepoHooks: a post-checkout hook planted in a repo (as a
// sandboxed folder-session agent could) does not run when Cerberus adds a
// worktree on the host.
func TestHostGitRunsNoRepoHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	newRepo(t, repo)
	marker := filepath.Join(dir, "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "config", "core.fsmonitor", "touch '"+marker+"'; false")
	if err := addWorktree(context.Background(), repo, filepath.Join(dir, "wt"), "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := diffWorktree(context.Background(), filepath.Join(dir, "wt"), "HEAD", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repo hook or fsmonitor command ran on the host")
	}
}

// TestCheckRepoConfig: repo-local keys that run commands are refused, LFS
// and named credential helpers pass.
func TestCheckRepoConfig(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	newRepo(t, repo)
	ctx := context.Background()
	gitT(t, repo, "config", "filter.lfs.smudge", "git-lfs smudge -- %f")
	gitT(t, repo, "config", "credential.helper", "store")
	if err := checkRepoConfig(ctx, repo); err != nil {
		t.Fatalf("lfs and a named helper refused: %v", err)
	}
	for _, kv := range [][2]string{
		{"filter.x.smudge", "touch /tmp/x"},
		{"diff.pdf.textconv", "sh -c evil"},
		{"core.sshCommand", "sh -c evil"},
		{"credential.helper", "!sh -c evil"},
		{"include.path", "../evil.cfg"},
	} {
		gitT(t, repo, "config", kv[0], kv[1])
		err := checkRepoConfig(ctx, repo)
		if err == nil || !strings.Contains(err.Error(), "outside the sandbox") {
			t.Errorf("%s accepted: %v", kv[0], err)
		}
		gitT(t, repo, "config", "--unset-all", kv[0])
	}
	gitT(t, repo, "config", "credential.helper", "store")
	if err := checkRepoConfig(ctx, repo); err != nil {
		t.Fatalf("after cleanup: %v", err)
	}
}
