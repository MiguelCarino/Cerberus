//go:build !windows

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/files"
	"carino.systems/cerberus/internal/session"
)

// filesEnv is the control-center test env with a real file store.
func filesEnv(t *testing.T, mut func(*config.Config)) *ccEnv {
	t.Helper()
	e := newCCEnv(t, mut)
	e.srv.files = files.New(e.cfg, e.m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e
}

// multipartBody builds a small multipart body in memory.
func multipartBody(t *testing.T, parts map[string]string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("note", "a plain field is skipped")
	for name, body := range parts {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(fw, body)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), &buf
}

// upload posts a multipart body as admin (or as a session agent).
func (e *ccEnv) upload(path, ctype string, body io.Reader, auth string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest("POST", e.ts.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	switch auth {
	case "admin":
		req.Header.Set("Authorization", "Bearer "+testToken)
	case "":
	default:
		req.Header.Set("Authorization", "Bearer "+e.cfg.SessionAgentToken(auth))
		req.Header.Set(sessionHeader, auth)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// get fetches a path and returns the response with its body read.
func (e *ccEnv) get(path string, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func admin() []string { return []string{"Authorization", "Bearer " + testToken} }

func TestFilesUploadListDeleteMkdir(t *testing.T) {
	e := filesEnv(t, nil)
	id := e.person("echo", "files")
	ctype, body := multipartBody(t, map[string]string{"notes.txt": "hello", "../evil name.md": "# x"})
	code, raw := e.upload("/api/sessions/"+id+"/files", ctype, body, "admin")
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, raw)
	}
	var got []files.Entry
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != 2 {
		t.Fatalf("entries: %s %v", raw, err)
	}
	for _, en := range got {
		if !strings.HasPrefix(en.Rel, "inbox/") || strings.Contains(en.Rel, "..") {
			t.Fatalf("stored outside the inbox: %+v", en)
		}
	}

	resp, raw := e.get("/api/sessions/"+id+"/files?dir=inbox", admin()...)
	var l files.Listing
	if resp.StatusCode != 200 || json.Unmarshal(raw, &l) != nil || len(l.Entries) != 2 || l.Rel != "inbox" {
		t.Fatalf("list: %d %s", resp.StatusCode, raw)
	}
	resp, raw = e.get("/api/sessions/"+id+"/files", admin()...)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"Rel":"inbox"`) {
		t.Fatalf("root list: %d %s", resp.StatusCode, raw)
	}

	if code, raw := e.call("POST", "/api/sessions/"+id+"/files/mkdir", `{"path":"inbox/sub"}`, "admin"); code != 201 {
		t.Fatalf("mkdir: %d %s", code, raw)
	}
	if code, _ := e.call("POST", "/api/sessions/"+id+"/files/mkdir", `{"path":"../out"}`, "admin"); code != 400 {
		t.Fatalf("mkdir outside: %d", code)
	}
	// Upload into a chosen folder.
	ctype, body = multipartBody(t, map[string]string{"deep.txt": "d"})
	if code, raw := e.upload("/api/sessions/"+id+"/files?dir=inbox/sub", ctype, body, "admin"); code != 201 ||
		!strings.Contains(string(raw), `"Rel":"inbox/sub/deep.txt"`) {
		t.Fatalf("upload into dir: %d %s", code, raw)
	}
	if code, _ := e.call("DELETE", "/api/sessions/"+id+"/files?path=inbox/sub", "", "admin"); code != 409 {
		t.Fatalf("delete non-empty folder: %d", code)
	}
	if code, _ := e.call("DELETE", "/api/sessions/"+id+"/files?path=inbox/sub&recursive=1", "", "admin"); code != 200 {
		t.Fatalf("recursive delete: %d", code)
	}
	if code, _ := e.call("DELETE", "/api/sessions/"+id+"/files?path=inbox/notes.txt", "", "admin"); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := e.call("DELETE", "/api/sessions/"+id+"/files?path=inbox/notes.txt", "", "admin"); code != 404 {
		t.Fatalf("delete again: %d", code)
	}
	if code, _ := e.call("DELETE", "/api/sessions/"+id+"/files?path=../../etc/passwd", "", "admin"); code != 403 {
		t.Fatalf("delete outside: %d", code)
	}
	if code, _ := e.call("DELETE", "/api/sessions/nope/files?path=a", "", "admin"); code != 404 {
		t.Fatalf("unknown session: %d", code)
	}
	// Not multipart.
	if code, _ := e.upload("/api/sessions/"+id+"/files", "application/json", strings.NewReader("{}"), "admin"); code != 400 {
		t.Fatalf("json upload: %d", code)
	}
	// The feed records the upload and the delete.
	if feed := e.m.Feed(time.Time{}, 0); !feedHas(feed, "added inbox/notes.txt") || !feedHas(feed, "deleted inbox/notes.txt") {
		t.Fatalf("feed: %+v", feed)
	}
}

func feedHas(items []session.FeedItem, text string) bool {
	for _, it := range items {
		if it.Kind == session.FeedFile && strings.Contains(it.Text, text) {
			return true
		}
	}
	return false
}

func TestFilesWritesNeedAuthAndCSRF(t *testing.T) {
	e := filesEnv(t, nil)
	id := e.person("echo", "csrf")
	send := func(hdr ...string) int {
		ctype, body := multipartBody(t, map[string]string{"a.txt": "a"})
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/sessions/"+id+"/files", body)
		req.Header.Set("Content-Type", ctype)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	cookie := "cerberus_token=" + testToken
	if c := send(); c != 401 {
		t.Fatalf("no auth: %d", c)
	}
	if c := send("Cookie", cookie); c != 403 {
		t.Fatalf("cookie without CSRF header: %d", c)
	}
	if c := send("Cookie", cookie, csrfHeader, "1"); c != 201 {
		t.Fatalf("cookie with CSRF header: %d", c)
	}
	if c := send("Cookie", cookie, csrfHeader, "1", "Sec-Fetch-Site", "cross-site"); c != 403 {
		t.Fatalf("cross-site: %d", c)
	}
	// An agent token never writes through the person's API.
	if c := send("Authorization", "Bearer "+e.cfg.SessionAgentToken(id)); c != 401 {
		t.Fatalf("agent token on the person's API: %d", c)
	}
	ctype, body := multipartBody(t, map[string]string{"a.txt": "a"})
	if code, _ := e.upload("/api/uploads", ctype, body, ""); code != 401 {
		t.Fatalf("staging without auth: %d", code)
	}
}

func TestFilesTooLarge(t *testing.T) {
	e := filesEnv(t, func(c *config.Config) { c.Files.MaxFileMiB = 1; c.Files.MaxTotalMiB = 3 })
	id := e.person("echo", "limits")
	big := strings.Repeat("x", 2<<20)

	// Over the per-file limit: refused while streaming, earlier files kept.
	buf := new(bytes.Buffer) // a fresh buffer per request: the client may still be sending after a 413
	mw := multipart.NewWriter(buf)
	fw, _ := mw.CreateFormFile("file", "small.txt")
	_, _ = io.WriteString(fw, "fits")
	fw, _ = mw.CreateFormFile("file", "big.bin")
	_, _ = io.WriteString(fw, big)
	_ = mw.Close()
	code, raw := e.upload("/api/sessions/"+id+"/files", mw.FormDataContentType(), buf, "admin")
	var fail uploadFailure
	if code != http.StatusRequestEntityTooLarge || json.Unmarshal(raw, &fail) != nil ||
		!strings.Contains(fail.Error, "per-file limit") ||
		fail.File != "big.bin" || len(fail.Entries) != 1 || fail.Entries[0].Name != "small.txt" {
		t.Fatalf("per-file 413: %d %s", code, raw)
	}
	inbox := filepath.Join(e.worktree(id), "inbox")
	if names := dirNames(t, inbox); strings.Join(names, ",") != "small.txt" {
		t.Fatalf("inbox after 413: %v (partial file left?)", names)
	}

	// A body over what is left of the session total: refused up front.
	buf = new(bytes.Buffer)
	mw = multipart.NewWriter(buf)
	for i := range 4 {
		fw, _ = mw.CreateFormFile("file", fmt.Sprintf("p%d.bin", i))
		_, _ = io.WriteString(fw, big[:1<<20-100])
	}
	_ = mw.Close()
	code, raw = e.upload("/api/sessions/"+id+"/files", mw.FormDataContentType(), buf, "admin")
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), "upload limit") {
		t.Fatalf("total 413: %d %s", code, raw)
	}

	// Staging follows the same limits, and a failed first upload leaves no folder.
	buf = new(bytes.Buffer)
	mw = multipart.NewWriter(buf)
	fw, _ = mw.CreateFormFile("file", "big.bin")
	_, _ = io.WriteString(fw, big)
	_ = mw.Close()
	code, raw = e.upload("/api/uploads", mw.FormDataContentType(), buf, "admin")
	if code != http.StatusRequestEntityTooLarge || strings.Contains(string(raw), `"staging"`) {
		t.Fatalf("staging 413: %d %s", code, raw)
	}
	if names := dirNames(t, e.m.UploadsDir()); len(names) != 0 {
		t.Fatalf("staging folder left after a failed upload: %v", names)
	}
}

func (e *ccEnv) worktree(id string) string {
	info, _ := e.m.Get(id)
	return info.Worktree
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, en := range ents {
		out = append(out, en.Name())
	}
	return out
}

func TestFilesRawHeaders(t *testing.T) {
	e := filesEnv(t, nil)
	id := e.person("echo", "raw")
	ctype, body := multipartBody(t, map[string]string{
		"page.html": "<script>alert(1)</script>", "pic.svg": "<svg xmlns='http://www.w3.org/2000/svg'/>",
		"a.png": "\x89PNG\r\n", "tool.bin": "<html><script>x</script>", "note.txt": "plain",
		"ñandú.pdf": "%PDF-1.4",
	})
	if code, raw := e.upload("/api/sessions/"+id+"/files", ctype, body, "admin"); code != 201 {
		t.Fatalf("upload: %d %s", code, raw)
	}
	cases := []struct {
		name, ctype, disp string
		sandbox           bool
	}{
		{"page.html", "text/html; charset=utf-8", "inline", true},
		{"pic.svg", "image/svg+xml", "inline", true},
		{"a.png", "image/png", "inline", false},
		{"tool.bin", "application/octet-stream", "attachment", false},
		{"note.txt", "text/plain; charset=utf-8", "inline", false},
		{"ñandú.pdf", "application/pdf", "inline", false},
	}
	for _, c := range cases {
		resp, _ := e.get("/api/sessions/"+id+"/files/raw?path=inbox/"+c.name, admin()...)
		h := resp.Header
		csp := h.Get("Content-Security-Policy")
		if resp.StatusCode != 200 || h.Get("Content-Type") != c.ctype || h.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.HasPrefix(h.Get("Content-Disposition"), c.disp) || strings.HasPrefix(csp, "sandbox") != c.sandbox {
			t.Errorf("%s: %d type=%q disp=%q csp=%q", c.name, resp.StatusCode, h.Get("Content-Type"),
				h.Get("Content-Disposition"), csp)
		}
		if c.sandbox && strings.Contains(csp, "allow-scripts") {
			t.Errorf("%s: scripts allowed: %q", c.name, csp)
		}
	}
	resp, _ := e.get("/api/sessions/"+id+"/files/raw?path=inbox/note.txt&download=1", admin()...)
	if d := resp.Header.Get("Content-Disposition"); d != `attachment; filename=note.txt` {
		t.Fatalf("download: %q", d)
	}
	resp, _ = e.get("/api/sessions/"+id+"/files/raw?path="+urlQuery("inbox/ñandú.pdf")+"&download=1", admin()...)
	if d := resp.Header.Get("Content-Disposition"); !strings.Contains(d, "filename*=utf-8''%C3%B1and%C3%BA.pdf") {
		t.Fatalf("utf-8 name: %q", d)
	}
	// Range requests work (video seek).
	resp, raw := e.get("/api/sessions/"+id+"/files/raw?path=inbox/note.txt", append(admin(), "Range", "bytes=1-3")...)
	if resp.StatusCode != http.StatusPartialContent || string(raw) != "lai" {
		t.Fatalf("range: %d %q", resp.StatusCode, raw)
	}
	// Folders, traversal and symlinks leading out are refused.
	for path, want := range map[string]int{"inbox": 400, "../../etc/passwd": 403, "inbox/none.txt": 404} {
		if resp, raw := e.get("/api/sessions/"+id+"/files/raw?path="+path, admin()...); resp.StatusCode != want {
			t.Errorf("raw %s: %d %s", path, resp.StatusCode, raw)
		}
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(e.worktree(id), "inbox", "out")); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.get("/api/sessions/"+id+"/files/raw?path=inbox/out", admin()...); resp.StatusCode != 403 {
		t.Fatalf("symlink out: %d", resp.StatusCode)
	}
}

func urlQuery(s string) string {
	return strings.NewReplacer("ñ", "%C3%B1", "ú", "%C3%BA").Replace(s)
}

func TestFilesAgentScopeOwnSessionOnly(t *testing.T) {
	e := filesEnv(t, nil)
	a := e.person("echo", "agent A")
	b := e.person("echo", "person B")
	for id, name := range map[string]string{a: "mine.txt", b: "theirs.txt"} {
		ctype, body := multipartBody(t, map[string]string{name: "secret of " + name})
		if code, raw := e.upload("/api/sessions/"+id+"/files", ctype, body, "admin"); code != 201 {
			t.Fatalf("upload: %d %s", code, raw)
		}
	}
	agent := []string{"Authorization", "Bearer " + e.cfg.SessionAgentToken(a), sessionHeader, a}

	resp, raw := e.get("/api/agent/files?dir=inbox", agent...)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "mine.txt") || strings.Contains(string(raw), "theirs.txt") {
		t.Fatalf("agent list: %d %s", resp.StatusCode, raw)
	}
	resp, raw = e.get("/api/agent/files/raw?path=inbox/mine.txt", agent...)
	if resp.StatusCode != 200 || string(raw) != "secret of mine.txt" {
		t.Fatalf("agent raw: %d %q", resp.StatusCode, raw)
	}
	// No way to name another session, nor to climb out of its own.
	if resp, _ := e.get("/api/agent/files/raw?path=../"+b+"/inbox/theirs.txt", agent...); resp.StatusCode != 403 {
		t.Fatalf("agent traversal: %d", resp.StatusCode)
	}
	if resp, _ := e.get("/api/agent/files/raw?path="+filepath.Join(e.worktree(b), "inbox", "theirs.txt"), agent...); resp.StatusCode == 200 {
		t.Fatal("agent read another session's file by absolute path")
	}
	for _, p := range []string{"/api/agent/sessions/" + b + "/files", "/api/agent/sessions/" + b + "/files/raw?path=inbox/theirs.txt"} {
		if resp, _ := e.get(p, agent...); resp.StatusCode != 404 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	// The agent token never reaches the person's file routes, and the agent
	// scope has no file writes.
	if resp, _ := e.get("/api/sessions/"+b+"/files/raw?path=inbox/theirs.txt", agent...); resp.StatusCode != 401 {
		t.Fatalf("agent on the person's raw route: %d", resp.StatusCode)
	}
	ctype, body := multipartBody(t, map[string]string{"x.txt": "x"})
	if code, _ := e.upload("/api/agent/files", ctype, body, a); code != 404 && code != 405 {
		t.Fatalf("agent upload: %d", code)
	}
	if code, _ := e.call("DELETE", "/api/agent/files?path=inbox/mine.txt", "", a); code != 404 && code != 405 {
		t.Fatalf("agent delete: %d", code)
	}
	// A person's token is refused on the agent scope.
	if resp, _ := e.get("/api/agent/files", admin()...); resp.StatusCode != 403 {
		t.Fatalf("admin on the agent scope: %d", resp.StatusCode)
	}
	// An agent token naming another session is refused.
	if resp, _ := e.get("/api/agent/files", "Authorization", "Bearer "+e.cfg.SessionAgentToken(a), sessionHeader, b); resp.StatusCode != 403 {
		t.Fatalf("mismatched session header: %d", resp.StatusCode)
	}
}

func TestFilesStagingRoundTrip(t *testing.T) {
	e := filesEnv(t, nil)
	ctype, body := multipartBody(t, map[string]string{"photo.png": "PNG", "spec.md": "# spec"})
	code, raw := e.upload("/api/uploads", ctype, body, "admin")
	var st stagingReply
	if code != 201 || json.Unmarshal(raw, &st) != nil || st.Staging == "" || len(st.Entries) != 2 {
		t.Fatalf("stage: %d %s", code, raw)
	}
	// A second request adds to the same staging folder.
	ctype, body = multipartBody(t, map[string]string{"more.txt": "more"})
	if code, raw := e.upload("/api/uploads?staging="+st.Staging, ctype, body, "admin"); code != 201 ||
		!strings.Contains(string(raw), `"staging":"`+st.Staging+`"`) {
		t.Fatalf("add to staging: %d %s", code, raw)
	}
	resp, raw := e.get("/api/uploads/"+st.Staging, admin()...)
	var l files.Listing
	if resp.StatusCode != 200 || json.Unmarshal(raw, &l) != nil || len(l.Entries) != 3 {
		t.Fatalf("staging list: %d %s", resp.StatusCode, raw)
	}
	ctype, body = multipartBody(t, map[string]string{"x.txt": "x"})
	if code, _ := e.upload("/api/uploads?staging=../../etc", ctype, body, "admin"); code != 404 {
		t.Fatalf("bad staging id: %d", code)
	}

	code, raw = e.call("POST", "/api/sessions", `{"task":"use the files","workspace":"scratch","agent":"echo","staging":"`+st.Staging+`"}`, "admin")
	var created []session.Info
	if code != 201 || json.Unmarshal(raw, &created) != nil || len(created) != 1 {
		t.Fatalf("create with staging: %d %s", code, raw)
	}
	id := created[0].ID
	e.waitFor("attachments delivered", func() bool {
		_, err := os.Stat(filepath.Join(e.worktree(id), "inbox", "more.txt"))
		return err == nil
	})
	resp, raw = e.get("/api/sessions/"+id+"/files?dir=inbox", admin()...)
	for _, n := range []string{"photo.png", "spec.md", "more.txt"} {
		if !strings.Contains(string(raw), `"Name":"`+n+`"`) {
			t.Fatalf("inbox after create: %d %s", resp.StatusCode, raw)
		}
	}
	if resp, _ := e.get("/api/uploads/"+st.Staging, admin()...); resp.StatusCode != 404 {
		t.Fatalf("staging still there: %d", resp.StatusCode)
	}
	// The same staging id cannot be used twice.
	if code, _ := e.call("POST", "/api/sessions", `{"task":"again","workspace":"scratch","staging":"`+st.Staging+`"}`, "admin"); code != 400 {
		t.Fatalf("reused staging: %d", code)
	}

	// Discarding staged attachments.
	ctype, body = multipartBody(t, map[string]string{"drop.txt": "x"})
	_, raw = e.upload("/api/uploads", ctype, body, "admin")
	_ = json.Unmarshal(raw, &st)
	if code, _ := e.call("DELETE", "/api/uploads/"+st.Staging, "", "admin"); code != 200 {
		t.Fatalf("delete staging: %d", code)
	}
	if code, _ := e.call("DELETE", "/api/uploads/"+st.Staging, "", "admin"); code != 404 {
		t.Fatalf("delete staging again: %d", code)
	}
}

// patternReader yields n deterministic bytes without holding them.
type patternReader struct{ left, off int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	if p.left <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(b)), p.left)
	for i := range n {
		b[i] = byte((p.off + i) * 31 % 251)
	}
	p.left -= n
	p.off += n
	return int(n), nil
}

// TestFilesStreamingUploadMemoryFlat sends a 50 MiB file through a pipe
// (neither side holds it) and checks the heap stays flat: the peak in-use
// heap and the bytes allocated while it streams are both far below the
// file size.
func TestFilesStreamingUploadMemoryFlat(t *testing.T) {
	const size = 50 << 20
	e := filesEnv(t, nil)
	id := e.person("echo", "big")

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	sent := sha256.New()
	go func() {
		fw, err := mw.CreateFormFile("file", "big.bin")
		if err == nil {
			_, err = io.Copy(io.MultiWriter(fw, sent), &patternReader{left: size})
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	rssBefore := rssKiB()

	// Sample the heap while the upload runs (test-only sampling).
	var peak atomic.Uint64
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak.Load() {
					peak.Store(ms.HeapInuse)
				}
			}
		}
	}()
	code, raw := e.upload("/api/sessions/"+id+"/files", mw.FormDataContentType(), pr, "admin")
	close(done)
	<-sampled
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	rssAfter := rssKiB()
	if code != 201 {
		t.Fatalf("upload: %d %s", code, raw)
	}

	heapGrowth := int64(peak.Load()) - int64(before.HeapInuse)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("50 MiB upload: peak heap growth %d KiB, allocated %d KiB, RSS %d → %d KiB",
		heapGrowth>>10, allocated>>10, rssBefore, rssAfter)
	if heapGrowth > size/5 {
		t.Fatalf("heap grew by %d bytes while streaming a %d byte file", heapGrowth, size)
	}
	if allocated > size/4 {
		t.Fatalf("allocated %d bytes while streaming a %d byte file", allocated, size)
	}
	if rssBefore > 0 && rssAfter > 0 && rssAfter-rssBefore > size/1024/2 {
		t.Fatalf("RSS grew by %d KiB", rssAfter-rssBefore)
	}

	f, err := os.Open(filepath.Join(e.worktree(id), "inbox", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := sha256.New()
	n, _ := io.Copy(got, f)
	if n != size || hex.EncodeToString(got.Sum(nil)) != hex.EncodeToString(sent.Sum(nil)) {
		t.Fatalf("stored %d bytes, hash match %v", n, bytes.Equal(got.Sum(nil), sent.Sum(nil)))
	}
	if info, _ := e.m.Get(id); info.UploadBytes != size {
		t.Fatalf("UploadBytes = %d", info.UploadBytes)
	}
}

// rssKiB is the process's resident set from /proc (0 where there is none).
func rssKiB() int64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return n
		}
	}
	return 0
}

func TestStatusFilesBlock(t *testing.T) {
	e := newCCEnv(t, func(c *config.Config) { c.Files.UploadDir = "drop"; c.Files.MaxFileMiB = 7; c.Files.MaxTotalMiB = 0 })
	code, raw := e.call("GET", "/api/status", "", "admin")
	want := `"files":{"upload_dir":"drop","max_file_mib":7,"max_total_mib":2048}`
	if code != 200 || !strings.Contains(string(raw), want) {
		t.Fatalf("status: %d %s, want %s", code, raw, want)
	}
}
