package api

import (
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/users"
)

// The Users admin panel (docs/SCOPE.md "API contract", role admin):
//
//	GET    /api/users                   {users, roles, agents}
//	POST   /api/users                   {name, display?, role, password?} → 201 {user, password?}
//	GET    /api/users/{id}              UserPublic
//	PATCH  /api/users/{id}              {display?, role?, disabled?, budget?, repos?, agents?} → {user, cut}
//	POST   /api/users/{id}/password     {password?} → {ok, password?, cut}
//	DELETE /api/users/{id}              {to?} → {ok, reassigned_to, moved, devices_revoked, cut}
//	DELETE /api/users/{id}/apikey       {ok}

func (s *server) adminUserRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/users", s.handleUserList)
	mux.HandleFunc("POST /api/users", s.handleUserAdd)
	mux.HandleFunc("GET /api/users/{id}", s.handleUserGet)
	mux.HandleFunc("PATCH /api/users/{id}", s.handleUserPatch)
	mux.HandleFunc("POST /api/users/{id}/password", s.handleUserPassword)
	mux.HandleFunc("DELETE /api/users/{id}", s.handleUserDelete)
	mux.HandleFunc("DELETE /api/users/{id}/apikey", s.handleUserAPIKeyRevoke)
}

// UserPublic is a user as the admin panel shows it.
type UserPublic struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Display     string            `json:"display"`
	Role        string            `json:"role"`
	Disabled    bool              `json:"disabled"`
	MustChange  bool              `json:"must_change"`
	Locked      bool              `json:"locked"`
	Created     time.Time         `json:"created"`
	Updated     time.Time         `json:"updated"`
	LastLogin   time.Time         `json:"last_login,omitzero"`
	LastAddr    string            `json:"last_addr,omitempty"`
	Budget      users.Budget      `json:"budget"`
	SpentUSD    float64           `json:"spent_usd"`
	SpentTokens int64             `json:"spent_tokens"`
	Sessions    int               `json:"sessions"`
	Chats       int               `json:"chats"`
	Flows       int               `json:"flows"`
	Devices     int               `json:"devices"`
	Repos       []users.RepoShare `json:"repos"`
	Agents      []string          `json:"agents"`
	APIKey      APIKeyInfo        `json:"api_key"`
	LegacyOwner bool              `json:"legacy_owner"`
}

// ownedCounts counts objects per current owner.
type ownedCounts struct {
	sessions, chats, flows, runs, inbox, devices map[string]int
}

func (s *server) countOwned() ownedCounts {
	c := ownedCounts{sessions: map[string]int{}, chats: map[string]int{}, flows: map[string]int{},
		runs: map[string]int{}, inbox: map[string]int{}, devices: map[string]int{}}
	if s.sessions != nil {
		for _, in := range s.sessions.List() {
			c.sessions[s.ownerOf(in.Owner)]++
		}
	}
	if s.chats != nil {
		for _, ch := range s.chats.List() {
			c.chats[s.ownerOf(ch.Owner)]++
		}
	}
	if s.flows != nil {
		for _, f := range s.flows.List() {
			c.flows[s.ownerOf(f.Owner)]++
		}
		for _, r := range s.flows.Runs("", 0) {
			c.runs[s.ownerOf(r.Owner)]++
		}
	}
	if s.inbox != nil {
		for _, it := range s.inbox.List(false, 0) {
			if it.Owner != "" {
				c.inbox[s.ownerOf(it.Owner)]++
			}
		}
	}
	if s.devices != nil {
		for _, d := range s.devices.List() {
			c.devices[s.ownerOf(d.User)]++
		}
	}
	return c
}

func (s *server) userPublic(u users.User, c *ownedCounts) UserPublic {
	locked, _ := s.users.Lockout.Locked(u.Name)
	sp := s.userSpend(u.ID)
	p := UserPublic{ID: u.ID, Name: u.Name, Display: u.Display, Role: u.Role, Disabled: u.Disabled,
		MustChange: u.MustChange, Locked: locked, Created: u.Created, Updated: u.Updated, LastLogin: u.LastLogin,
		LastAddr: u.LastAddr, Budget: u.Budget, SpentUSD: sp.CostUSD, SpentTokens: sp.Tokens,
		Repos: nonNil(u.Repos), Agents: nonNil(u.Agents), APIKey: keyInfo(u), LegacyOwner: s.legacyOwner() == u.ID}
	if c == nil {
		cc := s.countOwned()
		c = &cc
	}
	p.Sessions, p.Chats, p.Flows, p.Devices = c.sessions[u.ID], c.chats[u.ID], c.flows[u.ID], c.devices[u.ID]
	return p
}

func (s *server) usersReady(w http.ResponseWriter) bool {
	if s.users == nil {
		unavailable(w, "user accounts")
		return false
	}
	return true
}

func (s *server) handleUserList(w http.ResponseWriter, r *http.Request) {
	if !s.usersReady(w) {
		return
	}
	c := s.countOwned()
	out := []UserPublic{}
	for _, u := range s.users.List() {
		out = append(out, s.userPublic(u, &c))
	}
	var agents []string
	for _, p := range s.conf().AgentProfiles() {
		agents = append(agents, p.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "roles": users.Roles, "agents": nonNil(agents)})
}

func (s *server) handleUserGet(w http.ResponseWriter, r *http.Request) {
	if !s.usersReady(w) {
		return
	}
	u, ok := s.users.Get(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.userPublic(u, nil))
}

// userErr maps a users error to the API's status and code.
func userErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, users.ErrNotFound):
		notFound(w)
	case errors.Is(err, users.ErrNameTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "name"})
	case errors.Is(err, users.ErrName):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "name"})
	case errors.Is(err, users.ErrLastAdmin):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "last_admin"})
	case users.IsPasswordError(err):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "password"})
	case errors.Is(err, users.ErrRole), errors.Is(err, users.ErrFirstAdmin):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "role"})
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// GeneratePassword makes a 16-character password from an alphabet without
// look-alikes.
func GeneratePassword() string {
	const alpha = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
		if err != nil {
			panic(err)
		}
		b[i] = alpha[n.Int64()]
	}
	return string(b)
}

// userEvent writes a user.* audit record for the request's actor.
func (s *server) userEvent(r *http.Request, event, detail string, targets ...string) {
	pr := principalOf(r)
	actor := orDefault(pr.Actor, audit.ActorSystem)
	s.auditLog(audit.Record{Actor: actor, User: pr.Name, Remote: limitHost(r.RemoteAddr), Route: event,
		Targets: targets, Status: http.StatusOK, Detail: detail})
}

func (s *server) handleUserAdd(w http.ResponseWriter, r *http.Request) {
	if !s.usersReady(w) {
		return
	}
	var b struct {
		Name     string `json:"name"`
		Display  string `json:"display"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := decodeBodyLimit(w, r, &b, 16<<10); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	pw, generated := b.Password, false
	if pw == "" {
		pw, generated = GeneratePassword(), true
	}
	u, err := s.users.AddContext(r.Context(), b.Name, b.Display, b.Role, pw, generated)
	if err != nil {
		userErr(w, err)
		return
	}
	noteAudit(r, "added "+u.Name+" ("+u.Role+")", u.ID)
	s.userEvent(r, "user.add", u.Name+" ("+u.Role+")", u.ID)
	res := map[string]any{"user": s.userPublic(u, nil)}
	if generated {
		res["password"] = pw
	}
	writeJSON(w, http.StatusCreated, res)
}

// revokeUser ends every sign-in of the user except keepLogin, revokes their
// devices and MCP clients, and cuts their live requests. It returns the
// number of cut requests and revoked devices.
func (s *server) revokeUser(id, keepLogin string) (cut, devs int) {
	if s.users != nil {
		for _, lid := range s.users.Logins.RevokeUserIDs(id, keepLogin) {
			cut += s.conns.cut("login:" + lid)
		}
	}
	if s.devices != nil {
		gone, err := s.devices.RevokeUser(id, s.ownerOf)
		if err != nil {
			s.log.Warn("revoke devices of user", "user", id, "err", err)
		}
		for _, d := range gone {
			cut += s.conns.cut("device:" + d.ID)
		}
		devs = len(gone)
	}
	if keepLogin == "" {
		cut += s.conns.cut("user:" + id)
	}
	if s.audit != nil {
		s.auditLog(audit.Record{Actor: audit.ActorSystem, Route: "user.revoked", Targets: []string{id},
			Detail: strconv.Itoa(cut) + " live request(s) cut, " + strconv.Itoa(devs) + " device(s) revoked"})
	}
	return cut, devs
}

// knownRepo reports whether p is one of /api/repos.
func (s *server) knownRepo(p string) bool {
	p = filepath.Clean(p)
	for _, r := range s.repos() {
		if filepath.Clean(r.Path) == p {
			return true
		}
	}
	return false
}

func (s *server) handleUserPatch(w http.ResponseWriter, r *http.Request) {
	if !s.usersReady(w) {
		return
	}
	id := r.PathValue("id")
	u, ok := s.users.Get(id)
	if !ok {
		notFound(w)
		return
	}
	var b struct {
		Display  *string            `json:"display"`
		Role     *string            `json:"role"`
		Disabled *bool              `json:"disabled"`
		Budget   *users.Budget      `json:"budget"`
		Repos    *[]users.RepoShare `json:"repos"`
		Agents   *[]string          `json:"agents"`
	}
	if err := decodeBodyLimit(w, r, &b, 64<<10); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	pr := principalOf(r)
	self := pr.Kind != kindAdmin && pr.User == id
	if self && ((b.Disabled != nil && *b.Disabled) || (b.Role != nil && *b.Role != users.RoleAdmin)) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "You cannot disable or demote yourself.", "code": "self"})
		return
	}
	if b.Repos != nil {
		for _, rp := range *b.Repos {
			if !s.knownRepo(rp.Path) {
				writeError(w, http.StatusBadRequest, "repo "+rp.Path+" is not one of /api/repos")
				return
			}
		}
	}
	if b.Agents != nil {
		for _, a := range *b.Agents {
			if _, ok := s.conf().AgentProfile(a); !ok {
				writeError(w, http.StatusBadRequest, "no agent profile "+strconv.Quote(a))
				return
			}
		}
	}
	revoke := false
	var notes []string
	var err error
	if b.Role != nil && *b.Role != u.Role {
		if u, err = s.users.SetRole(id, *b.Role); err != nil {
			userErr(w, err)
			return
		}
		revoke = true
		notes = append(notes, "role "+u.Role)
		s.userEvent(r, "user.role", u.Name+" → "+u.Role, id)
	}
	if b.Disabled != nil && *b.Disabled != u.Disabled {
		if u, err = s.users.SetDisabled(id, *b.Disabled); err != nil {
			userErr(w, err)
			return
		}
		if u.Disabled {
			revoke = true
			notes = append(notes, "disabled")
			s.userEvent(r, "user.disable", u.Name, id)
		} else {
			notes = append(notes, "enabled")
			s.userEvent(r, "user.enable", u.Name, id)
		}
	}
	if b.Display != nil {
		if u, err = s.users.SetDisplay(id, *b.Display); err != nil {
			userErr(w, err)
			return
		}
	}
	if b.Budget != nil {
		if u, err = s.users.SetBudget(id, *b.Budget); err != nil {
			userErr(w, err)
			return
		}
		notes = append(notes, "budget")
	}
	if b.Repos != nil {
		if u, err = s.users.SetRepos(id, *b.Repos); err != nil {
			userErr(w, err)
			return
		}
		notes = append(notes, "repos")
	}
	if b.Agents != nil {
		if u, err = s.users.SetAgents(id, *b.Agents); err != nil {
			userErr(w, err)
			return
		}
		notes = append(notes, "agents")
	}
	noteAudit(r, u.Name+": "+strings.Join(notes, ", "), id)
	cut, stopped := 0, 0
	if revoke {
		cut, _ = s.revokeUser(id, "")
	}
	if u.Disabled {
		// A disabled user's agents lose their tokens: stop their sessions
		// cleanly instead of leaving them to fail on auth errors.
		stopped = s.stopUserSessions(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": s.userPublic(u, nil), "cut": cut, "sessions_stopped": stopped})
}

func (s *server) handleUserPassword(w http.ResponseWriter, r *http.Request) {
	if !s.usersReady(w) {
		return
	}
	id := r.PathValue("id")
	u, ok := s.users.Get(id)
	if !ok {
		notFound(w)
		return
	}
	var b struct {
		Password string `json:"password"`
	}
	if err := decodeBodyLimit(w, r, &b, 16<<10); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	pw, generated := b.Password, false
	if pw == "" {
		pw, generated = GeneratePassword(), true
	}
	if _, err := s.users.SetPasswordContext(r.Context(), id, pw, true); err != nil {
		userErr(w, err)
		return
	}
	s.users.Lockout.Clear(u.Name)
	noteAudit(r, "password reset for "+u.Name, id)
	s.userEvent(r, "user.password", "reset by an admin: "+u.Name, id)
	cut, _ := s.revokeUser(id, "")
	res := map[string]any{"ok": true, "cut": cut}
	if generated {
		res["password"] = pw
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if !s.usersReady(w) {
		return
	}
	id := r.PathValue("id")
	u, ok := s.users.Get(id)
	if !ok {
		notFound(w)
		return
	}
	var b struct {
		To string `json:"to"`
	}
	if err := decodeBodyLimit(w, r, &b, 16<<10); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	pr := principalOf(r)
	if pr.Kind != kindAdmin && pr.User == id {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "You cannot delete yourself.", "code": "self"})
		return
	}
	to := b.To
	if to == "" {
		to = pr.User
		if to == "" || to == id {
			to = s.otherAdmin(id)
		}
	}
	if t, ok := s.users.Get(to); !ok || t.Role != users.RoleAdmin || t.Disabled || to == id {
		if u.Role == users.RoleAdmin && !u.Disabled && s.otherAdmin(id) == "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": users.ErrLastAdmin.Error(), "code": "last_admin"})
			return
		}
		writeError(w, http.StatusBadRequest, `"to" must be another enabled admin`)
		return
	}
	before := s.countOwned()
	moved := map[string]int{"sessions": before.sessions[id], "chats": before.chats[id], "flows": before.flows[id],
		"runs": before.runs[id], "inbox": before.inbox[id]}
	// Revoke first (the user's devices still resolve to them), then remove.
	// Their running sessions stop: after the move their agents would
	// otherwise act for the admin who receives the data.
	cut, devs := s.revokeUser(id, "")
	stopped := s.stopUserSessions(id)
	if _, err := s.users.Remove(id, to); err != nil {
		userErr(w, err)
		return
	}
	// The removed user's flows move too, but they do not run until an admin
	// re-saves them (their steps were written by the removed account):
	// scheduled ones are turned off.
	var paused []string
	if s.flows != nil {
		for _, f := range s.flows.List() {
			if !movedOwner(s.users, f.Owner) || s.ownerOf(f.Owner) != to {
				continue
			}
			if f.Enabled {
				if _, err := s.flows.SetFlowEnabled(flowActor(r), f.ID, false); err == nil {
					paused = append(paused, f.ID)
				}
			}
		}
	}
	_ = s.users.RevokeAPIKey(id)
	noteAudit(r, "removed "+u.Name+"; data to "+to, id, to)
	s.userEvent(r, "user.remove", u.Name+"; data moved to "+s.ownerName(to), id, to)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reassigned_to": to, "moved": moved,
		"devices_revoked": devs, "cut": cut, "flows_paused": nonNil(paused), "sessions_stopped": stopped,
		"note": "Their flows do not run until you open and save each one."})
}

// stopUserSessions stops the running sessions owned by user id and
// returns how many it stopped.
func (s *server) stopUserSessions(id string) int {
	if s.sessions == nil {
		return 0
	}
	n := 0
	for _, info := range s.sessions.List() {
		if s.ownerOf(info.Owner) != id || sessionIdle(info.State) {
			continue
		}
		if err := s.sessions.Stop(info.ID); err == nil {
			n++
		}
	}
	return n
}

// otherAdmin is the first enabled admin other than id ("" when none).
func (s *server) otherAdmin(id string) string {
	for _, u := range s.users.List() {
		if u.ID != id && u.Role == users.RoleAdmin && !u.Disabled {
			return u.ID
		}
	}
	return ""
}

func (s *server) handleUserAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.usersReady(w) {
		return
	}
	id := r.PathValue("id")
	u, ok := s.users.Get(id)
	if !ok {
		notFound(w)
		return
	}
	if err := s.users.RevokeAPIKey(id); err != nil {
		userErr(w, err)
		return
	}
	noteAudit(r, "API key revoked for "+u.Name, id)
	s.userEvent(r, "user.apikey", "revoked for "+u.Name, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
