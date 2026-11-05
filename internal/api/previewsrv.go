package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

// Preview origins (docs/SCOPE.md "As built: work previews").
//
// With server.preview_ports set, every open preview (a session's static
// site, or one live server it owns) gets a port of that range: a lease.
// The port serves that preview at "/", on every host the app listens on,
// so absolute paths and client-side routing work. A preview origin differs
// from the app's (the port differs), so the browser keeps the page away
// from the app's DOM and storage, and the app refuses its requests
// (fromOwnUI: Sec-Fetch-Site same-site, Origin on a preview port).
//
// Auth on a preview origin never uses the app's credentials (they are
// stripped before anything reaches a dev server). The app hands out
// http(s)://<host>:<port>/?cerberus_preview=<cap>, where cap is an HMAC of
// the admin token over (session, source, port, lease nonce); the preview
// server checks it, sets the HttpOnly cookie cerberus_pv_<port> to it and
// redirects without it. A new lease gets a new nonce, so a port handed to
// another session never honours an old cookie or link.

const (
	previewParam   = "cerberus_preview"
	previewCookie  = "cerberus_pv_"
	previewJanitor = 15 * time.Second
	previewTouch   = 5 * time.Second
	ownCheckTTL    = time.Second
)

// PreviewBinder binds one preview port on every host the app listens on
// (TLS where that host serves HTTPS), refusing public addresses as the app
// does. An error means the port is not usable now.
type PreviewBinder func(port int) ([]net.Listener, error)

// Previews owns the preview ports. It is nil when server.preview_ports is
// empty (previews stay under /preview/<id>/).
type Previews struct {
	bind   PreviewBinder
	lo, hi int
	log    *slog.Logger
	srv    atomic.Pointer[server]

	mu     sync.Mutex
	leases map[string]*lease // "<id>|<source>"
	byPort map[int]*lease
	closed bool
}

// lease is one preview origin: a session's static site ("static") or one
// of its live servers (the port number).
type lease struct {
	id, source string
	port       int
	nonce      string
	servers    []*http.Server
	lastUse    atomic.Int64 // unix nanoseconds
	lastTouch  atomic.Int64
	cleared    atomic.Bool // the first entry clears the origin's storage
	proxy      *httputil.ReverseProxy
	ownMu      sync.Mutex
	ownAt      time.Time
	ownLive    session.LiveServer
	ownOK      bool
}

// NewPreviews reads server.preview_ports; nil, nil when it is empty.
func NewPreviews(portRange string, bind PreviewBinder, log *slog.Logger) (*Previews, error) {
	lo, hi, ok, err := config.ParsePortRange(portRange)
	if err != nil || !ok || bind == nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Previews{bind: bind, lo: lo, hi: hi, log: log, leases: map[string]*lease{}, byPort: map[int]*lease{}}, nil
}

// InRange reports whether port is a preview port.
func (p *Previews) InRange(port int) bool { return p != nil && port >= p.lo && port <= p.hi }

// Run frees leases whose session ended, whose source is gone, or that
// nobody used for sessions.preview_idle_min. It returns when ctx ends.
func (p *Previews) Run(ctx context.Context) {
	if p == nil {
		return
	}
	var events <-chan session.Event
	if s := p.srv.Load(); s != nil && s.sessions != nil {
		ch, cancel := s.sessions.Subscribe()
		defer cancel()
		events = ch
	}
	t := time.NewTicker(previewJanitor)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Type == session.EventRemoved || ev.Session.State == session.StateAccepted || ev.Session.State == session.StateDiscarded {
				p.dropSession(ev.Session.ID)
			}
		case <-t.C:
			p.sweep()
		}
	}
}

// Close frees every lease (daemon shutdown).
func (p *Previews) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	all := make([]*lease, 0, len(p.leases))
	for _, l := range p.leases {
		all = append(all, l)
	}
	p.leases, p.byPort = map[string]*lease{}, map[int]*lease{}
	p.mu.Unlock()
	for _, l := range all {
		l.close()
	}
}

func (l *lease) close() {
	for _, srv := range l.servers {
		_ = srv.Close()
	}
}

func (l *lease) used() time.Time { return time.Unix(0, l.lastUse.Load()) }

// acquire returns the lease of (id, source), binding a free port of the
// range for a new one.
func (p *Previews) acquire(id, source string) (*lease, error) {
	s := p.srv.Load()
	if s == nil {
		return nil, errors.New("previews not ready")
	}
	key := id + "|" + source
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("shutting down")
	}
	if l, ok := p.leases[key]; ok {
		l.lastUse.Store(time.Now().UnixNano())
		return l, nil
	}
	for port := p.lo; port <= p.hi; port++ {
		if _, busy := p.byPort[port]; busy {
			continue
		}
		lns, err := p.bind(port)
		if err != nil {
			continue
		}
		l := &lease{id: id, source: source, port: port, nonce: randHex(12)}
		l.lastUse.Store(time.Now().UnixNano())
		if source != "static" {
			l.proxy = s.devProxy(l)
		}
		h := s.withHeadersPreview(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.servePreviewOrigin(w, r, l) }))
		for _, ln := range lns {
			srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
				ErrorLog: slogLogger(p.log)}
			l.servers = append(l.servers, srv)
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					p.log.Warn("preview server", "port", port, "err", err)
				}
			}()
		}
		p.leases[key], p.byPort[port] = l, l
		p.log.Info("preview origin opened", "session", id, "source", source, "port", port)
		return l, nil
	}
	return nil, fmt.Errorf("every preview port (%d-%d) is in use", p.lo, p.hi)
}

// current reports whether l still holds its port.
func (p *Previews) current(l *lease) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byPort[l.port] == l
}

// drop frees the leases matching keep == false.
func (p *Previews) drop(match func(*lease) bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	var gone []*lease
	for key, l := range p.leases {
		if match(l) {
			gone = append(gone, l)
			delete(p.leases, key)
			delete(p.byPort, l.port)
		}
	}
	p.mu.Unlock()
	for _, l := range gone {
		p.log.Info("preview origin closed", "session", l.id, "source", l.source, "port", l.port)
		l.close()
	}
}

// dropSession frees every lease of session id.
func (p *Previews) dropSession(id string) { p.drop(func(l *lease) bool { return l.id == id }) }

// dropSource frees one lease.
func (p *Previews) dropSource(id, source string) {
	p.drop(func(l *lease) bool { return l.id == id && l.source == source })
}

// sweep frees leases that ended, lost their source or sat idle.
func (p *Previews) sweep() {
	s := p.srv.Load()
	if s == nil {
		return
	}
	idle := time.Duration(s.conf().Sessions.PreviewIdleMin) * time.Minute
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	pv, _ := s.sessions.(previewService)
	p.drop(func(l *lease) bool {
		if time.Since(l.used()) > idle {
			return true
		}
		info, ok := s.sessions.Get(l.id)
		if !ok || info.State == session.StateAccepted || info.State == session.StateDiscarded {
			return true
		}
		if l.source == "static" {
			_, ok := s.sessions.PreviewDir(l.id)
			return !ok
		}
		if pv == nil {
			return true
		}
		port, _ := strconv.Atoi(l.source)
		_, owned := pv.OwnsPort(l.id, port)
		return !owned
	})
}

// cap is the capability for l.
func (s *server) previewOriginCap(l *lease) string {
	tok := s.token()
	if tok == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(tok))
	mac.Write([]byte("cerberus preview origin\x00" + l.id + "\x00" + l.source + "\x00" + strconv.Itoa(l.port) + "\x00" + l.nonce))
	return hex.EncodeToString(mac.Sum(nil))[:40]
}

// previewService is what previews need of the session manager beyond
// sessionService (tests may leave it out).
type previewService interface {
	PreviewLive(id string) []session.LiveServer
	OwnsPort(id string, port int) (session.LiveServer, bool)
	PreviewManaged(id string) (session.ManagedPreview, error)
	StartPreview(ctx context.Context, id string) (session.ManagedPreview, error)
	StopPreview(id string) error
	TouchPreview(id string)
	PreviewDetect() bool
}

// withHeadersPreview sets a preview origin's own headers. Scripts and
// same-origin access are allowed (the page is alone on its origin); the
// page may be framed only by the app.
func (s *server) withHeadersPreview(l *lease, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors "+s.appOrigins(r))
		next.ServeHTTP(w, r)
	})
}

// appOrigins are the app's origins on the host the browser used, for a
// preview's frame-ancestors.
func (s *server) appOrigins(r *http.Request) string {
	host := cspHost(hostOnly(r.Host))
	seen := map[string]bool{}
	var out []string
	add := func(o string) {
		if o != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	for _, a := range append([]string{s.conf().Server.Listen}, s.conf().Server.ExtraListen...) {
		if _, port, err := net.SplitHostPort(a); err == nil && host != "" {
			add("http://" + host + ":" + port)
			add("https://" + host + ":" + port)
		}
	}
	if u, err := url.Parse(s.conf().Server.PublicURL); err == nil && u.Host != "" && cspHost(u.Hostname()) != "" {
		add(u.Scheme + "://" + u.Host)
	}
	if len(out) == 0 {
		return "'none'"
	}
	return strings.Join(out, " ")
}

// cspHost is host bracketed for IPv6, or "" when it holds anything a CSP
// source must not.
func cspHost(h string) string {
	if h == "" {
		return ""
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == ':') {
			return ""
		}
	}
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

// servePreviewOrigin answers one request on a preview port.
func (s *server) servePreviewOrigin(w http.ResponseWriter, r *http.Request, l *lease) {
	p := s.previews
	if !s.allowedHost(r.Host) {
		http.Error(w, "unknown Host header", http.StatusForbidden)
		return
	}
	if p == nil || !p.current(l) {
		http.Error(w, "this preview has closed: open it again from Cerberus", http.StatusGone)
		return
	}
	// A service worker would outlive the lease and answer for whatever
	// preview gets this port next.
	if r.Header.Get("Service-Worker") != "" {
		http.Error(w, "service workers are not allowed on preview origins", http.StatusForbidden)
		return
	}
	want := s.previewOriginCap(l)
	if q := r.URL.Query(); q.Has(previewParam) {
		if want == "" || subtle.ConstantTimeCompare([]byte(q.Get(previewParam)), []byte(want)) != 1 {
			previewDenied(w, http.StatusForbidden, "This preview link is not valid any more. Open the preview again from Cerberus.")
			return
		}
		q.Del(previewParam)
		http.SetCookie(w, &http.Cookie{Name: previewCookie + strconv.Itoa(l.port), Value: want, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
		if l.cleared.CompareAndSwap(false, true) {
			// A new lease: nothing a previous preview on this port stored
			// (storage, caches) may leak into this one. Cookies stay: they
			// are per host, so clearing them would sign the app out.
			w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
		}
		target := (&url.URL{Path: r.URL.Path, RawQuery: q.Encode()}).String()
		if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
			target = "/"
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	c, err := r.Cookie(previewCookie + strconv.Itoa(l.port))
	if err != nil || want == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) != 1 {
		previewDenied(w, http.StatusForbidden, "Open this preview from its session in Cerberus.")
		return
	}
	if s.sessions == nil {
		http.NotFound(w, r)
		return
	}
	info, ok := s.sessions.Get(l.id)
	if !ok || info.State == session.StateAccepted || info.State == session.StateDiscarded {
		http.Error(w, "this session has ended", http.StatusGone)
		return
	}
	now := time.Now()
	l.lastUse.Store(now.UnixNano())
	pv, _ := s.sessions.(previewService)
	if pv != nil && now.Sub(time.Unix(0, l.lastTouch.Load())) > previewTouch {
		l.lastTouch.Store(now.UnixNano())
		pv.TouchPreview(l.id)
	}
	if l.source == "static" {
		dir, ok := s.sessions.PreviewDir(l.id)
		if !ok {
			http.Error(w, "this session has no static preview now", http.StatusNotFound)
			return
		}
		serveStatic(w, r, dir, r.URL.Path)
		return
	}
	if pv == nil || !l.owned(pv) {
		http.Error(w, "the session no longer runs a server on this port", http.StatusBadGateway)
		return
	}
	l.proxy.ServeHTTP(w, r)
}

// owned re-checks (at most once per ownCheckTTL) that the session still
// owns the live server behind l.
func (l *lease) owned(pv previewService) bool {
	l.ownMu.Lock()
	defer l.ownMu.Unlock()
	if time.Since(l.ownAt) < ownCheckTTL {
		return l.ownOK
	}
	port, _ := strconv.Atoi(l.source)
	l.ownLive, l.ownOK = pv.OwnsPort(l.id, port)
	l.ownAt = time.Now()
	return l.ownOK
}

func (l *lease) target() string {
	l.ownMu.Lock()
	v6 := l.ownLive.V6
	l.ownMu.Unlock()
	if v6 {
		return "[::1]:" + l.source
	}
	return "127.0.0.1:" + l.source
}

// previewDenied is a small themed-less refusal page.
func previewDenied(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><meta name=viewport content='width=device-width'><title>Preview</title>"+
		"<body style='font:16px system-ui;background:#0b0b0c;color:#e8e2d0;padding:24px'><p>%s</p>", msg)
}

// serveStatic serves dir at the origin's root: os.Root keeps every open
// inside the folder, and dot segments are never served or listed.
func serveStatic(w http.ResponseWriter, r *http.Request, dir, p string) {
	rel := path.Clean("/" + p)
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
	if strings.HasSuffix(p, "/") && rel != "/" {
		r2.URL.Path += "/"
	}
	r2.URL.RawPath = ""
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.FileServerFS(noDotFS{root.FS()}).ServeHTTP(w, r2)
}

// devProxy is the reverse proxy from l's origin to the session's server on
// loopback. The dev server sees Host and Origin as localhost:<port> (Vite
// and friends check them) and never gets Cerberus' cookies or headers;
// its replies cannot set them either. WebSocket upgrades pass through.
func (s *server) devProxy(l *lease) *httputil.ReverseProxy {
	local := "localhost:" + l.source
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: l.target()})
			pr.Out.Host = local
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", "http://"+local)
			}
			if ref := pr.In.Header.Get("Referer"); ref != "" {
				if u, err := url.Parse(ref); err == nil {
					u.Scheme, u.Host = "http", local
					pr.Out.Header.Set("Referer", u.String())
				}
			}
			stripCerberus(pr.Out.Header)
			if tok := bearer(pr.Out); tok != "" {
				if _, ok := s.identify(tok); ok || s.validAgentToken(tok) {
					pr.Out.Header.Del("Authorization")
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			keep := resp.Header.Values("Set-Cookie")
			resp.Header.Del("Set-Cookie")
			for _, c := range keep {
				name, _, _ := strings.Cut(strings.TrimSpace(c), "=")
				if !cerberusCookie(strings.TrimSpace(name)) {
					resp.Header.Add("Set-Cookie", c)
				}
			}
			if loc := resp.Header.Get("Location"); loc != "" {
				if u, err := url.Parse(loc); err == nil && u.IsAbs() && (u.Host == local || u.Host == "127.0.0.1:"+l.source) {
					u.Scheme, u.Host = "", ""
					resp.Header.Set("Location", u.String())
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "the preview server did not answer: "+err.Error(), http.StatusBadGateway)
		},
		ErrorLog: slogLogger(s.log),
	}
}

// cerberusCookie reports whether a cookie name is one of Cerberus' own.
func cerberusCookie(name string) bool {
	return name == cookieName || strings.HasPrefix(name, previewCookie)
}

// stripCerberus removes Cerberus' cookies and headers from a request going
// to a dev server.
func stripCerberus(h http.Header) {
	if raw := h.Values("Cookie"); len(raw) > 0 {
		var keep []string
		for _, line := range raw {
			for part := range strings.SplitSeq(line, ";") {
				part = strings.TrimSpace(part)
				name, _, _ := strings.Cut(part, "=")
				if part != "" && !cerberusCookie(name) {
					keep = append(keep, part)
				}
			}
		}
		h.Del("Cookie")
		if len(keep) > 0 {
			h.Set("Cookie", strings.Join(keep, "; "))
		}
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-cerberus") {
			h.Del(k)
		}
	}
	h.Del("X-Forwarded-For")
	h.Del("Forwarded")
}

// slogLogger routes net/http's error log to slog at debug level.
func slogLogger(l *slog.Logger) *log.Logger { return slog.NewLogLogger(l.Handler(), slog.LevelDebug) }
