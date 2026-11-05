package api

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

// fakePV adds the preview methods to fakeSessions: dirs per session and
// the ports each session owns.
type fakePV struct {
	*fakeSessions
	mu      sync.Mutex
	dirs    map[string]string
	owns    map[string][]int
	started []string
}

func (f *fakePV) PreviewDir(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.dirs[id]
	return d, ok
}
func (f *fakePV) Get(id string) (session.Info, bool) {
	if id == "s2" {
		return session.Info{ID: "s2", State: session.StatePassed, Worktree: "/w2"}, true
	}
	info, ok := f.fakeSessions.Get(id)
	if ok && info.Worktree == "" {
		info.Worktree = "/w1"
	}
	return info, ok
}
func (f *fakePV) PreviewLive(id string) []session.LiveServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []session.LiveServer
	for _, p := range f.owns[id] {
		out = append(out, session.LiveServer{Port: p, Source: "detected"})
	}
	return out
}
func (f *fakePV) OwnsPort(id string, port int) (session.LiveServer, bool) {
	for _, l := range f.PreviewLive(id) {
		if l.Port == port {
			return l, true
		}
	}
	return session.LiveServer{}, false
}
func (f *fakePV) PreviewManaged(id string) (session.ManagedPreview, error) {
	return session.ManagedPreview{Command: "npm run dev -- --port {port}", Source: `package.json "dev"`}, nil
}
func (f *fakePV) StartPreview(_ context.Context, id string) (session.ManagedPreview, error) {
	f.mu.Lock()
	f.started = append(f.started, id)
	f.mu.Unlock()
	return session.ManagedPreview{State: session.PreviewStarting, Command: "x {port}", Port: 4000}, nil
}
func (f *fakePV) StopPreview(id string) error { return nil }
func (f *fakePV) TouchPreview(id string)      {}
func (f *fakePV) PreviewDetect() bool         { return true }

func (f *fakePV) own(id string, port int, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owns == nil {
		f.owns = map[string][]int{}
	}
	if on {
		f.owns[id] = append(f.owns[id], port)
	} else {
		f.owns[id] = nil
	}
}

// freePortRange finds a free loopback port and returns a small range
// starting there (ports of it that are busy are skipped by acquire).
func freePortRange(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if p > 65000 {
		p = 64000
	}
	return fmt.Sprintf("%d-%d", p, p+9)
}

type pvEnv struct {
	*testEnv
	pv   *fakePV
	prev *Previews
	site string
}

// newPVEnv is a test server with per-origin previews on loopback ports.
func newPVEnv(t *testing.T) *pvEnv {
	t.Helper()
	site := t.TempDir()
	for name, body := range map[string]string{
		"index.html":   `<!doctype html><link rel="stylesheet" href="/style.css"><h1>Hello</h1>`,
		"style.css":    `h1{color:rgb(1,2,3)}`,
		"app/page.txt": "deep",
		".git/config":  "secret",
	} {
		p := filepath.Join(site, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := NewPreviews(freePortRange(t), func(port int) ([]net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return nil, err
		}
		return []net.Listener{ln}, nil
	}, nil)
	if err != nil || prev == nil {
		t.Fatal(err)
	}
	t.Cleanup(prev.Close)
	env := newEnv(t, func(c *config.Config) { c.Server.Listen = "127.0.0.1:7340" })
	env.srv.previews = prev
	prev.srv.Store(env.srv)
	pv := &fakePV{fakeSessions: env.sessions, dirs: map[string]string{"s1": site, "s2": site}}
	env.srv.sessions = pv
	env.h = env.srv.handler()
	return &pvEnv{testEnv: env, pv: pv, prev: prev, site: site}
}

// enter follows the app's entry redirect and returns the preview URL with
// its capability.
func (e *pvEnv) enter(t *testing.T, target string) *url.URL {
	t.Helper()
	w := e.do("GET", target, "", withBearer(testToken), withHost("127.0.0.1:7340"))
	if w.Code != http.StatusFound {
		t.Fatalf("entry %s: %d %s", target, w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Query().Get(previewParam) == "" {
		t.Fatalf("entry redirect: %q", w.Header().Get("Location"))
	}
	if port, _ := strconv.Atoi(u.Port()); !e.prev.InRange(port) {
		t.Fatalf("redirect to port %s outside the range", u.Port())
	}
	return u
}

var noRedirect = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func get(t *testing.T, u string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// login takes the capability URL, checks the cookie and redirect, and
// returns the cookie.
func login(t *testing.T, u *url.URL) *http.Cookie {
	t.Helper()
	resp := get(t, u.String())
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" && !strings.HasPrefix(resp.Header.Get("Location"), "/") {
		t.Fatalf("capability entry: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if strings.Contains(resp.Header.Get("Location"), previewParam) {
		t.Fatalf("redirect keeps the capability: %q", resp.Header.Get("Location"))
	}
	for _, c := range resp.Cookies() {
		if c.Name == previewCookie+u.Port() {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookie flags: %+v", c)
			}
			return c
		}
	}
	t.Fatalf("no preview cookie in %v", resp.Header.Values("Set-Cookie"))
	return nil
}

// TestPreviewOriginStatic: the static site is served at the root of its
// own origin, so absolute paths work; the capability becomes a cookie;
// nothing else gets in.
func TestPreviewOriginStatic(t *testing.T) {
	e := newPVEnv(t)
	u := e.enter(t, "/preview/s1/")
	cookie := login(t, u)
	base := "http://" + u.Host

	resp := get(t, base+"/", cookie)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, `href="/style.css"`) {
		t.Fatalf("index: %d %s", resp.StatusCode, b)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors http://127.0.0.1:7340") || strings.Contains(csp, "sandbox") {
		t.Fatalf("CSP: %q", csp)
	}
	resp = get(t, base+"/style.css", cookie)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "rgb(1,2,3)") {
		t.Fatalf("absolute /style.css: %d %s", resp.StatusCode, b)
	}
	if resp := get(t, base+"/app/page.txt", cookie); body(t, resp) != "deep" {
		t.Fatal("nested file")
	}
	if resp := get(t, base+"/.git/config", cookie); resp.StatusCode != 404 {
		t.Fatalf("dot folder: %d", resp.StatusCode)
	}
	// No cookie, a wrong capability, the app's own cookie: refused.
	if resp := get(t, base+"/style.css"); resp.StatusCode != 403 {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}
	if resp := get(t, base+"/?"+previewParam+"=deadbeef"); resp.StatusCode != 403 {
		t.Fatalf("wrong capability: %d", resp.StatusCode)
	}
	if resp := get(t, base+"/style.css", &http.Cookie{Name: cookieName, Value: testToken}); resp.StatusCode != 403 {
		t.Fatalf("app cookie on the preview origin: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", base+"/style.css", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if resp, err := noRedirect.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("app token on the preview origin: %v %v", resp, err)
	}
	// Service worker scripts are refused.
	req, _ = http.NewRequest("GET", base+"/sw.js", nil)
	req.AddCookie(cookie)
	req.Header.Set("Service-Worker", "script")
	if resp, err := noRedirect.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("service worker: %v %v", resp, err)
	}
	// An unknown Host (DNS rebinding) is refused.
	req, _ = http.NewRequest("GET", base+"/", nil)
	req.Host = "evil.example:" + u.Port()
	req.AddCookie(cookie)
	if resp, err := noRedirect.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("foreign Host: %v %v", resp, err)
	}
	// The same entry reuses the lease; deep paths keep their path.
	u2 := e.enter(t, "/preview/s1/app/page.txt")
	if u2.Host != u.Host || u2.Path != "/app/page.txt" {
		t.Fatalf("second entry: %s", u2)
	}
}

// TestPreviewCookieOtherSession: one session's capability or cookie never
// opens another session's preview, and a dropped lease's cookie dies.
func TestPreviewCookieOtherSession(t *testing.T) {
	e := newPVEnv(t)
	u1 := e.enter(t, "/preview/s1/")
	c1 := login(t, u1)
	u2 := e.enter(t, "/preview/s2/")
	if u1.Host == u2.Host {
		t.Fatal("two sessions share an origin")
	}
	// s1's cookie value under s2's cookie name, and s1's capability on s2.
	forged := &http.Cookie{Name: previewCookie + u2.Port(), Value: c1.Value}
	if resp := get(t, "http://"+u2.Host+"/", forged); resp.StatusCode != 403 {
		t.Fatalf("s1's cookie on s2: %d", resp.StatusCode)
	}
	if resp := get(t, "http://"+u2.Host+"/?"+previewParam+"="+u1.Query().Get(previewParam)); resp.StatusCode != 403 {
		t.Fatalf("s1's capability on s2: %d", resp.StatusCode)
	}
	// After the lease ends the old cookie is worthless, even if the port
	// comes back for the same session.
	e.prev.dropSession("s1")
	u3 := e.enter(t, "/preview/s1/")
	if resp := get(t, "http://"+u3.Host+"/", &http.Cookie{Name: previewCookie + u3.Port(), Value: c1.Value}); resp.StatusCode != 403 {
		t.Fatalf("old cookie on a new lease: %d", resp.StatusCode)
	}
	if resp := get(t, u3.String()); resp.Header.Get("Clear-Site-Data") == "" {
		t.Fatal("a new lease must clear the origin's storage on entry")
	}
}

// TestPreviewCookieRefusedByApp: the app never takes a preview cookie as
// a credential, and conflicting app cookies (cookie tossing from a
// preview port) are refused.
func TestPreviewCookieRefusedByApp(t *testing.T) {
	e := newPVEnv(t)
	u := e.enter(t, "/preview/s1/")
	c := login(t, u)
	if w := e.do("GET", "/api/status", "", func(r *http.Request) { r.AddCookie(c) }); w.Code != 401 {
		t.Fatalf("preview cookie on the app: %d", w.Code)
	}
	if w := e.do("GET", "/api/status", "", withBearer(c.Value)); w.Code != 401 {
		t.Fatalf("preview cookie value as a bearer: %d", w.Code)
	}
	two := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: "cls-planted"})
		r.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	}
	if w := e.do("GET", "/api/status", "", two); w.Code != 401 || !strings.Contains(w.Body.String(), "conflicting") {
		t.Fatalf("conflicting cookies: %d %s", w.Code, w.Body.String())
	}
}

// TestPreviewPageCannotCallAPI: a page on a preview origin (same host,
// another port) cannot use the app's cookie on /api, and gets no CORS.
func TestPreviewPageCannotCallAPI(t *testing.T) {
	e := newPVEnv(t)
	u := e.enter(t, "/preview/s1/")
	origin := "http://" + u.Host
	host := withHost("127.0.0.1:7340")
	cases := []struct {
		name string
		opts []reqOpt
	}{
		{"modern browser", []reqOpt{withHeader("Sec-Fetch-Site", "same-site"), withHeader("Origin", origin)}},
		{"no fetch metadata", []reqOpt{withHeader("Origin", origin)}},
		{"opaque origin", []reqOpt{withHeader("Origin", "null")}},
	}
	for _, c := range cases {
		opts := append([]reqOpt{withCookie(testToken), host}, c.opts...)
		if w := e.do("GET", "/api/sessions/s1", "", opts...); w.Code != 403 {
			t.Errorf("%s GET: %d", c.name, w.Code)
		}
		opts = append(opts, withHeader(csrfHeader, "1"), withHeader("Content-Type", "application/json"))
		if w := e.do("POST", "/api/sessions/s1/preview/start", "{}", opts...); w.Code != 403 {
			t.Errorf("%s POST: %d", c.name, w.Code)
		}
	}
	// A preflight is never approved.
	w := e.do("OPTIONS", "/api/sessions/s1/stop", "", host, withHeader("Origin", origin),
		withHeader("Access-Control-Request-Method", "POST"), withHeader("Access-Control-Request-Headers", "x-cerberus"))
	if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Headers") != "" {
		t.Fatalf("preflight approved: %v", w.Header())
	}
	// The app itself still works with its cookie.
	if w := e.do("GET", "/api/sessions/s1", "", withCookie(testToken), host, withHeader("Sec-Fetch-Site", "same-origin")); w.Code != 200 {
		t.Fatalf("own UI: %d", w.Code)
	}
	// And frames preview origins of this host.
	w = e.do("GET", "/", "", withCookie(testToken), host)
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self' http://127.0.0.1:* https://127.0.0.1:*") {
		t.Fatalf("app CSP: %q", csp)
	}
}

// devServer is a tiny dev server: "/" reports what it received, "/ws" is
// a WebSocket echo.
func devServer(t *testing.T) (port int, seen chan http.Header) {
	t.Helper()
	seen = make(chan http.Header, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		h.Set("X-Got-Host", r.Host)
		seen <- h
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "evil", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "app", Value: "1", Path: "/"})
		if r.URL.Path == "/go" {
			http.Redirect(w, r, "http://localhost:"+strconv.Itoa(port)+"/there", http.StatusFound)
			return
		}
		fmt.Fprint(w, "dev page")
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "no upgrade", 400)
			return
		}
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]))
		rw.Flush()
		buf := make([]byte, 64)
		n, _ := rw.Read(buf)
		_, _ = conn.Write(append([]byte("echo:"), buf[:n]...))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return port, seen
}

// TestPreviewProxy: a live server the session owns is proxied at the root
// of its origin with Host/Origin as the dev server expects, without
// Cerberus' cookies either way, and WebSocket upgrades pass through.
func TestPreviewProxy(t *testing.T) {
	e := newPVEnv(t)
	devPort, seen := devServer(t)
	src := "/preview/s1/?src=" + strconv.Itoa(devPort)
	// Not owned yet: no entry.
	if w := e.do("GET", src, "", withBearer(testToken), withHost("127.0.0.1:7340")); w.Code != 404 {
		t.Fatalf("unowned port: %d", w.Code)
	}
	e.pv.own("s1", devPort, true)
	u := e.enter(t, src)
	c := login(t, u)
	base := "http://" + u.Host

	req, _ := http.NewRequest("GET", base+"/", nil)
	req.AddCookie(c)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	req.AddCookie(&http.Cookie{Name: "theme", Value: "x"})
	req.Header.Set("Origin", base)
	req.Header.Set(csrfHeader, "1")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, resp); b != "dev page" {
		t.Fatalf("proxied page: %d %q", resp.StatusCode, b)
	}
	h := <-seen
	want := "localhost:" + strconv.Itoa(devPort)
	if h.Get("X-Got-Host") != want || h.Get("Origin") != "http://"+want {
		t.Fatalf("Host %q Origin %q", h.Get("X-Got-Host"), h.Get("Origin"))
	}
	if ck := h.Get("Cookie"); ck != "theme=x" {
		t.Fatalf("cookies reaching the dev server: %q", ck)
	}
	if h.Get(csrfHeader) != "" {
		t.Fatal("X-Cerberus reached the dev server")
	}
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, cookieName+"=") {
			t.Fatalf("the dev server set Cerberus' cookie: %q", sc)
		}
	}
	if !strings.Contains(strings.Join(resp.Header.Values("Set-Cookie"), "\n"), "app=1") {
		t.Fatal("the dev server's own cookie was dropped")
	}
	// Absolute redirects to localhost:<port> become relative.
	resp = get(t, base+"/go", c)
	resp.Body.Close()
	<-seen
	if loc := resp.Header.Get("Location"); loc != "/there" {
		t.Fatalf("Location: %q", loc)
	}

	// WebSocket round trip through the proxy.
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nOrigin: %s\r\nCookie: %s=%s\r\n\r\n",
		u.Host, base, c.Name, c.Value)
	br := bufio.NewReader(conn)
	wsResp, err := http.ReadResponse(br, nil)
	if err != nil || wsResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", wsResp, err)
	}
	if wsResp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept: %q", wsResp.Header.Get("Sec-WebSocket-Accept"))
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 9)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "echo:ping" {
		t.Fatalf("websocket echo: %q %v", got, err)
	}

	// The session stops owning the port: the proxy refuses (after the
	// short ownership cache), and the sweep frees the lease.
	e.pv.own("s1", 0, false)
	time.Sleep(ownCheckTTL + 100*time.Millisecond)
	if resp := get(t, base+"/", c); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unowned after the fact: %d", resp.StatusCode)
	}
	e.prev.sweep()
	if resp, err := noRedirect.Get(base + "/"); err == nil {
		resp.Body.Close()
		t.Fatalf("lease still served after the sweep: %d", resp.StatusCode)
	}
}

// TestPreviewAPI: GET /api/sessions/{id}/preview, start and stop.
func TestPreviewAPI(t *testing.T) {
	e := newPVEnv(t)
	e.pv.own("s1", 5173, true)
	w := e.do("GET", "/api/sessions/s1/preview", "", withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	b := w.Body.String()
	for _, want := range []string{`"ports":true`, `"url":"/preview/s1/"`, `"dir":"."`, `"port":5173`, `"url":"/preview/s1/?src=5173"`,
		`"source":"detected"`, `"can_start":true`, `"command":"npm run dev -- --port {port}"`} {
		if !strings.Contains(b, want) {
			t.Errorf("reply lacks %s: %s", want, b)
		}
	}
	csrf := withHeader(csrfHeader, "1")
	if w := e.do("POST", "/api/sessions/s1/preview/start", "{}", withBearer(testToken), csrf); w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if len(e.pv.started) != 1 {
		t.Fatalf("started: %v", e.pv.started)
	}
	if w := e.do("POST", "/api/sessions/s1/preview/stop", "{}", withBearer(testToken), csrf); w.Code != 200 {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "/api/sessions/nope/preview", "", withBearer(testToken)); w.Code != 404 {
		t.Fatalf("unknown session: %d", w.Code)
	}
}

// TestPreviewPathMode: without preview ports the static preview keeps its
// capability path and live servers are not offered.
func TestPreviewPathMode(t *testing.T) {
	env := newEnv(t, nil)
	site := t.TempDir()
	_ = os.WriteFile(filepath.Join(site, "index.html"), []byte("hi"), 0o644)
	env.sessions.preview = site
	w := env.do("GET", "/preview/s1/", "", withBearer(testToken))
	if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/preview/s1/~") {
		t.Fatalf("path mode: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := env.do("GET", "/preview/s1/?src=5173", "", withBearer(testToken)); w.Code != 404 {
		t.Fatalf("live without ports: %d", w.Code)
	}
	if w := env.do("GET", "/", "", withBearer(testToken)); strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-src") {
		t.Fatal("frame-src without preview ports")
	}
}

var _ = httptest.NewRecorder
