package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"carino.systems/cerberus/internal/files"
	"carino.systems/cerberus/internal/files/fsafe"
	"carino.systems/cerberus/internal/session"
)

// File drop (day phase 2): files given to a session's workspace, staged
// New task attachments, and the read-only view an agent has of its own
// session's files. The Store (internal/files) does the safe path handling,
// naming and limits; these handlers stream request bodies into it.
//
//	POST   /api/sessions/{id}/files?dir=REL        multipart, one or more files → [Entry]
//	GET    /api/sessions/{id}/files?dir=REL        → Listing (one folder level, folders first)
//	GET    /api/sessions/{id}/files/raw?path=REL[&download=1] → the file
//	DELETE /api/sessions/{id}/files?path=REL[&recursive=1]    → {ok}
//	POST   /api/sessions/{id}/files/mkdir          {"path"} → {ok, path}
//	POST   /api/uploads[?staging=ID]               multipart → {staging, entries} (a new staging folder, or more files for ID)
//	GET    /api/uploads/{staging}                  → Listing
//	DELETE /api/uploads/{staging}                  → {ok}
//	GET    /api/agent/files?dir=REL                → Listing of the calling session's workspace
//	GET    /api/agent/files/raw?path=REL           → a file of the calling session's workspace
//
// A new session takes a staging folder through TaskSpec.Staging on POST
// /api/sessions. Writes need the token and, with the cookie, the CSRF header
// (withAuth). Uploads are read part by part with a mime/multipart Reader and
// streamed to disk: nothing is buffered whole, and the limits of
// cfg.Files (per file, per session or staging folder) answer 413 with a JSON
// {"error"} that names the limit, plus the entries already stored when an
// upload of several files stops part way.

// multipartSlack is the room allowed for multipart headers and boundaries on
// top of the byte limit when bounding a whole request body.
const multipartSlack = 1 << 20

func (s *server) fileRoutes(mux *routeMux) {
	mux.HandleFunc("POST /api/sessions/{id}/files", s.handleFilesUpload)
	mux.HandleFunc("GET /api/sessions/{id}/files", s.handleFilesList)
	mux.HandleFunc("GET /api/sessions/{id}/files/raw", s.handleFilesRaw)
	mux.HandleFunc("DELETE /api/sessions/{id}/files", s.handleFilesDelete)
	mux.HandleFunc("POST /api/sessions/{id}/files/mkdir", s.handleFilesMkdir)
	mux.HandleFunc("POST /api/uploads", s.handleUploadCreate)
	mux.HandleFunc("GET /api/uploads/{staging}", s.handleUploadList)
	mux.HandleFunc("DELETE /api/uploads/{staging}", s.handleUploadDelete)
}

// agentFileRoutes are the agent scope's read-only file routes: the caller's
// own session only, never another one, and no writes (the agent has its
// workspace on disk).
func (s *server) agentFileRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/agent/files", s.handleAgentFilesList)
	mux.HandleFunc("GET /api/agent/files/raw", s.handleAgentFilesRaw)
}

// filesReady answers 503 when there is no file store.
func (s *server) filesReady(w http.ResponseWriter) bool {
	if s.files == nil {
		unavailable(w, "file drop")
		return false
	}
	return true
}

// fileErrStatus maps a Store error to an HTTP status.
func fileErrStatus(err error) int {
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, files.ErrTooLarge), errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, files.ErrRefused), errors.Is(err, files.ErrOutside):
		return http.StatusForbidden
	case errors.Is(err, files.ErrNotFound), errors.Is(err, session.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, files.ErrIsDir), errors.Is(err, files.ErrBadTarget):
		return http.StatusBadRequest
	case errors.Is(err, files.ErrNotEmpty):
		return http.StatusConflict
	case errors.Is(err, context.Canceled):
		return http.StatusBadRequest
	}
	// The Store's other errors are about the session's state (not started,
	// accepted, discarded) or the disk.
	return http.StatusConflict
}

// fileLimits are cfg.Files' per-file and per-target limits in bytes, with
// the Store's defaults for unset values.
func (s *server) fileLimits() (perFile, total int64) {
	f, t := s.conf().Files.MaxFileMiB, s.conf().Files.MaxTotalMiB
	if f <= 0 {
		f = 200
	}
	if t <= 0 {
		t = 2048
	}
	return int64(f) << 20, int64(t) << 20
}

// tooLargeMsg is the 413 text for a body over its limit.
func (s *server) tooLargeMsg(err error) string {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		perFile, total := s.fileLimits()
		return fmt.Sprintf("too large: the upload passes the %s upload limit (%s per file)",
			files.LimitSize(total), files.LimitSize(perFile))
	}
	return err.Error()
}

// uploadFailure is the error body of an upload that stopped: the reason,
// the file it stopped at, and the entries stored before it.
type uploadFailure struct {
	Error   string        `json:"error"`
	File    string        `json:"file,omitempty"`
	Entries []files.Entry `json:"entries"`
	Staging string        `json:"staging,omitempty"`
}

// receive streams every file part of a multipart request into target
// (folder dir; "" = the session's inbox or the staging root). used is what
// already counts against the target's total, for the early Content-Length
// check. On failure it has answered and returns false; the entries stored
// before the failure are in the answer.
func (s *server) receive(w http.ResponseWriter, r *http.Request, target files.Target, dir string, used int64, staging string) ([]files.Entry, bool) {
	perFile, total := s.fileLimits()
	stored := []files.Entry{}
	fail := func(status int, msg, file string) ([]files.Entry, bool) {
		writeJSON(w, status, uploadFailure{Error: msg, File: file, Entries: stored, Staging: staging})
		return nil, false
	}
	room := max(total-used, 0)
	if r.ContentLength > room+multipartSlack {
		return fail(http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"too large: the upload is %s, %s of the %s upload limit is left (%s per file)",
			fsafe.HumanSize(r.ContentLength), fsafe.HumanSize(room), files.LimitSize(total),
			files.LimitSize(perFile)), "")
	}
	r.Body = http.MaxBytesReader(w, r.Body, total+multipartSlack)
	mr, err := r.MultipartReader()
	if err != nil {
		return fail(http.StatusBadRequest, "expected a multipart/form-data body with files: "+err.Error(), "")
	}
	store := s.files.As(hostOnly(r.RemoteAddr))
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			status := bodyErrStatus(err)
			if status == http.StatusRequestEntityTooLarge {
				return fail(status, s.tooLargeMsg(err), "")
			}
			return fail(status, "bad multipart body: "+err.Error(), "")
		}
		name := part.FileName()
		if name == "" { // a plain form field: not a file, skipped
			_ = part.Close()
			continue
		}
		e, err := store.PutIn(r.Context(), target, dir, name, part, -1)
		_ = part.Close()
		if err != nil {
			status := fileErrStatus(err)
			return fail(status, s.tooLargeMsg(err), name)
		}
		stored = append(stored, e)
	}
	if len(stored) == 0 {
		return fail(http.StatusBadRequest, "no files in the upload", "")
	}
	return stored, true
}

func (s *server) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	id := r.PathValue("id")
	info, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, session.ErrNotFound.Error())
		return
	}
	entries, ok := s.receive(w, r, files.Target{SessionID: id}, r.URL.Query().Get("dir"), info.UploadBytes, "")
	if !ok {
		return
	}
	noteAudit(r, uploadNote(entries))
	writeJSON(w, http.StatusCreated, entries)
}

func (s *server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	s.listFiles(w, files.Target{SessionID: r.PathValue("id")}, r.URL.Query().Get("dir"))
}

func (s *server) handleFilesRaw(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, files.Target{SessionID: r.PathValue("id")})
}

func (s *server) handleFilesDelete(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	q := r.URL.Query()
	rel := q.Get("path")
	if strings.TrimSpace(rel) == "" {
		writeError(w, http.StatusBadRequest, "give the path to delete")
		return
	}
	target := files.Target{SessionID: r.PathValue("id")}
	store := s.files.As(hostOnly(r.RemoteAddr))
	var err error
	if q.Get("recursive") == "1" {
		err = store.RemoveAll(target, rel)
	} else {
		err = store.Delete(target, rel)
	}
	if err != nil {
		writeError(w, fileErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleFilesMkdir(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var b struct {
		Path string `json:"path"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	rel, err := fsafe.CleanRel(b.Path)
	if err != nil || rel == "" {
		writeError(w, http.StatusBadRequest, "give a folder path inside the workspace")
		return
	}
	if err := s.files.Mkdir(files.Target{SessionID: r.PathValue("id")}, rel); err != nil {
		writeError(w, fileErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "path": rel})
}

// stagingReply is the answer of POST /api/uploads.
type stagingReply struct {
	Staging string        `json:"staging"`
	Entries []files.Entry `json:"entries"`
}

// handleUploadCreate stages New task attachments: a new staging folder, or
// more files for an existing one (?staging=ID), so a form can send each
// file in its own request with its own progress. A new folder whose upload
// fails stores nothing and is removed.
func (s *server) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	id := r.URL.Query().Get("staging")
	created := false
	var used int64
	if id != "" {
		if !s.canOwn(principalOf(r), s.files.StagingOwner(id)) {
			notFound(w)
			return
		}
		l, err := s.files.List(files.Target{Staging: id}, "")
		if err != nil {
			writeError(w, fileErrStatus(err), err.Error())
			return
		}
		for _, e := range l.Entries {
			used += e.Size
		}
	} else {
		var err error
		if id, err = s.files.NewStaging(); err != nil {
			writeError(w, http.StatusInternalServerError, "create a staging folder: "+err.Error())
			return
		}
		if owner := creator(principalOf(r)); owner != "" {
			if err := s.files.SetStagingOwner(id, owner); err != nil {
				_ = s.files.DeleteStaging(id)
				writeError(w, http.StatusInternalServerError, "create a staging folder: "+err.Error())
				return
			}
		}
		created = true
	}
	shown := id
	if created {
		shown = "" // nothing survives a failed first upload
	}
	entries, ok := s.receive(w, r, files.Target{Staging: id}, "", used, shown)
	if !ok {
		if created {
			_ = s.files.DeleteStaging(id)
		}
		return
	}
	writeJSON(w, http.StatusCreated, stagingReply{Staging: id, Entries: entries})
}

func (s *server) handleUploadList(w http.ResponseWriter, r *http.Request) {
	s.listFiles(w, files.Target{Staging: r.PathValue("staging")}, "")
}

func (s *server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	if err := s.files.DeleteStaging(r.PathValue("staging")); err != nil {
		writeError(w, fileErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleAgentFilesList(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.agentReady(w, r)
	if !ok {
		return
	}
	s.listFiles(w, files.Target{SessionID: caller}, r.URL.Query().Get("dir"))
}

func (s *server) handleAgentFilesRaw(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.agentReady(w, r)
	if !ok {
		return
	}
	s.serveFile(w, r, files.Target{SessionID: caller})
}

// listFiles answers one folder level of target.
func (s *server) listFiles(w http.ResponseWriter, target files.Target, dir string) {
	if !s.filesReady(w) {
		return
	}
	l, err := s.files.List(target, dir)
	if err != nil {
		writeError(w, fileErrStatus(err), err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, l)
}

// rawCSP sandboxes HTML, SVG and XML documents served from a workspace: no
// scripts, an opaque origin, nothing loaded from anywhere but inline styles
// and data: images. They render as static documents and can never act with
// the user's cookie.
const rawCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; media-src data:"

// serveFile sends one file of target (?path=REL). The type comes from the
// extension (never sniffed: nosniff); active documents (HTML, SVG, XML) are
// sandboxed by CSP; types that are not safe to show inline are sent as an
// attachment, and so is everything with ?download=1. Range requests work
// (audio and video seek).
func (s *server) serveFile(w http.ResponseWriter, r *http.Request, target files.Target) {
	if !s.filesReady(w) {
		return
	}
	q := r.URL.Query()
	f, e, err := s.files.Open(target, q.Get("path"))
	if err != nil {
		writeError(w, fileErrStatus(err), err.Error())
		return
	}
	defer f.Close()
	h := w.Header()
	mt := e.Mime
	if mt == "" {
		mt = "application/octet-stream"
	}
	if strings.HasPrefix(mt, "text/") {
		h.Set("Content-Type", mt+"; charset=utf-8")
	} else {
		h.Set("Content-Type", mt)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "private, no-cache")
	if activeDocument(mt) {
		h.Set("Content-Security-Policy", rawCSP)
	}
	disp := "inline"
	if q.Get("download") == "1" || !inlineSafe(mt) {
		disp = "attachment"
	}
	h.Set("Content-Disposition", contentDisposition(disp, e.Name))
	http.ServeContent(w, r, e.Name, e.Mod, f)
}

// activeDocument reports whether a browser would run scripts in a document
// of type mt when opened directly: HTML, XHTML, SVG and XML (XSLT).
func activeDocument(mt string) bool {
	switch mt {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "application/xml", "text/xml":
		return true
	}
	return strings.HasSuffix(mt, "+xml")
}

// inlineSafe reports whether a type may be shown inline: images, audio,
// video, PDF, plain text and JSON, and the active documents (sandboxed by
// rawCSP). Anything else (archives, binaries, unknown types) is downloaded.
func inlineSafe(mt string) bool {
	switch {
	case activeDocument(mt):
		return true
	case strings.HasPrefix(mt, "image/"), strings.HasPrefix(mt, "audio/"), strings.HasPrefix(mt, "video/"),
		strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/pdf", mt == "application/json":
		return true
	}
	return false
}

// contentDisposition builds the header with the file name (RFC 2231 for
// names that are not plain ASCII).
func contentDisposition(disp, name string) string {
	if v := mime.FormatMediaType(disp, map[string]string{"filename": name}); v != "" {
		return v
	}
	return disp
}

// uploadNote is the audit detail of an upload: how many files, how many
// bytes, and the first name (the log clips it).
func uploadNote(entries []files.Entry) string {
	var n int64
	for _, e := range entries {
		n += e.Size
	}
	note := fmt.Sprintf("uploaded %d file(s), %d bytes", len(entries), n)
	if len(entries) > 0 {
		note += ": " + entries[0].Rel
	}
	return note
}
