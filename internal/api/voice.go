package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"carino.systems/cerberus/internal/voice"
)

// VoiceInfo is GET /api/voice. SecureHint is shown when the page is not a
// secure context (browsers allow the microphone only over HTTPS or on
// localhost); it is empty when this request already came over TLS.
type VoiceInfo struct {
	Available  bool   `json:"available"`
	Backend    string `json:"backend"`
	Reason     string `json:"reason"`
	SecureHint string `json:"secure_hint"`
	MaxSeconds int    `json:"max_seconds"`
}

func (s *server) handleVoice(w http.ResponseWriter, r *http.Request) {
	vi := VoiceInfo{Reason: "voice is not set up", SecureHint: s.secureHint(r)}
	if s.voice != nil {
		vi.MaxSeconds = s.voice.MaxSeconds()
		ok, what := s.voice.Available()
		if ok {
			vi.Available, vi.Backend, vi.Reason = true, what, ""
		} else {
			vi.Reason = what
		}
	}
	writeJSON(w, http.StatusOK, vi)
}

// secureHint tells the user how to reach the UI over HTTPS.
func (s *server) secureHint(r *http.Request) string {
	if r.TLS != nil {
		return ""
	}
	if pu := s.conf().Server.PublicURL; strings.HasPrefix(strings.ToLower(pu), "https://") {
		return "microphone needs HTTPS: open " + pu
	}
	if s.tlsConfigured() {
		return "microphone needs HTTPS: open this page through an extra listen address with https://"
	}
	return "microphone needs HTTPS: see README (tailscale cert)"
}

// tlsConfigured reports whether extra listen addresses serve HTTPS.
func (s *server) tlsConfigured() bool {
	sc := s.conf().Server
	return sc.TLSCert != "" && sc.TLSKey != "" && len(sc.ExtraListen) > 0
}

// handleVoiceTranscribe takes the recording as the body (Content-Type = its
// MIME type, at most 25 MiB) and answers {"text"}.
func (s *server) handleVoiceTranscribe(w http.ResponseWriter, r *http.Request) {
	if s.voice == nil {
		unavailable(w, "voice")
		return
	}
	if r.ContentLength > voice.MaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, voice.ErrTooLarge.Error())
		return
	}
	// The server has no ReadTimeout (sessions stream for hours), so a stalled
	// upload from a phone on a flaky link would block the body read until
	// TCP gives up. The transcriber bounds the upload by voice.Timeout; this
	// deadline ends a Read that is already blocked. Errors mean the
	// connection cannot take deadlines (HTTP/2 over some proxies): the
	// transcriber's own context check still applies between reads.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(voice.Timeout))
	// One byte over the limit lets the transcriber tell "too large" apart.
	body := http.MaxBytesReader(w, r.Body, voice.MaxBytes+1)
	text, err := s.voice.Transcribe(r.Context(), body, r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, voiceErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func voiceErrStatus(err error) int {
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, voice.ErrTooLarge), errors.Is(err, voice.ErrTooLong), errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, voice.ErrUnsupported):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, voice.ErrNoAudio):
		return http.StatusBadRequest
	case errors.Is(err, voice.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, os.ErrDeadlineExceeded): // the upload stalled past its read deadline
		return http.StatusRequestTimeout
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
