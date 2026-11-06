package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/sandbox"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// Owner scoping (docs/SCOPE.md "Owner scoping"). A user sees an object
// when its (resolved) owner is the user, or the role is admin, or the role
// is viewer and the object is a session-family one. Writes need the owner
// or an admin (chats: see objectCheck). An object the caller cannot see is
// 404 {"error": "not found"}, never 403, so ids are not confirmed.

// hasUsers reports whether multi-user mode is on (at least one account).
func (s *server) hasUsers() bool { return s.users != nil && s.users.HasUsers() }

// legacyOwner is who owns data from before users ("" before users exist).
func (s *server) legacyOwner() string {
	if s.users == nil {
		return ""
	}
	return s.users.Owner("")
}

// ownerOf resolves a stored owner id to the current owner.
func (s *server) ownerOf(raw string) string {
	if s.users == nil {
		return raw
	}
	return s.users.Owner(raw)
}

// ownerName is the user name of a stored owner id ("" when none).
func (s *server) ownerName(raw string) string {
	if !s.hasUsers() {
		return ""
	}
	if u, ok := s.users.Get(s.users.Owner(raw)); ok {
		return u.Name
	}
	return ""
}

// sameOwner reports whether the object's owner is the caller. Before users
// exist every caller is the owner.
func (s *server) sameOwner(pr principal, raw string) bool {
	if !s.hasUsers() {
		return true
	}
	return pr.User != "" && s.users.Owner(raw) == pr.User
}

// canSeeSession: owner, admin or viewer.
func (s *server) canSeeSession(pr principal, owner string) bool {
	return pr.admin() || pr.Role == users.RoleViewer || s.sameOwner(pr, owner)
}

// canOwn: owner or admin (writes, and reads of non-session objects).
func (s *server) canOwn(pr principal, owner string) bool { return pr.admin() || s.sameOwner(pr, owner) }

// ownFilter is a predicate over stored owner ids: the caller's own data
// (all data for admins when all is true).
func (s *server) ownFilter(pr principal, all bool) func(string) bool {
	if all && pr.admin() || !s.hasUsers() {
		return nil
	}
	return func(raw string) bool { return s.sameOwner(pr, raw) }
}

// creator is the owner id stored on new objects: the caller's resolved user.
func creator(pr principal) string { return pr.User }

// notFound answers an object the caller may not see.
func notFound(w http.ResponseWriter) { writeError(w, http.StatusNotFound, "not found") }

// decorate fills the session's current owner and name for replies.
func (s *server) decorate(info session.Info) session.Info {
	if s.hasUsers() {
		info.Owner = s.users.Owner(info.Owner)
		info.OwnerName = s.ownerName(info.Owner)
	}
	return info
}

// objectCheck returns the owner check for a pattern's object (nil: none).
// It runs after the class check; an object that does not exist is left to
// the handler (which answers 404 as before).
func (s *server) objectCheck(pattern string) func(http.ResponseWriter, *http.Request, principal) bool {
	method := patternMethod(pattern)
	_, p, _ := strings.Cut(pattern, " ")
	if p == "" {
		p = pattern
	}
	write := method != "" && !safeMethod(method)
	switch {
	case strings.HasPrefix(p, "/api/sessions/{id}") || strings.HasPrefix(p, "/preview/{id}"):
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.sessions == nil {
				return true
			}
			info, ok := s.sessions.Get(r.PathValue("id"))
			if !ok {
				return true
			}
			allowed := s.canSeeSession(pr, info.Owner)
			if write {
				allowed = s.canOwn(pr, info.Owner)
			}
			if !allowed {
				notFound(w)
			}
			return allowed
		}
	case strings.HasPrefix(p, "/api/batches/{id}"):
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.fleet == nil {
				return true
			}
			bi, _, ok := s.fleet.Batch(r.PathValue("id"))
			if !ok {
				return true
			}
			allowed := s.canSeeSession(pr, bi.Owner)
			if write {
				allowed = s.canOwn(pr, bi.Owner)
			}
			if !allowed {
				notFound(w)
			}
			return allowed
		}
	case strings.HasPrefix(p, "/api/chats/{id}"):
		// Admins may read, delete and stop any chat; only the owner sends,
		// answers, renames or changes its rules.
		adminOK := !write || pattern == "DELETE /api/chats/{id}" || pattern == "POST /api/chats/{id}/stop"
		if pattern == "GET /api/chats/{id}/rules" {
			adminOK = false
		}
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.chats == nil {
				return true
			}
			owner, ok := s.chats.Owner(r.PathValue("id"))
			if !ok {
				return true
			}
			allowed := s.sameOwner(pr, owner) || (adminOK && pr.admin())
			if !allowed {
				notFound(w)
			}
			return allowed
		}
	case strings.HasPrefix(p, "/api/flows/runs/{run}"):
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.flows == nil {
				return true
			}
			d, err := s.flows.GetRun(r.PathValue("run"))
			if err != nil {
				return true
			}
			if !s.canOwn(pr, d.Run.Owner) {
				notFound(w)
				return false
			}
			return true
		}
	case strings.HasPrefix(p, "/api/flows/{id}"):
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.flows == nil {
				return true
			}
			f, err := s.flows.Get(r.PathValue("id"))
			if err != nil {
				return true
			}
			if !s.canOwn(pr, f.Owner) {
				notFound(w)
				return false
			}
			return true
		}
	case strings.HasPrefix(p, "/api/evals/runs/{run}"):
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.evals == nil {
				return true
			}
			owner, ok := s.evals.RunOwner(r.PathValue("run"))
			if !ok {
				return true
			}
			if !s.canOwn(pr, owner) {
				notFound(w)
				return false
			}
			return true
		}
	case strings.HasPrefix(p, "/api/evals/{id}"):
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.evals == nil {
				return true
			}
			e, err := s.evals.Get(r.PathValue("id"))
			if err != nil {
				return true
			}
			if !s.canOwn(pr, e.Owner) {
				notFound(w)
				return false
			}
			return true
		}
	case strings.HasPrefix(p, "/api/uploads/{staging}"):
		return func(w http.ResponseWriter, r *http.Request, pr principal) bool {
			if s.files == nil {
				return true
			}
			if !s.canOwn(pr, s.files.StagingOwner(r.PathValue("staging"))) {
				notFound(w)
				return false
			}
			return true
		}
	}
	return nil
}

// ---------- confinement of non-admin sessions ----------

// confineError is a refused confined start: 403 with a code, or 503 for
// the sandbox.
type confineError struct {
	status int
	code   string
	msg    string
}

func (e *confineError) Error() string { return e.msg }

func writeConfine(w http.ResponseWriter, e *confineError) {
	writeJSON(w, e.status, map[string]string{"error": e.msg, "code": e.code})
}

// sandboxCheck is session.SandboxAvailable; tests swap it.
var sandboxCheck = session.SandboxAvailable

// confine applies the rules for a non-admin owner to a task (workspace,
// agent, forced sandbox) and marks it confined. Admins pass unchanged.
func (s *server) confine(r *http.Request, pr principal, t *session.TaskSpec) *confineError {
	t.Owner = creator(pr)
	if pr.admin() || !s.hasUsers() {
		return nil
	}
	return confineTask(r.Context(), s.users, s.conf().DefaultAgent, t)
}

// confineTask applies the non-admin rules for t.Owner (a resolved user id
// whose role is not admin) to t: scratch, or a repo shared with them; an
// allowed agent profile; bwrap available. It marks t confined.
func confineTask(ctx context.Context, st *users.Store, defaultAgent string, t *session.TaskSpec) *confineError {
	u, ok := st.Get(t.Owner)
	if !ok || u.Disabled {
		return &confineError{http.StatusForbidden, "role", "no such user"}
	}
	if u.Role == users.RoleAdmin {
		return nil
	}
	if !users.Can(u.Role, users.CapSessions) {
		return &confineError{http.StatusForbidden, "role", "Your role (" + u.Role + ") cannot do this."}
	}
	dir := orDefault(t.Dir, t.Repo)
	switch {
	case t.Workspace == session.WorkspaceSame:
		// A spawned child stays in its parent's (already checked) workspace.
	case t.Workspace == session.WorkspaceFolder:
		return &confineError{http.StatusForbidden, "workspace", "Folder workspaces are for admins only: use a scratch folder or a repo shared with you."}
	case dir == "" && (t.Workspace == "" || t.Workspace == session.WorkspaceScratch):
		t.Workspace, t.Dir, t.Repo = session.WorkspaceScratch, "", ""
	default:
		if _, ok := u.Share(dir); !ok || (t.Workspace != "" && t.Workspace != session.WorkspaceRepo) {
			return &confineError{http.StatusForbidden, "workspace", "This repo is not shared with you."}
		}
		t.Workspace, t.Dir, t.Repo = session.WorkspaceRepo, dir, ""
	}
	// No agent named: the server default when this user may use it, else
	// the first profile they may use (the built-in agent without a list).
	if t.Agent == "" && !u.AllowsAgent(defaultAgent) {
		t.Agent = users.DefaultAgent
		if len(u.Agents) > 0 {
			t.Agent = u.Agents[0]
		}
	}
	agentOK := func(name string) bool {
		if name == "" {
			name = defaultAgent
		}
		return u.AllowsAgent(name)
	}
	if !agentOK(t.Agent) || (t.EscalateTo != "" && !agentOK(t.EscalateTo)) {
		return &confineError{http.StatusForbidden, "agent", "You may not use this agent profile."}
	}
	if err := sandboxCheck(ctx); err != nil {
		return &confineError{http.StatusServiceUnavailable, "sandbox",
			"This server cannot sandbox sessions for other users: bubblewrap (bwrap) is missing. Ask the admin to install it."}
	}
	t.Confined = true
	return nil
}

// ---------- budgets ----------

// monthStart is the first of the current month, local time.
func monthStart(now time.Time) time.Time {
	y, m, _ := now.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, now.Location())
}

// userSpend is a user's spend and tokens this month (data moved to them
// included).
func (s *server) userSpend(id string) usage.UserSum {
	src, ok := s.usage.(interface {
		ByUser(time.Time) map[string]usage.UserSum
	})
	if !ok || id == "" {
		return usage.UserSum{}
	}
	return spendOf(src.ByUser, s.ownerOf, id)
}

// spendOf sums this month's usage of every stored user id resolving to id.
func spendOf(byUser func(time.Time) map[string]usage.UserSum, owner func(string) string, id string) usage.UserSum {
	var sum usage.UserSum
	for raw, v := range byUser(monthStart(time.Now())) {
		if owner(raw) == id {
			sum.CostUSD += v.CostUSD
			sum.Tokens += v.Tokens
		}
	}
	return sum
}

// budgetMessage is the refusal for a user over their monthly budget ("",
// false when under it or without a budget).
func budgetMessage(u users.User, sp usage.UserSum) (string, bool) {
	if u.Budget.MonthlyUSD > 0 && sp.CostUSD >= u.Budget.MonthlyUSD {
		return fmt.Sprintf("Your monthly budget is used up (US$%.2f of US$%.2f). Ask an admin to raise it.",
			sp.CostUSD, u.Budget.MonthlyUSD), true
	}
	if u.Budget.MonthlyTokens > 0 && sp.Tokens >= u.Budget.MonthlyTokens {
		return fmt.Sprintf("Your monthly budget is used up (%d of %d tokens). Ask an admin to raise it.",
			sp.Tokens, u.Budget.MonthlyTokens), true
	}
	return "", false
}

// budgetNotes remembers the 80 % notices sent this month (user|month).
type budgetNotes struct {
	mu   sync.Mutex
	sent map[string]bool
}

// overBudget reports whether owner (a user id) has used their monthly
// budget, with the message to show. Admins with a budget are capped too.
func (s *server) overBudget(owner string) (string, bool) {
	if !s.hasUsers() || owner == "" {
		return "", false
	}
	u, ok := s.users.Get(s.ownerOf(owner))
	if !ok || (u.Budget.MonthlyUSD <= 0 && u.Budget.MonthlyTokens <= 0) {
		return "", false
	}
	sp := s.userSpend(u.ID)
	if msg, over := budgetMessage(u, sp); over {
		return msg, true
	}
	s.budgetWarn(u, sp)
	return "", false
}

// budgetWarn posts one inbox item per month when a user passes 80 %.
func (s *server) budgetWarn(u users.User, sp usage.UserSum) {
	near := (u.Budget.MonthlyUSD > 0 && sp.CostUSD >= 0.8*u.Budget.MonthlyUSD) ||
		(u.Budget.MonthlyTokens > 0 && float64(sp.Tokens) >= 0.8*float64(u.Budget.MonthlyTokens))
	if !near || s.inbox == nil {
		return
	}
	key := u.ID + "|" + monthStart(time.Now()).Format("2006-01")
	s.budgetHit.mu.Lock()
	if s.budgetHit.sent == nil {
		s.budgetHit.sent = map[string]bool{}
	}
	done := s.budgetHit.sent[key]
	s.budgetHit.sent[key] = true
	s.budgetHit.mu.Unlock()
	if done {
		return
	}
	s.inbox.Add(inbox.Item{Kind: "budget", Level: inbox.LevelWarn, Owner: u.ID, Link: "#/account",
		Title: "80 % of your monthly budget is used",
		Body:  fmt.Sprintf("US$%.2f and %d tokens so far this month.", sp.CostUSD, sp.Tokens)})
}

// budgetRefusedQuiet reports whether owner is over budget, writing nothing.
func (s *server) budgetRefusedQuiet(owner string) bool {
	_, over := s.overBudget(owner)
	return over
}

// budgetRefused writes the API's 429 when owner is over budget and
// reports whether it did.
func (s *server) budgetRefused(w http.ResponseWriter, owner string) bool {
	msg, over := s.overBudget(owner)
	if over {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": msg, "code": "budget"})
	}
	return over
}

// ---------- chat access per role ----------

// chatAccessor decides what a chat owner's runs may do.
type chatAccessor struct {
	users *users.Store
	cfg   func() *config.Config
}

// ChatAccess is chat.Options.Access for the daemon (docs/SCOPE.md "Chats
// of non-admin users"): admins get full access; role user works only in
// DATA/users/<id>/files (Jail) with run_command inside bwrap (none
// without bwrap); role chat (and anyone else) gets plain chat, no tools.
func ChatAccess(st *users.Store, cfg func() *config.Config) func(string) chat.Access {
	a := chatAccessor{users: st, cfg: cfg}
	return a.access
}

// ChatIsAdmin is chat.Options.IsAdmin: whether a chat's owner is an admin
// (everyone is before users exist).
func ChatIsAdmin(st *users.Store) func(string) bool {
	return func(owner string) bool {
		if st == nil || !st.HasUsers() {
			return true
		}
		u, ok := st.Get(st.Owner(owner))
		return ok && !u.Disabled && u.Role == users.RoleAdmin
	}
}

func (a chatAccessor) access(owner string) chat.Access {
	if a.users == nil || !a.users.HasUsers() {
		return chat.Access{Admin: true}
	}
	u, ok := a.users.Get(a.users.Owner(owner))
	if !ok || u.Disabled {
		return chat.Access{NoTools: true, Cwd: users.FilesDir(a.users.Dir(), "u-nobody")}
	}
	dir := users.FilesDir(a.users.Dir(), u.ID)
	switch u.Role {
	case users.RoleAdmin:
		return chat.Access{Admin: true}
	case users.RoleUser:
		return chat.Access{Cwd: dir, Jail: true, Wrap: a.wrap(dir)}
	}
	return chat.Access{NoTools: true, Cwd: dir}
}

// sandboxBin finds a usable bwrap (sandbox.Available); tests swap it.
var sandboxBin = sandbox.Available

// wrap runs a chat's commands in bwrap with only dir writable and
// Cerberus' data and config folders and the runtime folder hidden; nil
// when bwrap is not usable (then run_command is not offered).
func (a chatAccessor) wrap(dir string) func([]string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bin, err := sandboxBin(ctx)
	if err != nil {
		return nil
	}
	cfg := config.Defaults()
	if a.cfg != nil {
		if c := a.cfg(); c != nil {
			cfg = c
		}
	}
	home, _ := os.UserHomeDir()
	hide := []string{a.users.Dir()}
	if cfg.Path() != "" {
		hide = append(hide, cfg.Dir())
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		hide = append(hide, rt)
	}
	spec := sandbox.Spec{Home: home, Workspace: dir, Network: cfg.Sandbox.Network, ReadOnlyHome: true,
		ExtraRO: cfg.Sandbox.ExtraRO, HideHome: cfg.Sandbox.HideHome, Hide: hide, Confined: true}
	return func(argv []string) []string { return sandbox.Wrap(bin, spec, argv) }
}

// ---------- events filtering ----------

// visibleInbox is the inbox predicate for a caller: own items, plus
// system items ("" owner) for admins.
func (s *server) visibleInbox(pr principal) func(inbox.Item) bool {
	if !s.hasUsers() {
		return nil
	}
	return func(it inbox.Item) bool { return s.inboxOwnerVisible(pr, it.Owner) }
}

func (s *server) inboxOwnerVisible(pr principal, owner string) bool {
	if !s.hasUsers() {
		return true
	}
	if owner == "" {
		return pr.admin()
	}
	return s.users.Owner(owner) == pr.User
}

// flowVisible reports whether a flow event or object reaches the caller.
func (s *server) flowVisible(pr principal, owner string) bool { return s.canOwn(pr, owner) }

// decorateFlow fills owner names on flow objects.
func (s *server) decorateFlow(f *flows.Flow) {
	if s.hasUsers() {
		f.Owner = s.users.Owner(f.Owner)
		f.OwnerName = s.ownerName(f.Owner)
	}
}

func (s *server) decorateRun(r *flows.Run) {
	if s.hasUsers() {
		r.Owner = s.users.Owner(r.Owner)
		r.OwnerName = s.ownerName(r.Owner)
	}
}

// ownersOf lists the stored owner ids that resolve to id (for filters that
// match stored ids).
func (s *server) ownersOf(id string) []string {
	if s.users == nil {
		return []string{id}
	}
	out := s.users.MovedFrom(id)
	if !slices.Contains(out, id) {
		out = append(out, id)
	}
	return out
}
