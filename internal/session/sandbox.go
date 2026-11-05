package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/sandbox"
)

// agentConfigName is the built-in agent's config snapshot in the session
// folder: the parts of config.json it needs (agent, context, files), with no
// token. It is rewritten from the live config at every launch, so an agent
// that edits it changes nothing past its own run, and under bwrap it is
// bound read-only. The agent reads it through $CERBERUS_AGENT_CONFIG, since
// a sandbox hides Cerberus' config folder.
const agentConfigName = "agent-config.json"

// FeedWarning is a feed item about something the owner should know, such as
// a session running without the sandbox it asked for.
const FeedWarning = "warning"

// sandboxAvailable is sandbox.Available; tests swap it.
var sandboxAvailable = sandbox.Available

// ErrSandboxUnavailable refuses a confined (non-admin) session when bwrap
// is missing or fails its probe.
var ErrSandboxUnavailable = errors.New("this server cannot sandbox sessions for other users: bubblewrap (bwrap) is missing")

// SandboxAvailable reports whether bwrap is usable (the API checks it
// before starting a confined session).
func SandboxAvailable(ctx context.Context) error {
	_, err := sandboxAvailable(ctx)
	return err
}

// WithAudit records sandbox fallbacks and refusals in the audit log.
func WithAudit(l *audit.Logger) Option { return func(m *Manager) { m.audit = l } }

// sandboxPlan is how one run (or one checks pass) of a session is confined.
type sandboxPlan struct {
	bin      string // bwrap; "" = not sandboxed
	spec     sandbox.Spec
	fallback error // bwrap was wanted but is unavailable: running unconfined
}

// wrap returns argv inside the sandbox (unchanged when not sandboxed).
func (pl sandboxPlan) wrap(argv []string) []string {
	if pl.bin == "" {
		return argv
	}
	return sandbox.Wrap(pl.bin, pl.spec, argv)
}

// planSandbox resolves the session's sandbox (repo override > profile >
// config default). bwrap that is wanted but unavailable is an error under
// Sandbox.Strict and a fallback otherwise.
func (m *Manager) planSandbox(ctx context.Context, info Info, p config.AgentProfile, sessDir string, protect []string) (sandboxPlan, error) {
	repoMode := ""
	for _, r := range []string{info.Repo, info.Dir} {
		if r == "" {
			continue
		}
		if rc, ok := m.repoConfig(r); ok {
			repoMode = rc.Sandbox
			break
		}
	}
	// The owner's current role decides too: a stored Confined is never
	// the only word (a demoted owner's old session resumes confined).
	if !info.Confined && m.confineOwner != nil && m.confineOwner(info.Owner) {
		info.Confined = true
	}
	if !info.Confined && sandbox.Resolve(m.conf().Sandbox, p, repoMode) != sandbox.ModeBwrap {
		return sandboxPlan{}, nil
	}
	bin, err := sandboxAvailable(ctx)
	if err != nil {
		if info.Confined {
			// A non-admin user's session never runs unconfined.
			return sandboxPlan{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
		}
		if m.conf().Sandbox.Strict {
			return sandboxPlan{}, fmt.Errorf("sandbox required (sandbox.strict) but unavailable: %v", err)
		}
		return sandboxPlan{fallback: err}, nil
	}
	spec := sandbox.FromConfig(m.conf().Sandbox, p, m.home)
	if info.Confined {
		spec = spec.Confine(p)
		// The home is hidden: a shared repo's worktree still needs the
		// repo's git folder (read-only, as before).
		if info.Repo != "" {
			if common, err := gitLine(ctx, info.Repo, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil && filepath.IsAbs(common) {
				spec.KeepRO = append(spec.KeepRO, common)
			}
		}
	}
	spec.Workspace = info.Worktree
	spec.SessionDir = sessDir
	spec.Hide = m.sandboxHidden()
	spec.Protect = protect
	return sandboxPlan{bin: bin, spec: spec}, nil
}

// sandboxHidden are paths every sandbox hides whatever HideHome says:
// Cerberus' data folder (other sessions, the audit log, the feed; the
// session's own folder and workspace are bound back in), its config folder
// (the admin token) and the user runtime folder (the user's D-Bus and
// systemd sockets, which would let an agent start processes or read the
// keyring outside the sandbox).
func (m *Manager) sandboxHidden() []string {
	var out []string
	if d, err := platform.DataDir(); err == nil {
		out = append(out, d)
	}
	if m.conf().Path() != "" {
		out = append(out, m.conf().Dir())
	} else if p, err := config.DefaultPath(); err == nil {
		out = append(out, filepath.Dir(p))
	}
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		out = append(out, r)
	}
	var abs []string
	for _, p := range out {
		if filepath.IsAbs(p) && filepath.Clean(p) != "/" {
			abs = append(abs, filepath.Clean(p))
		}
	}
	return abs
}

// sandboxWarn tells the owner that a session runs without the sandbox it
// asked for: the terminal, the feed and the audit log.
func (m *Manager) sandboxWarn(s *session, what string, why error) {
	text := fmt.Sprintf("sandbox unavailable, %s without bwrap: %v", what, why)
	fmt.Fprintf(s.out, "\r\n\x1b[33m── cerberus: %s ──\x1b[0m\r\n", text)
	m.Record(s.info.ID, FeedWarning, text)
	if err := m.audit.Event(audit.ActorSystem, "sandbox.fallback", text, s.info.ID); err != nil {
		m.log.Warn("audit", "err", err)
	}
	m.log.Warn("sandbox fallback", "id", s.info.ID, "err", why)
}

// sandboxRefused records a refusal under Sandbox.Strict.
func (m *Manager) sandboxRefused(id string, err error) {
	if aerr := m.audit.Event(audit.ActorSystem, "sandbox.refused", err.Error(), id); aerr != nil {
		m.log.Warn("audit", "err", aerr)
	}
}

// writeAgentConfig writes the built-in agent's config snapshot into dir and
// returns its path.
func (m *Manager) writeAgentConfig(dir string) (string, error) {
	snap := map[string]any{
		"agent":   m.conf().Agent,
		"context": m.conf().Context,
		"files":   map[string]any{"upload_dir": m.conf().Files.UploadDir},
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, agentConfigName)
	return p, safeWrite(p, raw, 0o600)
}

// safeWrite replaces path atomically through a fresh temp file in the same
// folder. A sandboxed agent can write in its session folder, so it could
// plant a symlink at a fixed name such as "meta.json.tmp": CreateTemp
// (O_EXCL, random name) and rename never follow one.
func safeWrite(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, perm)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// verifyGitLink checks a linked worktree's ".git" file before Cerberus runs
// git in it on the host. The agent can rewrite that file to point at a git
// folder it made, whose config (core.fsmonitor, filters, gpg.program …)
// would then run its commands outside any sandbox. The file must be a
// regular file naming a folder inside the repo's common git dir/worktrees,
// and that folder's commondir must lead back to the same common dir. A
// folder without a ".git" file (a main checkout, a scratch folder) passes.
func verifyGitLink(wt, commonDir string) error {
	p := filepath.Join(wt, ".git")
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		if commonDir == "" {
			return nil // a main checkout
		}
		return fmt.Errorf("%s is a folder, not a worktree link", p)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return fmt.Errorf("%s does not start with gitdir:", p)
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wt, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if commonDir == "" {
		return fmt.Errorf("%s: unknown repo", p)
	}
	common, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(gitdir)
	if err != nil {
		return fmt.Errorf("%s points at %s: %w", p, gitdir, err)
	}
	if !within(real, filepath.Join(common, "worktrees")) {
		return fmt.Errorf("%s points outside the repo's git folder (%s)", p, gitdir)
	}
	if cd, err := os.ReadFile(filepath.Join(real, "commondir")); err == nil {
		c := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(c) {
			c = filepath.Join(real, c)
		}
		if rc, err := filepath.EvalSymlinks(c); err != nil || rc != common {
			return fmt.Errorf("%s: its commondir does not lead back to the repo", p)
		}
	}
	return nil
}

// checkWorktree runs verifyGitLink for a repo session's worktree.
func checkWorktree(ctx context.Context, repo, wt string) error {
	if repo == "" || wt == "" || filepath.Clean(repo) == filepath.Clean(wt) {
		return nil
	}
	common, err := gitLine(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if err := verifyGitLink(wt, common); err != nil {
		return fmt.Errorf("refusing to run git in the worktree: %w", err)
	}
	return nil
}
