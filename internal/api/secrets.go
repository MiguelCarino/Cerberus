package api

import (
	"errors"
	"net/http"
	"strings"

	"carino.systems/cerberus/internal/vault"
)

// maxSecretBody bounds PUT /api/secrets/{name}: a 16 KiB value plus a note
// and JSON escaping.
const maxSecretBody = 32 << 10

// Secret values travel in exactly one direction: from a PUT body into the
// vault. No handler reads a value back, and errors are scrubbed of the value
// before they are logged or returned, in case a backend ever echoes input.

func (s *server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		unavailable(w, "vault")
		return
	}
	items, err := s.secrets.List()
	if err != nil {
		s.log.Warn("vault list", "err", err)
		writeError(w, http.StatusInternalServerError, "vault: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backend": s.secrets.Backend(), "items": nonNil(items)})
}

func (s *server) handleSecretPut(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		unavailable(w, "vault")
		return
	}
	name := r.PathValue("name")
	if !vault.ValidName(name) {
		writeError(w, http.StatusBadRequest, vault.ErrInvalidName.Error())
		return
	}
	var b struct {
		Value string `json:"value"`
		Note  string `json:"note"`
	}
	if err := decodeBodyLimit(w, r, &b, maxSecretBody); err != nil {
		// The decoder's message can quote the body; never echo it.
		writeError(w, bodyErrStatus(err), "invalid body: expected {\"value\", \"note\"} of at most 32 KiB")
		return
	}
	if b.Value == "" {
		writeError(w, http.StatusBadRequest, vault.ErrValueSize.Error())
		return
	}
	if err := s.secrets.Set(name, b.Value, b.Note); err != nil {
		msg := scrub(err.Error(), b.Value)
		switch {
		case errors.Is(err, vault.ErrInvalidName), errors.Is(err, vault.ErrValueSize), errors.Is(err, vault.ErrNoteSize):
			writeError(w, http.StatusBadRequest, msg)
		default:
			s.log.Warn("vault set", "name", name, "err", msg)
			writeError(w, http.StatusInternalServerError, msg)
		}
		return
	}
	s.log.Info("secret stored", "name", name, "backend", s.secrets.Backend())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (s *server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		unavailable(w, "vault")
		return
	}
	name := r.PathValue("name")
	if !vault.ValidName(name) {
		writeError(w, http.StatusBadRequest, vault.ErrInvalidName.Error())
		return
	}
	if err := s.secrets.Delete(name); err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		s.log.Warn("vault delete", "name", name, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("secret deleted", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

// scrub masks value in msg.
func scrub(msg, value string) string {
	if value == "" {
		return msg
	}
	return strings.ReplaceAll(msg, value, "••••••")
}
