package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"carino.systems/cerberus/internal/session"
)

// Previews are agent-written pages, so they are sandboxed (previewCSP) into
// an opaque origin. Such a page carries no cookies on its own subresource
// requests, so after the authenticated entry at /preview/<id>/… the browser
// is redirected to /preview/<id>/~<cap>/…, where cap is an unforgeable
// per-session capability (HMAC of the server token) that grants reading that
// one preview and nothing else. Relative links keep the prefix.

// previewCap is the capability for session id's preview ("" without a token).
func (s *server) previewCap(id string) string {
	tok := s.token()
	if tok == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(tok))
	mac.Write([]byte("cerberus preview\x00" + id))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// previewCapPath reports whether clean path p is /preview/<id>/~<cap>/… with
// the right capability for <id>.
func (s *server) previewCapPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/preview/")
	if !ok {
		return false
	}
	id, rest, _ := strings.Cut(rest, "/")
	seg, _, _ := strings.Cut(rest, "/")
	capTok, ok := strings.CutPrefix(seg, "~")
	want := s.previewCap(id)
	return ok && id != "" && want != "" && subtle.ConstantTimeCompare([]byte(capTok), []byte(want)) == 1
}

// handlePreview serves GET /preview/{id}/{path...} from the session's
// preview folder. os.Root keeps every open inside that folder (symlinks
// included), and any path segment starting with "." is refused so .git,
// .env and friends are never served or listed.
func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		http.NotFound(w, r)
		return
	}
	id, sub := r.PathValue("id"), r.PathValue("path")
	first0, _, _ := strings.Cut(sub, "/")
	if !strings.HasPrefix(first0, "~") {
		if src := r.URL.Query().Get("src"); src != "" || s.previews != nil {
			s.previewEntry(w, r, id, src, sub)
			return
		}
	}
	dir, ok := s.sessions.PreviewDir(id)
	if !ok {
		http.Error(w, "no preview for this session", http.StatusNotFound)
		return
	}
	first, after, _ := strings.Cut(sub, "/")
	if !strings.HasPrefix(first, "~") {
		s.redirectToCap(w, r, id, sub)
		return
	}
	if !s.previewCapPath(path.Clean("/preview/" + id + "/" + first)) {
		http.NotFound(w, r)
		return
	}
	if first == sub { // "~cap" without the trailing slash
		http.Redirect(w, r, path.Base(r.URL.Path)+"/", http.StatusMovedPermanently)
		return
	}
	sub = after
	rel := path.Clean("/" + sub)
	if hasDotSegment(rel) {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		http.Error(w, "preview unavailable", http.StatusNotFound)
		return
	}
	defer root.Close()
	r2 := r.Clone(r.Context())
	r2.URL.Path = rel
	if strings.HasSuffix(sub, "/") && rel != "/" {
		r2.URL.Path += "/"
	}
	r2.URL.RawPath = ""
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer") // the capability is in the URL
	http.FileServerFS(noDotFS{root.FS()}).ServeHTTP(w, r2)
}

// redirectToCap sends an authenticated preview request to its capability URL.
func (s *server) redirectToCap(w http.ResponseWriter, r *http.Request, id, sub string) {
	c := s.previewCap(id)
	if c == "" {
		http.NotFound(w, r)
		return
	}
	target := (&url.URL{Path: "/preview/" + id + "/~" + c + "/" + sub}).EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

// hasDotSegment reports whether any segment of a slash path starts with ".".
func hasDotSegment(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// noDotFS hides dot-named entries: they cannot be opened or listed.
type noDotFS struct{ fs.FS }

func (f noDotFS) Open(name string) (fs.File, error) {
	if hasDotSegment(name) && name != "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if d, ok := file.(fs.ReadDirFile); ok && st.IsDir() {
		return noDotDir{d}, nil
	}
	return file, nil
}

// noDotDir filters dot-named entries out of a directory listing.
type noDotDir struct{ fs.ReadDirFile }

func (d noDotDir) ReadDir(n int) ([]fs.DirEntry, error) {
	for {
		entries, err := d.ReadDirFile.ReadDir(n)
		kept := entries[:0]
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				kept = append(kept, e)
			}
		}
		// With n > 0, an all-dot batch must not look like the end of the directory.
		if len(kept) > 0 || err != nil || n <= 0 {
			return kept, err
		}
	}
}

// previewEntry is the authenticated way into a preview (GET
// /preview/<id>/[path][?src=<port>]): with preview ports it leases the
// preview's origin and redirects there with a capability; without them a
// static preview goes to its capability path as before, and live servers
// are not offered.
func (s *server) previewEntry(w http.ResponseWriter, r *http.Request, id, src, sub string) {
	source := "static"
	if src != "" && src != "static" {
		port, err := strconv.Atoi(src)
		if err != nil || port <= 0 || port > 65535 {
			http.Error(w, "bad preview source", http.StatusBadRequest)
			return
		}
		source = strconv.Itoa(port)
	}
	if s.previews == nil {
		if source != "static" {
			http.Error(w, "live previews need server.preview_ports (Settings, Server)", http.StatusNotFound)
			return
		}
		if _, ok := s.sessions.PreviewDir(id); !ok {
			http.Error(w, "no preview for this session", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		q.Del("src")
		r.URL.RawQuery = q.Encode()
		s.redirectToCap(w, r, id, sub)
		return
	}
	if source == "static" {
		if _, ok := s.sessions.PreviewDir(id); !ok {
			http.Error(w, "no preview for this session", http.StatusNotFound)
			return
		}
	} else {
		pv, _ := s.sessions.(previewService)
		port, _ := strconv.Atoi(source)
		if pv == nil {
			http.Error(w, "live previews are not available", http.StatusNotFound)
			return
		}
		if _, ok := pv.OwnsPort(id, port); !ok {
			http.Error(w, "this session runs no server on port "+source, http.StatusNotFound)
			return
		}
	}
	l, err := s.previews.acquire(id, source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	q.Del("src")
	q.Set(previewParam, s.previewOriginCap(l))
	target := s.previewOrigin(r, l.port) + (&url.URL{Path: "/" + sub}).EscapedPath() + "?" + q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusFound)
}

// previewOrigin is http(s)://<the host the browser used>:<port>.
func (s *server) previewOrigin(r *http.Request, port int) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := hostOnly(r.Host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + ":" + strconv.Itoa(port)
}

// previewReply is GET /api/sessions/{id}/preview.
type previewReply struct {
	Ports    bool                   `json:"ports"`  // each preview gets its own origin (server.preview_ports)
	Detect   bool                   `json:"detect"` // live servers are detected on this OS
	Static   *staticPreview         `json:"static"`
	Live     []livePreview          `json:"live"`
	Managed  session.ManagedPreview `json:"managed"`
	CanStart bool                   `json:"can_start"` // the caller may start and stop the preview server
}

type staticPreview struct {
	URL string `json:"url"` // the entry path (app-relative)
	Dir string `json:"dir"` // the folder, relative to the worktree ("." = root)
}

type livePreview struct {
	Port   int    `json:"port"`
	URL    string `json:"url"` // "" without preview ports
	Source string `json:"source"`
	State  string `json:"state"`
}

func (s *server) previewInfo(r *http.Request, id string) (previewReply, bool) {
	info, ok := s.sessions.Get(id)
	if !ok {
		return previewReply{}, false
	}
	rep := previewReply{Ports: s.previews != nil, Live: []livePreview{}}
	if dir, ok := s.sessions.PreviewDir(id); ok {
		rel := "."
		if info.Worktree != "" {
			if wt, err := filepath.EvalSymlinks(info.Worktree); err == nil {
				if r, err := filepath.Rel(wt, dir); err == nil {
					rel = filepath.ToSlash(r)
				}
			}
		}
		rep.Static = &staticPreview{URL: "/preview/" + id + "/", Dir: rel}
	}
	pv, _ := s.sessions.(previewService)
	if pv == nil {
		return rep, true
	}
	rep.Detect = pv.PreviewDetect()
	rep.Managed, _ = pv.PreviewManaged(id)
	for _, l := range pv.PreviewLive(id) {
		lp := livePreview{Port: l.Port, Source: l.Source, State: session.PreviewRunning}
		if l.Source == "managed" && rep.Managed.State != "" {
			lp.State = rep.Managed.State
		}
		if rep.Ports {
			lp.URL = "/preview/" + id + "/?src=" + strconv.Itoa(l.Port)
		}
		rep.Live = append(rep.Live, lp)
	}
	pr := principalOf(r)
	rep.CanStart = info.State != session.StateAccepted && info.State != session.StateDiscarded && info.Worktree != "" &&
		(pr.Kind == "" || s.canOwn(pr, info.Owner) && classAdmits(clsWork, pr))
	return rep, true
}

// handlePreviewGet is GET /api/sessions/{id}/preview.
func (s *server) handlePreviewGet(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	rep, ok := s.previewInfo(r, r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handlePreviewStart is POST /api/sessions/{id}/preview/start (Run
// preview): owner or admin, audited.
func (s *server) handlePreviewStart(w http.ResponseWriter, r *http.Request) {
	pv, ok := s.sessions.(previewService)
	if s.sessions == nil || !ok {
		unavailable(w, "previews")
		return
	}
	id := r.PathValue("id")
	if _, ok := s.sessions.Get(id); !ok {
		notFound(w)
		return
	}
	mp, err := pv.StartPreview(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrNoPreviewCommand):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "no_command"})
		case errors.Is(err, session.ErrSandboxUnavailable):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error(), "code": "sandbox"})
		default:
			writeError(w, sessionErrStatus(err), err.Error())
		}
		return
	}
	noteAudit(r, "preview server: "+strings.ReplaceAll(mp.Command, "{port}", strconv.Itoa(mp.Port)))
	rep, _ := s.previewInfo(r, id)
	writeJSON(w, http.StatusOK, rep)
}

// handlePreviewStop is POST /api/sessions/{id}/preview/stop.
func (s *server) handlePreviewStop(w http.ResponseWriter, r *http.Request) {
	pv, ok := s.sessions.(previewService)
	if s.sessions == nil || !ok {
		unavailable(w, "previews")
		return
	}
	id := r.PathValue("id")
	before, _ := pv.PreviewManaged(id)
	if err := pv.StopPreview(id); err != nil {
		writeError(w, sessionErrStatus(err), err.Error())
		return
	}
	if before.Port > 0 {
		s.previews.dropSource(id, strconv.Itoa(before.Port))
	}
	rep, _ := s.previewInfo(r, id)
	writeJSON(w, http.StatusOK, rep)
}
