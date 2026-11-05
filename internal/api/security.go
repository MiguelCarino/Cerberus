package api

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/api/qr"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/users"
)

// Security endpoints (day phase 3):
//
//	GET    /api/audit?since=24h&actor=agent&limit=200   audit records, newest first
//	GET    /api/devices                                 paired devices (no hashes)
//	POST   /api/devices {"name":"phone"}                pair a device (admin token only)
//	DELETE /api/devices/{id}                            revoke: its live streams end at once
//	POST   /api/token/rotate                            new admin token (owners only)
//	GET    /api/doctor                                  the safety checklist

var errNoConfigFile = errors.New("the config has no file to save the new token to")

func (s *server) securityRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/audit", s.handleAudit)
	mux.HandleFunc("GET /api/devices", s.handleDeviceList)
	mux.HandleFunc("POST /api/devices", s.handleDevicePair)
	mux.HandleFunc("DELETE /api/devices/{id}", s.handleDeviceRevoke)
	mux.HandleFunc("POST /api/token/rotate", s.handleTokenRotate)
	mux.HandleFunc("GET /api/doctor", s.handleDoctor)
}

// ParseSince reads a "since" value: a duration back from now ("24h",
// "90m", "7d"), an RFC 3339 time, or a date (2006-01-02). "" is the zero
// time (no bound).
func ParseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("since: use a duration (24h, 7d) or a time (RFC 3339)")
}

// AuditView is the reply of GET /api/audit.
type AuditView struct {
	Records []audit.Record `json:"records"`
}

func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		unavailable(w, "audit log")
		return
	}
	q := r.URL.Query()
	since, err := ParseSince(q.Get("since"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	recs, err := s.audit.QueryFilter(audit.Filter{Since: since, Actor: q.Get("actor"), User: q.Get("user"), Limit: limit})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if recs == nil {
		recs = []audit.Record{}
	}
	writeJSON(w, http.StatusOK, AuditView{Records: recs})
}

// DeviceList is the reply of GET /api/devices. Current is the caller's
// device id, or "admin" for the admin token.
type DeviceList struct {
	Devices []devices.Public `json:"devices"`
	Current string           `json:"current"`
}

func (s *server) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		unavailable(w, "devices")
		return
	}
	pr := principalOf(r)
	cur := pr.Device
	if pr.Kind == kindAdmin {
		cur = "admin"
	}
	out := []devices.Public{}
	for _, d := range s.devices.List() {
		// Admins see every device, the others their own.
		if !pr.admin() && !s.sameOwner(pr, d.User) {
			continue
		}
		if s.hasUsers() {
			d.User = s.ownerOf(d.User)
			d.UserName = s.ownerName(d.User)
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, DeviceList{Devices: out, Current: cur})
}

// PairResult is the reply of POST /api/devices. Token is shown only here.
type PairResult struct {
	Device devices.Public `json:"device"`
	Token  string         `json:"token"`
	URL    string         `json:"url"`            // the UI with ?token=, on the address a phone can most likely open (pairURLs)
	QR     string         `json:"qr,omitempty"`   // URL as a QR symbol, one row per line, "#" dark, "." light
	URLs   []string       `json:"urls,omitempty"` // every candidate address with ?token=, URL first
}

func (s *server) handleDevicePair(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		unavailable(w, "devices")
		return
	}
	pr := principalOf(r)
	// The admin token pairs for anyone; a signed-in person (login cookie)
	// for themselves. A device token never pairs.
	if pr.Kind != kindAdmin && !(pr.Kind == kindLogin && s.hasUsers()) {
		writeError(w, http.StatusForbidden, "pairing a device needs the admin token (cerberus pair) or a signed-in user")
		return
	}
	var b struct {
		Name  string `json:"name"`
		Scope string `json:"scope"` // "" (a full device) or "mcp" (an MCP client: /api/agent/* only)
		User  string `json:"user"`  // the device's user (admin token only; default: the legacy owner)
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	if b.Scope != "" && b.Scope != devices.ScopeMCP {
		writeError(w, http.StatusBadRequest, `scope must be "" or "mcp"`)
		return
	}
	user := ""
	switch {
	case pr.Kind == kindLogin:
		if b.User != "" && b.User != pr.User {
			writeError(w, http.StatusForbidden, "you can pair devices only for yourself")
			return
		}
		user = pr.User
	case b.User != "":
		if !s.hasUsers() {
			writeError(w, http.StatusBadRequest, "there are no user accounts yet")
			return
		}
		if _, ok := s.users.Get(b.User); !ok {
			writeError(w, http.StatusBadRequest, "no such user")
			return
		}
		user = b.User
	}
	if b.Scope == devices.ScopeMCP && s.hasUsers() {
		role := users.RoleAdmin
		if u, ok := s.users.Get(s.ownerOf(user)); ok {
			role = u.Role
		}
		if !users.Can(role, users.CapSessions) {
			roleError(w, role)
			return
		}
	}
	tok, d, err := s.devices.PairScoped(b.Name, b.Scope, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b.Scope == devices.ScopeMCP {
		// An MCP client has no sign-in link: it never opens the UI.
		noteAudit(r, "paired MCP client "+d.Name, d.ID)
		writeJSON(w, http.StatusOK, PairResult{Device: d, Token: tok})
		return
	}
	noteAudit(r, "paired "+d.Name, d.ID)
	scheme := "http://"
	if r.TLS != nil {
		scheme = "https://"
	}
	ifaces := s.ifaceIPs
	if ifaces == nil {
		ifaces = interfaceIPs
	}
	urls := pairURLs(s.conf().Server, scheme+r.Host, ifaces())
	q := "/?token=" + url.QueryEscape(tok)
	for i := range urls {
		urls[i] += q
	}
	res := PairResult{Device: d, Token: tok, URL: urls[0], URLs: urls}
	if code, err := qr.Encode([]byte(res.URL)); err == nil {
		res.QR = code.String()
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		unavailable(w, "devices")
		return
	}
	pr := principalOf(r)
	if !pr.admin() {
		// Only one's own devices; others are not found.
		mine := false
		for _, x := range s.devices.List() {
			if (x.ID == r.PathValue("id") || x.Name == r.PathValue("id")) && s.sameOwner(pr, x.User) {
				mine = true
			}
		}
		if !mine {
			notFound(w)
			return
		}
	}
	d, err := s.devices.Revoke(r.PathValue("id"))
	if errors.Is(err, devices.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the device is still paired: could not save the revocation: "+err.Error())
		return
	}
	cut := s.conns.cut("device:" + d.ID)
	noteAudit(r, "revoked "+d.Name+", "+strconv.Itoa(cut)+" live request(s) cut")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "device": d, "cut": cut})
}

// RotateResult is the reply of POST /api/token/rotate. Token is returned
// only to a caller that used the admin token by header (the CLI); a browser
// on the admin cookie gets a fresh cookie instead, and a paired device
// keeps its own token and never learns the admin one.
type RotateResult struct {
	OK    bool   `json:"ok"`
	Token string `json:"token,omitempty"`
}

func (s *server) handleTokenRotate(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	if !pr.owner() {
		writeError(w, http.StatusForbidden, "only the owner can rotate the token")
		return
	}
	tok, err := s.rotateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rotate: "+err.Error())
		return
	}
	noteAudit(r, "admin token rotated; old cookies and agent tokens invalidated")
	res := RotateResult{OK: true}
	if pr.Kind == kindAdmin {
		if pr.Cookie {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", MaxAge: cookieMaxAge,
				HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
		} else {
			res.Token = tok
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	env := doctor.Env{Cfg: s.view(), DataDir: s.dataDir,
		TokenRotated: func() time.Time { return doctor.LastRotation(s.audit) }}
	env.Users = s.users
	if s.doctorEnv != nil {
		env = s.doctorEnv(env)
	}
	rep := doctor.Run(r.Context(), env)
	if !principalOf(r).admin() {
		// Viewers and monitors see the counts only.
		writeJSON(w, http.StatusOK, map[string]any{"counts": rep.Counts, "checks": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// pairURLs are the base URLs a new device could open, best first: the
// PublicURL; the address the request came to when it is not loopback (the
// owner is on the LAN or tailnet already); each non-loopback listen
// address, or for a wildcard listen every interface address; and the
// request's own address last. The desktop UI usually runs on 127.0.0.1,
// which a phone cannot open, so loopback never comes first unless it is
// all there is.
func pairURLs(sc config.ServerConfig, reqBase string, ifaceIPs []string) []string {
	var good, loop []string
	seen := map[string]bool{}
	add := func(base string) {
		base = strings.TrimRight(base, "/")
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || seen[base] {
			return
		}
		seen[base] = true
		h := u.Hostname()
		if ip := net.ParseIP(h); strings.EqualFold(h, "localhost") || ip != nil && ip.IsLoopback() {
			loop = append(loop, base)
			return
		}
		good = append(good, base)
	}
	if sc.PublicURL != "" {
		add(sc.PublicURL)
	}
	add(reqBase)
	// Extra addresses serve only HTTPS when a certificate is set (serve.go
	// listenAll); the primary Listen is always plain HTTP.
	tlsExtra := sc.TLSCert != "" && sc.TLSKey != ""
	for i, a := range append([]string{sc.Listen}, sc.ExtraListen...) {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		scheme := "http://"
		if i > 0 && tlsExtra {
			scheme = "https://"
		}
		if !doctor.Wildcard(host) {
			add(scheme + net.JoinHostPort(host, port))
			continue
		}
		for _, ip := range ifaceIPs {
			add(scheme + net.JoinHostPort(ip, port))
		}
	}
	out := append(good, loop...)
	if len(out) == 0 {
		out = []string{strings.TrimRight(reqBase, "/")}
	}
	return out
}

// interfaceIPs are this host's non-loopback unicast addresses (LAN and
// tailnet ranges included).
func interfaceIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.IsGlobalUnicast() {
			out = append(out, n.IP.String())
		}
	}
	return out
}
