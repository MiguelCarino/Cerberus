package api

import (
	"context"
	"net/http"
	"time"

	"carino.systems/cerberus/internal/users"
)

// The caller's own account (docs/SCOPE.md "API contract"):
//
//	GET    /api/me                  Me
//	POST   /api/me/password         {current?, new} → {ok} + a new cookie
//	GET    /api/me/logins           {logins: [...]} (signed-in browsers)
//	DELETE /api/me/logins/{id}      {ok}
//	POST   /api/me/apikey           {key, hint} (shown once)
//	DELETE /api/me/apikey           {ok}

func (s *server) meRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/me/password", s.handleMePassword)
	mux.HandleFunc("GET /api/me/logins", s.handleMeLogins)
	mux.HandleFunc("DELETE /api/me/logins/{id}", s.handleMeLoginRevoke)
	mux.HandleFunc("POST /api/me/apikey", s.handleMeAPIKey)
	mux.HandleFunc("DELETE /api/me/apikey", s.handleMeAPIKeyRevoke)
}

// MeUser is the account part of Me.
type MeUser struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Display    string    `json:"display"`
	Role       string    `json:"role"`
	MustChange bool      `json:"must_change"`
	Created    time.Time `json:"created"`
	LastLogin  time.Time `json:"last_login,omitzero"`
}

// MeBudget is the caller's monthly budget and spend.
type MeBudget struct {
	MonthlyUSD    float64 `json:"monthly_usd"`
	MonthlyTokens int64   `json:"monthly_tokens"`
	SpentUSD      float64 `json:"spent_usd"`
	SpentTokens   int64   `json:"spent_tokens"`
}

// APIKeyInfo describes a personal /v1 key (never the key).
type APIKeyInfo struct {
	Set  bool      `json:"set"`
	Hint string    `json:"hint,omitempty"`
	Made time.Time `json:"made,omitzero"`
}

// Me is GET /api/me.
type Me struct {
	Kind      string     `json:"kind"` // login | device | admin-token
	User      *MeUser    `json:"user"`
	Role      string     `json:"role"`
	Caps      []string   `json:"caps"`
	Home      string     `json:"home"`
	HasUsers  bool       `json:"has_users"`
	Bootstrap bool       `json:"bootstrap"`
	Budget    MeBudget   `json:"budget"`
	APIKey    APIKeyInfo `json:"api_key"`
	Device    string     `json:"device"`
}

func keyInfo(u users.User) APIKeyInfo {
	return APIKeyInfo{Set: u.APIKeyHash != "", Hint: u.APIKeyHint, Made: u.APIKeyMade}
}

// me builds the Me object of a principal.
func (s *server) me(pr principal) Me {
	has := s.hasUsers()
	role := orDefault(pr.Role, users.RoleAdmin)
	m := Me{Kind: "admin-token", Role: role, Caps: users.Caps(role), Home: users.Home(role), HasUsers: has,
		Device: pr.Device}
	switch pr.Kind {
	case kindLogin:
		m.Kind = "login"
	case kindDevice:
		m.Kind = "device"
	}
	m.Bootstrap = pr.Kind == kindAdmin && !has
	if has && pr.Kind != kindAdmin && pr.User != "" {
		if u, ok := s.users.Get(pr.User); ok {
			m.User = &MeUser{ID: u.ID, Name: u.Name, Display: u.Display, Role: u.Role, MustChange: u.MustChange,
				Created: u.Created, LastLogin: u.LastLogin}
			sp := s.userSpend(u.ID)
			m.Budget = MeBudget{MonthlyUSD: u.Budget.MonthlyUSD, MonthlyTokens: u.Budget.MonthlyTokens,
				SpentUSD: sp.CostUSD, SpentTokens: sp.Tokens}
			m.APIKey = keyInfo(u)
		}
	}
	return m
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.me(principalOf(r)))
}

func writeNoUser(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": "The admin token has no account: sign in as a user to do this.", "code": "no_user"})
}

func (s *server) handleMePassword(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	if s.users == nil || pr.User == "" || pr.Kind == kindAdmin {
		writeNoUser(w)
		return
	}
	var b struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeBodyLimit(w, r, &b, 16<<10); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	u, ok := s.users.Get(pr.User)
	if !ok {
		writeNoUser(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if !u.MustChange {
		if locked, retry := s.users.Lockout.Locked(u.Name); locked {
			writeLocked(w, retry)
			return
		}
		if _, err := s.users.VerifyContext(ctx, u.Name, b.Current); err != nil {
			s.users.Lockout.Fail(u.Name)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "The current password is wrong.", "code": "login"})
			return
		}
	}
	if err := users.CheckPassword(u.Name, b.New); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "password"})
		return
	}
	if _, err := s.users.SetPasswordContext(ctx, u.ID, b.New, false); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	noteAudit(r, "own password changed")
	s.userEvent(r, "user.password", "own password changed", u.Name)
	// A fresh sign-in for this browser; every other one ends.
	keep := ""
	if pr.Kind == kindLogin {
		tok, lg, err := s.users.Logins.Create(u.ID, limitHost(r.RemoteAddr), r.UserAgent())
		if err == nil {
			setLoginCookie(w, r, tok, int(time.Until(lg.Expires).Seconds()))
			keep = lg.ID
		}
	}
	// Other sign-ins, devices and MCP clients end; this request keeps its
	// (new) sign-in, so it is not cut.
	s.revokeUser(u.ID, keep)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// LoginView is one signed-in browser.
type LoginView struct {
	ID       string    `json:"id"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	Expires  time.Time `json:"expires"`
	Addr     string    `json:"addr,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Current  bool      `json:"current"`
}

func (s *server) handleMeLogins(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	out := []LoginView{}
	if s.users != nil && pr.User != "" && pr.Kind != kindAdmin {
		for _, l := range s.users.Logins.List(pr.User) {
			out = append(out, LoginView{ID: l.ID, Created: l.Created, LastSeen: l.LastSeen, Expires: l.Expires,
				Addr: l.Addr, Agent: l.Agent, Current: l.ID == pr.Login})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logins": out})
}

func (s *server) handleMeLoginRevoke(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	id := r.PathValue("id")
	if s.users == nil || pr.User == "" || pr.Kind == kindAdmin {
		notFound(w)
		return
	}
	l, ok := s.users.Logins.Get(id)
	if !ok || l.User != pr.User {
		notFound(w)
		return
	}
	s.users.Logins.Revoke(id)
	noteAudit(r, "signed out a browser", id)
	if id == pr.Login {
		clearCookie(w, r)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.conns.cut("login:" + id) // after the reply: it may be this very request's sign-in
}

func (s *server) handleMeAPIKey(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	if s.users == nil || pr.User == "" || pr.Kind == kindAdmin {
		writeNoUser(w)
		return
	}
	key, err := s.users.NewAPIKey(pr.User)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	noteAudit(r, "personal API key made")
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "hint": key[len(key)-4:]})
}

func (s *server) handleMeAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	if s.users == nil || pr.User == "" || pr.Kind == kindAdmin {
		writeNoUser(w)
		return
	}
	if err := s.users.RevokeAPIKey(pr.User); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	noteAudit(r, "personal API key revoked")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
