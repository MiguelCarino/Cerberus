package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// Status is the JSON of GET /api/status.
type Status struct {
	Version  string                `json:"version"`
	Build    string                `json:"build,omitempty"`
	Host     string                `json:"host"`
	Uptime   int64                 `json:"uptime"` // seconds
	GPU      gpu.Snapshot          `json:"gpu"`
	Models   []engine.ModelInfo    `json:"models"`
	Sessions map[string]int        `json:"sessions"` // count per state
	Guard    GuardStatus           `json:"guard"`
	Agents   []session.AgentStatus `json:"agents"`
	Repos    []Repo                `json:"repos"`

	VaultBackend string      `json:"vault_backend"` // "secret-service" | "file" | "" (no vault)
	Voice        VoiceStatus `json:"voice"`
	TLS          bool        `json:"tls"`           // HTTPS is configured for the extra listen addresses
	DefaultAgent string      `json:"default_agent"` // preselected in the task dialogs
	Files        FilesStatus `json:"files"`
	// FlowsEnabled is false when there is no flows engine or flows.enabled
	// is off (runs would be refused).
	FlowsEnabled bool `json:"flows_enabled"`
}

// FilesStatus is what the web UI needs to know about file drop: the inbox
// folder name and the per-file limit (so too-large files are refused
// before they are sent).
type FilesStatus struct {
	UploadDir   string `json:"upload_dir"`
	MaxFileMiB  int    `json:"max_file_mib"`
	MaxTotalMiB int    `json:"max_total_mib"`
}

// VoiceStatus says whether the mic button can work.
type VoiceStatus struct {
	Available bool   `json:"available"`
	Backend   string `json:"backend"`
}

// GuardStatus is the resource guard's view.
type GuardStatus struct {
	MemAvailableMiB int `json:"mem_available_mib"` // -1 = unknown
	MaxSessions     int `json:"max_sessions"`
	Running         int `json:"running"`
	// MemoryEnforced is false when runs get no systemd scope (not Linux, no
	// slice configured, no systemd-run, or no user bus): a session's
	// MemoryMax is then not applied. SessionMemoryMax is the default cap.
	MemoryEnforced   bool   `json:"memory_enforced"`
	SessionMemoryMax string `json:"session_memory_max"`
}

// Repo is one entry of GET /api/repos.
type Repo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Checks  bool   `json:"checks"`
	Preview bool   `json:"preview"`
	Push    bool   `json:"push"` // RepoConfig.Push: default of the accept "push" toggle
}

// liveStates hold a guard slot (mirrors the session manager's rule).
var liveStates = map[string]bool{
	session.StateStarting: true, session.StateRunning: true, session.StateWaiting: true,
	session.StateChecking: true, session.StateRetrying: true, session.StateEscalated: true,
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	st := Status{Version: s.version, Build: s.build, Host: host, Uptime: int64(time.Since(s.started) / time.Second),
		Models: []engine.ModelInfo{}, Sessions: map[string]int{}, Agents: []session.AgentStatus{},
		Guard: GuardStatus{MemAvailableMiB: s.memAvail(), MaxSessions: s.conf().Guard.MaxSessions}}
	if s.gpu != nil {
		st.GPU = s.gpu.Snapshot()
	}
	pr := principalOf(r)
	role := orDefault(pr.Role, "admin")
	if !s.hasUsers() {
		role = "admin"
	}
	work := role == "admin" || role == "user"
	if s.models != nil && (work || role == "chat") {
		st.Models = nonNil(s.withModelCost(s.models.Models(r.Context())))
	}
	if s.sessions != nil {
		for _, in := range s.sessions.List() {
			if liveStates[in.State] {
				st.Guard.Running++
			}
			if (role == "admin" || role == "viewer") || (role == "user" && s.sameOwner(pr, in.Owner)) {
				st.Sessions[in.State]++
			}
		}
		if work {
			st.Agents = nonNil(s.visibleAgents(pr, s.sessions.Agents()))
		}
	}
	st.Repos = []Repo{}
	if work {
		st.Repos = s.visibleRepos(pr)
	}
	if s.secrets != nil {
		st.VaultBackend = s.secrets.Backend()
	}
	if s.voice != nil {
		if ok, name := s.voice.Available(); ok {
			st.Voice = VoiceStatus{Available: true, Backend: name}
		}
	}
	st.Guard.SessionMemoryMax = s.conf().Guard.SessionMemoryMax
	if s.control != nil {
		st.Guard.MemoryEnforced = s.control.ScopesEnforced()
	}
	st.TLS = s.tlsConfigured()
	st.DefaultAgent = s.conf().DefaultAgent
	st.FlowsEnabled = s.flows != nil && s.conf().Flows.Enabled && work
	perFile, total := s.fileLimits()
	st.Files = FilesStatus{UploadDir: session.UploadDir(s.conf()), MaxFileMiB: int(perFile >> 20), MaxTotalMiB: int(total >> 20)}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) handleRepos(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.visibleRepos(principalOf(r)))
}

// visibleRepos is /api/repos for the caller: all for admins, the repos
// shared with them for users.
func (s *server) visibleRepos(pr principal) []Repo {
	all := s.repos()
	if !s.hasUsers() || pr.admin() {
		return all
	}
	u, ok := s.users.Get(pr.User)
	out := []Repo{}
	if !ok {
		return out
	}
	for _, rp := range all {
		if _, shared := u.Share(rp.Path); shared {
			out = append(out, rp)
		}
	}
	return out
}

// visibleAgents keeps the agent profiles a non-admin may use.
func (s *server) visibleAgents(pr principal, all []session.AgentStatus) []session.AgentStatus {
	if !s.hasUsers() || pr.admin() {
		return all
	}
	u, ok := s.users.Get(pr.User)
	out := []session.AgentStatus{}
	if !ok {
		return out
	}
	for _, a := range all {
		if u.AllowsAgent(a.Name) {
			out = append(out, a)
		}
	}
	return out
}

func (s *server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		unavailable(w, "usage store")
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = min(max(v, 1), 1000)
	}
	now := time.Now()
	pr := principalOf(r)
	if s.hasUsers() && !pr.admin() {
		// Own records and spend only; no global budgets or plans.
		var mine []usage.Record
		for _, rec := range s.usage.Recent(0) {
			if s.ownerOf(rec.User) == pr.User {
				mine = append(mine, rec)
				if len(mine) >= limit {
					break
				}
			}
		}
		sp := s.userSpend(pr.User)
		writeJSON(w, http.StatusOK, map[string]any{
			"recent": nonNil(mine),
			"spend":  map[string]any{"month_usd": sp.CostUSD, "month_tokens": sp.Tokens},
		})
		return
	}
	res := map[string]any{
		"recent":  nonNil(s.usage.Recent(limit)),
		"agg_24h": nonNil(s.usage.Aggregate(now.Add(-24 * time.Hour))),
		"spend":   s.spendSummary(now),
		"budgets": s.budgetList(),
		"plans":   s.planList(),
	}
	if s.hasUsers() {
		type userSpend struct {
			ID          string       `json:"id"`
			Name        string       `json:"name"`
			SpentUSD    float64      `json:"spent_usd"`
			SpentTokens int64        `json:"spent_tokens"`
			Budget      users.Budget `json:"budget"`
		}
		list := []userSpend{}
		for _, u := range s.users.List() {
			sp := s.userSpend(u.ID)
			list = append(list, userSpend{ID: u.ID, Name: u.Name, SpentUSD: sp.CostUSD, SpentTokens: sp.Tokens, Budget: u.Budget})
		}
		res["users"] = list
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- models ----------

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		unavailable(w, "engine")
		return
	}
	writeJSON(w, http.StatusOK, nonNil(s.withModelCost(s.models.Models(r.Context()))))
}

// handleModelAction serves POST /api/models/<id>/{load,unload,delete}. The id may
// contain "/" (provider models), so the action is the last segment.
func (s *server) handleModelAction(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		writeError(w, http.StatusNotFound, "expected /api/models/<id>/load or /unload")
		return
	}
	id, action := rest[:i], rest[i+1:]
	if s.models == nil {
		unavailable(w, "engine")
		return
	}
	var fn func(context.Context, string) error
	switch action {
	case "load":
		fn = s.models.Load
	case "unload":
		fn = s.models.Unload
	case "delete":
		// Removes the model's files from disk (unloaded first). Refused for
		// cloud models and while requests run on it.
		noteAudit(r, "delete model "+id, id)
		fn = s.models.Delete
	default:
		writeError(w, http.StatusNotFound, "unknown model action "+strconv.Quote(action))
		return
	}
	if err := fn(r.Context(), id); err != nil {
		writeError(w, modelErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func modelErrStatus(err error) int {
	switch {
	case errors.Is(err, engine.ErrUnknownModel):
		return http.StatusNotFound
	case errors.Is(err, engine.ErrNoRoom), errors.Is(err, engine.ErrInUse):
		return http.StatusConflict
	case errors.Is(err, engine.ErrNotDeletable):
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// ---------- repos ----------

// repos lists configured repos, then git repos directly under each repo
// root, skipping hidden folders and anything a "**/<name>/**" deny pattern
// of the built-in agent names (so private folders are never offered).
func (s *server) repos() []Repo {
	out := []Repo{}
	seen := map[string]bool{}
	for _, rc := range s.conf().Repos {
		p := filepath.Clean(rc.Path)
		if seen[p] {
			continue
		}
		seen[p] = true
		name := rc.Name
		if name == "" {
			name = filepath.Base(p)
		}
		out = append(out, Repo{Name: name, Path: p, Checks: len(rc.Checks) > 0, Preview: rc.Preview != nil, Push: rc.Push})
	}
	denied := deniedNames(s.conf().Agent.DenyPaths)
	var found []Repo
	for _, root := range s.conf().RepoRoots {
		for _, rp := range scanRoot(root, denied) {
			if !seen[rp.Path] {
				seen[rp.Path] = true
				found = append(found, rp)
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return strings.ToLower(found[i].Name) < strings.ToLower(found[j].Name) })
	return append(out, found...)
}

func scanRoot(root string, denied map[string]bool) []Repo {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Repo
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || denied[name] {
			continue
		}
		dir := filepath.Join(filepath.Clean(root), name)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}
		checks, preview := cerberusFile(dir)
		out = append(out, Repo{Name: name, Path: dir, Checks: checks, Preview: preview})
	}
	return out
}

// cerberusFile reports whether <dir>/.cerberus.json declares checks or a preview.
func cerberusFile(dir string) (checks, preview bool) {
	raw, err := os.ReadFile(filepath.Join(dir, ".cerberus.json"))
	if err != nil {
		return false, false
	}
	var f struct {
		Checks  []string        `json:"checks"`
		Preview json.RawMessage `json:"preview"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return false, false
	}
	return len(f.Checks) > 0, len(f.Preview) > 0 && string(f.Preview) != "null"
}

// deniedNames extracts NAME from deny globs shaped "**/NAME/**".
func deniedNames(globs []string) map[string]bool {
	out := map[string]bool{}
	for _, g := range globs {
		if name, ok := strings.CutPrefix(g, "**/"); ok {
			if name, ok = strings.CutSuffix(name, "/**"); ok && name != "" && !strings.ContainsAny(name, "/*?[") {
				out[name] = true
			}
		}
	}
	return out
}

// nonNil makes empty lists encode as [] instead of null.
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

var _ usageSource = (*usage.Store)(nil)
