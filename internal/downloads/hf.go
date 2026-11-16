package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Hugging Face GGUF downloads for llama.cpp.

const maxRedirects = 10

var (
	hfSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	hfFile    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,200}\.[Gg][Gg][Uu][Ff]$`)
	hexSHA    = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// hfSource is a validated Hugging Face file URL.
type hfSource struct {
	url         string // canonical https://huggingface.co/<owner>/<repo>/resolve/<rev>/<path>
	owner, repo string
	file        string // base name, *.gguf
}

// parseHFURL accepts https://huggingface.co (or hf.co) file links, either
// /resolve/ (download) or /blob/ (the page), and returns the download URL.
// Query and fragment are dropped.
func parseHFURL(raw string) (hfSource, error) {
	bad := func(why string) (hfSource, error) {
		return hfSource{}, fmt.Errorf("%w: %s", ErrBadRef, why)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return bad("not a URL")
	}
	if u.Scheme != "https" {
		return bad("only https Hugging Face URLs are accepted")
	}
	if u.User != nil {
		return bad("URLs with credentials are refused")
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && p != "443" {
		return bad("unexpected port")
	}
	if host != "huggingface.co" && host != "hf.co" && host != "www.huggingface.co" {
		return bad("only huggingface.co and hf.co URLs are accepted")
	}
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i, s := range segs {
		d, err := url.PathUnescape(s)
		if err != nil || d == "" || d == "." || d == ".." || strings.ContainsAny(d, `/\`) {
			return bad("malformed path")
		}
		segs[i] = d
	}
	// owner/repo/{resolve|blob}/rev/path…/file.gguf
	if len(segs) < 5 || (segs[2] != "resolve" && segs[2] != "blob") {
		return bad("expected https://huggingface.co/<owner>/<repo>/resolve/<revision>/<file>.gguf")
	}
	owner, repo, file := segs[0], segs[1], segs[len(segs)-1]
	if !hfSegment.MatchString(owner) || !hfSegment.MatchString(repo) {
		return bad("unexpected owner or repository name")
	}
	if !hfFile.MatchString(file) {
		return bad("the URL must name a .gguf file")
	}
	segs[2] = "resolve"
	esc := make([]string, len(segs))
	for i, s := range segs {
		esc[i] = url.PathEscape(s)
	}
	return hfSource{url: "https://huggingface.co/" + strings.Join(esc, "/"), owner: owner, repo: repo, file: file}, nil
}

// checkHFURL allows https on huggingface.co, hf.co and their subdomains (the
// CDN and storage hosts a /resolve/ link redirects to). The first request
// must go to huggingface.co or hf.co itself.
func checkHFURL(raw string, redirect bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: not a URL", ErrBadRef)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: refusing non-https URL %s", ErrBadRef, u.Redacted())
	}
	host := strings.ToLower(u.Hostname())
	if host == "huggingface.co" || host == "hf.co" || host == "www.huggingface.co" {
		return nil
	}
	if redirect && (strings.HasSuffix(host, ".huggingface.co") || strings.HasSuffix(host, ".hf.co")) {
		return nil
	}
	return fmt.Errorf("%w: refusing host %s", ErrBadRef, host)
}

// linkedInfo collects what Hugging Face says about the file on the way: the
// SHA-256 of an LFS file (X-Linked-Etag) and its size (X-Linked-Size).
type linkedInfo struct {
	mu   sync.Mutex
	sha  string
	size int64
}

func (l *linkedInfo) observe(h http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range []string{"X-Linked-Etag", "X-Linked-ETag"} {
		if v := strings.Trim(strings.TrimPrefix(h.Get(k), "W/"), `"`); hexSHA.MatchString(v) && l.sha == "" {
			l.sha = strings.ToLower(v)
		}
	}
	if n, err := strconv.ParseInt(h.Get("X-Linked-Size"), 10, 64); err == nil && n > 0 && l.size == 0 {
		l.size = n
	}
}

// runHF downloads a GGUF into <models>/<owner>/<repo>/<file>.
func (m *Manager) runHF(ctx context.Context, j *job) error {
	src, err := parseHFURL(j.req.Ref)
	if err != nil {
		return err
	}
	dirs := m.opt.Config().Engines.LlamaCpp.ModelDirs
	if len(dirs) == 0 || dirs[0] == "" {
		return fmt.Errorf("%w: no llama.cpp models folder is configured", ErrUnavailable)
	}
	dir := filepath.Join(dirs[0], src.owner, src.repo)
	if set := hfShards(src); len(set) > 1 {
		return m.runHFSplit(ctx, j, src, set, dir)
	}
	dest := filepath.Join(dir, src.file)
	part := dest + ".part"
	m.update(j, true, func(it *Item) { it.Path = dest })
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_, err = m.fetchHF(ctx, j, hfFetch{url: src.url, part: part, dir: dir, want: j.req.SHA256})
	if err != nil {
		if m.discarded(j) {
			_ = os.Remove(part) // the user threw it away; a failure or shutdown keeps it to resume
		}
		return err
	}
	if m.discarded(j) {
		// Canceled while the finished file was being synced: honour it
		// rather than install a model the item reports as canceled.
		_ = os.Remove(part)
		return context.Canceled
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	if err := os.Rename(part, dest); err != nil {
		return err
	}
	// Where it came from, for exports (a failure here costs only that).
	_ = os.WriteFile(dest+SourceSuffix, []byte(src.url+"\n"), 0o600)
	return nil
}

// SourceSuffix names the note written beside a downloaded .gguf file with
// the URL it came from (engine.SourceSuffix is the same).
const SourceSuffix = ".source"

// HFDest returns where a Hugging Face .gguf link downloads to under the
// llama.cpp models folder dir (<dir>/<owner>/<repo>/<file>). For a file of
// a split set it is the set's first file, the one the engine lists.
func HFDest(dir, ref string) (string, error) {
	src, err := parseHFURL(ref)
	if err != nil {
		return "", err
	}
	src = firstShard(src)
	if dir == "" {
		return "", fmt.Errorf("%w: no llama.cpp models folder is configured", ErrUnavailable)
	}
	return filepath.Join(dir, src.owner, src.repo, src.file), nil
}

// HFGuess rebuilds the download link of a .gguf file stored as
// <owner>/<repo>/<file> under a models folder (the layout downloads use),
// for files with no source note. rel uses slashes. "" when rel has another
// shape.
func HFGuess(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) != 3 {
		return ""
	}
	src, err := parseHFURL("https://huggingface.co/" + parts[0] + "/" + parts[1] + "/resolve/main/" + parts[2])
	if err != nil {
		return ""
	}
	return src.url
}

// HFSource reads the source note of a downloaded file ("" when none or it
// is not a Hugging Face link).
func HFSource(gguf string) string {
	raw, err := os.ReadFile(gguf + SourceSuffix)
	if err != nil || len(raw) > 4096 {
		return ""
	}
	src, err := parseHFURL(strings.TrimSpace(string(raw)))
	if err != nil {
		return ""
	}
	return src.url
}

// hfFetch is one file fetchHF brings down.
type hfFetch struct {
	url, part, dir string
	want           string // the SHA-256 to check ("" = what Hugging Face says)
	// A file of a split set: progress counts base bytes before it out of
	// grand (0 = unknown), the item's total and checksum are the set's,
	// and the disk check covered the whole set when grand is known.
	split       bool
	base, grand int64
}

// fetchHF brings part up to the whole file (resuming what is there) and
// verifies it; verified says a checksum was checked. A checksum mismatch
// removes part.
func (m *Manager) fetchHF(ctx context.Context, j *job, fx hfFetch) (verified bool, err error) {
	rawURL, part, dir := fx.url, fx.part, fx.dir
	var off int64
	if st, err := os.Lstat(part); err == nil {
		if !st.Mode().IsRegular() {
			return false, fmt.Errorf("%s is not a regular file", part)
		}
		off = st.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	info := &linkedInfo{}
	client := m.hfClient(info)
	if err := m.checkURL(rawURL, false); err != nil {
		return false, err
	}
	resp, err := hfGet(ctx, client, rawURL, off)
	if err != nil {
		return false, err
	}
	defer func() { resp.Body.Close() }()
	info.observe(resp.Header)

	var total int64 = -1
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, end, size, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != off {
			return false, fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))
		}
		total = size
		if total < 0 {
			total = end + 1
		}
		m.update(j, true, func(it *Item) { it.Resumed = true })
	case http.StatusOK:
		off = 0 // the server ignored the range: start over
		total = resp.ContentLength
	case http.StatusRequestedRangeNotSatisfiable:
		_, _, size, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !(ok && size == off && off > 0) {
			// The part is not a prefix we can trust: start again from zero.
			resp.Body.Close()
			if err := os.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
			off = 0
			if resp, err = hfGet(ctx, client, rawURL, 0); err != nil {
				return false, err
			}
			info.observe(resp.Header)
			if resp.StatusCode != http.StatusOK {
				return false, fmt.Errorf("Hugging Face: HTTP %d", resp.StatusCode)
			}
			total = resp.ContentLength
		} else {
			total = off // already complete: only verify
		}
	default:
		return false, fmt.Errorf("Hugging Face: HTTP %d", resp.StatusCode)
	}
	if total < 0 && info.size > 0 {
		total = info.size
	}

	want := fx.want
	if want == "" {
		want = info.sha
	}
	if !fx.split {
		m.update(j, true, func(it *Item) {
			it.SHA256 = want
			if total > 0 {
				it.Total = total
			}
		})
	}

	// Disk: what is still to come plus the headroom (a split set's whole
	// size was checked before its first file).
	need := int64(DiskHeadroom)
	if total > 0 {
		need += total - off
	}
	if fx.split && fx.grand > 0 {
		need = 0
	}
	if free, err := m.opt.FreeSpace(dir); err == nil && need > 0 && free < uint64(need) {
		return false, fmt.Errorf("%w: need %s (file) + 2 GiB, %s free in %s", ErrNoSpace, gib(need-DiskHeadroom), gib(int64(free)), dir)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if off == 0 {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}
	f, err := os.OpenFile(part, flags|oNoFollow, 0o600)
	if err != nil {
		return false, err
	}
	_ = f.Chmod(0o600) // a .part kept from an older version may have wider bits
	h := sha256.New()
	if want != "" && off > 0 {
		if err := hashPrefix(part, off, h); err != nil {
			f.Close()
			return false, err
		}
	}
	done := off
	m.progress(j, fx.base+done, fx.total(total))
	if total < 0 || done < total || resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		var w io.Writer = f
		if want != "" {
			w = io.MultiWriter(f, h)
		}
		buf := make([]byte, 256<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					f.Close()
					return false, werr
				}
				done += int64(n)
				m.progress(j, fx.base+done, fx.total(total))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				f.Close()
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				return false, rerr
			}
		}
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return false, err
	}
	if total > 0 && done != total {
		return false, fmt.Errorf("download ended early: %d of %d bytes", done, total)
	}
	if want == "" {
		return false, nil
	}
	m.update(j, true, func(it *Item) { it.State = StateVerifying })
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(part)
		return false, fmt.Errorf("%w: got %s, want %s", ErrChecksum, got, want)
	}
	if !fx.split {
		m.update(j, true, func(it *Item) { it.Verified = true })
	}
	return true, nil
}

// total is the item's total for a file of size n: the set's when split.
func (fx hfFetch) total(n int64) int64 {
	if fx.split {
		return fx.grand
	}
	return n
}

// hfClient follows Hugging Face redirects (allowed hosts only), noting
// what each hop says about the file in info.
func (m *Manager) hfClient(info *linkedInfo) *http.Client {
	return &http.Client{Transport: m.hfTransport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if req.Response != nil {
			info.observe(req.Response.Header)
		}
		return m.checkURL(req.URL.String(), true)
	}}
}

func hfGet(ctx context.Context, client *http.Client, rawURL string, off int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cerberus")
	if off > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
	}
	return client.Do(req)
}

// hashPrefix feeds the first n bytes of path to h.
func hashPrefix(path string, n int64, h io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := io.CopyN(h, f, n)
	if err == nil && got != n {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// parseContentRange reads "bytes a-b/size" or "bytes */size" (size -1 when
// "*").
func parseContentRange(v string) (start, end, size int64, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	rng, sz, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, 0, false
	}
	size = -1
	if sz != "*" {
		n, err := strconv.ParseInt(sz, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		size = n
	}
	if rng == "*" {
		return -1, -1, size, true
	}
	a, b, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, 0, false
	}
	s, err1 := strconv.ParseInt(a, 10, 64)
	e, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || s < 0 || e < s {
		return 0, 0, 0, false
	}
	return s, e, size, true
}

func gib(n int64) string {
	return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GiB"
}
