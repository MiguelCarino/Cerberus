package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/files/fsafe"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/session"
)

type env struct {
	t   *testing.T
	cfg *config.Config
	m   *session.Manager
	s   *Store
	id  string // a scratch session
	ws  string // its workspace
}

// newEnv makes a session manager on temp folders with one scratch session.
// The scheduler never runs, so the session stays queued: its scratch
// workspace exists from Create on.
func newEnv(t *testing.T, mod func(*config.Config)) *env {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	cfg := config.Defaults()
	cfg.RepoRoots = nil
	cfg.Notify = config.NotifyConfig{Disabled: true}
	cfg.Agents = []config.AgentProfile{{Name: "fake", Command: []string{"go", "version"}}}
	cfg.DefaultAgent = "fake"
	if mod != nil {
		mod(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := session.NewManager(cfg, nil, nil, notify.New(cfg.Notify, log), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	})
	infos, err := m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, cfg: cfg, m: m, s: New(cfg, m, nil, log), id: infos[0].ID, ws: infos[0].Worktree}
}

func (e *env) target() Target { return Target{SessionID: e.id} }

func (e *env) put(name, body string) (Entry, error) {
	return e.s.Put(context.Background(), e.target(), name, strings.NewReader(body), int64(len(body)))
}

func (e *env) mustPut(name, body string) Entry {
	e.t.Helper()
	en, err := e.put(name, body)
	if err != nil {
		e.t.Fatalf("put %q: %v", name, err)
	}
	return en
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestPutIntoInboxSanitisedAndRecorded(t *testing.T) {
	e := newEnv(t, nil)
	en, err := e.s.As("192.0.2.7").Put(context.Background(), e.target(), "../../étude.txt", strings.NewReader("hello"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if en.Rel != "inbox/étude.txt" || en.Size != 5 || !en.Text || en.Mime != "text/plain" {
		t.Fatalf("entry %+v", en)
	}
	raw, err := os.ReadFile(filepath.Join(e.ws, "inbox", "étude.txt"))
	if err != nil || string(raw) != "hello" {
		t.Fatalf("file %q %v", raw, err)
	}
	if runtime.GOOS != "windows" {
		if m := mode(t, filepath.Join(e.ws, "inbox", "étude.txt")); m != 0o644 {
			t.Fatalf("mode %o", m)
		}
	}
	found := false
	for _, it := range e.m.Feed(time.Time{}, 0) {
		if it.SessionID == e.id && it.Kind == session.FeedFile && strings.Contains(it.Text, "added inbox/étude.txt (5 B)") &&
			strings.Contains(it.Text, "192.0.2.7") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no feed item: %+v", e.m.Feed(time.Time{}, 0))
	}
	if info, _ := e.m.Get(e.id); info.UploadBytes != 5 {
		t.Fatalf("upload bytes %d", info.UploadBytes)
	}
	for _, bad := range []string{"..", "", "...", "/"} {
		if _, err := e.put(bad, "x"); !errors.Is(err, ErrRefused) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
}

func TestCollisionNaming(t *testing.T) {
	e := newEnv(t, nil)
	e.mustPut("a.txt", "1")
	if en := e.mustPut("a.txt", "2"); en.Name != "a (2).txt" {
		t.Fatalf("second: %q", en.Name)
	}
	if en := e.mustPut("a.txt", "3"); en.Name != "a (3).txt" {
		t.Fatalf("third: %q", en.Name)
	}
	raw, _ := os.ReadFile(filepath.Join(e.ws, "inbox", "a.txt"))
	if string(raw) != "1" {
		t.Fatal("first file overwritten")
	}
	// Racing uploads of one name all survive under distinct names.
	var wg sync.WaitGroup
	names := make([]string, 8)
	for i := range names {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			en, err := e.put("race.bin", fmt.Sprint(i))
			if err != nil {
				t.Error(err)
			}
			names[i] = en.Name
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("two uploads got %q: %v", n, names)
		}
		seen[n] = true
	}
}

func TestExecBitNeverSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no exec bit")
	}
	e := newEnv(t, nil)
	e.mustPut("run.sh", "#!/bin/sh\necho hi\n")
	if m := mode(t, filepath.Join(e.ws, "inbox", "run.sh")); m&0o111 != 0 || m != 0o644 {
		t.Fatalf("run.sh mode %o", m)
	}
	if en := e.mustPut(".env", "A=1"); en.Name != "env" {
		t.Fatalf(".env stored as %q", en.Name)
	}
	if m := mode(t, filepath.Join(e.ws, "inbox", "env")); m != 0o600 {
		t.Fatalf("env mode %o", m)
	}
	e.mustPut("server.key", "k")
	if m := mode(t, filepath.Join(e.ws, "inbox", "server.key")); m != 0o600 {
		t.Fatalf("key mode %o", m)
	}

	x := newEnv(t, func(c *config.Config) { c.Files.AllowExec = true })
	x.mustPut("run.sh", "#!/bin/sh\n")
	x.mustPut("data.bin", "plain")
	if m := mode(t, filepath.Join(x.ws, "inbox", "run.sh")); m != 0o755 {
		t.Fatalf("allow_exec script mode %o", m)
	}
	if m := mode(t, filepath.Join(x.ws, "inbox", "data.bin")); m != 0o644 {
		t.Fatalf("allow_exec non-script mode %o", m)
	}
}

// countingReader serves n bytes and counts what was read.
type countingReader struct{ left, read int64 }

func (r *countingReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.left {
		n = r.left
	}
	r.left -= n
	r.read += n
	return int(n), nil
}

func TestSizeLimitsMidStream(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Files.MaxFileMiB = 1; c.Files.MaxTotalMiB = 3 })
	r := &countingReader{left: 8 << 20}
	_, err := e.s.Put(context.Background(), e.target(), "big.bin", r, -1) // size unknown: caught while streaming
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err %v", err)
	}
	if r.read > 1<<20+chunkSize {
		t.Fatalf("read %d bytes before stopping", r.read)
	}
	ents, _ := os.ReadDir(filepath.Join(e.ws, "inbox"))
	if len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	// A declared size over the limit is refused before reading anything.
	r = &countingReader{left: 2 << 20}
	if _, err := e.s.Put(context.Background(), e.target(), "big.bin", r, 2<<20); !errors.Is(err, ErrTooLarge) || r.read != 0 {
		t.Fatalf("declared: %v, read %d", err, r.read)
	}
	// The per-session total counts every upload.
	chunk := strings.Repeat("x", 900<<10)
	for i := 0; i < 3; i++ {
		e.mustPut(fmt.Sprintf("part%d", i), chunk)
	}
	if _, err := e.s.Put(context.Background(), e.target(), "part3", strings.NewReader(chunk), -1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("total: %v", err)
	}
}

func TestTraversalRefused(t *testing.T) {
	e := newEnv(t, nil)
	e.mustPut("a.txt", "x")
	ctx := context.Background()
	for _, rel := range []string{"..", "../x", "inbox/../../x", "/etc", `inbox\..\..`} {
		if _, err := e.s.PutIn(ctx, e.target(), rel, "f.txt", strings.NewReader("x"), 1); !errors.Is(err, ErrRefused) {
			t.Errorf("PutIn %q: %v", rel, err)
		}
		if _, err := e.s.List(e.target(), rel); !errors.Is(err, ErrRefused) {
			t.Errorf("List %q: %v", rel, err)
		}
		if _, _, err := e.s.Open(e.target(), rel+"/a.txt"); !errors.Is(err, ErrRefused) {
			t.Errorf("Open %q: %v", rel, err)
		}
		if err := e.s.Delete(e.target(), rel); !errors.Is(err, ErrRefused) {
			t.Errorf("Delete %q: %v", rel, err)
		}
	}
	if err := e.s.Delete(e.target(), ""); !errors.Is(err, ErrRefused) {
		t.Errorf("deleting the root: %v", err)
	}
	if err := e.s.Mkdir(e.target(), "inbox/../../up"); !errors.Is(err, ErrRefused) {
		t.Errorf("mkdir traversal: %v", err)
	}
}

func TestSymlinkRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	e := newEnv(t, nil)
	e.mustPut("a.txt", "x")
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(e.ws, "inbox")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(outside, filepath.Join(inbox, "outdir")))
	must(os.Symlink(secret, filepath.Join(inbox, "outfile")))
	must(os.Symlink("a.txt", filepath.Join(inbox, "inlink")))

	l, err := e.s.List(e.target(), "inbox")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, en := range l.Entries {
		got[en.Name] = en
	}
	if !got["outdir"].Outside || !got["outfile"].Outside || !got["outdir"].Link {
		t.Fatalf("outside links not flagged: %+v", l.Entries)
	}
	if in := got["inlink"]; !in.Link || in.Outside || in.Size != 1 {
		t.Fatalf("inside link %+v", in)
	}
	if _, _, err := e.s.Open(e.target(), "inbox/outfile"); !errors.Is(err, ErrOutside) {
		t.Fatalf("open outside file link: %v", err)
	}
	if _, _, err := e.s.Open(e.target(), "inbox/outdir/secret.txt"); !errors.Is(err, ErrOutside) {
		t.Fatalf("open through outside dir link: %v", err)
	}
	if f, _, err := e.s.Open(e.target(), "inbox/inlink"); err != nil {
		t.Fatalf("open inside link: %v", err)
	} else {
		f.Close()
	}
	if _, err := e.s.PutIn(context.Background(), e.target(), "inbox/outdir", "x.txt", strings.NewReader("x"), 1); !errors.Is(err, ErrOutside) {
		t.Fatalf("put through outside link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("a file was written outside")
	}
	if _, err := e.s.List(e.target(), "inbox/outdir"); !errors.Is(err, ErrOutside) {
		t.Fatalf("list outside dir: %v", err)
	}
	for _, rel := range []string{"inbox/outfile", "inbox/outdir"} {
		if err := e.s.Delete(e.target(), rel); !errors.Is(err, ErrOutside) {
			t.Fatalf("delete %s: %v", rel, err)
		}
	}
	if err := e.s.Mkdir(e.target(), "inbox/outdir/new"); !errors.Is(err, ErrOutside) {
		t.Fatalf("mkdir through link: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("outside file touched")
	}
	// Deleting an inside link removes the link only.
	if err := e.s.Delete(e.target(), "inbox/inlink"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "a.txt")); err != nil {
		t.Fatal("link target deleted")
	}
}

func TestDenyPathsExtAndGit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Files.DenyExt = []string{"exe", ".BAT"} })
	if _, err := e.put("secrets.json", "{}"); !errors.Is(err, ErrRefused) { // default deny_paths **/secrets.json
		t.Fatalf("deny_paths: %v", err)
	}
	for _, n := range []string{"setup.EXE", "run.bat"} {
		if _, err := e.put(n, "x"); !errors.Is(err, ErrRefused) {
			t.Fatalf("deny_ext %s: %v", n, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(e.ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.ws, "secrets.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := e.s.List(e.target(), "")
	if err != nil {
		t.Fatal(err)
	}
	if l.Rel != "." || l.Parent != "" {
		t.Fatalf("root listing %+v", l)
	}
	for _, en := range l.Entries {
		if en.Name == ".git" || en.Name == "secrets.json" {
			t.Fatalf("listed %s", en.Name)
		}
	}
	if _, err := e.s.PutIn(context.Background(), e.target(), ".git", "config", strings.NewReader("x"), 1); !errors.Is(err, ErrRefused) {
		t.Fatalf("put into .git: %v", err)
	}
	if _, _, err := e.s.Open(e.target(), "secrets.json"); !errors.Is(err, ErrRefused) {
		t.Fatalf("open denied: %v", err)
	}
}

func TestListOrderMkdirDelete(t *testing.T) {
	e := newEnv(t, nil)
	e.mustPut("b.png", "x")
	e.mustPut("A.md", "x")
	if err := e.s.Mkdir(e.target(), "inbox/zeta/deep"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Mkdir(e.target(), "inbox/bad:name"); !errors.Is(err, ErrRefused) {
		t.Fatalf("unsanitised folder name: %v", err)
	}
	l, err := e.s.List(e.target(), "inbox")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, en := range l.Entries {
		order = append(order, en.Name)
	}
	if strings.Join(order, ",") != "zeta,A.md,b.png" || l.Parent != "." || !l.Entries[2].Image {
		t.Fatalf("order %v parent %q", order, l.Parent)
	}
	if err := e.s.Delete(e.target(), "inbox/zeta"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("non-empty folder: %v", err)
	}
	if err := e.s.RemoveAll(e.target(), "inbox"); !errors.Is(err, ErrRefused) {
		t.Fatalf("recursive delete of the inbox itself: %v", err)
	}
	if err := e.s.RemoveAll(e.target(), "inbox/zeta"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Delete(e.target(), "inbox/b.png"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Open(e.target(), "inbox/b.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted file opens: %v", err)
	}
	f, en, err := e.s.Open(e.target(), "inbox/A.md")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if en.Mime != "text/markdown" || !en.Text {
		t.Fatalf("entry %+v", en)
	}
	if _, _, err := e.s.Open(e.target(), "inbox"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("open folder: %v", err)
	}
}

func TestStagingAndExpiry(t *testing.T) {
	e := newEnv(t, nil)
	id, err := e.s.NewStaging()
	if err != nil {
		t.Fatal(err)
	}
	st := Target{Staging: id}
	if _, err := e.s.Put(context.Background(), st, "photo.jpg", bytes.NewReader([]byte("jpg")), 3); err != nil {
		t.Fatal(err)
	}
	l, err := e.s.List(st, "")
	if err != nil || len(l.Entries) != 1 || l.Entries[0].Rel != "photo.jpg" {
		t.Fatalf("staging list %+v %v", l, err)
	}
	if _, err := e.s.List(Target{Staging: "../../etc"}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad staging id: %v", err)
	}
	if _, err := e.s.List(Target{Staging: id, SessionID: e.id}, ""); !errors.Is(err, ErrBadTarget) {
		t.Fatalf("both set: %v", err)
	}
	old, err := e.s.NewStaging()
	if err != nil {
		t.Fatal(err)
	}
	// Something that is not a staging folder is never swept.
	other := filepath.Join(e.s.UploadsDir(), "keep-me")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-StagingTTL - time.Hour)
	for _, p := range []string{filepath.Join(e.s.UploadsDir(), old), other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.s.Sweep(time.Now()); n != 1 {
		t.Fatalf("swept %d", n)
	}
	if _, err := os.Stat(filepath.Join(e.s.UploadsDir(), old)); !os.IsNotExist(err) {
		t.Fatal("expired staging kept")
	}
	if _, err := os.Stat(filepath.Join(e.s.UploadsDir(), id)); err != nil {
		t.Fatal("fresh staging swept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("non-staging folder swept")
	}
	// Everything expires a day later.
	if n := e.s.Sweep(time.Now().Add(StagingTTL + time.Minute)); n != 1 {
		t.Fatalf("second sweep %d", n)
	}
	if err := e.s.DeleteStaging(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete swept: %v", err)
	}
}

func TestStagingMovesIntoInboxAtCreate(t *testing.T) {
	e := newEnv(t, nil)
	id, err := e.s.NewStaging()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.png", "notes.txt"} {
		if _, err := e.s.Put(context.Background(), Target{Staging: id}, n, strings.NewReader(n), int64(len(n))); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := e.m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Task: "look", Staging: id})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.png", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(infos[0].Worktree, "inbox", n)); err != nil {
			t.Fatalf("%s not in the inbox: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.s.UploadsDir(), id)); !os.IsNotExist(err) {
		t.Fatal("staging folder left behind")
	}
	if infos[0].UploadBytes != int64(len("a.png")+len("notes.txt")) {
		t.Fatalf("upload bytes %d", infos[0].UploadBytes)
	}
	// The same staging id cannot be used twice.
	if _, err := e.m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Task: "x", Staging: id}); err == nil {
		t.Fatal("used staging accepted again")
	}
}

// gatedReader serves first, then waits for gate before serving rest.
type gatedReader struct {
	first, rest []byte
	gate        chan struct{}
	sent        chan struct{}
	stage       int
}

func (r *gatedReader) Read(p []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage = 1
		return copy(p, r.first), nil
	case 1:
		close(r.sent)
		<-r.gate
		r.stage = 2
	}
	if len(r.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func TestConcurrentUploadsShareTheTotal(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Files.MaxFileMiB = 3; c.Files.MaxTotalMiB = 3 })
	staging, err := e.s.NewStaging()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{e.target(), {Staging: staging}} {
		// B starts and stalls after a little; A then fills almost the whole
		// budget and finishes; B's rest must not fit any more.
		b := &gatedReader{first: []byte("bb"), rest: bytes.Repeat([]byte("b"), 1<<20),
			gate: make(chan struct{}), sent: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			_, err := e.s.Put(context.Background(), target, "b.bin", b, -1)
			done <- err
		}()
		<-b.sent
		if _, err := e.s.Put(context.Background(), target, "a.bin", bytes.NewReader(bytes.Repeat([]byte("a"), 5<<19)), -1); err != nil {
			t.Fatalf("%+v: a: %v", target, err)
		}
		close(b.gate)
		if err := <-done; !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%+v: b passed the total with a placed meanwhile: %v", target, err)
		}
	}
}

func TestRecursiveDeleteRefusesSymlinkOnTheWay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	e := newEnv(t, nil)
	e.mustPut("x.txt", "x") // makes the inbox
	sub := filepath.Join(e.ws, "src", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "work.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../src", filepath.Join(e.ws, "inbox", "link")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RemoveAll(e.target(), "inbox/link/sub"); !errors.Is(err, ErrRefused) {
		t.Fatalf("recursive delete through a symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "work.go")); err != nil {
		t.Fatalf("src/sub was deleted: %v", err)
	}
	// A real folder inside the inbox still goes with its contents.
	if err := e.s.Mkdir(e.target(), "inbox/old/deep"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RemoveAll(e.target(), "inbox/old"); err != nil {
		t.Fatal(err)
	}
}

func TestReadingCreatesNoInbox(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.s.List(e.target(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(e.ws, "inbox")); !os.IsNotExist(err) {
		t.Fatalf("listing made the inbox: %v", err)
	}
	// A file named like the inbox breaks uploads only, never reading.
	if err := os.WriteFile(filepath.Join(e.ws, "inbox"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err := e.s.List(e.target(), ""); err != nil || len(l.Entries) != 1 {
		t.Fatalf("list with a file named inbox: %+v %v", l, err)
	}
	f, _, err := e.s.Open(e.target(), "inbox")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.Close()
	if _, err := e.put("a.txt", "a"); err == nil {
		t.Fatal("upload into a file accepted")
	}
}

func TestLimitSize(t *testing.T) {
	for n, want := range map[int64]string{200 << 20: "200 MiB", 2048 << 20: "2 GiB", 1536 << 20: "1.5 GiB", 1: "1 MiB"} {
		if got := LimitSize(n); got != want {
			t.Errorf("LimitSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// A staged secret is recorded as private before it is visible, keeps 0600
// all the way into the inbox even when keep_names renames it, and an upload
// whose mark cannot be written fails instead of waiting unmarked.
func TestStagedSecretMarkedBeforeVisible(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Files.KeepNames = false })
	id, err := e.s.NewStaging()
	if err != nil {
		t.Fatal(err)
	}
	ent, err := e.s.Put(context.Background(), Target{Staging: id}, ".env", strings.NewReader("K=V"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if fsafe.Secretish(ent.Rel) {
		t.Fatalf("test needs a renamed file, got %q", ent.Rel)
	}
	list, err := os.ReadFile(filepath.Join(e.s.UploadsDir(), id, fsafe.PrivateList))
	if err != nil || !strings.Contains(string(list), ent.Rel+"\n") {
		t.Fatalf("private list %q %v", list, err)
	}
	infos, err := e.m.Create(context.Background(), session.TaskSpec{Workspace: session.WorkspaceScratch, Task: "t", Staging: id})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(infos[0].Worktree, "inbox", ent.Rel))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("delivered secret %v %v", st, err)
	}

	// The mark cannot be written (a folder in the list's place): the upload
	// fails and leaves no file behind.
	id2, err := e.s.NewStaging()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.s.UploadsDir(), id2)
	if err := os.Remove(filepath.Join(dir, fsafe.PrivateList)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, fsafe.PrivateList), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Put(context.Background(), Target{Staging: id2}, ".env", strings.NewReader("K=V"), 3); err == nil {
		t.Fatal("upload with an unwritable private mark succeeded")
	}
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if en.Name() != fsafe.PrivateList {
			t.Fatalf("file left behind: %s", en.Name())
		}
	}
}
