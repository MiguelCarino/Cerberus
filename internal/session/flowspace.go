package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"carino.systems/cerberus/internal/agent"
)

// Workspaces that build on an earlier session (flows' workspace "step").
// A session with TaskSpec.SameAs works in that session's scratch or folder
// workspace in place; one with TaskSpec.Seeds starts in a new scratch
// folder that already holds copies of the earlier sessions' files.

// Seed copies the workspace files of Session into the folder Into
// (relative, slash-separated) of a new scratch workspace.
type Seed struct {
	Session string
	Into    string
}

// Limits of a seeded workspace: more is refused, never cut silently.
const (
	maxSeeds         = 50
	maxSeedFiles     = 5000
	maxSeedBytes     = 256 << 20
	maxLastReplyRead = 64 << 20
)

// sourceInfo is the earlier session id as a workspace source for owner:
// it must exist, belong to the same owner, and be done (passed, failed,
// stopped or error: a running agent may still write, and an accepted or
// discarded session has no workspace any more).
func (m *Manager) sourceInfo(id, owner string) (Info, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	var info Info
	if ok {
		info = s.info
	}
	m.mu.Unlock()
	if !ok {
		return Info{}, fmt.Errorf("session %s not found", id)
	}
	if info.Owner != owner {
		return Info{}, fmt.Errorf("session %s belongs to someone else", id)
	}
	switch info.State {
	case StatePassed, StateFailed, StateStopped, StateError:
	case StateAccepted, StateDiscarded:
		return Info{}, fmt.Errorf("session %s is %s: its workspace is gone", id, info.State)
	default:
		return Info{}, fmt.Errorf("session %s is still %s", id, info.State)
	}
	if info.Worktree == "" {
		return Info{}, fmt.Errorf("session %s has no workspace", id)
	}
	if st, err := os.Stat(info.Worktree); err != nil || !st.IsDir() {
		return Info{}, fmt.Errorf("the workspace of session %s no longer exists", id)
	}
	return info, nil
}

// sameAsWorkspace points t (Workspace "same", SameAs set, no Parent) at
// the workspace of session t.SameAs. A folder session's folder is used
// again as a folder (checked again by resolveWorkspace: done is false). A
// scratch session's folder is Cerberus' own, so it is set here directly
// (done is true), and so is a scratch folder that a folder session itself
// borrowed this way. A repo session's worktree is refused: it is not
// accepted yet, and a second session in it would muddle that.
func (m *Manager) sameAsWorkspace(t *TaskSpec) (done bool, err error) {
	if t.Count > 1 {
		return false, errors.New("several sessions cannot share one workspace")
	}
	info, err := m.sourceInfo(t.SameAs, t.Owner)
	if err != nil {
		return false, err
	}
	switch info.Workspace {
	case WorkspaceRepo:
		return false, fmt.Errorf("session %s works in a repo worktree, which is not shared: accept it, then use the repo", t.SameAs)
	case WorkspaceFolder:
		t.Workspace, t.Dir, t.Repo = WorkspaceFolder, info.Dir, ""
		if within(info.Dir, m.sessDir) {
			// A scratch folder an earlier "same" session borrowed (a chain
			// A -> B -> C): Cerberus' own, as in the scratch case below.
			// resolveWorkspace would refuse it as Cerberus' data.
			return true, nil
		}
		return false, nil
	default:
		if !within(info.Worktree, m.sessDir) {
			return false, fmt.Errorf("the workspace of session %s no longer exists", t.SameAs)
		}
		t.Workspace, t.Dir, t.Repo = WorkspaceFolder, info.Worktree, ""
		return true, nil
	}
}

// checkSeeds validates t.Seeds: a single scratch session, folders that
// stay inside the workspace, and sources done and owned by t.Owner.
func (m *Manager) checkSeeds(t *TaskSpec) error {
	if len(t.Seeds) > maxSeeds {
		return fmt.Errorf("at most %d sessions can seed a workspace", maxSeeds)
	}
	if t.Workspace == "" {
		t.Workspace = WorkspaceScratch
	}
	if t.Workspace != WorkspaceScratch || t.Repo != "" || t.Dir != "" {
		return errors.New("only a scratch workspace can start with files from other sessions")
	}
	if t.Count > 1 {
		return errors.New("a seeded workspace is for one session")
	}
	for _, sd := range t.Seeds {
		rel := filepath.FromSlash(sd.Into)
		if sd.Into == "" || !filepath.IsLocal(rel) || strings.HasPrefix(sd.Into, ".git") {
			return fmt.Errorf("seed folder %q is not a folder inside the workspace", sd.Into)
		}
		if _, err := m.sourceInfo(sd.Session, t.Owner); err != nil {
			return err
		}
	}
	return nil
}

// copySeeds copies each seed's workspace files into wt/<Into>/ (symlinks
// and the .git entry are skipped). Both ends go through os.Root.
func (m *Manager) copySeeds(wt string, seeds []Seed, owner string) error {
	dst, err := os.OpenRoot(wt)
	if err != nil {
		return err
	}
	defer dst.Close()
	files, total := 0, int64(0)
	for _, sd := range seeds {
		info, err := m.sourceInfo(sd.Session, owner)
		if err != nil {
			return err
		}
		list, err := listFiles(info.Worktree)
		if err != nil {
			return fmt.Errorf("files of session %s: %w", sd.Session, err)
		}
		into := filepath.FromSlash(sd.Into)
		if err := dst.MkdirAll(into, 0o755); err != nil {
			return err
		}
		sub, err := dst.OpenRoot(into)
		if err != nil {
			return err
		}
		src, err := os.OpenRoot(info.Worktree)
		if err != nil {
			sub.Close()
			return err
		}
		err = func() error {
			for _, rel := range list {
				if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
					continue
				}
				if pat := agent.DeniedBy(m.conf().Agent.DenyPaths, filepath.Join(info.Worktree, rel)); pat != "" {
					continue // never handed on: protected by deny_paths
				}
				st, err := src.Lstat(rel)
				if err != nil || !st.Mode().IsRegular() {
					continue
				}
				files++
				total += st.Size()
				if files > maxSeedFiles || total > maxSeedBytes {
					return fmt.Errorf("the earlier sessions hold more than %d files or %d MiB; too much to copy", maxSeedFiles, maxSeedBytes>>20)
				}
				if err := copyFile(src, sub, rel); err != nil {
					return fmt.Errorf("copy %s from session %s: %w", rel, sd.Session, err)
				}
			}
			return nil
		}()
		src.Close()
		sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// LastReply is the built-in agent's last non-empty assistant message in
// the session's conversation.json, as written (not masked: the caller
// masks). "" when there is none (another agent, or no reply yet).
func (m *Manager) LastReply(id string) (string, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	dir := ""
	if err == nil {
		dir = s.dir
	}
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, conversationFile)
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.Size() > maxLastReplyRead {
		return "", errors.New("the conversation is too large to read")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var c struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == "assistant" && strings.TrimSpace(c.Messages[i].Content) != "" {
			return strings.TrimSpace(c.Messages[i].Content), nil
		}
	}
	return "", nil
}
