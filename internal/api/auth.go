package api

import (
	"context"
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/users"
)

// Authentication rules:
//
//   - "Authorization: Bearer <token>" works everywhere.
//   - The cookie cerberus_token works everywhere. A state-changing request
//     (anything but GET, HEAD and OPTIONS) that relies on the cookie must also
//     send the header "X-Cerberus: 1". Browsers cannot add a custom header to
//     a cross-site request without a CORS preflight (which Cerberus never
//     approves), so this blocks CSRF even where SameSite is not honoured.
//   - "?token=<token>" on a GET sets the cookie and redirects to the same URL
//     without the token, so it never stays in the address bar or history.
//   - /v1/* from a loopback peer needs nothing when Server.LocalNoAuth is set,
//     provided the request does not look like it came from a web page: no
//     Origin header, a Host that is loopback or one of our own names (DNS
//     rebinding), and a JSON Content-Type on writes (a page's no-cors "simple"
//     POST cannot send one). The peer is r.RemoteAddr only; X-Forwarded-For
//     is never trusted.
//   - Agent tokens work for /v1/* and never for the control API. There are
//     two: the shared Config.AgentToken() (older sessions) and the
//     per-session Config.SessionAgentToken(id), which the session manager
//     now gives each agent as OPENAI_API_KEY. On /v1 a session token also
//     pins X-Cerberus-Session to its own session, so usage cannot be
//     charged to another one.
//   - /api/agent/* is the restricted agent scope (agentscope.go): it takes
//     a session agent token, together with an X-Cerberus-Session header
//     naming the same session, which must exist, or an MCP client token
//     ("ccl-…", a device paired with scope "mcp" for an outside agent such
//     as Claude Code), which sends no session header and sees only the
//     sessions it started. The admin token, the cookie, full device tokens
//     and the shared agent token are all refused there, so a person's
//     credentials never act as an agent and an agent never acts as a person;
//     an MCP client token is refused everywhere else.
//   - A cookie-authenticated /api request is refused when it comes from
//     another site or from a preview page (Sec-Fetch-Site, Referer): previews
//     are agent-written pages served from our origin.
//   - /preview/<id>/~<cap>/… needs only its capability (see preview.go).
//   - /healthz, /login, /assets/* and the PWA files (/manifest.webmanifest,
//     /sw.js) are public.
//
// Safety (day phase 3, safety.go):
//
//   - A paired device's token ("cdv-…", subpackage devices) works wherever
//     the admin token does, as bearer, cookie or ?token=, except pairing
//     more devices; revoking it cancels its live requests and streams.
//   - Rotating the admin token cancels every live request made with the
//     old one; session agent tokens derived from it keep /v1 (only) while
//     their session runs.
//   - Every request must carry a Host header naming this server
//     (withHost), and every admitted request carries a principal (who it
//     acts as) for the audit log and the per-actor stream cap.
//   - A bad credential from an address costs one of 5 per minute (the same
//     bad value counts once); past that, requests from the address that do
//     not carry a valid credential get 429 for 10 minutes (a valid admin,
//     device or agent token still works, so nobody sharing the address can
//     lock the owner or the agents out). A request with no credential at
//     all is not counted, nor is a ?token= sent from another site (an
//     <img> on a web page), which a browser cannot be stopped from sending.
const (
	cookieName    = "cerberus_token"
	csrfHeader    = "X-Cerberus"
	sessionHeader = "X-Cerberus-Session"
	actorHeader   = "X-Cerberus-Actor" // "cli" from `cerberus …` commands (admin token only)
	userHeader    = "X-Cerberus-User"  // set by withAuth for /v1: the user a request is charged to
	cookieMaxAge  = 365 * 24 * 3600
)

var memAvailable = platform.MemAvailableMiB

// withAuth enforces the rules above in front of next. Every admitted
// request carries its principal (who it acts as) in the context; see admit.
func (s *server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)
		if publicPath(p) || s.previewCapPath(p) || s.notFoundRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Read before any credential is checked; admit refuses a request
		// whose credential was revoked in between (see connTracker.track).
		r = r.WithContext(context.WithValue(r.Context(), epochKey{}, s.conns.epoch()))
		// The auth-failure block is enforced only once the request has
		// failed to present a valid credential (authFailed): a block must
		// never lock out the owner, the CLI or running agents, who all
		// share loopback with whoever caused it.
		v1 := strings.HasPrefix(p, "/v1/")
		// X-Cerberus-User is set here only, for the gateway's attribution.
		r.Header.Del(userHeader)
		if v1 {
			id, ok := s.verifySessionToken(bearer(r))
			if !ok {
				id, ok = s.verifyOldSessionToken(bearer(r))
			}
			if ok {
				r.Header.Set(sessionHeader, id)
				pr := principal{Actor: audit.Agent(id), Kind: kindAgent}
				if !s.bindSessionOwner(&pr, id) {
					s.deny(w, r, p)
					return
				}
				s.admit(w, r, next, pr)
				return
			}
		}
		// A personal key counts for its user even from loopback, before the
		// keyless local rule (which would charge the admin).
		if v1 && s.users != nil && strings.HasPrefix(bearer(r), users.APIKeyPrefix) {
			if u, ok := s.users.VerifyAPIKey(bearer(r)); ok {
				s.admit(w, r, next, principal{Actor: audit.User(u.Name), Kind: kindUserKey, User: u.ID, Name: u.Name,
					Role: u.Role, Must: u.MustChange})
				return
			}
		}
		if v1 && s.conf().Server.LocalNoAuth && isLoopback(r.RemoteAddr) && s.localClient(r) {
			// A confined session or user chat shares the host network and
			// so loopback: while one runs, a keyless local request could be
			// it, acting as the admin outside its owner's budget.
			// A request with a credential is then judged on it (below).
			if !s.confinedActive() {
				s.admit(w, r, next, principal{Actor: audit.Owner("local"), Kind: kindLocal, Role: users.RoleAdmin, User: s.legacyOwner()})
				return
			}
			if bearer(r) == "" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="cerberus"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"a key is required: keyless local access (server.local_no_auth) is paused while another user's sandboxed work runs","type":"authentication_error","code":"local_no_auth_paused"}}`))
				return
			}
		}
		if v1 && s.validAgentToken(bearer(r)) {
			s.admit(w, r, next, principal{Actor: audit.Agent("shared"), Kind: kindAgentShared, Role: users.RoleAdmin, User: s.legacyOwner()})
			return
		}
		if p == "/api/agent" || strings.HasPrefix(p, "/api/agent/") {
			s.agentAuth(w, r, next)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Has("token") {
			s.tokenLogin(w, r, p)
			return
		}
		if pr, ok := s.identify(bearer(r)); ok {
			if pr.Kind == kindAdmin && r.Header.Get(actorHeader) == audit.ActorCLI {
				pr.Actor = audit.ActorCLI
			}
			s.admit(w, r, next, pr)
			return
		}
		if c, err := r.Cookie(cookieName); err == nil {
			if conflictingCookies(r) {
				// A page on another port of this host (a preview) can set
				// a cookie of this name with a narrower path, which the
				// browser sends first: never act as whoever it names.
				writeError(w, http.StatusUnauthorized, "conflicting "+cookieName+" cookies: sign in again (clear this site's cookies if it keeps happening)")
				return
			}
			if pr, ok := s.identify(c.Value); ok {
				if !safeMethod(r.Method) && r.Header.Get(csrfHeader) != "1" {
					writeError(w, http.StatusForbidden, "missing "+csrfHeader+": 1 header")
					return
				}
				if strings.HasPrefix(p, "/api/") && (!fromOwnUI(r) || s.fromPreviewOrigin(r)) {
					writeError(w, http.StatusForbidden, "cross-site or preview request refused")
					return
				}
				pr.Cookie = true
				s.admit(w, r, next, pr)
				return
			}
		}
		s.deny(w, r, p)
	})
}

// confinedActive reports whether code of a non-admin user may be running
// on this machine now: a confined session that has not finished, or a
// running chat of an account whose chats run commands (role user).
func (s *server) confinedActive() bool {
	if !s.hasUsers() {
		return false
	}
	if s.sessions != nil {
		for _, info := range s.sessions.List() {
			if info.Confined && !sessionIdle(info.State) {
				return true
			}
		}
	}
	if s.chats != nil {
		for _, owner := range s.chats.BusyOwners() {
			if u, ok := s.users.Get(s.users.Owner(owner)); !ok || u.Role != users.RoleAdmin {
				return true
			}
		}
	}
	return false
}

// sessionIdle reports whether a session in state st runs no process.
func sessionIdle(st string) bool {
	switch st {
	case session.StateQueued, session.StatePassed, session.StateFailed, session.StateAccepted,
		session.StateDiscarded, session.StateStopped, session.StateError:
		return true
	}
	return false
}

// identify maps a presented token to the admin token, a password sign-in
// ("cls-…") or a paired device, in that order. An MCP client token is not
// one: it acts only in the agent scope (agentAuth). A sign-in or device
// whose user is disabled or removed does not identify.
func (s *server) identify(tok string) (principal, bool) {
	if s.validToken(tok) {
		return s.adminPrincipal(), true
	}
	if s.users != nil && strings.HasPrefix(tok, users.LoginPrefix) {
		lg, ok := s.users.Logins.Verify(tok)
		if !ok {
			return principal{}, false
		}
		u, ok := s.users.Get(lg.User)
		if !ok || u.Disabled {
			return principal{}, false
		}
		return principal{Actor: audit.User(u.Name), Kind: kindLogin, User: u.ID, Name: u.Name, Role: u.Role,
			Login: lg.ID, Must: u.MustChange}, true
	}
	if s.devices != nil {
		if d, ok := s.devices.Verify(tok); ok && d.Scope == "" {
			pr := principal{Actor: audit.Owner(d.Name), Kind: kindDevice, Device: d.ID}
			if !s.bindUser(&pr, d.User) {
				return principal{}, false
			}
			return pr, true
		}
	}
	return principal{}, false
}

// adminPrincipal is the admin token: role admin, acting as the legacy
// owner (the first admin; "" before users exist).
func (s *server) adminPrincipal() principal {
	return principal{Actor: audit.Owner("admin"), Kind: kindAdmin, Role: users.RoleAdmin, User: s.legacyOwner()}
}

// bindUser gives pr the role and user of the owner id raw (a device's or a
// session's owner). Before users exist everyone is admin. It is false
// when users exist and the owner is missing or disabled.
func (s *server) bindUser(pr *principal, raw string) bool {
	if !s.hasUsers() {
		if raw != "" {
			return false // bound to an account while none exist: fail closed
		}
		pr.Role = users.RoleAdmin
		return true
	}
	u, ok := s.users.Get(s.users.Owner(raw))
	if !ok || u.Disabled {
		return false
	}
	pr.User, pr.Name, pr.Role, pr.Must = u.ID, u.Name, u.Role, u.MustChange
	return true
}

// bindSessionOwner binds an agent token's principal to its session's owner.
// A session of a removed user does not bind: its data moved to an admin,
// and its agent must not act with that admin's rights.
func (s *server) bindSessionOwner(pr *principal, id string) bool {
	owner, confined := "", false
	if s.sessions != nil {
		if info, ok := s.sessions.Get(id); ok {
			owner, confined = info.Owner, info.Confined
		}
	}
	if !s.hasUsers() {
		// Single-user mode (no accounts, on purpose: see users.MarkerName):
		// the admin's own sessions keep their agents; a confined one never
		// turns into the admin.
		if confined {
			return false
		}
		pr.Role = users.RoleAdmin
		return true
	}
	if movedOwner(s.users, owner) {
		return false
	}
	return s.bindUser(pr, owner)
}

// notFoundRoute reports whether the mux would serve r with the public
// catch-all "/" (an unknown non-API path: the 404 page).
func (s *server) notFoundRoute(r *http.Request) bool {
	if s.mux == nil || r.URL.Path == "" || path.Clean(r.URL.Path) != r.URL.Path {
		return false // the mux would redirect to the clean path: not ours to answer
	}
	_, pat := s.mux.Handler(r)
	return pat == "/"
}

// agentAuth admits an /api/agent request made with a session agent token
// whose session matches X-Cerberus-Session and still exists (not accepted
// or discarded). The caller's id travels in the request context
// (callerID). Tokens from before a rotation never open this scope.
func (s *server) agentAuth(w http.ResponseWriter, r *http.Request, next http.Handler) {
	tok := bearer(r)
	if s.devices != nil && strings.HasPrefix(tok, devices.ClientPrefix) {
		s.clientAuth(w, r, next, tok)
		return
	}
	id, ok := s.verifySessionToken(tok)
	if !ok {
		if s.validToken(tok) || s.validAgentToken(tok) || s.authenticated(r) {
			writeError(w, http.StatusForbidden, "the agent scope takes a session agent token (OPENAI_API_KEY of a Cerberus session)")
			return
		}
		if _, old := s.verifyOldSessionToken(tok); old {
			writeError(w, http.StatusForbidden, "this agent token predates a token rotation: it works for /v1 only until its session ends")
			return
		}
		if s.authFailed(w, r, path.Clean("/"+r.URL.Path)) {
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="cerberus-agent"`)
		writeError(w, http.StatusUnauthorized, "missing or invalid session agent token")
		return
	}
	if h := r.Header.Get(sessionHeader); h != id {
		writeError(w, http.StatusForbidden, sessionHeader+" must name the session the agent token belongs to")
		return
	}
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	info, ok := s.sessions.Get(id)
	if !ok || info.State == session.StateAccepted || info.State == session.StateDiscarded {
		writeError(w, http.StatusForbidden, "the calling session no longer exists")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), callerKey{}, id))
	pr := principal{Actor: audit.Agent(id), Kind: kindAgent}
	if !s.bindSessionOwner(&pr, id) {
		writeError(w, http.StatusForbidden, "the calling session's owner is disabled or removed")
		return
	}
	s.admit(w, r, next, pr)
}

// clientAuth admits an /api/agent request made with an MCP client token.
// The client is a root caller: it names no session, and the agent scope
// shows it only the sessions it started (ClientSessions).
func (s *server) clientAuth(w http.ResponseWriter, r *http.Request, next http.Handler, tok string) {
	d, ok := s.devices.Verify(tok)
	if !ok || d.Scope != devices.ScopeMCP {
		if s.authFailed(w, r, path.Clean("/"+r.URL.Path)) {
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="cerberus-agent"`)
		writeError(w, http.StatusUnauthorized, "missing or invalid MCP client token")
		return
	}
	if r.Header.Get(sessionHeader) != "" {
		writeError(w, http.StatusForbidden, "an MCP client token acts as itself: send no "+sessionHeader)
		return
	}
	pr := principal{Actor: audit.Client(d.Name), Kind: kindClient, Device: d.ID}
	if !s.bindUser(&pr, d.User) {
		writeError(w, http.StatusForbidden, "this MCP client's user is disabled or removed")
		return
	}
	if !users.Can(pr.Role, users.CapSessions) {
		roleError(w, pr.Role)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), clientKey{}, clientName(d.Name, d.ID)))
	s.admit(w, r, next, pr)
}

// clientName is the Client recorded on the sessions an MCP client starts.
// It carries the device id: names are reused after a revoke (the default is
// "mcp-client"), and a new client must not inherit an old one's sessions.
func clientName(name, id string) string { return "mcp:" + name + "#" + id }

// clientKey carries the calling MCP client ("mcp:<name>#<id>") on /api/agent
// requests made with a client token.
type clientKey struct{}

// clientID is the calling MCP client of an /api/agent request ("" elsewhere).
func clientID(r *http.Request) string {
	id, _ := r.Context().Value(clientKey{}).(string)
	return id
}

// callerKey carries the calling session's id on /api/agent requests.
type callerKey struct{}

// callerID is the calling session of an /api/agent request ("" elsewhere).
func callerID(r *http.Request) string {
	id, _ := r.Context().Value(callerKey{}).(string)
	return id
}

// tokenLogin turns ?token= into a cookie and redirects without it.
func (s *server) tokenLogin(w http.ResponseWriter, r *http.Request, p string) {
	q := r.URL.Query()
	tok := q.Get("token")
	q.Del("token")
	// Rebuild the target from the cleaned path: a raw "//host" would make the
	// redirect leave the site.
	clean := p
	if strings.HasSuffix(r.URL.Path, "/") && p != "/" {
		clean += "/"
	}
	target := (&url.URL{Path: clean}).EscapedPath()
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	if _, ok := s.identify(tok); !ok {
		s.deny(w, r, p)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", MaxAge: cookieMaxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	http.Redirect(w, r, target, http.StatusFound)
}

// deny sends browsers asking for the UI to /login and everyone else a 401.
func (s *server) deny(w http.ResponseWriter, r *http.Request, p string) {
	if s.authFailed(w, r, p) {
		return
	}
	if r.Method == http.MethodGet && p == "/" {
		http.Redirect(w, r, keepQuery("/login", r), http.StatusFound)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="cerberus"`)
	if strings.HasPrefix(p, "/v1/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"missing or invalid token","type":"authentication_error"}}`))
		return
	}
	writeError(w, http.StatusUnauthorized, "missing or invalid token")
}

// keepQuery is target with r's query (?theme=color …) minus any token.
func keepQuery(target string, r *http.Request) string {
	if r.URL.RawQuery == "" {
		return target
	}
	q := r.URL.Query()
	q.Del("token")
	if enc := q.Encode(); enc != "" {
		return target + "?" + enc
	}
	return target
}

// authenticated reports whether r carries a valid admin or device token
// (bearer or cookie).
func (s *server) authenticated(r *http.Request) bool {
	if _, ok := s.identify(bearer(r)); ok {
		return true
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	_, ok := s.identify(c.Value)
	return ok
}

// validAgentToken reports whether t is the agents' /v1-only token.
func (s *server) validAgentToken(t string) bool {
	want := s.tokenConfig().AgentToken()
	if want == "" || t == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(want)) == 1
}

// localClient reports whether an unauthenticated loopback /v1 request looks
// like a desktop client rather than a web page in the local browser.
func (s *server) localClient(r *http.Request) bool {
	if r.Header.Get("Origin") != "" || !s.ownHost(r.Host) {
		return false
	}
	if safeMethod(r.Method) {
		return true
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// ownHost reports whether host (a Host header) names this server: a loopback
// IP, localhost, or the host of a configured listen address or PublicURL.
func (s *server) ownHost(host string) bool {
	h := hostOnly(host)
	if h == "" {
		return false
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	names := append([]string{s.conf().Server.Listen}, s.conf().Server.ExtraListen...)
	if u, err := url.Parse(s.conf().Server.PublicURL); err == nil && u.Host != "" {
		names = append(names, u.Host)
	}
	for _, n := range names {
		if c := hostOnly(n); c != "" && strings.EqualFold(c, h) {
			return true
		}
	}
	return false
}

// hostOnly strips the port and IPv6 brackets from a host[:port].
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// fromOwnUI reports whether a cookie-carrying request came from our own UI:
// not from another site (Sec-Fetch-Site) and not from a preview page
// (Referer). Requests without these headers (older browsers, curl with a
// cookie) are allowed; the CSRF header still guards their writes.
func fromOwnUI(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && strings.HasPrefix(path.Clean("/"+u.Path), "/preview/") {
			return false
		}
	}
	return true
}

// fromPreviewOrigin reports whether r says it comes from a preview page:
// an opaque origin ("null", a sandboxed path-mode preview) or one of this
// host's preview ports.
func (s *server) fromPreviewOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	if o == "null" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return true
	}
	port, _ := strconv.Atoi(u.Port())
	return s.previews.InRange(port) && s.allowedHost(u.Host)
}

// conflictingCookies reports whether r carries more than one distinct
// cerberus_token cookie.
func conflictingCookies(r *http.Request) bool {
	first := ""
	for _, c := range r.Cookies() {
		if c.Name != cookieName {
			continue
		}
		if first == "" {
			first = c.Value
		} else if c.Value != first {
			return true
		}
	}
	return false
}

// validToken compares in constant time. An unset server token matches nothing.
func (s *server) validToken(t string) bool {
	want := s.token()
	if want == "" || t == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(want)) == 1
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func publicPath(p string) bool {
	return p == "/healthz" || p == "/login" || p == "/api/auth" || p == "/api/login" || p == "/carino-clock.js" || p == "/manifest.webmanifest" || p == "/sw.js" ||
		p == "/assets" || strings.HasPrefix(p, "/assets/")
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// isLoopback reports whether the TCP peer is a loopback address.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// previewCSP sandboxes preview pages: their scripts run, but in an opaque
// origin, so an agent-written page cannot read our pages or call /api with
// the user's cookie.
const previewCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"

// withHeaders adds the security headers. Previews are the agent's site and
// run their own scripts, sandboxed into an opaque origin.
func (s *server) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/preview/") {
			h.Set("Content-Security-Policy", previewCSP)
		} else {
			csp := "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'"
			if host := cspHost(hostOnly(r.Host)); s.previews != nil && host != "" {
				// The Preview tab frames preview origins: this host, other ports.
				csp += "; frame-src 'self' http://" + host + ":* https://" + host + ":*"
			}
			h.Set("Content-Security-Policy", csp)
			h.Set("Referrer-Policy", "no-referrer")
		}
		next.ServeHTTP(w, r)
	})
}
