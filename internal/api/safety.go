package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

// Safety (day phase 3): who a request acts as, the auth-failure rate limit,
// the per-actor stream cap, revocation of live requests, the Host header
// check and the audit middleware. The handlers for /api/audit,
// /api/devices, /api/token/rotate and /api/doctor are in security.go.

// Principal kinds.
const (
	kindAdmin       = "admin"        // the admin token (Server.Token)
	kindDevice      = "device"       // a paired device token
	kindAgent       = "agent"        // a per-session agent token
	kindAgentShared = "agent-shared" // the shared /v1-only agent token
	kindLocal       = "local"        // /v1 from loopback with LocalNoAuth
	kindClient      = "client"       // an MCP client token (devices.ScopeMCP): /api/agent/* only
	kindLogin       = "login"        // a password sign-in cookie ("cls-…")
	kindUserKey     = "user-key"     // a personal /v1 key ("cuk-…"): /v1 only
)

// principal is who an admitted request acts as.
type principal struct {
	Actor  string // audit actor: owner:<device> | user:<name> | agent:<session> | cli
	Kind   string
	Device string // device id (kindDevice, kindClient)
	Cookie bool   // authenticated by the cookie

	// Users and roles (Phase 4a).
	User  string // resolved owner id ("" only before users exist)
	Name  string // user name for the audit log ("" for the admin token)
	Role  string // admin | user | viewer | monitor | chat
	Login string // login id, for kindLogin
	Must  bool   // the user must change their password first
}

// owner reports whether the principal is a person (admin token, device or
// signed-in user).
func (p principal) owner() bool {
	return p.Kind == kindAdmin || p.Kind == kindDevice || p.Kind == kindLogin
}

// admin reports whether the principal acts with the admin role.
func (p principal) admin() bool { return p.Role == "admin" }

type principalKey struct{}

// principalOf is the principal withAuth admitted r as (zero for public paths).
func principalOf(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

const (
	maxStreamsPerActor = 32
	failBurst          = 5                // failures allowed …
	failWindow         = time.Minute      // … per this window
	blockFor           = 10 * time.Minute // then 429 for this long
	maxLimiterEntries  = 4096
	seenCreds          = 8 // a repeated bad credential is counted once
)

// admit runs next as pr: the context carries the principal and is
// cancelled when the credential is revoked (device revocation, admin token
// rotation), so live SSE streams end at once; an SSE route counts against
// the actor's stream cap.
func (s *server) admit(w http.ResponseWriter, r *http.Request, next http.Handler, pr principal) {
	ctx, cancel := context.WithCancel(context.WithValue(r.Context(), principalKey{}, pr))
	defer cancel()
	if keys := revokeKeys(pr); len(keys) > 0 {
		// The credential was checked before tracking: a revocation that
		// cut a key in between found nothing to cancel, so refuse here.
		since, ok := r.Context().Value(epochKey{}).(uint64)
		if !ok {
			since = s.conns.epoch()
		}
		release, live := s.conns.trackAll(keys, cancel, since)
		if !live {
			writeError(w, http.StatusUnauthorized, "credential revoked")
			return
		}
		defer release()
	}
	if r.Method == http.MethodGet && isStreamPath(path.Clean("/"+r.URL.Path)) {
		if !s.conns.acquire(pr.Actor) {
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusTooManyRequests, "too many open streams for "+pr.Actor)
			return
		}
		defer s.conns.release(pr.Actor)
	}
	if (pr.Kind == kindDevice || pr.Kind == kindClient) && s.devices != nil {
		s.devices.Touch(pr.Device, limitHost(r.RemoteAddr))
	}
	if pr.Kind == kindLogin && s.users != nil {
		s.users.Logins.Touch(pr.Login, limitHost(r.RemoteAddr))
	}
	next.ServeHTTP(w, r.WithContext(ctx))
}

// revokeKeys group the requests that one revocation cuts: a cut on any of
// a request's keys ends it.
func revokeKeys(pr principal) []string {
	var keys []string
	switch pr.Kind {
	case kindAdmin:
		keys = append(keys, "admin")
	case kindDevice, kindClient:
		keys = append(keys, "device:"+pr.Device)
	case kindLogin:
		keys = append(keys, "login:"+pr.Login)
	}
	// A session's agent is not cut with its owner's sign-ins: a role
	// change or password reset leaves running sessions alone, and a
	// disable or delete stops them (stopUserSessions).
	if pr.User != "" && pr.Kind != kindAdmin && pr.Kind != kindLocal && pr.Kind != kindAgentShared && pr.Kind != kindAgent {
		keys = append(keys, "user:"+pr.User)
	}
	return keys
}

// isStreamPath reports whether p is one of the SSE routes.
func isStreamPath(p string) bool {
	return p == "/api/events" || (strings.HasPrefix(p, "/api/") && strings.HasSuffix(p, "/stream"))
}

// ---------- live requests: revocation and the stream cap ----------

type connTracker struct {
	mu      sync.Mutex
	cancels map[string]map[int]context.CancelFunc
	next    int
	streams map[string]int
	gen     uint64            // bumped by every cut
	cutAt   map[string]uint64 // key -> gen of its last cut
}

// epochKey carries the tracker epoch read before a request's credential
// was checked (withAuth), so admit can tell a revocation in between.
type epochKey struct{}

func newConnTracker() *connTracker {
	return &connTracker{cancels: map[string]map[int]context.CancelFunc{}, streams: map[string]int{},
		cutAt: map[string]uint64{}}
}

// epoch is the current cut generation; read it before checking a
// credential and pass it to track.
func (c *connTracker) epoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// track registers cancel under key and returns its release. live is false
// (and nothing is registered) when key was cut after epoch since, i.e.
// after the caller checked its credential.
func (c *connTracker) track(key string, cancel context.CancelFunc, since uint64) (release func(), live bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cutAt[key] > since {
		return func() {}, false
	}
	c.next++
	id := c.next
	if c.cancels[key] == nil {
		c.cancels[key] = map[int]context.CancelFunc{}
	}
	c.cancels[key][id] = cancel
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.cancels[key], id)
		if len(c.cancels[key]) == 0 {
			delete(c.cancels, key)
		}
	}, true
}

// trackAll is track under several keys at once: refused when any key was
// cut after since; release drops every registration.
func (c *connTracker) trackAll(keys []string, cancel context.CancelFunc, since uint64) (release func(), live bool) {
	c.mu.Lock()
	for _, k := range keys {
		if c.cutAt[k] > since {
			c.mu.Unlock()
			return func() {}, false
		}
	}
	c.mu.Unlock()
	var rels []func()
	for _, k := range keys {
		rel, ok := c.track(k, cancel, since)
		if !ok {
			for _, r := range rels {
				r()
			}
			return func() {}, false
		}
		rels = append(rels, rel)
	}
	return func() {
		for _, r := range rels {
			r()
		}
	}, true
}

// cut cancels every live request under key and returns how many; requests
// whose credential was checked before the cut but not yet tracked are
// refused by track.
func (c *connTracker) cut(key string) int {
	c.mu.Lock()
	fns := c.cancels[key]
	delete(c.cancels, key)
	c.gen++
	c.cutAt[key] = c.gen
	c.mu.Unlock()
	for _, f := range fns {
		f()
	}
	return len(fns)
}

func (c *connTracker) acquire(actor string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams[actor] >= maxStreamsPerActor {
		return false
	}
	c.streams[actor]++
	return true
}

func (c *connTracker) release(actor string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams[actor]--; c.streams[actor] <= 0 {
		delete(c.streams, actor)
	}
}

// ---------- auth-failure rate limit ----------

// failLimiter is a token bucket per remote address (an IPv6 /64 counts as
// one): failBurst failures per failWindow, then every non-public request
// from that address gets 429 for blockFor. The same bad credential sent
// again (a browser holding a stale cookie after a rotation) is counted
// once, so only guessing uses the budget up.
type failLimiter struct {
	mu  sync.Mutex
	m   map[string]*bucket
	now func() time.Time
}

type bucket struct {
	tokens   float64
	last     time.Time
	until    time.Time
	seen     []string
	inflight int // password sign-ins between begin and end
}

func newFailLimiter() *failLimiter {
	return &failLimiter{m: map[string]*bucket{}, now: time.Now}
}

// blocked reports whether key is in its 429 period, and for how long.
func (l *failLimiter) blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		return 0, false
	}
	if now := l.now(); now.Before(b.until) {
		return b.until.Sub(now), true
	}
	return 0, false
}

// begin reserves a password sign-in from key: false (with the wait) while
// key is blocked, or while the sign-ins already in flight could use up what
// is left of its budget, so concurrent guesses cannot all slip past the
// check before the first failure is counted. A true begin needs an end.
func (l *failLimiter) begin(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= maxLimiterEntries {
			l.pruneLocked(now)
		}
		b = &bucket{tokens: failBurst, last: now}
		l.m[key] = b
	}
	if now.Before(b.until) {
		return b.until.Sub(now), false
	}
	tokens := min(failBurst, b.tokens+now.Sub(b.last).Seconds()*failBurst/failWindow.Seconds())
	if float64(b.inflight) > tokens {
		return time.Second, false
	}
	b.inflight++
	return 0, true
}

// end releases a begin.
func (l *failLimiter) end(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.m[key]; b != nil && b.inflight > 0 {
		b.inflight--
	}
}

// fail records a failed credential from key. counted is false for a
// repeat; block is true when this failure starts the 429 period.
func (l *failLimiter) fail(key, cred string) (counted, block bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= maxLimiterEntries {
			l.pruneLocked(now)
		}
		b = &bucket{tokens: failBurst, last: now}
		l.m[key] = b
	}
	b.tokens = min(failBurst, b.tokens+now.Sub(b.last).Seconds()*failBurst/failWindow.Seconds())
	b.last = now
	sum := sha256.Sum256([]byte(cred))
	h := hex.EncodeToString(sum[:8])
	for _, s := range b.seen {
		if s == h {
			return false, false
		}
	}
	b.seen = append(b.seen, h)
	if len(b.seen) > seenCreds {
		b.seen = b.seen[1:]
	}
	b.tokens--
	if b.tokens < 0 {
		b.tokens = 0
		b.until = now.Add(blockFor)
		b.seen = nil
		return true, true
	}
	return true, false
}

// pruneLocked drops idle entries (full bucket, not blocked); if that frees
// nothing, the oldest half goes.
func (l *failLimiter) pruneLocked(now time.Time) {
	for k, b := range l.m {
		refill := b.tokens + now.Sub(b.last).Seconds()*failBurst/failWindow.Seconds()
		if refill >= failBurst && !now.Before(b.until) && b.inflight == 0 {
			delete(l.m, k)
		}
	}
	if len(l.m) < maxLimiterEntries {
		return
	}
	n := 0
	for k, b := range l.m {
		if !now.Before(b.until) {
			delete(l.m, k)
			if n++; n >= maxLimiterEntries/2 {
				return
			}
		}
	}
}

// limitHost is the peer's IP (X-Forwarded-For is never trusted).
func limitHost(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}

// limitKey is the rate-limit key of a peer: its IPv4 address, or its IPv6
// /64 (one host usually owns a whole /64).
func limitKey(remoteAddr string) string {
	h := limitHost(remoteAddr)
	ip := net.ParseIP(h)
	if ip == nil || ip.To4() != nil {
		return h
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// presented is the credential a failed request carried ("" if none).
func presented(r *http.Request) (kind, value string) {
	if b := bearer(r); b != "" {
		return "bearer token", b
	}
	if r.Method == http.MethodGet && r.URL.Query().Has("token") {
		return "?token", r.URL.Query().Get("token")
	}
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return "cookie", c.Value
	}
	return "", ""
}

// authFailed counts and audits a request that presented a bad credential.
// While the address is blocked it answers 429 for any such request. A
// request with no credential at all (a browser opening the UI) is not a
// failure. It answers 429 and returns true when this failure starts the
// block; otherwise the caller answers 401 as usual. A bad cookie is also
// cleared, so the browser stops sending it.
func (s *server) authFailed(w http.ResponseWriter, r *http.Request, p string) bool {
	if wait, blocked := s.limiter.blocked(limitKey(r.RemoteAddr)); blocked {
		s.tooMany(w, p, wait)
		return true
	}
	kind, cred := presented(r)
	if cred == "" {
		return false
	}
	if kind == "?token" && crossSite(r) {
		return false // a web page's <img src=…?token=…>: refused, not counted
	}
	if s.validAgentToken(cred) {
		return false // a real credential on the wrong scope, not a guess
	}
	if _, ok := s.verifySessionToken(cred); ok {
		return false
	}
	if _, ok := s.verifyOldSessionToken(cred); ok {
		return false
	}
	if kind == "cookie" {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	}
	key := limitKey(r.RemoteAddr)
	counted, block := s.limiter.fail(key, cred)
	if !counted {
		return false
	}
	route := s.routePattern(r)
	status := http.StatusUnauthorized
	if block {
		status = http.StatusTooManyRequests
	}
	s.auditLog(audit.Record{Actor: audit.ActorSystem, Remote: limitHost(r.RemoteAddr), Method: r.Method,
		Route: "auth.failure", Targets: []string{route}, Status: status, Detail: "invalid " + kind})
	if block {
		s.auditLog(audit.Record{Actor: audit.ActorSystem, Remote: limitHost(r.RemoteAddr), Route: "auth.blocked",
			Status: status, Detail: "too many failed logins from " + key + "; refused for 10 min"})
		s.log.Warn("auth failures: address blocked", "addr", key, "for", blockFor)
		s.tooMany(w, p, blockFor)
		return true
	}
	return false
}

// crossSite reports whether the browser marked r as sent by another site
// (Sec-Fetch-Site cross-site or same-site). Clients without the header
// (curl, older browsers) are not cross-site.
func crossSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return true
	}
	return false
}

// tooMany answers a blocked address.
func (s *server) tooMany(w http.ResponseWriter, p string, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	if strings.HasPrefix(p, "/v1/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"too many failed authentication attempts; try again later","type":"rate_limit_error"}}`))
		return
	}
	writeError(w, http.StatusTooManyRequests, "too many failed authentication attempts; try again later")
}

// routePattern is the mux pattern that would serve r (never the raw URL).
func (s *server) routePattern(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	if s.mux != nil {
		if _, pat := s.mux.Handler(r); pat != "" {
			return pat
		}
	}
	return r.Method + " (unmatched)"
}

// ---------- tokens and rotation ----------

// token is the current admin token.
func (s *server) token() string {
	s.tokMu.RLock()
	defer s.tokMu.RUnlock()
	return s.cfg.TokenSnapshot()
}

// tokenConfig is a config holding only the current token, for the
// config package's token derivations without racing a rotation.
func (s *server) tokenConfig() *config.Config {
	return &config.Config{Server: config.ServerConfig{Token: s.token()}}
}

func (s *server) verifySessionToken(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	return s.tokenConfig().VerifySessionAgentToken(tok)
}

// prevToken is an admin token this process rotated away, with the
// sessions that were live at that moment: only they keep /v1 with session
// tokens derived from it, and each loses it for good once it ends.
type prevToken struct {
	tok  string
	live map[string]bool
}

// verifyOldSessionToken accepts a session agent token derived from an
// admin token this process rotated away, for a session that was live at
// the rotation and has not ended since: rotation re-derives agent tokens,
// and live agents get the new one at their next spawn. A session that
// ended (even if later resumed) is dropped for good.
func (s *server) verifyOldSessionToken(tok string) (string, bool) {
	if tok == "" || !strings.HasPrefix(tok, "cas-") || s.sessions == nil {
		return "", false
	}
	s.tokMu.RLock()
	prev := append([]prevToken(nil), s.prevTokens...)
	s.tokMu.RUnlock()
	for _, old := range prev {
		c := &config.Config{Server: config.ServerConfig{Token: old.tok}}
		id, ok := c.VerifySessionAgentToken(tok)
		if !ok {
			continue
		}
		s.tokMu.Lock()
		live := old.live[id]
		s.tokMu.Unlock()
		if !live {
			return "", false
		}
		info, found := s.sessions.Get(id)
		if !found || sessionEnded(info.State) {
			s.tokMu.Lock()
			delete(old.live, id)
			s.tokMu.Unlock()
			return "", false
		}
		return id, true
	}
	return "", false
}

// sessionEnded reports whether a session's agent has finished for good.
func sessionEnded(state string) bool {
	switch state {
	case session.StateAccepted, session.StateDiscarded, session.StateStopped, session.StateError,
		session.StatePassed, session.StateFailed:
		return true
	}
	return false
}

// maxPrevTokens bounds the rotated-away tokens kept for live agents.
const maxPrevTokens = 8

// rotateToken replaces the admin token, saves it to config.json (only
// the token fields, re-read from disk) and cuts every live request made
// with the old token. The live token changes only after the file is
// written, so a failed save leaves everything as it was. It refuses a
// config with no file (it would otherwise be written to the default path).
func (s *server) rotateToken() (string, error) {
	if s.cfg.Path() == "" {
		return "", errNoConfigFile
	}
	live := map[string]bool{}
	if s.sessions != nil {
		for _, info := range s.sessions.List() {
			if !sessionEnded(info.State) {
				live[info.ID] = true
			}
		}
	}
	tok := config.NewToken()
	s.cfgMu.Lock() // a settings save must not write the old token back
	defer s.cfgMu.Unlock()
	s.tokMu.Lock()
	old := s.cfg.TokenSnapshot()
	if err := s.cfg.RotateToken(tok); err != nil {
		s.tokMu.Unlock()
		return "", err
	}
	if old != "" {
		s.prevTokens = append(s.prevTokens, prevToken{tok: old, live: live})
		if len(s.prevTokens) > maxPrevTokens {
			s.prevTokens = s.prevTokens[len(s.prevTokens)-maxPrevTokens:]
		}
	}
	s.tokMu.Unlock()
	s.noteDisk()
	s.afterRotate()
	s.conns.cut("admin")
	return tok, nil
}

// ---------- Host header check ----------

// withHost refuses a request whose Host header names none of this server's
// addresses: a listen address, the PublicURL host, localhost (and loopback
// IPs), or a Server.AllowedHosts entry ("name", "name:port" or "*.suffix").
// When a listen address is a wildcard, any IP literal is accepted too: DNS
// rebinding needs a host name, so an IP Host cannot be a rebinding attack.
func (s *server) withHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			msg := "unknown Host header " + strconvQuote(r.Host) + "; add it to server.allowed_hosts if this is your name for this machine"
			if strings.HasPrefix(r.URL.Path, "/v1/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"message":"unknown Host header","type":"forbidden"}}`))
				return
			}
			writeError(w, http.StatusForbidden, msg)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func strconvQuote(s string) string {
	if len(s) > 100 {
		s = s[:100]
	}
	return strconv.Quote(s)
}

func (s *server) allowedHost(host string) bool {
	h := hostOnly(host)
	if h == "" {
		return false
	}
	if s.ownHost(host) {
		return true
	}
	lh := strings.ToLower(strings.TrimSuffix(h, "."))
	for _, a := range s.conf().Server.AllowedHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.Contains(a, "://") {
			if u, err := url.Parse(a); err == nil {
				a = u.Host
			}
		}
		ah := strings.TrimSuffix(hostOnly(a), ".")
		if suffix, ok := strings.CutPrefix(ah, "*."); ok {
			if strings.HasSuffix(lh, "."+suffix) {
				return true
			}
			continue
		}
		if ah == lh {
			return true
		}
	}
	if net.ParseIP(h) != nil {
		for _, l := range append([]string{s.conf().Server.Listen}, s.conf().Server.ExtraListen...) {
			if lhost, _, err := net.SplitHostPort(l); err == nil && doctor.Wildcard(lhost) {
				return true
			}
		}
	}
	return false
}

// ---------- audit ----------

// auditNote lets a handler add a short detail and extra targets to its
// request's audit record (never a body: the audit log masks and clips).
type auditNote struct {
	mu      sync.Mutex
	detail  string
	targets []string
	skip    bool // the handler records this request itself (inputAudit)
}

type noteKey struct{}

// noteAudit sets the audit detail of r (and appends targets).
func noteAudit(r *http.Request, detail string, targets ...string) {
	n, _ := r.Context().Value(noteKey{}).(*auditNote)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if detail != "" {
		n.detail = detail
	}
	n.targets = append(n.targets, targets...)
}

// skipAudit tells withAudit not to record r: the handler has handed its
// record to someone else (terminal input goes to inputAudit).
func skipAudit(r *http.Request) {
	if n, _ := r.Context().Value(noteKey{}).(*auditNote); n != nil {
		n.mu.Lock()
		n.skip = true
		n.mu.Unlock()
	}
}

// unaudited are writes that change nothing worth a record: the usage
// "someone is looking" switch. The gateway (/v1) is recorded by usage.
func unaudited(r *http.Request) bool {
	p := path.Clean("/" + r.URL.Path)
	return strings.HasPrefix(p, "/v1/") || p == "/v1" || p == "/api/usage/watch"
}

// withAudit records every state-changing request after it ran: actor,
// remote address, method, route pattern, the pattern's path values as
// targets, the status and the handler's note. It sits right outside the
// mux, which sets r.Pattern.
func (s *server) withAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) || s.audit == nil || unaudited(r) {
			next.ServeHTTP(w, r)
			return
		}
		note := &auditNote{}
		r = r.WithContext(context.WithValue(r.Context(), noteKey{}, note))
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		pr := principalOf(r)
		route := r.Pattern
		if route == "" {
			route = s.routePattern(r)
		}
		note.mu.Lock()
		targets := patternTargets(r, route)
		for _, t := range note.targets { // a handler may repeat a path value
			if !slices.Contains(targets, t) {
				targets = append(targets, t)
			}
		}
		detail, skip := note.detail, note.skip
		note.mu.Unlock()
		if skip {
			return
		}
		s.auditLog(audit.Record{Actor: orDefault(pr.Actor, audit.ActorSystem), User: pr.Name, Remote: limitHost(r.RemoteAddr),
			Method: r.Method, Route: route, Targets: targets, Status: sw.code(), Detail: detail})
	})
}

func (s *server) auditLog(rec audit.Record) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Log(rec); err != nil {
		s.log.Warn("audit write failed", "err", err)
	}
}

// patternTargets are the values of the pattern's wildcards, in order.
func patternTargets(r *http.Request, pattern string) []string {
	var out []string
	for rest := pattern; ; {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return out
		}
		name := strings.TrimSuffix(rest[i+1:i+j], "...")
		rest = rest[i+j+1:]
		if name == "$" || name == "" {
			continue
		}
		if v := r.PathValue(name); v != "" {
			out = append(out, v)
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// statusWriter remembers the status a handler sent.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
