package api

import (
	"net/http"
	"strings"

	"carino.systems/cerberus/internal/users"
)

// Route classes (docs/SCOPE.md "Route → role class table"). Every pattern
// registered on the mux has one; routeMux panics at startup on a pattern
// without a class, so a new route fails closed until someone decides who
// may call it.
type roleClass string

const (
	clsPublic   roleClass = "P"     // no credential
	clsNotFound roleClass = "NF"    // the public catch-all 404
	clsAny      roleClass = "ANY"   // every signed-in person (all roles, admin token, devices)
	clsAdmin    roleClass = "ADM"   // admin only
	clsWork     roleClass = "WORK"  // admin, user (own)
	clsWatch    roleClass = "WATCH" // admin, user (own), viewer (all); GET only
	clsMon      roleClass = "MON"   // admin, viewer, monitor
	clsChat     roleClass = "CHAT"  // admin, user, chat (own)
	clsGateway  roleClass = "GW"    // /v1: admin, user, agent tokens
	clsAgent    roleClass = "AGENT" // the agent scope (session agent tokens, MCP clients)
)

// classRoles are the roles each class admits (public and agent classes
// are decided elsewhere).
var classRoles = map[roleClass][]string{
	clsAny:     users.Roles,
	clsAdmin:   {users.RoleAdmin},
	clsWork:    {users.RoleAdmin, users.RoleUser},
	clsWatch:   {users.RoleAdmin, users.RoleUser, users.RoleViewer},
	clsMon:     {users.RoleAdmin, users.RoleViewer, users.RoleMonitor},
	clsChat:    {users.RoleAdmin, users.RoleUser, users.RoleChat},
	clsGateway: {users.RoleAdmin, users.RoleUser},
}

// routeRoles classifies every mux pattern.
var routeRoles = map[string]roleClass{
	"GET /healthz":                clsPublic,
	"GET /login":                  clsPublic,
	"GET /assets/":                clsPublic,
	"GET /manifest.webmanifest":   clsPublic,
	"GET /sw.js":                  clsPublic,
	"GET /carino-clock.js":        clsPublic,
	"GET /{$}":                    clsAny,
	"/":                           clsNotFound,
	"GET /preview/{id}":           clsWatch,
	"GET /preview/{id}/{path...}": clsWatch,
	"/v1/":                        clsGateway,

	"GET /api/status":                            clsAny,
	"GET /api/models":                            clsWork,
	"POST /api/models/{rest...}":                 clsAdmin,
	"GET /api/usage":                             clsWork,
	"POST /api/usage/watch":                      clsWork,
	"GET /api/repos":                             clsWork,
	"GET /api/repos/find":                        clsAdmin,
	"GET /api/bench":                             clsWork,
	"POST /api/bench":                            clsAdmin,
	"GET /api/bench/{id}":                        clsWork,
	"GET /api/guardbench":                        clsAdmin,
	"POST /api/guardbench":                       clsAdmin,
	"GET /api/records":                           clsAdmin,
	"GET /api/records/{day}/{file}/{offset}":     clsAdmin,
	"DELETE /api/records":                        clsAdmin,
	"GET /api/secrets":                           clsAdmin,
	"PUT /api/secrets/{name}":                    clsAdmin,
	"DELETE /api/secrets/{name}":                 clsAdmin,
	"GET /api/search":                            clsWatch,
	"POST /api/prune":                            clsAdmin,
	"GET /api/sessions/{id}/transcript":          clsWatch,
	"GET /api/batches":                           clsWatch,
	"POST /api/batches":                          clsWork,
	"GET /api/batches/{id}":                      clsWatch,
	"POST /api/batches/{id}/{action}":            clsWork,
	"GET /api/voice":                             clsChat,
	"POST /api/voice/transcribe":                 clsChat,
	"GET /api/sessions":                          clsWatch,
	"POST /api/sessions":                         clsWork,
	"GET /api/sessions/{id}":                     clsWatch,
	"GET /api/sessions/{id}/diff":                clsWatch,
	"GET /api/sessions/{id}/stream":              clsWatch,
	"GET /api/sessions/{id}/archive":             clsWatch,
	"GET /api/sessions/{id}/preview":             clsWatch,
	"POST /api/sessions/{id}/preview/start":      clsWork,
	"POST /api/sessions/{id}/preview/stop":       clsWork,
	"GET /api/fs":                                clsAdmin,
	"POST /api/sessions/{id}/{action}":           clsWork,
	"DELETE /api/sessions/{id}":                  clsWork,
	"GET /api/events":                            clsAny,
	"GET /api/monitor":                           clsMon,
	"GET /api/monitor/processes":                 clsMon,
	"GET /api/monitor/series":                    clsMon,
	"GET /api/monitor/stream":                    clsMon,
	"POST /api/monitor/processes/{pid}/{action}": clsAdmin,
	"/api/": clsAny,

	"PATCH /api/sessions/{id}": clsWork,
	"POST /api/sessions/bulk":  clsWork,
	"GET /api/feed":            clsWatch,
	"GET /api/feed/stream":     clsWatch,
	"GET /api/wall/stream":     clsWatch,

	"POST /api/agent/spawn":              clsAgent,
	"GET /api/agent/children":            clsAgent,
	"GET /api/agent/models":              clsAgent,
	"POST /api/agent/ask":                clsAgent,
	"POST /api/agent/rate":               clsAgent,
	"GET /api/agent/sessions/{id}/tail":  clsAgent,
	"POST /api/agent/sessions/{id}/send": clsAgent,
	"GET /api/agent/sessions/{id}/wait":  clsAgent,
	"POST /api/agent/sessions/{id}/stop": clsAgent,
	"GET /api/agent/files":               clsAgent,
	"GET /api/agent/files/raw":           clsAgent,
	"GET /api/agent/runs":                clsAgent,
	"GET /api/agent/runs/{name}":         clsAgent,
	"/api/agent/":                        clsAgent,

	"POST /api/sessions/{id}/files":       clsWork,
	"GET /api/sessions/{id}/files":        clsWatch,
	"GET /api/sessions/{id}/files/raw":    clsWatch,
	"DELETE /api/sessions/{id}/files":     clsWork,
	"POST /api/sessions/{id}/files/mkdir": clsWork,
	"POST /api/uploads":                   clsWork,
	"GET /api/uploads/{staging}":          clsWork,
	"DELETE /api/uploads/{staging}":       clsWork,

	"GET /api/audit":           clsAdmin,
	"GET /api/devices":         clsAny,
	"POST /api/devices":        clsAny,
	"DELETE /api/devices/{id}": clsAny,
	"POST /api/token/rotate":   clsAdmin,
	"GET /api/doctor":          clsMon,

	"GET /api/config":            clsAdmin,
	"GET /api/config/schema":     clsAdmin,
	"PATCH /api/config":          clsAdmin,
	"POST /api/config/restart":   clsAdmin,
	"POST /api/config/init-done": clsAdmin,

	"GET /api/chats":                       clsChat,
	"POST /api/chats":                      clsChat,
	"GET /api/chats/models":                clsChat,
	"GET /api/chats/search":                clsChat,
	"GET /api/chats/atrium":                clsAdmin,
	"POST /api/chats/atrium/{id}/continue": clsAdmin,
	"GET /api/chats/claude":                clsAdmin,
	"POST /api/chats/claude/{id}/continue": clsAdmin,
	"GET /api/chats/asks":                  clsChat,
	"GET /api/chats/rules":                 clsChat,
	"DELETE /api/chats/rules":              clsChat,
	"GET /api/chats/{id}":                  clsChat,
	"PATCH /api/chats/{id}":                clsChat,
	"DELETE /api/chats/{id}":               clsChat,
	"POST /api/chats/{id}/messages":        clsChat,
	"GET /api/chats/{id}/stream":           clsChat,
	"POST /api/chats/{id}/answer":          clsChat,
	"POST /api/chats/{id}/stop":            clsChat,
	"GET /api/chats/{id}/rules":            clsChat,
	"DELETE /api/chats/{id}/rules":         clsChat,
	"POST /api/quick":                      clsChat,

	"GET /api/inbox":                     clsAny,
	"POST /api/inbox/read":               clsAny,
	"DELETE /api/inbox/{id}":             clsAny,
	"GET /api/sessions/{id}/runs":        clsWatch,
	"GET /api/sessions/{id}/setup":       clsWatch,
	"GET /api/sessions/{id}/trajectory":  clsWork,
	"GET /api/sessions/{id}/runs/{name}": clsWatch,
	"GET /api/downloads":                 clsAdmin,
	"POST /api/downloads":                clsAdmin,
	"DELETE /api/downloads/{id}":         clsAdmin,
	"GET /api/leaderboard":               clsWatch,
	"GET /api/delegation":                clsWatch,
	"GET /api/sessions/{id}/delegation":  clsWatch,
	"GET /api/finetune":                  clsAdmin,
	"GET /api/finetune/export":           clsAdmin,

	"GET /api/finetune/examples":               clsAdmin,
	"GET /api/finetune/examples/{id}":          clsAdmin,
	"POST /api/finetune/examples/{id}/rate":    clsAdmin,
	"POST /api/finetune/examples/{id}/exclude": clsAdmin,
	"POST /api/finetune/train":                 clsAdmin,
	"GET /api/finetune/runs":                   clsAdmin,

	"GET /api/sessions/{id}/flow": clsWatch,
	"GET /api/chats/{id}/flow":    clsChat,

	"GET /api/flows":                      clsWork,
	"POST /api/flows":                     clsWork,
	"GET /api/flows/templates":            clsWork,
	"POST /api/flows/validate":            clsWork,
	"POST /api/flows/test":                clsWork,
	"GET /api/flows/runs":                 clsWork,
	"GET /api/flows/runs/{run}":           clsWork,
	"POST /api/flows/runs/{run}/cancel":   clsWork,
	"POST /api/flows/runs/{run}/approve":  clsWork,
	"POST /api/flows/runs/{run}/continue": clsWork,
	"GET /api/flows/runs/{run}/flow":      clsWork,
	"GET /api/flows/{id}":                 clsWork,
	"PUT /api/flows/{id}":                 clsWork,
	"DELETE /api/flows/{id}":              clsWork,
	"POST /api/flows/{id}/run":            clsWork,

	"GET /api/evals":                     clsWork,
	"POST /api/evals":                    clsWork,
	"POST /api/evals/validate":           clsWork,
	"GET /api/evals/runs":                clsWork,
	"GET /api/evals/runs/{run}":          clsWork,
	"POST /api/evals/runs/{run}/cancel":  clsWork,
	"POST /api/evals/runs/{run}/rejudge": clsWork,
	"GET /api/evals/{id}":                clsWork,
	"PUT /api/evals/{id}":                clsWork,
	"DELETE /api/evals/{id}":             clsWork,
	"POST /api/evals/{id}/run":           clsWork,

	"GET /api/config/export":         clsAdmin,
	"POST /api/config/import":        clsAdmin,
	"POST /api/config/models/forget": clsAdmin,

	// Users and roles (Phase 4a).
	"GET /api/auth":                 clsPublic,
	"POST /api/login":               clsPublic,
	"POST /api/logout":              clsAny,
	"GET /api/me":                   clsAny,
	"POST /api/me/password":         clsAny,
	"GET /api/me/logins":            clsAny,
	"DELETE /api/me/logins/{id}":    clsAny,
	"POST /api/me/apikey":           clsWork,
	"DELETE /api/me/apikey":         clsWork,
	"GET /api/users":                clsAdmin,
	"POST /api/users":               clsAdmin,
	"GET /api/users/{id}":           clsAdmin,
	"PATCH /api/users/{id}":         clsAdmin,
	"POST /api/users/{id}/password": clsAdmin,
	"DELETE /api/users/{id}":        clsAdmin,
	"DELETE /api/users/{id}/apikey": clsAdmin,

	// Saved permissions (permits.go): own work.
	"GET /api/permits":         clsWork,
	"POST /api/permits":        clsWork,
	"DELETE /api/permits/{id}": clsWork,
}

// mustChangeAllowed are the routes a signed-in person who must change their
// password may still use (GET /api/events carries only pings for them).
var mustChangeAllowed = map[string]bool{
	"GET /api/me":           true,
	"POST /api/me/password": true,
	"POST /api/logout":      true,
	"GET /api/auth":         true,
	"GET /api/events":       true,
	"GET /{$}":              true,
}

// classAdmits reports whether a principal may call a route of class c.
func classAdmits(c roleClass, pr principal) bool {
	switch c {
	case clsPublic, clsNotFound:
		return true
	case clsAgent:
		return pr.Kind == kindAgent || pr.Kind == kindClient
	case clsGateway:
		switch pr.Kind {
		case kindAgent, kindAgentShared, kindLocal:
			return true
		}
	}
	if pr.Kind == kindAgent || pr.Kind == kindAgentShared || pr.Kind == kindLocal || pr.Kind == kindClient {
		return false // agents act only in their scope and /v1 (withAuth enforces it too)
	}
	for _, r := range classRoles[c] {
		if r == pr.Role {
			return true
		}
	}
	return false
}

// roleError is the 403 for a role a route's class refuses.
func roleError(w http.ResponseWriter, role string) {
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "Your role (" + orDefault(role, "none") + ") cannot do this.", "code": "role"})
}

// mustChangeError is the 403 for anything but the must-change allow-list.
func mustChangeError(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "Change your password first.", "code": "must_change"})
}

// gate wraps a route's handler with its class check, the must-change gate
// and the object owner check (owner.go). It panics for an unclassified
// pattern (fail closed at startup).
func (s *server) gate(pattern string, next http.Handler) http.Handler {
	c, ok := routeRoles[pattern]
	if !ok {
		panic("api: route " + pattern + " has no role class (internal/api/roles.go routeRoles)")
	}
	if c == clsPublic || c == clsNotFound {
		return next
	}
	objCheck := s.objectCheck(pattern)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr := principalOf(r)
		if pr.Kind == "" {
			// Not admitted by withAuth (tests that call the mux directly):
			// nothing to check here.
			next.ServeHTTP(w, r)
			return
		}
		if !classAdmits(c, pr) {
			if c == clsGateway {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"message":"your role cannot use the gateway","type":"permission_error","code":"role"}}`))
				return
			}
			roleError(w, pr.Role)
			return
		}
		if pr.Must && !mustChangeAllowed[pattern] {
			mustChangeError(w)
			return
		}
		if objCheck != nil && !objCheck(w, r, pr) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// patternMethod is the method of a pattern ("" when it names none).
func patternMethod(p string) string {
	if m, _, ok := strings.Cut(p, " "); ok {
		return m
	}
	return ""
}
