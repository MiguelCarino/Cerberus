package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestAssetGzip: a compressible asset is served gzip to a browser that
// takes it, with its own ETag, and identity otherwise (web-api-ops-10).
func TestAssetGzip(t *testing.T) {
	js := strings.Repeat("export const a = 'cerberus';\n", 400)
	env := pwaEnv(t, fstest.MapFS{
		"dist/assets/js/app.js":     {Data: []byte(js)},
		"dist/assets/tiny.css":      {Data: []byte("a{}")},
		"dist/assets/fonts/x.woff2": {Data: []byte(strings.Repeat("x", 4000))},
		"dist/manifest.webmanifest": {Data: []byte(`{"name":"Cerberus"}`)},
		"dist/sw.js":                {Data: []byte(js)},
	})
	gz := withHeader("Accept-Encoding", "gzip, deflate, br")

	w := env.do("GET", "/assets/js/app.js", "", withCookie(testToken), gz)
	h := w.Header()
	if w.Code != 200 || h.Get("Content-Encoding") != "gzip" || h.Get("Vary") != "Accept-Encoding" ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/javascript") || !strings.HasSuffix(h.Get("ETag"), `-gz"`) {
		t.Fatalf("gzip variant: %d %v", w.Code, h)
	}
	if w.Body.Len() >= len(js)/2 || gunzip(t, w.Body.Bytes()) != js {
		t.Fatalf("gzip body: %d bytes", w.Body.Len())
	}
	gzTagSeen := h.Get("ETag")

	// Revalidating the gzip variant: 304 with no body.
	w = env.do("GET", "/assets/js/app.js", "", withCookie(testToken), gz, withHeader("If-None-Match", gzTagSeen))
	if w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("gzip 304: %d %d", w.Code, w.Body.Len())
	}

	// No Accept-Encoding: identity, the plain ETag, and Vary still set.
	w = env.do("GET", "/assets/js/app.js", "", withCookie(testToken))
	h = w.Header()
	if w.Code != 200 || h.Get("Content-Encoding") != "" || w.Body.String() != js || h.Get("Vary") != "Accept-Encoding" ||
		strings.HasSuffix(h.Get("ETag"), `-gz"`) || h.Get("ETag") == "" {
		t.Fatalf("identity: %d %v", w.Code, h)
	}
	// The gzip tag does not validate the identity variant.
	if w := env.do("GET", "/assets/js/app.js", "", withCookie(testToken), withHeader("If-None-Match", gzTagSeen)); w.Code != 200 {
		t.Fatalf("identity with the gzip tag: %d", w.Code)
	}
	// gzip;q=0 refuses it.
	if w := env.do("GET", "/assets/js/app.js", "", withCookie(testToken), withHeader("Accept-Encoding", "gzip;q=0")); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("q=0 got gzip")
	}
	// A range request reads identity bytes.
	if w := env.do("GET", "/assets/js/app.js", "", withCookie(testToken), gz, withHeader("Range", "bytes=0-9")); w.Code != 206 || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("range: %d %v", w.Code, w.Header())
	}
	// Too small to pay, and fonts: identity.
	for _, p := range []string{"/assets/tiny.css", "/assets/fonts/x.woff2"} {
		if w := env.do("GET", p, "", withCookie(testToken), gz); w.Code != 200 || w.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s: %d %v", p, w.Code, w.Header())
		}
	}
	// The service worker too, keeping its scope header.
	w = env.do("GET", "/sw.js", "", gz)
	h = w.Header()
	if w.Code != 200 || h.Get("Content-Encoding") != "gzip" || h.Get("Service-Worker-Allowed") != "/" ||
		gunzip(t, w.Body.Bytes()) != js || !strings.HasSuffix(h.Get("ETag"), `-gz"`) {
		t.Fatalf("sw gzip: %d %v", w.Code, h)
	}
	if w := env.do("GET", "/sw.js", "", gz, withHeader("If-None-Match", h.Get("ETag"))); w.Code != 304 {
		t.Fatalf("sw gzip 304: %d", w.Code)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for in, want := range map[string]bool{
		"": false, "gzip": true, "br, gzip": true, "GZIP": true, "gzip;q=0": false, "gzip; q=0.5": true,
		"*": true, "*;q=0": false, "deflate": false, "identity": false,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", in)
		if got := acceptsGzip(r); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}

// TestEventsNeverGzip: the SSE routes never carry Content-Encoding.
func TestEventsNeverGzip(t *testing.T) {
	if gzType("/api/events") != "" || gzType("/api/sessions/x/stream") != "" {
		t.Fatal("an SSE path has a gzip type")
	}
}
