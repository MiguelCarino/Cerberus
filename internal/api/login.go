package api

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/users"
)

// Sign-in (docs/SCOPE.md "Authentication flow"):
//
//	GET  /api/auth     (public) {has_users, token_login}
//	POST /api/login    (public) {name, password} | {token} → Me + cookie
//	POST /api/logout   ends the caller's sign-in and clears the cookie
//
// The login guard: X-Cerberus: 1, a JSON body and Sec-Fetch-Site empty,
// same-origin or none (a cross-site page cannot sign a browser in to an
// account of the attacker's). Failures count for the per-address limiter
// and the per-name lockout (also for names that do not exist).

const msgBadLogin = "Wrong user name or password."

func (s *server) userRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/auth", s.handleAuth)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.meRoutes(mux)
	s.adminUserRoutes(mux)
	s.permitRoutes(mux)
}

// applyUsersConfig sets sign-in expiry and lockout limits from cfg.
func (s *server) applyUsersConfig(cfg *config.Config) { applyUsers(s.users, cfg) }

func applyUsers(st *users.Store, cfg *config.Config) {
	if st == nil || cfg == nil {
		return
	}
	uc := cfg.Users
	st.Logins.SetLimits(time.Duration(uc.SessionIdleHours)*time.Hour, time.Duration(uc.SessionMaxDays)*24*time.Hour)
	st.Lockout.Set(uc.LockoutFailures, time.Duration(uc.LockoutMinutes)*time.Minute)
}

// UsersReloader applies users.* settings live after a config change.
func UsersReloader(st *users.Store) Reloader { return usersReloader{st} }

type usersReloader struct{ st *users.Store }

func (r usersReloader) Reload(cfg *config.Config) { applyUsers(r.st, cfg) }

// handleAuth also says whether the browser already holds a password
// sign-in (signed_in), so the Login page can send it home instead of
// showing the form. A token or device cookie does not count: its holder may
// sign in with a password on purpose (after creating the first admin).
func (s *server) handleAuth(w http.ResponseWriter, r *http.Request) {
	signedIn := false
	if c, err := r.Cookie(cookieName); err == nil {
		if pr, ok := s.identify(c.Value); ok && pr.Kind == kindLogin {
			signedIn = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"has_users": s.hasUsers(), "token_login": true, "signed_in": signedIn})
}

// loginGuard refuses a sign-in a web page of another site could have sent.
func loginGuard(r *http.Request) bool {
	if r.Header.Get(csrfHeader) != "1" {
		return false
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// setLoginCookie sets the session cookie to tok for maxAge seconds.
func setLoginCookie(w http.ResponseWriter, r *http.Request, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
}

func clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
}

func writeLoginFailed(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msgBadLogin, "code": "login"})
}

func writeLocked(w http.ResponseWriter, retry time.Duration) {
	secs := int(retry.Seconds() + 0.999)
	mins := (secs + 59) / 60
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	unit := "minutes"
	if mins == 1 {
		unit = "minute"
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": "Too many attempts. Try again in " + strconv.Itoa(mins) + " " + unit + ".", "code": "locked",
		"retry_after": secs})
}

// loginFailure counts a failed sign-in for the address limiter; it answers
// 429 and returns true when this failure starts the block. Password
// sign-ins from loopback are not counted per address (see passwordAddrLimit).
func (s *server) loginFailure(w http.ResponseWriter, r *http.Request, cred string, password bool) bool {
	if password && !passwordAddrLimit(r) {
		return false
	}
	key := limitKey(r.RemoteAddr)
	counted, block := s.limiter.fail(key, cred)
	if counted && block {
		s.auditLog(audit.Record{Actor: audit.ActorSystem, Remote: limitHost(r.RemoteAddr), Route: "auth.blocked",
			Status: http.StatusTooManyRequests, Detail: "too many failed logins from " + key + "; refused for 10 min"})
		s.tooMany(w, "/api/login", blockFor)
		return true
	}
	return false
}

// passwordAddrLimit reports whether password sign-ins from r's peer are
// limited per address. Not from loopback: behind a local reverse proxy
// (tailscale serve, Caddy) every sign-in arrives from 127.0.0.1, and one
// person's typos would lock everyone out. The per-name lockout still
// applies to them.
func passwordAddrLimit(r *http.Request) bool { return !isLoopback(r.RemoteAddr) }

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	skipAudit(r) // recorded as auth.* events below, never with the body
	if !loginGuard(r) {
		writeError(w, http.StatusForbidden, "sign-in refused: send JSON with "+csrfHeader+": 1 from this site")
		return
	}
	// The block is checked here for token sign-ins and loopback; a
	// password sign-in from elsewhere reserves its attempt below (begin).
	if wait, blocked := s.limiter.blocked(limitKey(r.RemoteAddr)); blocked && passwordAddrLimit(r) {
		s.tooMany(w, "/api/login", wait)
		return
	}
	var b struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := decodeBodyLimit(w, r, &b, 16<<10); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	remote := limitHost(r.RemoteAddr)
	if b.Token != "" {
		pr, ok := s.identify(b.Token)
		if !ok || pr.Kind == kindLogin {
			s.auditLog(audit.Record{Actor: audit.ActorSystem, Remote: remote, Route: "auth.login_failed",
				Status: http.StatusUnauthorized, Detail: "token sign-in"})
			if wait, blocked := s.limiter.blocked(limitKey(r.RemoteAddr)); blocked {
				s.tooMany(w, "/api/login", wait)
				return
			}
			if s.loginFailure(w, r, b.Token, false) {
				return
			}
			writeLoginFailed(w)
			return
		}
		setLoginCookie(w, r, b.Token, cookieMaxAge)
		pr.Cookie = true
		s.auditLog(audit.Record{Actor: pr.Actor, User: pr.Name, Remote: remote, Route: "auth.login",
			Status: http.StatusOK, Detail: "token sign-in"})
		writeJSON(w, http.StatusOK, s.me(pr))
		return
	}
	if s.users == nil || b.Name == "" || b.Password == "" {
		writeLoginFailed(w)
		return
	}
	name := users.NormName(b.Name)
	// Reserve the attempt (address, then name) before hashing: concurrent
	// guesses count against the limits while in flight, so a burst cannot
	// check more passwords than the limits allow.
	if passwordAddrLimit(r) {
		key := limitKey(r.RemoteAddr)
		if wait, ok := s.limiter.begin(key); !ok {
			s.tooMany(w, "/api/login", wait)
			return
		}
		defer s.limiter.end(key)
	}
	ok, retry := s.users.Lockout.Begin(name)
	if !ok {
		writeLocked(w, retry)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	u, err := s.users.VerifyContext(ctx, name, b.Password)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.users.Lockout.End(name, false)
			writeError(w, http.StatusServiceUnavailable, "the server is busy; try again")
			return
		}
		s.auditLog(audit.Record{Actor: audit.ActorSystem, Remote: remote, Route: "auth.login_failed",
			Status: http.StatusUnauthorized, Detail: audit.Clip(name)})
		locked, _ := s.users.Lockout.End(name, true)
		if locked {
			s.auditLog(audit.Record{Actor: audit.ActorSystem, Remote: remote, Route: "auth.locked",
				Status: http.StatusTooManyRequests, Detail: audit.Clip(name)})
		}
		if s.loginFailure(w, r, "login:"+name+"\x00"+b.Password, true) {
			return
		}
		writeLoginFailed(w)
		return
	}
	s.users.Lockout.End(name, false)
	// A concurrent failure may have locked the name while this hash ran.
	if locked, retry := s.users.Lockout.Locked(name); locked {
		writeLocked(w, retry)
		return
	}
	s.users.Lockout.Clear(name)
	s.users.NoteLogin(u.ID, remote)
	tok, lg, err := s.users.Logins.Create(u.ID, remote, r.UserAgent())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the sign-in: "+err.Error())
		return
	}
	setLoginCookie(w, r, tok, int(time.Until(lg.Expires).Seconds()))
	pr := principal{Actor: audit.User(u.Name), Kind: kindLogin, User: u.ID, Name: u.Name, Role: u.Role,
		Login: lg.ID, Must: u.MustChange, Cookie: true}
	s.auditLog(audit.Record{Actor: pr.Actor, User: u.Name, Remote: remote, Route: "auth.login",
		Status: http.StatusOK, Targets: []string{lg.ID}})
	writeJSON(w, http.StatusOK, s.me(pr))
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	clearCookie(w, r)
	if pr.Kind == kindLogin && s.users != nil {
		s.users.Logins.Revoke(pr.Login)
		noteAudit(r, "signed out", pr.Login)
		s.auditLog(audit.Record{Actor: pr.Actor, User: pr.Name, Remote: limitHost(r.RemoteAddr), Route: "auth.logout",
			Status: http.StatusOK, Targets: []string{pr.Login}})
	} else {
		noteAudit(r, "cookie cleared")
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if pr.Kind == kindLogin {
		// After the reply: this request is one of the login's live ones.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		s.conns.cut("login:" + pr.Login)
	}
}

// ---------- 404 ----------

// builtin404 answers when web/dist/404.html is missing from the build.
const builtin404 = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>Not found · Cerberus</title>
<style>body{font-family:system-ui,sans-serif;background:#101010;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0}
main{text-align:center;padding:16px}a{color:#e0b84f}</style></head>
<body><main><h1>Nothing here</h1><p>This page does not exist.</p><p><a href="/">Open Cerberus</a></p></main></body></html>
`

// handleNotFound is the public catch-all "/": the themed 404 page for GET
// and HEAD, JSON for other methods.
func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	body := s.notFound
	if body == nil {
		body = []byte(builtin404)
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// isAPIPath reports whether p is under /api.
func isAPIPath(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }
