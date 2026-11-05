package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"strings"
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
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "no-cache")
	http.FileServerFS(s.ui).ServeHTTP(w, r)
}
