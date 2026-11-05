package session

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"carino.systems/cerberus/internal/agent"
)

// Workspace kinds: where a session's agent works.
const (
	// WorkspaceRepo is a detached git worktree of a repo: diff, checks and
	// accept onto main.
	WorkspaceRepo = "repo"
	// WorkspaceFolder is a plain folder the agent edits in place: changes are
	// live, there is no diff or accept, and Cerberus never deletes it.
	WorkspaceFolder = "folder"
	// WorkspaceScratch is an empty folder Cerberus owns
	// (DATA/sessions/<id>/workspace); its files can be saved to a folder or
	// downloaded as a zip at any time.
	WorkspaceScratch = "scratch"
)

// resolveWorkspace decides t.Workspace and validates its folder:
//
//   - Repo set (older clients) or Workspace "repo": a git repo top level.
//   - Workspace "" or "folder" with Dir: a git repo top level becomes a repo
//     session, any other folder a folder session.
//   - nothing: scratch.
func (m *Manager) resolveWorkspace(ctx context.Context, t *TaskSpec) error {
	switch t.Workspace {
	case "", WorkspaceRepo, WorkspaceFolder, WorkspaceScratch:
	default:
		return fmt.Errorf("unknown workspace %q (repo, folder or scratch)", t.Workspace)
	}
	if t.Workspace == WorkspaceScratch {
		t.Repo, t.Dir = "", ""
		return nil
	}
	if t.Workspace == WorkspaceRepo && t.Repo == "" {
		t.Repo = t.Dir
	}
	if t.Workspace == "" && t.Repo == "" && t.Dir == "" {
		t.Workspace = WorkspaceScratch
		return nil
	}
	if t.Repo != "" && (t.Workspace == "" || t.Workspace == WorkspaceRepo) {
		repo, err := m.validateRepo(ctx, t.Repo)
		if err != nil {
			return err
		}
		t.Workspace, t.Repo, t.Dir = WorkspaceRepo, repo, repo
		return nil
	}
	if t.Dir == "" {
		return errors.New("choose a folder for this session")
	}
	real, err := m.allowedDir(t.Dir)
	if err != nil {
		return err
	}
	if isRepoTop(ctx, real) {
		repo, err := m.validateRepo(ctx, real)
		if err != nil {
			return err
		}
		t.Workspace, t.Repo, t.Dir = WorkspaceRepo, repo, repo
		return nil
	}
	if real == m.home {
		return errors.New("the agent cannot work directly in your home folder; pick a folder inside it")
	}
	t.Workspace, t.Repo, t.Dir = WorkspaceFolder, "", real
	return nil
}

// isRepoTop reports whether dir is the top level of a git repository.
func isRepoTop(ctx context.Context, dir string) bool {
	top, err := gitLine(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	t, err := filepath.EvalSymlinks(top)
	return err == nil && t == dir
}

// allowedDir resolves dir and checks that a session may use it: an existing
// folder under the home folder, a configured repo or a repo root, not
// protected by deny_paths, and neither inside nor containing Cerberus' own
// data or config folders.
func (m *Manager) allowedDir(dir string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("folder must be an absolute path, got %q", dir)
	}
	clean := filepath.Clean(dir)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("folder %s: %w", dir, err)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a folder", dir)
	}
	for _, p := range []string{clean, real} {
		if pat := agent.DeniedBy(m.conf().Agent.DenyPaths, p); pat != "" {
			return "", fmt.Errorf("%s is protected by deny_paths (%s)", dir, pat)
		}
	}
	for _, own := range m.ownDirs() {
		if real == own || within(real, own) || within(own, real) && real != m.home {
			return "", fmt.Errorf("%s holds Cerberus' own data or config; pick another folder", dir)
		}
	}
	if !m.underAllowedRoot(real) {
		return "", fmt.Errorf("%s is outside your home folder", dir)
	}
	return real, nil
}

// ownDirs are Cerberus' data and config folders (resolved).
func (m *Manager) ownDirs() []string {
	var out []string
	for _, d := range []string{filepath.Dir(m.sessDir), m.configDir()} {
		if d == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(d); err == nil {
			d = r
		}
		out = append(out, d)
	}
	return out
}

func (m *Manager) configDir() string {
	if m.conf().Path() != "" {
		return m.conf().Dir()
	}
	return ""
}

// underAllowedRoot reports whether real is the home folder or inside it, or
// inside a configured repo or repo root.
func (m *Manager) underAllowedRoot(real string) bool {
	roots := []string{m.home}
	for _, r := range m.conf().Repos {
		roots = append(roots, r.Path)
	}
	roots = append(roots, m.conf().RepoRoots...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(root); err == nil && (real == r || within(real, r)) {
			return true
		}
	}
	return false
}

// within reports whether path is strictly inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// FSEntry is one folder in a Browse listing.
type FSEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Git  bool   `json:"git"`
}

// Listing is the folder browser's view of one folder.
type Listing struct {
	Path    string    `json:"path"`
	Parent  string    `json:"parent"` // "" at the home folder
	Git     bool      `json:"git"`    // Path itself is a git repo top level
	Home    bool      `json:"home"`   // Path is the home folder (not usable as a folder session)
	Entries []FSEntry `json:"entries"`
}

// Browse lists the sub-folders of dir (the home folder when empty) for the
// folder picker: folders only, sorted, dot-folders only with hidden, denied
// and Cerberus-owned folders left out. Nothing outside the home folder.
func (m *Manager) Browse(dir string, hidden bool) (Listing, error) {
	if dir == "" {
		dir = m.home
	}
	if !filepath.IsAbs(dir) {
		return Listing{}, fmt.Errorf("path must be absolute, got %q", dir)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return Listing{}, err
	}
	if real != m.home && !within(real, m.home) {
		return Listing{}, fmt.Errorf("%s is outside your home folder", dir)
	}
	if pat := agent.DeniedBy(m.conf().Agent.DenyPaths, real); pat != "" {
		return Listing{}, fmt.Errorf("%s is protected by deny_paths (%s)", dir, pat)
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return Listing{}, err
	}
	l := Listing{Path: real, Home: real == m.home, Git: isGitDir(real), Entries: []FSEntry{}}
	if real != m.home {
		l.Parent = filepath.Dir(real)
	}
	own := m.ownDirs()
	for _, e := range entries {
		name := e.Name()
		if !hidden && strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(real, name)
		st, err := os.Stat(p) // follows symlinks to folders
		if err != nil || !st.IsDir() {
			continue
		}
		if agent.DeniedBy(m.conf().Agent.DenyPaths, p) != "" || slices.Contains(own, p) {
			continue
		}
		if t, err := filepath.EvalSymlinks(p); err != nil || (t != m.home && !within(t, m.home)) {
			continue // a link out of the home folder
		}
		l.Entries = append(l.Entries, FSEntry{Name: name, Path: p, Git: isGitDir(p)})
	}
	slices.SortFunc(l.Entries, func(a, b FSEntry) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return l, nil
}

// isGitDir is a cheap check for a repo top level (a .git folder or file).
func isGitDir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// scratchDir is a scratch session's workspace folder.
func (m *Manager) scratchDir(id string) string {
	return filepath.Join(m.sessDir, id, "workspace")
}

// workspaceOf returns the scratch workspace of a session, or an error when
// the session is not a scratch session.
func (m *Manager) workspaceOf(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookup(id)
	if err != nil {
		return "", err
	}
	if s.info.Workspace != WorkspaceScratch {
		return "", errors.New("only scratch sessions can be saved or downloaded")
	}
	if s.info.State == StateDiscarded {
		return "", errors.New("session is discarded")
	}
	return s.info.Worktree, nil
}

// SaveTo copies a scratch session's files into dir (created if missing).
// Existing files are refused unless overwrite; denied paths always are.
// It can run at any time, also while the agent works (a snapshot).
func (m *Manager) SaveTo(id, dir string, overwrite bool) (n int, err error) {
	src, err := m.workspaceOf(id)
	if err != nil {
		return 0, err
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return 0, fmt.Errorf("folder must be an absolute path, got %q", dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	dst, err := m.allowedDir(dir)
	if err != nil {
		return 0, err
	}
	if dst == m.home {
		return 0, errors.New("pick a folder inside your home folder, not the home folder itself")
	}
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		return 0, err
	}
	defer dstRoot.Close()
	return m.copyInto(id, src, dstRoot, dst, overwrite)
}

// SaveInto is SaveTo for a confined (non-admin) caller: dir must lie inside
// root (their own folder, which may sit under Cerberus' data folder), and
// every step is resolved through os.Root, so a symlink the user planted in
// root cannot send the files anywhere outside it.
func (m *Manager) SaveInto(id, root, dir string, overwrite bool) (n int, err error) {
	src, err := m.workspaceOf(id)
	if err != nil {
		return 0, err
	}
	if dir == "" || !filepath.IsAbs(dir) || !filepath.IsAbs(root) {
		return 0, fmt.Errorf("folder must be an absolute path, got %q", dir)
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(dir))
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return 0, fmt.Errorf("%s is outside your folder %s", dir, root)
	}
	top, err := os.OpenRoot(root)
	if err != nil {
		return 0, err
	}
	defer top.Close()
	if rel != "." {
		if err := top.MkdirAll(rel, 0o700); err != nil {
			return 0, fmt.Errorf("%s: %w", dir, err)
		}
	}
	dstRoot, err := top.OpenRoot(rel) // symlinks resolve inside root only
	if err != nil {
		return 0, fmt.Errorf("%s: %w", dir, err)
	}
	defer dstRoot.Close()
	return m.copyInto(id, src, dstRoot, filepath.Join(root, rel), overwrite)
}

// copyInto copies the scratch workspace src into dstRoot (shown as dst).
func (m *Manager) copyInto(id, src string, dstRoot *os.Root, dst string, overwrite bool) (n int, err error) {
	files, err := listFiles(src)
	if err != nil {
		return 0, err
	}
	if !overwrite {
		var clash []string
		for _, rel := range files {
			if _, err := dstRoot.Lstat(rel); err == nil {
				clash = append(clash, rel)
			}
		}
		if len(clash) > 0 {
			if len(clash) > 5 {
				clash = append(clash[:5], "…")
			}
			return 0, fmt.Errorf("%d file(s) already exist in %s: %s (save with overwrite to replace them)",
				len(clash), dst, strings.Join(clash, ", "))
		}
	}
	copied := 0
	defer func() { // a user action: the feed shows it, also when it stopped partway
		if copied > 0 || err == nil {
			what := "saved"
			if overwrite {
				what = "saved (overwrite)"
			}
			text := fmt.Sprintf("%s %d of %d files to %s", what, copied, len(files), dst)
			if err == nil {
				text = fmt.Sprintf("%s %d files to %s", what, copied, dst)
			}
			m.Record(id, FeedUpdate, text)
		}
	}()
	// Both ends are opened through os.Root: the agent may swap a listed
	// file (or a folder on its path) for a symlink after listFiles, and a
	// symlink in the destination must not redirect a write either.
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		return 0, err
	}
	defer srcRoot.Close()
	for _, rel := range files {
		target := filepath.Join(dst, rel)
		if pat := agent.DeniedBy(m.conf().Agent.DenyPaths, target); pat != "" {
			return 0, fmt.Errorf("%s is protected by deny_paths (%s); nothing more was copied", target, pat)
		}
		if err := copyFile(srcRoot, dstRoot, rel); err != nil {
			return 0, err
		}
		copied++
	}
	return len(files), nil
}

// listFiles is every regular file under root, relative, slash-free order.
// Symlinks are skipped: they could point anywhere.
func listFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// copyFile copies rel from src to dst, both confined to their roots. A
// source that is no longer a regular file (swapped for a fifo, a device or
// a symlink leaving the root) is refused.
func copyFile(src, dst *os.Root, rel string) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := dst.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	in, err := src.Open(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file", rel)
	}
	out, err := dst.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Archive writes a scratch session's files to w as a zip.
func (m *Manager) Archive(id string, w io.Writer) error {
	src, err := m.workspaceOf(id)
	if err != nil {
		return err
	}
	files, err := listFiles(src)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(src) // listed files may be swapped for symlinks
	if err != nil {
		return err
	}
	defer root.Close()
	zw := zip.NewWriter(w)
	for _, rel := range files {
		if err := addZip(zw, root, rel); err != nil {
			return err
		}
	}
	return zw.Close()
}

func addZip(zw *zip.Writer, root *os.Root, rel string) error {
	f, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file", rel)
	}
	name := filepath.ToSlash(rel)
	hdr, err := zip.FileInfoHeader(st)
	if err != nil {
		return err
	}
	hdr.Name, hdr.Method = name, zip.Deflate
	dst, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, f)
	return err
}
