package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

const (
	checksOutputMax = 64 << 10
	checkKillGrace  = 3 * time.Second
)

// repoSettings are a repo's checks and preview, from config or .cerberus.json.
type repoSettings struct {
	Checks  []string
	Preview *config.PreviewConfig
}

// cerberusFile is <worktree>/.cerberus.json.
type cerberusFile struct {
	Checks  []string              `json:"checks"`
	Preview *config.PreviewConfig `json:"preview"`
}

// validateRepo resolves repo and requires it to be the top of a git repo
// under the home folder (or configured in cfg.Repos or under a
// cfg.RepoRoots entry), not protected by cfg.Agent.DenyPaths, and clear of
// Cerberus' own data and config folders.
func (m *Manager) validateRepo(ctx context.Context, repo string) (string, error) {
	if repo == "" || !filepath.IsAbs(repo) {
		return "", fmt.Errorf("repo must be an absolute path, got %q", repo)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(repo))
	if err != nil {
		return "", fmt.Errorf("repo %s: %w", repo, err)
	}
	for _, p := range []string{filepath.Clean(repo), real} {
		if pat := agent.DeniedBy(m.conf().Agent.DenyPaths, p); pat != "" {
			return "", fmt.Errorf("%s is protected by deny_paths (%s)", repo, pat)
		}
	}
	top, err := gitLine(ctx, real, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not a git repository", repo)
	}
	if t, err := filepath.EvalSymlinks(top); err != nil || t != real {
		return "", fmt.Errorf("%s is not the top of a git repository", repo)
	}
	for _, own := range m.ownDirs() {
		if real == own || within(real, own) || within(own, real) {
			return "", fmt.Errorf("%s holds Cerberus' own data or config", repo)
		}
	}
	if !m.underAllowedRoot(real) {
		return "", fmt.Errorf("%s is outside your home folder (add it to repos in the config to allow it)", repo)
	}
	if err := checkRepoConfig(ctx, real); err != nil {
		return "", err
	}
	return real, nil
}

// repoConfig finds the cfg.Repos entry for repo (already resolved).
func (m *Manager) repoConfig(repo string) (config.RepoConfig, bool) {
	for _, r := range m.conf().Repos {
		if p, err := filepath.EvalSymlinks(r.Path); err == nil && p == repo {
			return r, true
		}
	}
	return config.RepoConfig{}, false
}

// settings returns the repo's checks and preview. A repo without a config
// entry is read from .cerberus.json in the worktree (the agent may add it).
func (m *Manager) settings(repo, worktree string) repoSettings {
	if rc, ok := m.repoConfig(repo); ok {
		return repoSettings{Checks: rc.Checks, Preview: rc.Preview}
	}
	if worktree == "" {
		return repoSettings{}
	}
	raw, err := os.ReadFile(filepath.Join(worktree, ".cerberus.json"))
	if err != nil {
		return repoSettings{}
	}
	var cf cerberusFile
	if json.Unmarshal(raw, &cf) != nil {
		m.log.Warn("invalid .cerberus.json", "worktree", worktree)
		return repoSettings{}
	}
	return repoSettings{Checks: cf.Checks, Preview: cf.Preview}
}

// previewDir is the absolute preview folder inside worktree, or "" when
// there is no preview or it would escape the worktree.
func previewDir(worktree string, p *config.PreviewConfig) string {
	if p == nil || worktree == "" {
		return ""
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return ""
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(p.Dir)))
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

// PreviewDir is the folder api serves as the session's static preview: the
// configured one or a detected one (staticDir), while the worktree exists.
func (m *Manager) PreviewDir(id string) (string, bool) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	var repo, wt string
	if ok && !final(s.info.State) {
		repo, wt = s.info.Repo, s.info.Worktree
	}
	m.mu.Unlock()
	if wt == "" {
		return "", false
	}
	d := m.staticDir(repo, wt)
	return d, d != ""
}

// previewURL is Info.PreviewURL for a session's current worktree.
func (m *Manager) previewURL(id, repo, wt string) string {
	if wt == "" || m.staticDir(repo, wt) == "" {
		return ""
	}
	return "/preview/" + id + "/"
}

// runChecks runs each check with bash -c in dir, stopping at the first
// failure. It returns whether all passed and the combined output, clipped.
// wrap (nil = none) puts each check's argv inside the session's sandbox.
func runChecks(ctx context.Context, dir string, checks []string, timeout time.Duration, env []string, wrap func([]string) []string) (bool, string) {
	var out tailBuffer
	out.max = checksOutputMax
	for _, c := range checks {
		fmt.Fprintf(&out, "$ %s\n", c)
		err := runCheck(ctx, dir, c, timeout, env, wrap, &out)
		switch {
		case err == nil:
			continue
		case ctx.Err() != nil:
			fmt.Fprintf(&out, "\n(checks cancelled)\n")
		case errors.Is(err, context.DeadlineExceeded):
			fmt.Fprintf(&out, "\n(timed out after %s)\n", timeout)
		default:
			fmt.Fprintf(&out, "\n(%v)\n", err)
		}
		return false, out.String()
	}
	return true, out.String()
}

// runCheck runs one check in its own process group, killed as a group on
// timeout or cancellation.
func runCheck(ctx context.Context, dir, check string, timeout time.Duration, env []string, wrap func([]string) []string, out *tailBuffer) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := []string{"bash", "-c", check}
	if wrap != nil {
		argv = wrap(argv)
	}
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = out, out
	platform.NewGroup(cmd)
	exited := make(chan struct{})
	cmd.Cancel = func() error {
		platform.Terminate(cmd.Process, checkKillGrace, exited)
		return nil
	}
	cmd.WaitDelay = checkKillGrace + time.Second
	err := cmd.Run()
	close(exited)
	if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return context.DeadlineExceeded
	}
	return err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max     int
	buf     []byte
	clipped bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		n := copy(t.buf, t.buf[len(t.buf)-t.max:])
		t.buf = t.buf[:n]
		t.clipped = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.buf
	if len(b) > t.max {
		b = b[len(b)-t.max:]
		t.clipped = true
	}
	if t.clipped {
		return "…(output clipped)…\n" + string(b)
	}
	return string(b)
}
