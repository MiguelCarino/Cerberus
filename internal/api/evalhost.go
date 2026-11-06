package api

// The daemon side of the eval engine (internal/evals), as flowhost.go is
// for flows:
//
//	ModelCaller  one judge call through the in-process gateway, recorded by
//	             usage with client "cerberus-eval" and the run id as session,
//	             at temperature 0 with at most 600 tokens; thinking is turned
//	             off for local models (chat_template_kwargs, reasoning_effort)
//	Sessions     the session manager: one agent session per setup and
//	             repeat, no parent, tagged "eval" and the eval id, owned by
//	             the person who started the run (confined like their own
//	             sessions), never auto-accepted; a repo eval's base commit is
//	             pinned (TaskSpec.Base). Wait follows the session event
//	             stream. Usage sums the gateway records under the session id
//	             (tokens, cost) and its wall-side energy.
//	Catalog      the engine's model list, configured models and providers,
//	             the agent profiles, evals.personas
//	Publisher    EvalFeed, which the event stream (/api/events) reads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/evals"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// EvalSessions is what the eval host needs of the session manager
// (*session.Manager satisfies it).
type EvalSessions interface {
	FlowSessions
	Transcript(id string) ([]byte, error)
	Diff(id string) (string, error)
	MaskSecrets(strs []string) []string
	Agents() []session.AgentStatus
}

// EvalHostDeps are the daemon services behind the eval engine; any may be
// nil (runs then fail with a clear error).
type EvalHostDeps struct {
	Config   func() *config.Config
	Gateway  http.Handler
	Usage    *usage.Store
	Sessions EvalSessions
	Models   func(ctx context.Context) []engine.ModelInfo
	Audit    *audit.Logger
	Feed     *EvalFeed
	Users    *users.Store
}

// EvalHost implements the eval engine's dependencies.
type EvalHost struct {
	d      EvalHostDeps
	claims *usageClaims

	useMu sync.Mutex
	use   map[string]*evals.Usage // gateway usage of the sessions evals started

	catMu   sync.Mutex
	catAt   time.Time
	catList []engine.ModelInfo
}

// NewEvalHost wires the host and registers its usage hooks.
func NewEvalHost(d EvalHostDeps) *EvalHost {
	h := &EvalHost{d: d, claims: newUsageClaims(evals.ClientName), use: map[string]*evals.Usage{}}
	if d.Usage != nil {
		d.Usage.OnAdd(h.claims.add)
		d.Usage.OnAdd(h.addSessionUsage)
	}
	return h
}

// Options fills the engine options with this host's implementations; dir
// is DATA/evals.
func (h *EvalHost) Options(dir string) evals.Options {
	o := evals.Options{Dir: dir, Catalog: evalCatalog{h}, Models: evalModels{h}, Settings: h.settings}
	if h.d.Sessions != nil {
		o.Sessions = evalSessions{h}
	}
	if h.d.Audit != nil {
		o.Auditor = h.d.Audit
	}
	if h.d.Feed != nil {
		o.Events = h.d.Feed
	}
	return o
}

func (h *EvalHost) conf() *config.Config {
	if h.d.Config != nil {
		if c := h.d.Config(); c != nil {
			return c
		}
	}
	return config.Defaults()
}

func (h *EvalHost) settings() evals.Settings {
	c := h.conf()
	return evals.Settings{Enabled: c.Evals.Enabled, JudgeModel: c.Evals.JudgeModel, DefaultModel: c.Engines.DefaultModel,
		MaxParallel: c.Evals.MaxParallel, EvidenceKB: c.Evals.EvidenceKB, Personas: c.Evals.Personas}
}

func (h *EvalHost) owner(raw string) string {
	if h.d.Users == nil {
		return raw
	}
	return h.d.Users.Owner(raw)
}

func (h *EvalHost) ownerUser(raw string) (users.User, bool) {
	if h.d.Users == nil || !h.d.Users.HasUsers() {
		return users.User{}, false
	}
	return h.d.Users.Get(h.d.Users.Owner(raw))
}

func (h *EvalHost) budgetErr(raw string) error {
	u, ok := h.ownerUser(raw)
	if !ok || h.d.Usage == nil {
		return nil
	}
	if msg, over := budgetMessage(u, spendOf(h.d.Usage.ByUser, h.owner, u.ID)); over {
		return errors.New(msg)
	}
	return nil
}

func (h *EvalHost) modelList() []engine.ModelInfo {
	if h.d.Models == nil {
		return nil
	}
	h.catMu.Lock()
	defer h.catMu.Unlock()
	if h.catList != nil && time.Since(h.catAt) < 15*time.Second {
		return h.catList
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list := h.d.Models(ctx)
	if list == nil {
		list = []engine.ModelInfo{}
	}
	h.catList, h.catAt = list, time.Now()
	return list
}

// localModel reports whether the engine lists id as a local model.
func (h *EvalHost) localModel(id string) bool {
	for _, m := range h.modelList() {
		if m.ID == id {
			return m.Local
		}
	}
	return false
}

// ---------- events ----------

// EvalFeed fans run events out to the event streams (evalrun events on
// /api/events). Publish never blocks: a subscriber whose buffer is full is
// dropped (its channel closed) and its stream sends {"what":"evals"}.
type EvalFeed struct {
	mu   sync.Mutex
	subs map[chan evals.Event]struct{}
}

// NewEvalFeed returns an empty feed.
func NewEvalFeed() *EvalFeed { return &EvalFeed{subs: map[chan evals.Event]struct{}{}} }

// Publish sends ev to every subscriber (evals.Publisher).
func (f *EvalFeed) Publish(ev evals.Event) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- ev:
		default:
			delete(f.subs, ch)
			close(ch)
		}
	}
}

// Subscribe streams events until cancel is called.
func (f *EvalFeed) Subscribe() (<-chan evals.Event, func()) {
	ch := make(chan evals.Event, feedBuffer)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[ch]; ok {
			delete(f.subs, ch)
			close(ch)
		}
	}
}

// ---------- the judge ----------

type evalModels struct{ h *EvalHost }

// Complete runs one judge call through the gateway: loaded, queued,
// priced, budgeted and recorded like any request.
func (m evalModels) Complete(ctx context.Context, req evals.ModelRequest) (evals.ModelReply, error) {
	h := m.h
	if h.d.Gateway == nil {
		return evals.ModelReply{}, errors.New("the gateway is not available")
	}
	if err := h.budgetErr(req.Owner); err != nil {
		return evals.ModelReply{}, err
	}
	body := map[string]any{"model": req.Model, "messages": req.Messages, "stream": false, "temperature": 0}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if h.localModel(req.Model) {
		// Thinking off, as the bench does; cloud APIs refuse these fields.
		body["reasoning_effort"] = "none"
		body["chat_template_kwargs"] = map[string]bool{"enable_thinking": false}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return evals.ModelReply{}, err
	}
	hr := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(raw)).WithContext(ctx)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("X-Cerberus-Session", req.Run)
	hr.Header.Set("X-Cerberus-Client", evals.ClientName)
	if o := h.owner(req.Owner); o != "" {
		hr.Header.Set(userHeader, o)
	}
	rec := httptest.NewRecorder()
	h.claims.begin(req.Run)
	h.d.Gateway.ServeHTTP(rec, hr)
	text, model, prompt, completion, err := parseCompletion(rec)
	got, found := h.claims.end(req.Run, prompt, completion)
	if err != nil {
		if ctx.Err() != nil {
			return evals.ModelReply{}, ctx.Err()
		}
		return evals.ModelReply{}, err
	}
	out := evals.ModelReply{Text: text, Model: model, PromptTokens: prompt, CompletionTokens: completion}
	if found {
		if got.Model != "" {
			out.Model = got.Model
		}
		out.CostUSD = got.CostUSD
		out.EnergyWh = got.EnergyJ / 3600
	}
	if out.Model == "" {
		out.Model = req.Model
	}
	return out, nil
}

// ---------- sessions ----------

type evalSessions struct{ h *EvalHost }

// evalSessionTags are "eval" and the eval id (session tags allow only
// [a-z0-9-], at most 24 characters).
func evalSessionTags(eval string) []string {
	out := []string{"eval"}
	var b strings.Builder
	for _, r := range strings.ToLower(eval) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	t := b.String()
	if len(t) > 24 {
		t = t[:24]
	}
	if t = strings.Trim(t, "-"); t != "" && t != "eval" {
		out = append(out, t)
	}
	return out
}

func (s evalSessions) Start(ctx context.Context, req evals.StartRequest) (string, error) {
	h := s.h
	t := session.TaskSpec{Task: req.Task, Agent: req.Agent, Model: req.Model, Count: 1, Name: clipRunes(req.Name, 80),
		Tags: evalSessionTags(req.Eval), Base: req.Base}
	switch req.Workspace {
	case evals.WorkspaceRepo:
		repo, err := resolveRepoName(h.conf(), req.Repo)
		if err != nil {
			return "", err
		}
		t.Workspace, t.Dir, t.Repo = session.WorkspaceRepo, repo, repo
	default:
		t.Workspace, t.Base = session.WorkspaceScratch, ""
	}
	if h.d.Users != nil && h.d.Users.HasUsers() && movedOwner(h.d.Users, req.Owner) {
		return "", errors.New("this run belonged to a removed user")
	}
	t.Owner = h.owner(req.Owner)
	if u, ok := h.ownerUser(req.Owner); ok && u.Role != users.RoleAdmin {
		if ce := confineTask(ctx, h.d.Users, h.conf().DefaultAgent, &t); ce != nil {
			return "", ce
		}
	}
	if err := h.budgetErr(req.Owner); err != nil {
		return "", err
	}
	infos, err := h.d.Sessions.Create(context.WithoutCancel(ctx), t)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", errors.New("no session was created")
	}
	id := infos[0].ID
	h.pruneUse()
	h.useMu.Lock()
	if h.use[id] == nil {
		h.use[id] = &evals.Usage{}
	}
	h.useMu.Unlock()
	return id, nil
}

// pruneUse drops the tracked usage of sessions the manager no longer has
// (removed or pruned), so the map holds at most the sessions
// that exist. A pruned session's Usage falls back to the usage totals.
func (h *EvalHost) pruneUse() {
	h.useMu.Lock()
	ids := make([]string, 0, len(h.use))
	for id := range h.use {
		ids = append(ids, id)
	}
	h.useMu.Unlock()
	var gone []string
	for _, id := range ids {
		if _, ok := h.d.Sessions.Get(id); !ok {
			gone = append(gone, id)
		}
	}
	if len(gone) == 0 {
		return
	}
	h.useMu.Lock()
	for _, id := range gone {
		delete(h.use, id)
	}
	h.useMu.Unlock()
}

func evalInfo(in session.Info) evals.SessionInfo {
	act := in.Activity
	if act == "" {
		act = in.Error
	}
	return evals.SessionInfo{ID: in.ID, State: in.State, Activity: act, Workspace: in.Workspace, Worktree: in.Worktree,
		BaseCommit: in.BaseCommit, Checks: in.ChecksOutput, SetupHash: in.SetupHash, Agent: in.Agent, Model: in.Model,
		RunStarted: in.RunStarted, RunEnded: in.RunEnded, AttemptStarted: in.AttemptStarted}
}

// Wait follows the session event stream until the session is done or
// removed, or ctx ends (as the flow host does).
func (s evalSessions) Wait(ctx context.Context, id string) (evals.SessionInfo, error) {
	m := s.h.d.Sessions
	for {
		ch, cancel := m.Subscribe()
		in, ok := m.Get(id)
		if !ok {
			cancel()
			return evals.SessionInfo{ID: id, State: "removed"}, nil
		}
		if flowSessionFinal(in.State) {
			cancel()
			return evalInfo(in), nil
		}
	follow:
		for {
			select {
			case <-ctx.Done():
				cancel()
				return evals.SessionInfo{}, ctx.Err()
			case ev, open := <-ch:
				if !open {
					break follow
				}
				if ev.Session.ID != id {
					continue
				}
				if ev.Type == "removed" {
					cancel()
					return evals.SessionInfo{ID: id, State: "removed"}, nil
				}
				if flowSessionFinal(ev.Session.State) {
					cancel()
					// Get again: the event's snapshot may predate the checks output.
					if in, ok := m.Get(id); ok {
						return evalInfo(in), nil
					}
					return evalInfo(ev.Session), nil
				}
			}
		}
		cancel()
	}
}

func (s evalSessions) Stop(id string) error { return s.h.d.Sessions.Stop(id) }

// evalResumer is what the feedback loop needs of the session manager
// (*session.Manager has both).
type evalResumer interface {
	Resume(ctx context.Context, id, text string) error
	Record(id, kind, text string)
}

// Resume sends judge feedback to a session as a new attempt in its
// workspace (evals.Resumer) and records it in the session feed as a
// follow-up, "Eval feedback: …". The run's owner is budgeted like a new
// session.
func (s evalSessions) Resume(ctx context.Context, id, text string) error {
	rs, ok := s.h.d.Sessions.(evalResumer)
	if !ok {
		return errors.New("sessions cannot be resumed here")
	}
	if in, ok := s.h.d.Sessions.Get(id); ok {
		if err := s.h.budgetErr(in.Owner); err != nil {
			return err
		}
	}
	if err := rs.Resume(context.WithoutCancel(ctx), id, text); err != nil {
		return err
	}
	rs.Record(id, session.FeedFollowUp, evals.FeedbackPrefix+text)
	return nil
}

func (s evalSessions) Info(id string) (evals.SessionInfo, bool) {
	in, ok := s.h.d.Sessions.Get(id)
	if !ok {
		return evals.SessionInfo{}, false
	}
	return evalInfo(in), true
}

func (s evalSessions) Transcript(id string) ([]byte, error) { return s.h.d.Sessions.Transcript(id) }

func (s evalSessions) Diff(id string) (string, error) { return s.h.d.Sessions.Diff(id) }

// Mask hides the vault's values (and those pinned into live runs), then
// every token shape: judging runs after the session ended, so its own
// injected values (the per-session agent token above all, never in the
// vault) are no longer pinned, and a workspace .env may hold keys the
// vault never had. The judge model may be a cloud one.
func (s evalSessions) Mask(text string) string {
	if out := s.h.d.Sessions.MaskSecrets([]string{text}); len(out) == 1 {
		text = out[0]
	}
	return session.MaskTokens(text)
}

// Usage is the gateway usage recorded under the session (tokens and cost;
// for a session started before this daemon did, tokens from the usage
// totals) and the session's wall-side energy.
func (s evalSessions) Usage(id string) evals.Usage {
	h := s.h
	var out evals.Usage
	h.useMu.Lock()
	u, tracked := h.use[id]
	if tracked {
		out = *u
	}
	h.useMu.Unlock()
	if !tracked && h.d.Usage != nil {
		for _, row := range h.d.Usage.BySession([]string{id}) {
			out.Tokens += row.PromptTokens + row.CompletionTokens
		}
	}
	if in, ok := h.d.Sessions.Get(id); ok {
		out.EnergyWh = in.EnergyJ / 3600
	}
	return out
}

// addSessionUsage is the usage store's hook: records under a session an
// eval started (never the judge's own calls, which carry the run id).
func (h *EvalHost) addSessionUsage(r usage.Record) {
	if r.SessionID == "" || r.Client == evals.ClientName {
		return
	}
	h.useMu.Lock()
	defer h.useMu.Unlock()
	if u := h.use[r.SessionID]; u != nil {
		u.Tokens += r.PromptTokens + r.CompletionTokens
		u.CostUSD += r.CostUSD
	}
}

// ResolveBase is the full commit a repo eval's sessions start from: base
// when set (it must be a commit of the repo), else the repo's main head
// (main, else master, else HEAD). A confined (non-admin) owner's repo must
// be shared with them before git looks at it: otherwise a run could tell
// whether any path on disk is a repo and read its head.
func (s evalSessions) ResolveBase(ctx context.Context, owner, repoName, base string) (string, error) {
	repo, err := resolveRepoName(s.h.conf(), repoName)
	if err != nil {
		return "", err
	}
	if u, ok := s.h.ownerUser(owner); ok && u.Role != users.RoleAdmin {
		if _, shared := u.Share(repo); !shared || u.Disabled || !users.Can(u.Role, users.CapSessions) {
			return "", &confineError{http.StatusForbidden, "workspace", "This repo is not shared with you."}
		}
	}
	refs := []string{"refs/heads/main", "refs/heads/master", "HEAD"}
	if base != "" {
		if !evals.ValidCommit(base) {
			return "", fmt.Errorf("%q is not a commit id", base)
		}
		refs = []string{base}
	}
	for _, ref := range refs {
		cmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "-q", ref+"^{commit}")
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if sha := strings.TrimSpace(string(out)); err == nil && sha != "" {
			return sha, nil
		}
	}
	if base != "" {
		return "", fmt.Errorf("%s is not a commit of %s", base, repo)
	}
	return "", fmt.Errorf("%s has no commits on main", repo)
}

// ---------- catalog ----------

type evalCatalog struct{ h *EvalHost }

func (c evalCatalog) Model(id string) (bool, bool) {
	return catalogModel(c.h.modelList(), c.h.conf(), id)
}

// Agent: a profile of that name, and whether its command is found.
func (c evalCatalog) Agent(name string) (bool, bool) {
	if _, ok := c.h.conf().AgentProfile(name); !ok {
		return false, false
	}
	if c.h.d.Sessions == nil {
		return true, true
	}
	for _, a := range c.h.d.Sessions.Agents() {
		if a.Name == name {
			return true, a.Available
		}
	}
	return true, false
}

func (c evalCatalog) Persona(name string) bool {
	_, ok := c.h.conf().Evals.Personas[name]
	return ok
}
