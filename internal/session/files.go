package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/files/fsafe"
)

// File drop (day phase 2): files given to a session land in its workspace's
// inbox (cfg.Files.UploadDir, default "inbox"). A repo session's inbox is
// listed in the repo's info/exclude, so uploads never reach an accepted
// commit unless the agent moves them out. Files attached on the New task
// form wait in DATA/uploads/<staging>/ (internal/files writes them) and are
// handed to the session at Create: kept in DATA/sessions/<id>/attach/ and
// moved into the inbox as soon as the workspace exists (at once for folder
// and scratch sessions, when the worktree is made for repo sessions).

// FeedFile is the feed kind of a file added to or deleted from a session.
const FeedFile = "file"

const (
	attachDir       = "attach" // DATA/sessions/<id>/attach: attachments waiting for the worktree
	maxPendingFiles = 50       // inbox paths remembered for the next follow-up's mention
	excludeComment  = "# Cerberus: the session inbox (uploaded files) stays out of commits"
)

// UploadDir is the inbox folder, relative to a workspace and slash
// separated: cfg.Files.UploadDir when it is a valid relative path, else
// "inbox".
func UploadDir(cfg *config.Config) string {
	if rel, err := fsafe.CleanRel(cfg.Files.UploadDir); err == nil && rel != "" {
		return rel
	}
	return "inbox"
}

// UploadsDir is DATA/uploads, the parent of every staging folder.
func (m *Manager) UploadsDir() string { return m.uploadsDir }

// stagingPath returns DATA/uploads/<id> for a valid staging id that exists.
func (m *Manager) stagingPath(id string) (string, error) {
	if !fsafe.ValidStaging(id) {
		return "", errors.New("unknown attachments (bad staging id)")
	}
	p := filepath.Join(m.uploadsDir, id)
	st, err := os.Lstat(p)
	if err != nil || !st.IsDir() {
		return "", errors.New("the attachments expired or were removed; attach them again")
	}
	return p, nil
}

// Workspace returns a session's workspace folder without creating
// anything or running git: what reading its files (listing, preview,
// download, delete) needs. A repo session that has not started has no
// workspace yet; a discarded or accepted session has none any more.
func (m *Manager) Workspace(id string) (string, error) {
	wt, _, err := m.liveWorkspace(id)
	return wt, err
}

// liveWorkspace returns a session's workspace folder and kind.
func (m *Manager) liveWorkspace(id string) (wt, kind string, err error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err != nil {
		m.mu.Unlock()
		return "", "", err
	}
	wt, kind, state := s.info.Worktree, s.info.Workspace, s.info.State
	m.mu.Unlock()
	if final(state) {
		return "", "", fmt.Errorf("session is %s", state)
	}
	if wt == "" {
		return "", "", errors.New("the workspace does not exist yet (the session has not started)")
	}
	return wt, kind, nil
}

// Inbox returns a session's workspace folder and its inbox folder (created
// if missing): what writing into the inbox needs. For a repo session it
// also makes sure the inbox is in the repo's info/exclude (asked of git once
// per worktree).
func (m *Manager) Inbox(ctx context.Context, id string) (workspace, inbox string, err error) {
	wt, ws, err := m.liveWorkspace(id)
	if err != nil {
		return "", "", err
	}
	inbox = filepath.Join(wt, filepath.FromSlash(UploadDir(m.conf())))
	if st, err := os.Lstat(inbox); err == nil && !st.IsDir() {
		return "", "", fmt.Errorf("%s is not a plain folder", UploadDir(m.conf()))
	} else if err != nil {
		if err := os.MkdirAll(inbox, 0o755); err != nil {
			return "", "", err
		}
	}
	if ws == WorkspaceRepo {
		if err := m.excludeOnce(ctx, wt); err != nil {
			return "", "", fmt.Errorf("keep the inbox out of commits: %w", err)
		}
	}
	return wt, inbox, nil
}

// excludeOnce is excludeInbox for the configured inbox, remembered per
// worktree so a stream of uploads forks git once.
func (m *Manager) excludeOnce(ctx context.Context, wt string) error {
	if _, ok := m.excluded.Load(wt); ok {
		return nil
	}
	if err := excludeInbox(ctx, wt, UploadDir(m.conf())); err != nil {
		return err
	}
	m.excluded.Store(wt, true)
	return nil
}

// excludeInbox adds "/<inbox>/" to the info/exclude file git reads for the
// worktree wt. A linked worktree's .git is a file pointing into the main
// repo, and git reads info/exclude from the common git dir shared by every
// worktree, so the path is asked of git itself (rev-parse --git-path)
// rather than guessed. The entry is anchored to the top level and added
// once.
func excludeInbox(ctx context.Context, wt, rel string) error {
	p, err := gitLine(ctx, wt, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(wt, p)
	}
	line := "/" + strings.Trim(rel, "/") + "/"
	raw, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	add := excludeComment + "\n" + line + "\n"
	if len(raw) > 0 && !strings.HasSuffix(string(raw), "\n") {
		add = "\n" + add
	}
	_, werr := f.WriteString(add)
	return errors.Join(werr, f.Close())
}

// NoteFiles remembers inbox paths (slash separated, relative to the
// workspace) that were added to a session, so the next follow-up to an
// agent other than the built-in one mentions them ("Files added: …"). The
// built-in agent finds new inbox files itself.
func (m *Manager) NoteFiles(id string, rels []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || len(rels) == 0 {
		return
	}
	for _, r := range rels {
		if r != "" && !slices.Contains(s.pendingFiles, r) {
			s.pendingFiles = append(s.pendingFiles, r)
		}
	}
	if n := len(s.pendingFiles); n > maxPendingFiles {
		s.pendingFiles = slices.Clone(s.pendingFiles[n-maxPendingFiles:])
	}
	m.persistLocked(s)
}

// AddUploadBytes counts n uploaded bytes against the session
// (Info.UploadBytes, limited by cfg.Files.MaxTotalMiB) and returns the new
// total.
func (m *Manager) AddUploadBytes(id string, n int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookup(id)
	if err != nil {
		return 0, err
	}
	s.info.UploadBytes += n
	m.persistLocked(s)
	return s.info.UploadBytes, nil
}

// filesMention is the sentence other agents get about added files. It is
// typed into a running agent's terminal as one line, so control and bidi
// characters in the paths (a folder name may hold a CR or an escape
// sequence) are shown as "?" and can never split or steer the input.
func filesMention(rels []string) string {
	if len(rels) == 0 {
		return ""
	}
	safe := make([]string, len(rels))
	for i, r := range rels {
		safe[i] = strings.Map(func(c rune) rune {
			if unicode.IsControl(c) || unicode.Is(unicode.Bidi_Control, c) || c == '\u2028' || c == '\u2029' {
				return '?'
			}
			return c
		}, r)
	}
	return "Files added: " + strings.Join(safe, ", ")
}

// isBuiltinAgent reports whether p runs Cerberus' own agent, which lists new
// inbox files to its model by itself.
func isBuiltinAgent(p config.AgentProfile) bool {
	return len(p.Command) >= 2 && p.Command[0] == "{self}" && p.Command[1] == "agent"
}

// takeAttachments puts the staging folder's files into the session folder
// dir (DATA/sessions/<id>/attach): moved for the last (or only) session of
// a Create, copied (never hard-linked) for the others of a fan-out.
func takeAttachments(staging, dir string, allowExec, move bool) error {
	dst := filepath.Join(dir, attachDir)
	var err error
	if move {
		_, err = fsafe.MoveIntoHeld(staging, dst, allowExec)
	} else {
		_, err = fsafe.CopyIntoHeld(staging, dst, allowExec)
	}
	return err
}

// deliverAttachments moves the files waiting in sessDir/attach into the
// workspace's inbox (adding the repo exclude entry for repo sessions) and
// returns their inbox paths and total size. It is a no-op without
// attachments.
func (m *Manager) deliverAttachments(ctx context.Context, sessDir, workspace, wt string) ([]string, []fsafe.Moved, error) {
	src := filepath.Join(sessDir, attachDir)
	if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	rel := UploadDir(m.conf())
	moved, err := fsafe.MoveInto(src, filepath.Join(wt, filepath.FromSlash(rel)), m.conf().Files.AllowExec)
	rels := make([]string, 0, len(moved))
	for _, mv := range moved {
		rels = append(rels, rel+"/"+mv.Name)
	}
	if err != nil {
		return rels, moved, fmt.Errorf("move attachments into %s: %w", rel, err)
	}
	if workspace == WorkspaceRepo {
		if err := m.excludeOnce(ctx, wt); err != nil {
			return rels, moved, fmt.Errorf("keep the inbox out of commits: %w", err)
		}
	}
	return rels, moved, nil
}

// returnAttachments undoes takeAttachments and deliverAttachments for a
// Create that failed: when the session took the staging folder itself
// (move), the files delivered into the inbox (delivered) and those still in
// sessDir/attach go back into staging; for a copy, the delivered copies are
// removed. The staging folder ends up as it was before, so a retry gets
// every file.
func (m *Manager) returnAttachments(staging, sessDir, wt string, delivered []fsafe.Moved, move bool) error {
	var errs []error
	if move {
		if err := os.MkdirAll(staging, 0o700); err != nil {
			return err
		}
	}
	inbox := filepath.Join(wt, filepath.FromSlash(UploadDir(m.conf())))
	for _, mv := range delivered {
		p := filepath.Join(inbox, mv.Name)
		if !move {
			errs = append(errs, os.Remove(p))
			continue
		}
		errs = append(errs, returnFile(p, staging, mv.Name, m.conf().Files.AllowExec))
	}
	attach := filepath.Join(sessDir, attachDir)
	if _, err := os.Lstat(attach); move && err == nil {
		_, err := fsafe.MoveIntoHeld(attach, staging, m.conf().Files.AllowExec)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// returnFile moves a delivered attachment from the inbox back into the
// held staging folder: 0600 again, recorded as private when its workspace
// mode was.
func returnFile(p, staging, name string, allowExec bool) error {
	// p is in the workspace, where an agent may have put a symlink in the
	// file's place: only a regular file goes back, and what landed in
	// staging is checked again before its mode is touched (a chmod by path
	// would follow a symlink).
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file", p)
	}
	if err := fsafe.InitHeld(staging); err != nil {
		return err
	}
	final, err := fsafe.MoveFile(p, staging, name)
	if err != nil {
		return err
	}
	back := filepath.Join(staging, final)
	if lst, err := os.Lstat(back); err != nil || !lst.Mode().IsRegular() {
		_ = os.Remove(back)
		return fmt.Errorf("%s is no longer a regular file", p)
	}
	if err := os.Chmod(back, fsafe.HeldMode(allowExec, st.Mode()&0o111 != 0)); err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 == 0 {
		return fsafe.MarkPrivate(staging, final)
	}
	return nil
}

// attachedText is the feed text for delivered attachments.
func attachedText(moved []fsafe.Moved) string {
	parts := make([]string, 0, len(moved))
	for _, mv := range moved {
		parts = append(parts, mv.Name+" ("+fsafe.HumanSize(mv.Size)+")")
	}
	noun := "files"
	if len(moved) == 1 {
		noun = "file"
	}
	return fmt.Sprintf("attached %d %s: %s", len(moved), noun, strings.Join(parts, ", "))
}

// movedBytes sums the sizes of moved files.
func movedBytes(moved []fsafe.Moved) int64 {
	var n int64
	for _, mv := range moved {
		n += mv.Size
	}
	return n
}

// onAttachments records delivered attachments on s: the feed item, the
// upload total and, for agents other than the built-in one, the mention
// added to the instruction of the next run. m.mu held.
func (m *Manager) onAttachmentsLocked(s *session, rels []string, moved []fsafe.Moved) {
	if len(moved) == 0 {
		return
	}
	s.info.UploadBytes += movedBytes(moved)
	if p, ok := m.conf().AgentProfile(s.info.Agent); ok && !isBuiltinAgent(p) {
		mention := filesMention(rels)
		s.task = strings.TrimSpace(s.task + "\n\n" + mention)
		if s.next != "" {
			s.next = strings.TrimSpace(s.next + "\n\n" + mention)
		}
	}
	m.recordLocked(s, FeedFile, attachedText(moved))
}
