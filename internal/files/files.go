// Package files gives files to agents and takes files back (day phase 2,
// "File drop"). A Store writes uploads into a session's workspace, under its
// inbox (cfg.Files.UploadDir, default "inbox"), lists and opens workspace
// files for download and preview, and deletes them; files attached to the
// New task form wait in a staging folder (DATA/uploads/<staging>/) until the
// session manager moves them into the new session's inbox at Create
// (session.TaskSpec.Staging). Staging folders expire after 24 hours (Run).
//
// Safety, for every operation:
//
//   - names are sanitised (internal/files/fsafe: NFC, no separators, NUL,
//     control characters, leading dots or "..", at most 180 bytes) and
//     collisions become "name (2).ext";
//   - every path is opened through an os.Root on the workspace, so neither
//     ".." nor a symlink can reach outside it; a symlink pointing outside is
//     listed (Entry.Outside) but refused for Open, Put and Delete;
//   - writes go to a temp file in the target folder, then a no-clobber link
//     (or rename) into place; files are 0644, or 0600 for secret-ish names
//     (*.pem, *.key, id_*, .env…), and never get the exec bit unless
//     cfg.Files.AllowExec (then only a file starting with "#!");
//   - size limits (cfg.Files.MaxFileMiB per file, MaxTotalMiB per session
//     or staging folder) are enforced while streaming, nothing is buffered
//     whole;
//   - paths matching the agent's deny_paths (agent.DeniedBy) are refused,
//     listings skip them, and ".git" is neither listed nor writable.
//
// Every Put and Delete on a session is recorded in the activity feed (kind
// "file") with the name, size and client address (Store.As).
package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/files/fsafe"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/vault"
)

// Errors, for mapping to HTTP statuses (errors.Is).
var (
	ErrTooLarge  = errors.New("too large")                              // 413
	ErrRefused   = errors.New("refused")                                // 403: name, extension, deny_paths, .git
	ErrOutside   = errors.New("outside the workspace")                  // 403: a path or symlink leading out
	ErrNotFound  = errors.New("not found")                              // 404
	ErrIsDir     = errors.New("is a folder")                            // 400
	ErrNotEmpty  = errors.New("folder is not empty")                    // 409
	ErrBadTarget = errors.New("give exactly one of session or staging") // 400
)

const (
	// StagingTTL is how long New task attachments wait for a Create.
	StagingTTL = 24 * time.Hour
	// MaxEntries caps one List (Listing.Truncated reports the cut).
	MaxEntries   = 2000
	sweepEvery   = time.Hour
	tempPrefix   = fsafe.TempPrefix
	chunkSize    = 32 << 10
	defaultMiB   = 200
	defaultTotal = 2048
)

// Target names where files go: a session's workspace or a staging folder.
// Exactly one is set.
type Target struct {
	SessionID string
	Staging   string
}

// Entry is one file or folder.
type Entry struct {
	Name    string    // base name
	Rel     string    // slash-separated path from the workspace (or staging) root
	Size    int64     // bytes (0 for folders)
	Mod     time.Time // modification time
	Dir     bool
	Mime    string // from the extension; "" for folders
	Text    bool   // previews as text
	Image   bool   // previews as an image
	Link    bool   // a symlink (Size, Dir and Mod describe its target when it stays inside)
	Outside bool   // a symlink pointing outside the workspace: listed, never opened, written or deleted
}

// Listing is one folder level, folders first.
type Listing struct {
	Rel       string  // "." for the root
	Parent    string  // "" at the root, "." for a top-level folder
	Entries   []Entry // folders first, then files, each by name (case-insensitive)
	Truncated bool    // more than MaxEntries entries; the rest are not listed
}

// Store reads and writes dropped files. Use As for a view that records a
// client address in the feed; views share all state.
type Store struct {
	*core
	client string
}

type core struct {
	cfg      *config.Config                // startup config
	live     atomic.Pointer[config.Config] // last Reload (reload.go); conf() picks
	sessions *session.Manager
	vault    *vault.Vault // kept for the phase 3 audit log; the feed masks vault values itself
	log      *slog.Logger
	uploads  string // DATA/uploads

	mu      sync.Mutex
	tallies map[string]*tally // per session ("s:id") or staging ("u:id"), while uploads run
}

// tally counts the bytes of a target's running uploads. Bytes move from
// inflight to committed in one step once their file is in place (and, for
// a session, counted in Info.UploadBytes), so an upload that read the
// target's used total before another one finished still counts it.
type tally struct {
	inflight  int64 // bytes streamed by uploads not yet in place
	committed int64 // bytes placed since the tally was made
	users     int   // uploads holding the tally
}

// New returns a Store. sessions may be nil (staging only, for tests).
func New(cfg *config.Config, sessions *session.Manager, v *vault.Vault, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	uploads := ""
	if sessions != nil {
		uploads = sessions.UploadsDir()
	} else if data, err := platform.DataDir(); err == nil {
		uploads = filepath.Join(data, "uploads")
	}
	return &Store{core: &core{cfg: cfg, sessions: sessions, vault: v, log: log, uploads: uploads,
		tallies: map[string]*tally{}}}
}

// As returns a view of s that names client (the request's address) in the
// activity feed.
func (s *Store) As(client string) *Store { return &Store{core: s.core, client: client} }

// UploadsDir is DATA/uploads, the parent of the staging folders.
func (s *Store) UploadsDir() string { return s.uploads }

// place is a resolved Target.
type place struct {
	root      string // real absolute path of the workspace or staging folder
	key       string // inflight key
	sessionID string
	staging   string // the staging id, for a staging folder
	inbox     string // default folder for Put, slash separated ("" = root)
}

// resolve finds the target's folder. With create, a staging folder is made
// when missing; a session's inbox is never made here (inbox does that, for
// writes into it), so reading a workspace creates nothing and runs no git.
func (s *Store) resolve(ctx context.Context, t Target, create bool) (place, error) {
	switch {
	case (t.SessionID == "") == (t.Staging == ""):
		return place{}, ErrBadTarget
	case t.SessionID != "":
		if s.sessions == nil {
			return place{}, fmt.Errorf("%w: session %s", ErrNotFound, t.SessionID)
		}
		ws, err := s.sessions.Workspace(t.SessionID)
		if errors.Is(err, session.ErrNotFound) {
			return place{}, fmt.Errorf("%w: session %s", ErrNotFound, t.SessionID)
		}
		if err != nil {
			return place{}, err
		}
		real, err := filepath.EvalSymlinks(ws)
		if err != nil {
			return place{}, err
		}
		return place{root: real, key: "s:" + t.SessionID, sessionID: t.SessionID, inbox: session.UploadDir(s.conf())}, nil
	default:
		if !fsafe.ValidStaging(t.Staging) || s.uploads == "" {
			return place{}, fmt.Errorf("%w: staging %q", ErrNotFound, t.Staging)
		}
		dir := filepath.Join(s.uploads, t.Staging)
		if create {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return place{}, err
			}
		}
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() {
			return place{}, fmt.Errorf("%w: staging %s (expired?)", ErrNotFound, t.Staging)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return place{}, err
		}
		return place{root: real, key: "u:" + t.Staging, staging: t.Staging}, nil
	}
}

// inbox makes sure a session's inbox exists (and, for a repo session, is
// kept out of commits) when rel, a folder about to be written, is the inbox
// or inside it.
func (s *Store) inbox(ctx context.Context, p place, rel string) error {
	if p.sessionID == "" || p.inbox == "" || (rel != p.inbox && !strings.HasPrefix(rel, p.inbox+"/")) {
		return nil
	}
	if _, _, err := s.sessions.Inbox(ctx, p.sessionID); err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return fmt.Errorf("%w: session %s", ErrNotFound, p.sessionID)
		}
		return err
	}
	return nil
}

// cleanRel validates rel and maps fsafe's error onto ErrRefused.
func cleanRel(rel string) (string, error) {
	c, err := fsafe.CleanRel(rel)
	if err != nil {
		return "", fmt.Errorf("%w: path %q", ErrRefused, rel)
	}
	return c, nil
}

// osRel is rel for os.Root ("." for the root).
func osRel(rel string) string {
	if rel == "" {
		return "."
	}
	return filepath.FromSlash(rel)
}

// denied returns the deny_paths pattern abs (or what it resolves to) matches.
func (s *Store) denied(abs string) string {
	if pat := agent.DeniedBy(s.conf().Agent.DenyPaths, abs); pat != "" {
		return pat
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil && r != abs {
		return agent.DeniedBy(s.conf().Agent.DenyPaths, r)
	}
	return ""
}

// inside reports whether p is root or below it.
func inside(p, root string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// linkOutside reports whether the symlink at abs leads outside root (or
// nowhere that can be resolved).
func linkOutside(abs, root string) bool {
	r, err := filepath.EvalSymlinks(abs)
	return err != nil || !inside(r, root)
}

// check refuses rel when it is denied or reaches outside through a
// symlink. It returns the Lstat result of rel (nil when it does not exist).
func (s *Store) check(p place, rel string) (fs.FileInfo, error) {
	abs := filepath.Join(p.root, filepath.FromSlash(rel))
	if pat := s.denied(abs); pat != "" {
		return nil, fmt.Errorf("%w: %s is protected by deny_paths (%s)", ErrRefused, rel, pat)
	}
	st, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Mode()&fs.ModeSymlink != 0 && linkOutside(abs, p.root) {
		return st, fmt.Errorf("%w: %s is a symlink leading out of the workspace", ErrOutside, rel)
	}
	return st, nil
}

// limits are the per-file and per-target byte limits.
func (s *Store) limits() (perFile, total int64) {
	f, t := s.conf().Files.MaxFileMiB, s.conf().Files.MaxTotalMiB
	if f <= 0 {
		f = defaultMiB
	}
	if t <= 0 {
		t = defaultTotal
	}
	return int64(f) << 20, int64(t) << 20
}

// used is what already counts against the target's total.
func (s *Store) used(p place) int64 {
	if p.sessionID != "" {
		if info, ok := s.sessions.Get(p.sessionID); ok {
			return info.UploadBytes
		}
		return 0
	}
	var n int64
	entries, _ := os.ReadDir(p.root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			continue // in flight: counted by the tally
		}
		if st, err := e.Info(); err == nil && st.Mode().IsRegular() {
			n += st.Size()
		}
	}
	return n
}

// budget is one upload's view of its target's total.
type budget struct {
	key   string
	base  int64 // used when the upload began
	since int64 // the tally's committed count when base was read
	total int64
}

// begin joins the target's tally and reads its used total. Call end when
// the upload is over.
func (s *Store) begin(p place, total int64) budget {
	s.mu.Lock()
	t := s.tallies[p.key]
	if t == nil {
		t = &tally{}
		s.tallies[p.key] = t
	}
	t.users++
	since := t.committed
	s.mu.Unlock()
	// Read after joining: a file placed from now on is counted in
	// committed-since (and maybe in base too, which only errs on the safe
	// side for a moment).
	return budget{key: p.key, base: s.used(p), since: since, total: total}
}

// end leaves the tally; the last upload out drops it.
func (s *Store) end(b budget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.tallies[b.key]; t != nil {
		if t.users--; t.users <= 0 {
			delete(s.tallies, b.key)
		}
	}
}

// reserve adds n streamed bytes to the target's in-flight count and reports
// whether the used total, what other uploads placed since, and everything
// in flight still fit.
func (s *Store) reserve(b budget, n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tallies[b.key]
	t.inflight += n
	return b.base+(t.committed-b.since)+t.inflight <= b.total
}

// release drops n reserved bytes of an upload that failed.
func (s *Store) release(b budget, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tallies[b.key].inflight -= n
}

// commit turns n reserved bytes into placed ones.
func (s *Store) commit(b budget, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tallies[b.key]
	t.inflight -= n
	t.committed += n
}

// Put writes r as a new file named name into the target's default folder:
// the session's inbox, or the staging folder. size is the declared length
// (-1 when unknown); the limits apply to what is actually read.
func (s *Store) Put(ctx context.Context, target Target, name string, r io.Reader, size int64) (Entry, error) {
	return s.PutIn(ctx, target, "", name, r, size)
}

// PutIn is Put into the folder dir (relative to the workspace; "" = the
// default folder). The folder must exist.
func (s *Store) PutIn(ctx context.Context, target Target, dir, name string, r io.Reader, size int64) (Entry, error) {
	p, err := s.resolve(ctx, target, true)
	if err != nil {
		return Entry{}, err
	}
	if dir == "" {
		dir = p.inbox
	}
	if dir, err = cleanRel(dir); err != nil {
		return Entry{}, err
	}
	if err := s.inbox(ctx, p, dir); err != nil {
		return Entry{}, err
	}
	final, err := s.fileName(name)
	if err != nil {
		return Entry{}, err
	}
	perFile, total := s.limits()
	b := s.begin(p, total)
	defer s.end(b)
	used := b.base
	if size > perFile {
		return Entry{}, fmt.Errorf("%w: %s is %s, the limit is %s per file", ErrTooLarge, final, fsafe.HumanSize(size), LimitSize(perFile))
	}
	if size > 0 && used+size > total {
		return Entry{}, fmt.Errorf("%w: %s would pass the %s upload limit", ErrTooLarge, final, LimitSize(total))
	}
	if st, err := s.check(p, dir); err != nil {
		return Entry{}, err
	} else if st == nil {
		return Entry{}, fmt.Errorf("%w: folder %q", ErrNotFound, dir)
	}
	if pat := s.denied(filepath.Join(p.root, filepath.FromSlash(dir), final)); pat != "" {
		return Entry{}, fmt.Errorf("%w: %s is protected by deny_paths (%s)", ErrRefused, path.Join(dir, final), pat)
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	if st, err := root.Stat(osRel(dir)); err != nil {
		return Entry{}, mapErr(err, dir)
	} else if !st.IsDir() {
		return Entry{}, fmt.Errorf("%w: %s is not a folder", ErrRefused, dir)
	}

	tmp := path.Join(dir, tempPrefix+randomHex(8)+".part")
	f, err := root.OpenFile(osRel(tmp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Entry{}, mapErr(err, dir)
	}
	written, shebang, err := s.stream(ctx, f, r, b, perFile, final)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	fail := func(err error) (Entry, error) {
		_ = root.Remove(osRel(tmp))
		s.release(b, written)
		return Entry{}, err
	}
	if err != nil {
		return fail(err)
	}
	mode := fsafe.FileMode(final, name, s.conf().Files.AllowExec, shebang)
	private := mode&0o077 == 0
	held := p.sessionID == "" // a staging folder, under DATA: 0600 while it waits
	if held {
		if err := fsafe.InitHeld(filepath.Join(p.root, filepath.FromSlash(dir))); err != nil {
			return fail(err)
		}
		mode = fsafe.HeldMode(s.conf().Files.AllowExec, shebang)
	}
	if err := root.Chmod(osRel(tmp), mode); err != nil {
		return fail(err)
	}
	var before func(string) error
	if held {
		// Linking into a staging folder is held against a Create handing
		// the folder out (session.Manager.PlaceStaging): it either sees
		// this file whole, with its private mark, or this upload fails.
		if p.staging != "" && s.sessions != nil {
			release, err := s.sessions.PlaceStaging(p.staging)
			if err != nil {
				return fail(err)
			}
			defer release()
		}
		if private {
			// Recorded before the link, so no reader of the folder ever
			// sees this file without its mark (in a held folder the mark
			// is all that keeps a renamed ".env" private). A name taken
			// in a race may end up listed too: private by mistake is the
			// safe side.
			heldDir := filepath.Join(p.root, filepath.FromSlash(dir))
			before = func(name string) error { return fsafe.MarkPrivate(heldDir, name) }
		}
	}
	final, err = placeNoClobber(root, tmp, dir, final, mode, before)
	if err != nil {
		return fail(err)
	}
	rel := path.Join(dir, final)
	if p.sessionID != "" {
		if _, err := s.sessions.AddUploadBytes(p.sessionID, written); err != nil {
			s.log.Warn("count upload", "session", p.sessionID, "err", err)
		}
	}
	// Counted in used (the session total, or the file on disk) before the
	// reservation goes, so no other upload ever misses these bytes.
	s.commit(b, written)
	if p.sessionID != "" {
		s.sessions.NoteFiles(p.sessionID, []string{rel})
		s.record(p, "added "+rel+" ("+fsafe.HumanSize(written)+")")
	}
	st, err := root.Lstat(osRel(rel))
	if err != nil {
		return Entry{}, err
	}
	return entryOf(rel, st), nil
}

// fileName sanitises name, applies keep_names and deny_ext.
func (s *Store) fileName(name string) (string, error) {
	clean, err := fsafe.Name(name)
	if err != nil {
		return "", fmt.Errorf("%w: name %q", ErrRefused, name)
	}
	_, ext := fsafe.SplitExt(clean)
	lowerExt := strings.ToLower(ext)
	for _, d := range s.conf().Files.DenyExt {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if !strings.HasPrefix(d, ".") {
			d = "." + d
		}
		if lowerExt == d || strings.HasSuffix(strings.ToLower(clean), d) {
			return "", fmt.Errorf("%w: %s files are not accepted (files.deny_ext)", ErrRefused, d)
		}
	}
	if !s.conf().Files.KeepNames {
		clean = "upload-" + time.Now().Format("20060102-150405") + "-" + randomHex(2) + lowerExt
	}
	return clean, nil
}

// stream copies r into f in chunks, enforcing the per-file limit and the
// target's total (shared with uploads running at the same time), and
// reports whether the content starts with "#!".
//
// Every byte read is reserved in the target's tally; the caller commits or
// releases written.
func (s *Store) stream(ctx context.Context, f *os.File, r io.Reader, b budget, perFile int64, name string) (int64, bool, error) {
	buf := make([]byte, chunkSize)
	var written int64
	shebang := false
	for {
		if err := ctx.Err(); err != nil {
			return written, false, err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if written == 0 {
				shebang = n >= 2 && buf[0] == '#' && buf[1] == '!'
			}
			if written+int64(n) > perFile {
				return written, false, fmt.Errorf("%w: %s is larger than the %s per-file limit", ErrTooLarge, name, LimitSize(perFile))
			}
			ok := s.reserve(b, int64(n))
			written += int64(n)
			if !ok {
				return written, false, fmt.Errorf("%w: the %s upload limit is reached", ErrTooLarge, LimitSize(b.total))
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return written, false, err
			}
		}
		if errors.Is(rerr, io.EOF) {
			return written, shebang, nil
		}
		if rerr != nil {
			return written, false, rerr
		}
	}
}

// placeNoClobber moves the temp file tmp to dir/name, or the next free
// collision name, never replacing an existing file. before, when set, runs
// with each candidate name just before the file is linked under it; its
// error stops the placement.
func placeNoClobber(root *os.Root, tmp, dir, name string, mode fs.FileMode, before func(string) error) (string, error) {
	for tries := 0; tries < 100; tries++ {
		final, err := fsafe.Unique(name, func(n string) bool {
			_, err := root.Lstat(osRel(path.Join(dir, n)))
			return err == nil
		})
		if err != nil {
			return "", err
		}
		if before != nil {
			if err := before(final); err != nil {
				return "", err
			}
		}
		to := osRel(path.Join(dir, final))
		err = root.Link(osRel(tmp), to)
		switch {
		case err == nil:
			_ = root.Remove(osRel(tmp))
			return final, nil
		case errors.Is(err, fs.ErrExist):
			continue
		}
		// No hard links on this file system: copy into the name, created
		// exclusively. A rename would replace a file another upload placed
		// there a moment ago.
		if err := copyExclusiveRoot(root, tmp, to, mode); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", err
		}
		_ = root.Remove(osRel(tmp))
		return final, nil
	}
	return "", fmt.Errorf("no free name for %q", name)
}

// copyExclusiveRoot copies the file tmp to a new file to (which must not
// exist) inside root and gives it mode.
func copyExclusiveRoot(root *os.Root, tmp, to string, mode fs.FileMode) error {
	in, err := root.Open(osRel(tmp))
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	if err := errors.Join(cerr, out.Close()); err != nil {
		_ = root.Remove(to)
		return err
	}
	return root.Chmod(to, mode)
}

// List returns one folder level of the target, folders first. .git, the
// Store's own temp files and denied entries are left out.
func (s *Store) List(target Target, rel string) (Listing, error) {
	p, err := s.resolve(context.Background(), target, false)
	if err != nil {
		return Listing{}, err
	}
	if rel, err = cleanRel(rel); err != nil {
		return Listing{}, err
	}
	if _, err := s.check(p, rel); err != nil {
		return Listing{}, err
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return Listing{}, err
	}
	defer root.Close()
	d, err := root.Open(osRel(rel))
	if err != nil {
		return Listing{}, mapErr(err, rel)
	}
	defer d.Close()
	if st, err := d.Stat(); err != nil {
		return Listing{}, err
	} else if !st.IsDir() {
		return Listing{}, fmt.Errorf("%w: %s is not a folder", ErrRefused, rel)
	}
	l := Listing{Rel: ".", Entries: []Entry{}}
	if rel != "" {
		l.Rel = rel
		l.Parent = path.Dir(rel)
	}
	for {
		batch, err := d.ReadDir(256)
		for _, de := range batch {
			name := de.Name()
			if strings.EqualFold(name, ".git") || strings.HasPrefix(name, tempPrefix) {
				continue
			}
			erel := path.Join(rel, name)
			abs := filepath.Join(p.root, filepath.FromSlash(erel))
			if s.denied(abs) != "" {
				continue
			}
			st, err := de.Info()
			if err != nil {
				continue
			}
			if len(l.Entries) >= MaxEntries {
				l.Truncated = true
				break
			}
			e := entryOf(erel, st)
			if st.Mode()&fs.ModeSymlink != 0 {
				e.Link = true
				if linkOutside(abs, p.root) {
					e.Outside = true
				} else if t, err := root.Stat(osRel(erel)); err == nil {
					e.Dir, e.Size, e.Mod = t.IsDir(), t.Size(), t.ModTime()
					if e.Dir {
						e.Size, e.Mime, e.Text, e.Image = 0, "", false, false
					}
				}
			}
			l.Entries = append(l.Entries, e)
		}
		if err != nil || len(batch) == 0 || l.Truncated {
			break
		}
	}
	slices.SortFunc(l.Entries, func(a, b Entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return l, nil
}

// Open opens a file of the target for download or preview. Folders, denied
// paths and symlinks leading out are refused.
func (s *Store) Open(target Target, rel string) (*os.File, Entry, error) {
	p, err := s.resolve(context.Background(), target, false)
	if err != nil {
		return nil, Entry{}, err
	}
	if rel, err = cleanRel(rel); err != nil {
		return nil, Entry{}, err
	}
	if rel == "" {
		return nil, Entry{}, fmt.Errorf("%w: the workspace root", ErrIsDir)
	}
	if _, err := s.check(p, rel); err != nil {
		return nil, Entry{}, err
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return nil, Entry{}, err
	}
	defer root.Close()
	f, err := root.Open(osRel(rel))
	if err != nil {
		return nil, Entry{}, mapErr(err, rel)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, Entry{}, err
	}
	if st.IsDir() {
		f.Close()
		return nil, Entry{}, fmt.Errorf("%w: %s", ErrIsDir, rel)
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, Entry{}, fmt.Errorf("%w: %s is not a regular file", ErrRefused, rel)
	}
	e := entryOf(rel, st)
	if lst, err := root.Lstat(osRel(rel)); err == nil && lst.Mode()&fs.ModeSymlink != 0 {
		e.Link = true
	}
	return f, e, nil
}

// Delete removes a file, a symlink (inside links only; the link, never its
// target) or an empty folder. The root and anything denied are refused.
func (s *Store) Delete(target Target, rel string) error {
	return s.remove(target, rel, false)
}

// RemoveAll removes a folder with everything in it, but only inside the
// session's inbox (or anywhere in a staging folder).
func (s *Store) RemoveAll(target Target, rel string) error {
	return s.remove(target, rel, true)
}

func (s *Store) remove(target Target, rel string, recursive bool) error {
	p, err := s.resolve(context.Background(), target, false)
	if err != nil {
		return err
	}
	if rel, err = cleanRel(rel); err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w: the workspace root cannot be deleted", ErrRefused)
	}
	st, err := s.check(p, rel)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, rel)
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return err
	}
	defer root.Close()
	var size int64
	if st.Mode().IsRegular() {
		size = st.Size()
	}
	if recursive && st.IsDir() {
		if p.sessionID != "" && (p.inbox == "" || !strings.HasPrefix(rel, p.inbox+"/")) {
			return fmt.Errorf("%w: only folders inside %s/ can be deleted with their contents", ErrRefused, p.inbox)
		}
		// The rule above is about the real place: a symlink on the way
		// (inbox/link -> ../src) would lead the delete elsewhere.
		if link := linkOnTheWay(root, rel); link != "" {
			return fmt.Errorf("%w: %s is a symlink; folders are deleted with their contents only through real folders", ErrRefused, link)
		}
		err = root.RemoveAll(osRel(rel))
	} else {
		err = root.Remove(osRel(rel))
		if err != nil && st.IsDir() && !errors.Is(err, fs.ErrNotExist) {
			if ents, rerr := os.ReadDir(filepath.Join(p.root, filepath.FromSlash(rel))); rerr == nil && len(ents) > 0 {
				return fmt.Errorf("%w: %s", ErrNotEmpty, rel)
			}
		}
	}
	if err != nil {
		return mapErr(err, rel)
	}
	what := rel
	if st.IsDir() {
		what += "/"
	} else if size > 0 {
		what += " (" + fsafe.HumanSize(size) + ")"
	}
	s.record(p, "deleted "+what)
	return nil
}

// linkOnTheWay returns the first folder above rel (slash separated, inside
// root) that is a symlink, or "".
func linkOnTheWay(root *os.Root, rel string) string {
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		p := strings.Join(segs[:i], "/")
		st, err := root.Lstat(osRel(p))
		if err != nil || st.Mode()&fs.ModeSymlink != 0 {
			return p
		}
	}
	return ""
}

// Mkdir creates a folder (and missing parents). Every segment must already
// be a sanitised name.
func (s *Store) Mkdir(target Target, rel string) error {
	p, err := s.resolve(context.Background(), target, true)
	if err != nil {
		return err
	}
	if rel, err = cleanRel(rel); err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w: give a folder name", ErrRefused)
	}
	for _, seg := range strings.Split(rel, "/") {
		if clean, err := fsafe.Name(seg); err != nil || clean != seg {
			return fmt.Errorf("%w: folder name %q", ErrRefused, seg)
		}
	}
	if err := s.inbox(context.Background(), p, rel); err != nil {
		return err
	}
	if _, err := s.check(p, rel); err != nil {
		return err
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(osRel(rel), 0o755); err != nil {
		return mapErr(err, rel)
	}
	return nil
}

// record adds a file item to the session's activity feed.
func (s *Store) record(p place, text string) {
	if p.sessionID == "" || s.sessions == nil {
		return
	}
	if s.client != "" {
		text += " · from " + s.client
	}
	s.sessions.Record(p.sessionID, session.FeedFile, text)
}

// entryOf builds an Entry from rel and its Lstat result.
func entryOf(rel string, st fs.FileInfo) Entry {
	e := Entry{Name: path.Base(rel), Rel: rel, Mod: st.ModTime(), Dir: st.IsDir()}
	if !e.Dir {
		e.Size = st.Size()
		e.Mime, e.Text, e.Image = kindOf(e.Name)
	}
	return e
}

// mapErr turns os errors into the package's errors, naming rel.
func mapErr(err error, rel string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s", ErrNotFound, rel)
	case strings.Contains(err.Error(), "path escapes from parent"), strings.Contains(err.Error(), "escapes"):
		return fmt.Errorf("%w: %s", ErrOutside, rel)
	}
	return err
}

// LimitSize writes a configured limit in binary units, as max_file_mib and
// max_total_mib count it: "200 MiB", "2 GiB", "1.5 GiB".
func LimitSize(n int64) string {
	const gib = 1 << 30
	if n >= gib {
		if n%gib == 0 {
			return fmt.Sprintf("%d GiB", n/gib)
		}
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/gib), ".0") + " GiB"
	}
	return fmt.Sprintf("%d MiB", (n+(1<<20)-1)>>20)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
