package downloads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

const waitLong = 20 * time.Second

// env is a manager on a temp models dir with a recorder of changes.
type env struct {
	t      *testing.T
	m      *Manager
	cfg    *config.Config
	models string
	free   uint64
	mu     sync.Mutex
	done   []Item
	ch     chan Item
}

func newEnv(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no real lms
	cfg := config.Defaults()
	e := &env{t: t, cfg: cfg, models: filepath.Join(home, "models"), free: 1 << 50, ch: make(chan Item, 4096)}
	cfg.Engines.LlamaCpp.ModelDirs = []string{e.models}
	opt := Options{
		Config:    func() *config.Config { return cfg },
		OnChange:  func(it Item) { e.ch <- it },
		OnDone:    func(it Item) { e.mu.Lock(); e.done = append(e.done, it); e.mu.Unlock() },
		FreeSpace: func(string) (uint64, error) { return e.free, nil },
		LMSBinary: func() string { return "" },
	}
	if mod != nil {
		mod(&opt)
	}
	e.m = New(opt)
	t.Cleanup(e.m.Close)
	return e
}

// wait returns the first change of id that satisfies ok.
func (e *env) wait(id string, ok func(Item) bool) Item {
	e.t.Helper()
	if it, err := e.m.Get(id); err == nil && ok(it) {
		return it
	}
	deadline := time.After(waitLong)
	for {
		select {
		case it := <-e.ch:
			if it.ID == id && ok(it) {
				return it
			}
		case <-deadline:
			it, _ := e.m.Get(id)
			e.t.Fatalf("timed out; item %+v", it)
		}
	}
}

func (e *env) final(id string) Item { return e.wait(id, Item.Final) }

// hfFake serves a GGUF the way Hugging Face does: /resolve/ redirects to a
// CDN host with X-Linked-Etag/X-Linked-Size, the CDN honours Range.
type hfFake struct {
	content  []byte
	sha      string // advertised
	redirect string // where /resolve/ points (default the CDN)
	slow     bool   // stream the first 64 KiB, then hang until the client leaves
	mu       sync.Mutex
	ranges   []string
	hosts    []string
}

func (f *hfFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hosts = append(f.hosts, r.Host)
	f.mu.Unlock()
	switch {
	case strings.Contains(r.URL.Path, "/resolve/"):
		to := f.redirect
		if to == "" {
			to = "https://cdn-lfs.hf.co/repos/xx/model.gguf?sig=1"
		}
		w.Header().Set("X-Linked-Etag", `"`+f.sha+`"`)
		w.Header().Set("X-Linked-Size", fmt.Sprint(len(f.content)))
		http.Redirect(w, r, to, http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/repos/"):
		f.mu.Lock()
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		f.mu.Unlock()
		if f.slow {
			w.Header().Set("Content-Length", fmt.Sprint(len(f.content)))
			w.WriteHeader(http.StatusOK)
			w.Write(f.content[:64<<10])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		http.ServeContent(w, r, "model.gguf", time.Time{}, bytes.NewReader(f.content))
	default:
		http.NotFound(w, r)
	}
}

// rewrite sends every request to the test server, whatever host it names.
type rewrite struct {
	target *url.URL
	rt     http.RoundTripper
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Host = req.URL.Host
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return r.rt.RoundTrip(req)
}

func (e *env) serveHF(f *hfFake) {
	srv := httptest.NewServer(f)
	e.t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	e.m.hfTransport = rewrite{target: u, rt: srv.Client().Transport}
}

func blob(n int) ([]byte, string) {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:])
}

const hfURL = "https://huggingface.co/acme/tiny-GGUF/blob/main/tiny-Q4_K_M.gguf"

func (e *env) dest() string { return filepath.Join(e.models, "acme", "tiny-GGUF", "tiny-Q4_K_M.gguf") }

func TestHFResumeAndVerify(t *testing.T) {
	e := newEnv(t, nil)
	content, sum := blob(3<<20 + 123)
	f := &hfFake{content: content, sha: sum}
	e.serveHF(f)
	if err := os.MkdirAll(filepath.Dir(e.dest()), 0o755); err != nil {
		t.Fatal(err)
	}
	half := len(content) / 2
	if err := os.WriteFile(e.dest()+".part", content[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	it, err := e.m.Add(Request{Ref: hfURL})
	if err != nil {
		t.Fatal(err)
	}
	if it.Engine != EngineLlamaCpp || it.Name != "tiny-Q4_K_M.gguf" || !strings.Contains(it.Ref, "/resolve/main/") {
		t.Fatalf("item %+v", it)
	}
	it = e.final(it.ID)
	if it.State != StateDone || !it.Verified || !it.Resumed || it.SHA256 != sum || it.Bytes != int64(len(content)) || it.Path != e.dest() {
		t.Fatalf("final %+v", it)
	}
	got, err := os.ReadFile(e.dest())
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("file differs (%d bytes, %v)", len(got), err)
	}
	if _, err := os.Stat(e.dest() + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part left behind")
	}
	if st, _ := os.Stat(e.dest()); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("resumed from a 0644 .part: mode %v, want 0600", st.Mode())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ranges) != 1 || f.ranges[0] != fmt.Sprintf("bytes=%d-", half) {
		t.Fatalf("ranges %q", f.ranges)
	}
	if f.hosts[0] != "huggingface.co" || f.hosts[1] != "cdn-lfs.hf.co" {
		t.Fatalf("hosts %q", f.hosts)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.done) != 1 || e.done[0].State != StateDone {
		t.Fatalf("OnDone %+v", e.done)
	}
	if _, level, title, body, link := e.done[0].Notice(); level != "info" || title != "Model downloaded" || !strings.Contains(body, "not loaded") || link != "#/models" {
		t.Fatalf("notice %q %q %q", level, title, body)
	}
}

// TestHFFileModes: a download's folders are 0700 and its .part (hence the
// model it becomes) 0600, like everything else Cerberus writes.
func TestHFFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	e := newEnv(t, nil)
	content, sum := blob(256 << 10)
	e.serveHF(&hfFake{content: content, sha: sum})
	it, err := e.m.Add(Request{Ref: hfURL})
	if err != nil {
		t.Fatal(err)
	}
	if it = e.final(it.ID); it.State != StateDone {
		t.Fatalf("final %+v", it)
	}
	for p, want := range map[string]os.FileMode{
		e.dest(): 0o600, filepath.Dir(e.dest()): 0o700, filepath.Dir(filepath.Dir(e.dest())): 0o700,
	} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %o", p, st, err, want)
		}
	}
}

func TestHFChecksumMismatch(t *testing.T) {
	e := newEnv(t, nil)
	content, _ := blob(1 << 20)
	e.serveHF(&hfFake{content: content, sha: strings.Repeat("ab", 32)})
	it, err := e.m.Add(Request{Ref: hfURL})
	if err != nil {
		t.Fatal(err)
	}
	it = e.final(it.ID)
	if it.State != StateFailed || !strings.Contains(it.Error, "checksum mismatch") {
		t.Fatalf("final %+v", it)
	}
	for _, p := range []string{e.dest(), e.dest() + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept after a mismatch", p)
		}
	}
	if _, level, _, _, _ := it.Notice(); level != "warn" {
		t.Fatal("failed notice not a warning")
	}
}

func TestHFDiskRefusal(t *testing.T) {
	e := newEnv(t, nil)
	content, sum := blob(1 << 20)
	e.serveHF(&hfFake{content: content, sha: sum})
	e.free = DiskHeadroom + 1<<19 // less than size + 2 GiB
	it, err := e.m.Add(Request{Ref: hfURL})
	if err != nil {
		t.Fatal(err)
	}
	it = e.final(it.ID)
	if it.State != StateFailed || !strings.Contains(it.Error, ErrNoSpace.Error()) {
		t.Fatalf("final %+v", it)
	}
	if _, err := os.Stat(e.dest() + ".part"); !os.IsNotExist(err) {
		t.Fatal("wrote despite the refusal")
	}
}

func TestHFCancelAndQueue(t *testing.T) {
	e := newEnv(t, nil)
	content, sum := blob(4 << 20)
	f := &hfFake{content: content, sha: sum, slow: true}
	e.serveHF(f)
	first, err := e.m.Add(Request{Ref: hfURL})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.m.Add(Request{Ref: "https://hf.co/acme/tiny-GGUF/resolve/main/other.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != StateQueued {
		t.Fatalf("second not queued: %+v", second)
	}
	if dup, _ := e.m.Add(Request{Ref: hfURL}); dup.ID != first.ID {
		t.Fatal("duplicate request queued twice")
	}
	e.wait(first.ID, func(it Item) bool { return it.Bytes > 0 })
	if err := e.m.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	if it := e.final(first.ID); it.State != StateCanceled {
		t.Fatalf("first %+v", it)
	}
	if _, err := os.Stat(e.dest() + ".part"); !os.IsNotExist(err) {
		t.Fatal("canceled .part kept")
	}
	// The queue moves on; cancel the second while it runs too.
	e.wait(second.ID, func(it Item) bool { return it.State == StateRunning })
	if err := e.m.Cancel(second.ID); err != nil {
		t.Fatal(err)
	}
	e.final(second.ID)
	e.mu.Lock()
	n := len(e.done)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("OnDone called for canceled downloads")
	}
	// Cancel on a finished item forgets it.
	if err := e.m.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Get(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("finished item not forgotten")
	}
	if err := e.m.Cancel("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestHFRedirectToOtherHostRefused(t *testing.T) {
	e := newEnv(t, nil)
	content, sum := blob(1024)
	e.serveHF(&hfFake{content: content, sha: sum, redirect: "https://evil.example.com/repos/x"})
	it, _ := e.m.Add(Request{Ref: hfURL})
	it = e.final(it.ID)
	if it.State != StateFailed || !strings.Contains(it.Error, "evil.example.com") {
		t.Fatalf("final %+v", it)
	}
	e2 := newEnv(t, nil)
	e2.serveHF(&hfFake{content: content, sha: sum, redirect: "http://cdn-lfs.hf.co/repos/x"})
	it, _ = e2.m.Add(Request{Ref: hfURL})
	if it = e2.final(it.ID); it.State != StateFailed || !strings.Contains(it.Error, "non-https") {
		t.Fatalf("http redirect: %+v", it)
	}
}

func TestHFExistingFileRefused(t *testing.T) {
	e := newEnv(t, nil)
	content, sum := blob(1024)
	e.serveHF(&hfFake{content: content, sha: sum})
	os.MkdirAll(filepath.Dir(e.dest()), 0o755)
	os.WriteFile(e.dest(), []byte("mine"), 0o644)
	it, _ := e.m.Add(Request{Ref: hfURL})
	if it = e.final(it.ID); it.State != StateFailed || !strings.Contains(it.Error, "already exists") {
		t.Fatalf("final %+v", it)
	}
	if b, _ := os.ReadFile(e.dest()); string(b) != "mine" {
		t.Fatal("existing model overwritten")
	}
}

func TestParseHFURL(t *testing.T) {
	good := map[string]string{
		"https://huggingface.co/a/b/resolve/main/m.gguf":                  "https://huggingface.co/a/b/resolve/main/m.gguf",
		"https://hf.co/a/b/blob/v1.0/sub/M-Q8_0.GGUF?download=true":       "https://huggingface.co/a/b/resolve/v1.0/sub/M-Q8_0.GGUF",
		"https://huggingface.co:443/a/b/resolve/main/m.gguf#x":            "https://huggingface.co/a/b/resolve/main/m.gguf",
		"https://www.huggingface.co/bartowski/x-GGUF/resolve/main/y.gguf": "https://huggingface.co/bartowski/x-GGUF/resolve/main/y.gguf",
	}
	for in, want := range good {
		src, err := parseHFURL(in)
		if err != nil || src.url != want {
			t.Errorf("%s: %q %v", in, src.url, err)
		}
	}
	for _, bad := range []string{
		"http://huggingface.co/a/b/resolve/main/m.gguf",
		"https://evil.com/a/b/resolve/main/m.gguf",
		"https://huggingface.co.evil.com/a/b/resolve/main/m.gguf",
		"https://cdn-lfs.hf.co/a/b/resolve/main/m.gguf",
		"https://user:pw@huggingface.co/a/b/resolve/main/m.gguf",
		"https://huggingface.co:8443/a/b/resolve/main/m.gguf",
		"https://huggingface.co/a/b/resolve/main/m.bin",
		"https://huggingface.co/a/b/tree/main/m.gguf",
		"https://huggingface.co/a/b/resolve/main/../m.gguf",
		"https://huggingface.co/a/b/resolve/main/%2e%2e/m.gguf",
		"https://huggingface.co/a/b/resolve/main/x%2fm.gguf",
		"https://huggingface.co/../b/resolve/main/m.gguf",
		"ftp://huggingface.co/a/b/resolve/main/m.gguf",
		"huggingface.co/a/b/resolve/main/m.gguf",
	} {
		if _, err := parseHFURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if checkHFURL("https://cas-bridge.xethub.hf.co/x", true) != nil || checkHFURL("https://cdn-lfs.huggingface.co/x", true) != nil {
		t.Error("CDN redirect refused")
	}
	if checkHFURL("https://cdn-lfs.hf.co/x", false) == nil || checkHFURL("https://evilhf.co/x", true) == nil {
		t.Error("bad host allowed")
	}
}

func TestParseContentRange(t *testing.T) {
	if s, e, n, ok := parseContentRange("bytes 100-199/1000"); !ok || s != 100 || e != 199 || n != 1000 {
		t.Fatal(s, e, n, ok)
	}
	if _, _, n, ok := parseContentRange("bytes */500"); !ok || n != 500 {
		t.Fatal(n, ok)
	}
	for _, bad := range []string{"", "bytes 5-1/10", "items 0-1/2", "bytes 0-1"} {
		if _, _, _, ok := parseContentRange(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOllamaPull(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		if got["model"] == "missing:latest" {
			fmt.Fprintln(w, `{"status":"pulling manifest"}`)
			fmt.Fprintln(w, `{"error":"pull model manifest: file does not exist"}`)
			return
		}
		fl := w.(http.Flusher)
		for _, l := range []string{
			`{"status":"pulling manifest"}`,
			`{"status":"pulling aaa","digest":"sha256:aaa","total":1000,"completed":500}`,
			`{"status":"pulling bbb","digest":"sha256:bbb","total":3000,"completed":0}`,
			`{"status":"pulling aaa","digest":"sha256:aaa","total":1000,"completed":1000}`,
			`{"status":"pulling bbb","digest":"sha256:bbb","total":3000,"completed":3000}`,
			`{"status":"verifying sha256 digest"}`,
			`{"status":"success"}`,
		} {
			fmt.Fprintln(w, l)
			fl.Flush()
		}
	}))
	defer srv.Close()
	e := newEnv(t, nil)
	e.cfg.Engines.Ollama.URL = srv.URL
	it, err := e.m.Add(Request{Engine: EngineOllama, Ref: "qwen2.5-coder:7b"})
	if err != nil {
		t.Fatal(err)
	}
	it = e.final(it.ID)
	if it.State != StateDone || it.Total != 4000 || it.Bytes != 4000 || it.Percent != 100 {
		t.Fatalf("final %+v", it)
	}
	if got["model"] != "qwen2.5-coder:7b" || got["stream"] != true {
		t.Fatalf("request %v", got)
	}
	it, _ = e.m.Add(ParseRef("ollama:missing:latest"))
	if it = e.final(it.ID); it.State != StateFailed || !strings.Contains(it.Error, "file does not exist") {
		t.Fatalf("missing %+v", it)
	}
	for _, bad := range []string{"", "-rm", "../x", "a b", "a;b", "http://x/y"} {
		if _, err := e.m.Add(Request{Engine: EngineOllama, Ref: bad}); !errors.Is(err, ErrBadRef) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestOllamaCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"pulling aaa","digest":"sha256:aaa","total":1000,"completed":10}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	e := newEnv(t, nil)
	e.cfg.Engines.Ollama.URL = srv.URL
	it, _ := e.m.Add(Request{Engine: EngineOllama, Ref: "llama3.2"})
	e.wait(it.ID, func(it Item) bool { return it.Bytes == 10 })
	e.m.Cancel(it.ID)
	if it = e.final(it.ID); it.State != StateCanceled {
		t.Fatalf("final %+v", it)
	}
}

func TestValidationAndGuess(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.m.Add(Request{Engine: EngineLMStudio, Ref: "qwen"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("lms missing: %v", err)
	}
	if _, err := e.m.Add(Request{Engine: "vllm", Ref: "x"}); !errors.Is(err, ErrBadRef) {
		t.Fatal(err)
	}
	if _, err := e.m.Add(Request{Ref: hfURL, SHA256: "xyz"}); !errors.Is(err, ErrBadRef) {
		t.Fatal(err)
	}
	e.cfg.Engines.LlamaCpp.ModelDirs = nil
	if _, err := e.m.Add(Request{Ref: hfURL}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if r := e.m.guessEngine(Request{Ref: "llama3.2"}); r.Engine != EngineOllama {
		t.Fatalf("guess without lms: %+v", r)
	}
	for in, want := range map[string]Request{
		"lms:qwen/qwen3-8b":      {Engine: EngineLMStudio, Ref: "qwen/qwen3-8b"},
		"lmstudio: gemma":        {Engine: EngineLMStudio, Ref: "gemma"},
		"ollama:llama3.2":        {Engine: EngineOllama, Ref: "llama3.2"},
		"http://huggingface.co/": {Engine: EngineLlamaCpp, Ref: "http://huggingface.co/"},
		"llama3.2":               {Ref: "llama3.2"},
	} {
		if got := ParseRef(in); got != want {
			t.Errorf("ParseRef(%q) = %+v", in, got)
		}
	}
	// A non-https ref is refused at Add.
	if _, err := e.m.Add(ParseRef("http://huggingface.co/a/b/resolve/main/m.gguf")); !errors.Is(err, ErrBadRef) {
		t.Fatal(err)
	}
}

func TestProgressRateAndETA(t *testing.T) {
	e := newEnv(t, nil)
	now := time.Unix(1000, 0)
	e.m.now = func() time.Time { return now }
	j := &job{it: Item{ID: "x"}, sampleAt: now}
	e.m.progress(j, 0, 1000)
	now = now.Add(2 * time.Second)
	e.m.progress(j, 200, 0)
	if j.it.Rate != 100 || j.it.ETASec != 8 || j.it.Percent != 20 || j.it.Total != 1000 {
		t.Fatalf("%+v", j.it)
	}
	e.m.percent(j, 50)
	if j.it.Bytes != 500 {
		t.Fatalf("percent %+v", j.it)
	}
}

func TestCloseStopsRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	e := newEnv(t, nil)
	e.cfg.Engines.Ollama.URL = srv.URL
	a, _ := e.m.Add(Request{Engine: EngineOllama, Ref: "a"})
	b, _ := e.m.Add(Request{Engine: EngineOllama, Ref: "b"})
	e.wait(a.ID, func(it Item) bool { return it.State == StateRunning })
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	done := make(chan struct{})
	go func() { e.m.Close(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Close hung")
	}
	for _, id := range []string{a.ID, b.ID} {
		if it, _ := e.m.Get(id); it.State != StateCanceled {
			t.Fatalf("%s %+v", id, it)
		}
	}
	if _, err := e.m.Add(Request{Engine: EngineOllama, Ref: "c"}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

// TestSHA256OnlyForHF: a checksum is refused where nothing would check it.
func TestSHA256OnlyForHF(t *testing.T) {
	e := newEnv(t, nil)
	_, sum := blob(10)
	for _, req := range []Request{
		{Engine: EngineOllama, Ref: "qwen3:8b", SHA256: sum},
		{Engine: EngineLMStudio, Ref: "qwen3-8b", SHA256: sum},
		{Ref: "qwen3", SHA256: sum}, // guessed: never llama.cpp
	} {
		if _, err := e.m.Add(req); !errors.Is(err, ErrBadRef) || !strings.Contains(err.Error(), "only checked") {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

// TestReAddWhileCanceling: re-adding a download whose cancel has not
// landed yet queues a fresh job instead of returning the dying one.
func TestReAddWhileCanceling(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprintln(w, `{"status":"pulling aaa","digest":"sha256:aaa","total":1000,"completed":10}`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprintln(w, `{"status":"success"}`)
	}))
	defer srv.Close()
	e := newEnv(t, nil)
	e.cfg.Engines.Ollama.URL = srv.URL
	first, _ := e.m.Add(Request{Engine: EngineOllama, Ref: "llama3.2"})
	e.wait(first.ID, func(it Item) bool { return it.Bytes == 10 })
	// The window between Cancel and the runner stopping: canceled, but the
	// item still reads "running".
	e.m.mu.Lock()
	j := e.m.jobs[first.ID]
	j.canceled, j.discard = true, true
	cancel := j.cancel
	e.m.mu.Unlock()
	again, err := e.m.Add(Request{Engine: EngineOllama, Ref: "llama3.2"})
	if err != nil || again.ID == first.ID || again.State != StateQueued {
		t.Fatalf("re-add %+v %v", again, err)
	}
	cancel()
	if it := e.final(first.ID); it.State != StateCanceled {
		t.Fatalf("first %+v", it)
	}
	if it := e.final(again.ID); it.State != StateDone {
		t.Fatalf("second %+v", it)
	}
}

// TestHFCancelAfterFetch: a cancel that lands once the bytes are in (during
// the final sync) discards the file and reports canceled, consistently.
func TestHFCancelAfterFetch(t *testing.T) {
	e := newEnv(t, nil)
	content, sum := blob(1 << 16)
	e.serveHF(&hfFake{content: content, sha: sum})
	if err := os.MkdirAll(filepath.Dir(e.dest()), 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := parseHFURL(hfURL)
	j := &job{req: Request{Engine: EngineLlamaCpp, Ref: src.url}, discard: true, canceled: true, sampleAt: time.Now()}
	if err := e.m.runHF(context.Background(), j); !errors.Is(err, context.Canceled) {
		t.Fatalf("runHF: %v", err)
	}
	for _, p := range []string{e.dest(), e.dest() + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}
}

// TestProgressRateAfterResume: bytes already on disk are not counted as
// transferred in the first second.
func TestProgressRateAfterResume(t *testing.T) {
	e := newEnv(t, nil)
	now := time.Unix(1000, 0)
	e.m.now = func() time.Time { return now }
	j := &job{it: Item{ID: "x"}, sampleAt: now.Add(-time.Second)}
	e.m.progress(j, 3<<30, 4<<30) // the resumed .part
	now = now.Add(2 * time.Second)
	e.m.progress(j, 3<<30+200<<20, 0)
	if j.it.Rate != 100<<20 {
		t.Fatalf("rate %v, want 100 MiB/s", j.it.Rate)
	}
}
