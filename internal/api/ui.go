package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// loadUI reads the embedded index.html and computes an ETag per asset, so
// browsers revalidate cheaply (embedded files carry no modification time).
func (s *server) loadUI(root fs.FS) {
	ui, err := fs.Sub(root, "dist")
	if err != nil {
		s.log.Error("web UI missing", "err", err)
		return
	}
	s.ui = ui
	s.index, err = fs.ReadFile(ui, "index.html")
	if err != nil {
		s.log.Error("web UI index missing", "err", err)
	}
	if nf, err := fs.ReadFile(ui, "404.html"); err == nil {
		s.notFound = nf // else handleNotFound answers a built-in page
	}
	s.etags = map[string]string{}
	_ = fs.WalkDir(ui, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, err := fs.ReadFile(ui, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(raw)
		s.etags["/"+p] = `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
		return nil
	})
	s.pwa = map[string]pwaFile{}
	for name, ctype := range pwaFiles {
		raw, err := fs.ReadFile(ui, name)
		if err != nil {
			continue // not built: the route answers 404
		}
		sum := sha256.Sum256(raw)
		s.pwa[name] = pwaFile{body: raw, ctype: ctype, etag: `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`}
	}
}

func (s *server) uiRoutes(mux *routeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if s.authenticated(r) {
			http.Redirect(w, r, keepQuery("/", r), http.StatusFound)
			return
		}
		s.handleIndex(w, r)
	})
	mux.HandleFunc("GET /assets/", s.handleAsset)
	// The PWA files live at the root: a service worker's default scope is
	// the folder it is served from.
	mux.HandleFunc("GET /manifest.webmanifest", s.handlePWA)
	mux.HandleFunc("GET /sw.js", s.handlePWA)
	// carino-navbar.js loads its clock as the page-relative "carino-clock.js".
	mux.HandleFunc("GET /carino-clock.js", func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/assets/carino-clock.js"
		s.handleAsset(w, r2)
	})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.index == nil {
		http.Error(w, "web UI not built", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.index)
}

// handleAsset serves files under /assets/ (no directory listings). A miss
// is the themed 404 page, like any unknown non-API path.
func (s *server) handleAsset(w http.ResponseWriter, r *http.Request) {
	tag, ok := s.etags[r.URL.Path]
	if !ok || strings.HasSuffix(r.URL.Path, "/") {
		s.handleNotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	if gzType(r.URL.Path) != "" {
		h.Set("Vary", "Accept-Encoding") // on both variants: a cache must keep them apart
		// A range request reads the identity bytes; ranges of the gzip
		// stream would mean nothing to the browser.
		if acceptsGzip(r) && r.Header.Get("Range") == "" {
			if gz := gzipped(s.ui, strings.TrimPrefix(r.URL.Path, "/"), tag); gz != nil {
				serveGzip(w, r, gz, gzType(r.URL.Path), tag)
				return
			}
		}
	}
	h.Set("ETag", tag)
	http.FileServerFS(s.ui).ServeHTTP(w, r)
}

// gzTypes are the asset types worth compressing, with the Content-Type the
// gzip variant is served with (FileServer picks the same for identity).
// Fonts and images are compressed already. SSE never comes through here.
var gzTypes = map[string]string{
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".svg":         "image/svg+xml",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
}

func gzType(p string) string {
	ext := strings.ToLower(path.Ext(p))
	t, ok := gzTypes[ext]
	if !ok {
		return ""
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m // what FileServer sends for the identity variant
	}
	return t
}

// acceptsGzip reports whether the request's Accept-Encoding takes gzip
// (q=0 refuses it; "*" counts).
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		if q > 0 {
			return true
		}
	}
	return false
}

// gzEntry is one compressed asset; nil body means gzip did not pay.
type gzEntry struct {
	once sync.Once
	body []byte
}

// gzCache holds the gzip variant of each embedded file, keyed by its
// content tag: the files never change in a running binary, so each one is
// compressed once per process, on its first gzip request. Doing it lazily
// keeps start-up (and every test server) free of the work.
var gzCache sync.Map // etag -> *gzEntry

// gzipped returns the gzip bytes of name, or nil when the file is missing
// or compresses to 90% of its size or more.
func gzipped(fsys fs.FS, name, tag string) []byte {
	if fsys == nil || tag == "" {
		return nil
	}
	v, _ := gzCache.LoadOrStore(tag, &gzEntry{})
	e := v.(*gzEntry)
	e.once.Do(func() {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return
		}
		e.body = gzipBytes(raw)
	})
	return e.body
}

func gzipBytes(raw []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(raw); err != nil || zw.Close() != nil {
		return nil
	}
	if buf.Len()*10 >= len(raw)*9 {
		return nil
	}
	return buf.Bytes()
}

// gzTag is the ETag of a gzip variant: a different representation needs
// its own validator.
func gzTag(tag string) string {
	return strings.TrimSuffix(tag, `"`) + `-gz"`
}

// serveGzip writes stored gzip bytes, answering a matching If-None-Match
// with 304. tag is the identity ETag.
func serveGzip(w http.ResponseWriter, r *http.Request, gz []byte, ctype, tag string) {
	h := w.Header()
	etag := gzTag(tag)
	h.Set("ETag", etag)
	h.Set("Vary", "Accept-Encoding")
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", ctype)
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", strconv.Itoa(len(gz)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(gz)
	}
}

// etagMatch is If-None-Match's weak comparison against one tag.
func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}
