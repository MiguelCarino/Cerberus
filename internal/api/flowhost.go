package api

// The daemon side of the flow engine (internal/flows): real
// implementations of the engine's interfaces over this daemon's services,
// so cmd/cerberus serve can open the engine with them and hand it to New.
//
//	ModelCaller    one chat completion through the in-process gateway (as
//	               delegation's callLocal does), recorded by usage with client
//	               "cerberus-flow" and the run id as session; cost and energy
//	               come from that usage record
//	SessionRunner  the session manager: one agent session per request, no
//	               parent, tagged "flow" and "fl-…" (session tags allow only
//	               [a-z0-9-], so "flow:<id>" becomes these two), never
//	               auto-accepted; Wait follows the session event stream.
//	               It is also a SessionMeter: the usage records the gateway
//	               adds under the session's id are handed to the engine, so
//	               session steps count toward a run's token and cost limits
//	               (an agent that calls its vendor directly is not seen)
//	Notifier       Notify: a desktop/push notification (also kept in the
//	               inbox, like every notification, except approval requests,
//	               whose inbox item Post already made); Post: an inbox item
//	Auditor        the audit log
//	Guard          the session guard's own rule: max sessions and free memory
//	Catalog        the engine's model list, configured models and providers,
//	               and the agent profiles
//	Publisher      FlowFeed, which the event stream (/api/events) reads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// FlowSessions is what the flow host needs of the session manager
// (*session.Manager satisfies it).
type FlowSessions interface {
	Create(ctx context.Context, t session.TaskSpec) ([]session.Info, error)
	Get(id string) (session.Info, bool)
	List() []session.Info
	Stop(id string) error
	Subscribe() (<-chan session.Event, func())
}

// FlowHostDeps are the daemon services behind the flow engine. Any may be
// nil: the engine then fails the steps that need it with a clear error
// (or skips the check).
type FlowHostDeps struct {
	Config   func() *config.Config                        // the live config (nil: defaults)
	Gateway  http.Handler                                 // the /v1 handler (gateway.Handler())
	Usage    *usage.Store                                 // cost and energy of each model call
	Sessions FlowSessions                                 // agent sessions (session steps)
	Models   func(ctx context.Context) []engine.ModelInfo // the model list (validation)
	Send     func(notify.Message)                         // a notification, kept in the inbox too (notifier.Send)
	Push     func(notify.Message)                         // desktop/push only, nothing kept (nil: Send)
	Inbox    *inbox.Store                                 // inbox items (approvals, output to "inbox")
	Audit    *audit.Logger
	Feed     *FlowFeed  // flowrun events for /api/events
	MemAvail func() int // MiB available (nil: platform's; -1 unknown)
	// Users are the accounts (Phase 4a; nil: single owner). A flow acts for
	// its owner: charged to them, confined and budgeted like them.
	Users *users.Store
}

// FlowHost implements the flow engine's dependencies. Build it with
// NewFlowHost and pass Options to flows.Open.
type FlowHost struct {
	d      FlowHostDeps
	claims *usageClaims
	meters *sessionMeters

	catMu    sync.Mutex
	catAt    time.Time
	catList  []engine.ModelInfo
	catEvery time.Duration
}

// NewFlowHost wires the host. It registers a usage hook (once per host)
// that matches gateway records to the flow calls waiting for them.
func NewFlowHost(d FlowHostDeps) *FlowHost {
	if d.MemAvail == nil {
		d.MemAvail = memAvailable
	}
	h := &FlowHost{d: d, claims: newUsageClaims(), meters: newSessionMeters(), catEvery: 15 * time.Second}
	if d.Usage != nil {
		d.Usage.OnAdd(h.claims.add)
		d.Usage.OnAdd(h.meters.add)
	}
	return h
}

// Options fills the engine options with this host's implementations;
// dir is DATA/flows. The caller may set Clock, Location, MaxRuns and Log.
func (h *FlowHost) Options(dir string) flows.Options {
	o := flows.Options{Dir: dir, Catalog: flowCatalog{h}, Guard: flowGuard{h}, Notifier: flowNotifier{h},
		Models: flowModels{h}}
	if h.d.Sessions != nil {
		o.Sessions = flowSessionRunner{h}
	}
	if h.d.Audit != nil {
		o.Auditor = h.d.Audit
	}
	if h.d.Feed != nil {
		o.Events = h.d.Feed
	}
	if h.d.Users != nil {
		o.OwnerActive = h.ownerActive
	}
	return o
}

// ownerActive reports whether a flow's owner may still run it. A flow
// whose author was removed (its data moved to an admin) does not run until
// an admin re-saves it: its steps were written by the removed account and
// would otherwise run with the admin's rights.
func (h *FlowHost) ownerActive(owner string) bool {
	if h.d.Users == nil || !h.d.Users.HasUsers() {
		return true
	}
	if movedOwner(h.d.Users, owner) {
		return false
	}
	u, ok := h.d.Users.Get(h.d.Users.Owner(owner))
	return ok && !u.Disabled && users.Can(u.Role, users.CapFlows)
}

// errMovedFlow refuses a session step of a flow whose author was removed.
var errMovedFlow = errors.New("this flow belonged to a removed user: an admin must open and save it before it runs")

// movedOwner reports whether a stored owner id is a removed account (its
// data now resolves to someone else). "" (data from before users) is not.
func movedOwner(st *users.Store, raw string) bool {
	if raw == "" {
		return false
	}
	if _, ok := st.Get(raw); ok {
		return false
	}
	return st.Owner(raw) != raw
}

// owner resolves a flow's owner ("" before users exist).
func (h *FlowHost) owner(raw string) string {
	if h.d.Users == nil {
		return raw
	}
	return h.d.Users.Owner(raw)
}

// ownerUser is the flow owner's account, when users exist.
func (h *FlowHost) ownerUser(raw string) (users.User, bool) {
	if h.d.Users == nil || !h.d.Users.HasUsers() {
		return users.User{}, false
	}
	return h.d.Users.Get(h.d.Users.Owner(raw))
}

// budgetErr refuses a step when the flow's owner is over their budget.
func (h *FlowHost) budgetErr(raw string) error {
	u, ok := h.ownerUser(raw)
	if !ok || h.d.Usage == nil {
		return nil
	}
	if msg, over := budgetMessage(u, spendOf(h.d.Usage.ByUser, h.owner, u.ID)); over {
		return errors.New(msg)
	}
	return nil
}

func (h *FlowHost) conf() *config.Config {
	if h.d.Config != nil {
		if c := h.d.Config(); c != nil {
			return c
		}
	}
	return config.Defaults()
}

// FlowReloader applies flows.enabled and flows.max_runs live.
func FlowReloader(e *flows.Engine) Reloader { return flowReloader{e} }

type flowReloader struct{ e *flows.Engine }

func (r flowReloader) Reload(cfg *config.Config) {
	if r.e == nil || cfg == nil {
		return
	}
	r.e.SetEnabled(cfg.Flows.Enabled)
	r.e.SetMaxRuns(cfg.Flows.MaxRuns)
}

// ---------- events ----------

// FlowFeed fans run events out to the event streams (flowrun events on
// /api/events). Publish never blocks: a subscriber whose buffer is full is
// dropped (its channel closed) and its stream sends {"what":"flows"} as a
// resync.
type FlowFeed struct {
	mu   sync.Mutex
	subs map[chan flows.Event]struct{}
}

// NewFlowFeed returns an empty feed.
func NewFlowFeed() *FlowFeed { return &FlowFeed{subs: map[chan flows.Event]struct{}{}} }

// Publish sends ev to every subscriber (flows.Publisher).
func (f *FlowFeed) Publish(ev flows.Event) {
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
func (f *FlowFeed) Subscribe() (<-chan flows.Event, func()) {
	ch := make(chan flows.Event, feedBuffer)
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

// ---------- models ----------

type flowModels struct{ h *FlowHost }

// Complete runs one chat completion through the gateway, so it is loaded,
// queued, priced, budgeted and recorded like any other request.
func (m flowModels) Complete(ctx context.Context, req flows.ModelRequest) (flows.ModelReply, error) {
	h := m.h
	if h.d.Gateway == nil {
		return flows.ModelReply{}, errors.New("the gateway is not available")
	}
	client := req.Client
	if client == "" {
		client = flows.ClientName
	}
	body, err := json.Marshal(map[string]any{"model": req.Model, "messages": req.Messages, "stream": false})
	if err != nil {
		return flows.ModelReply{}, err
	}
	if err := h.budgetErr(req.Owner); err != nil {
		return flows.ModelReply{}, err
	}
	hr := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("X-Cerberus-Session", req.Run)
	hr.Header.Set("X-Cerberus-Client", client)
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
			return flows.ModelReply{}, ctx.Err()
		}
		return flows.ModelReply{}, err
	}
	out := flows.ModelReply{Text: text, Model: model, PromptTokens: prompt, CompletionTokens: completion}
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

// parseCompletion reads a non-streamed chat completion reply (or the
// gateway's error).
func parseCompletion(rec *httptest.ResponseRecorder) (text, model string, prompt, completion int, err error) {
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK {
		msg := strings.TrimSpace(rec.Body.String())
		var e struct {
			Message string `json:"message"`
		}
		var s string
		switch {
		case json.Unmarshal(out.Error, &e) == nil && e.Message != "":
			msg = e.Message
		case json.Unmarshal(out.Error, &s) == nil && s != "":
			msg = s
		}
		if msg == "" {
			msg = http.StatusText(rec.Code)
		}
		return "", "", 0, 0, &flows.ModelError{Status: rec.Code, Message: msg}
	}
	if len(out.Choices) == 0 {
		return "", "", 0, 0, errors.New("the model gave no answer")
	}
	return stripThink(out.Choices[0].Message.Content), out.Model, out.Usage.Prompt, out.Usage.Completion, nil
}

// usageClaims hands each flow call its own usage record. The gateway adds
// the record (synchronously, through the usage hook) before ServeHTTP
// returns; records are kept only for runs with a call in progress and
// matched by token counts, so concurrent fan-out calls of one run each get
// theirs.
type usageClaims struct {
	mu     sync.Mutex
	active map[string]int
	recs   map[string][]usage.Record
}

func newUsageClaims() *usageClaims {
	return &usageClaims{active: map[string]int{}, recs: map[string][]usage.Record{}}
}

func (u *usageClaims) add(r usage.Record) {
	if r.Client != flows.ClientName || r.SessionID == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.active[r.SessionID] > 0 {
		u.recs[r.SessionID] = append(u.recs[r.SessionID], r)
	}
}

func (u *usageClaims) begin(run string) {
	u.mu.Lock()
	u.active[run]++
	u.mu.Unlock()
}

func (u *usageClaims) end(run string, prompt, completion int) (usage.Record, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	defer func() {
		if u.active[run]--; u.active[run] <= 0 {
			delete(u.active, run)
			delete(u.recs, run)
		}
	}()
	rs := u.recs[run]
	if len(rs) == 0 {
		return usage.Record{}, false
	}
	pick := 0
	for i, r := range rs {
		if r.PromptTokens == prompt && r.CompletionTokens == completion {
			pick = i
			break
		}
	}
	r := rs[pick]
	u.recs[run] = append(rs[:pick:pick], rs[pick+1:]...)
	return r, true
}

// ---------- sessions ----------

type flowSessionRunner struct{ h *FlowHost }

// flowSessionFinal: a session in these states is done as far as a flow is
// concerned (passed and failed wait for a person to accept or discard).
func flowSessionFinal(state string) bool {
	switch state {
	case session.StatePassed, session.StateFailed, session.StateStopped, session.StateError,
		session.StateAccepted, session.StateDiscarded:
		return true
	}
	return false
}

// flowSessionTags turns the engine's "flow:<id>" tags into session tags
// ([a-z0-9-]{1,24}): "flow" and the flow id, clipped to 24 characters.
func flowSessionTags(tags []string, flow string) []string {
	out := []string{"flow"}
	add := func(t string) {
		t = strings.ToLower(strings.TrimPrefix(t, "flow:"))
		var b strings.Builder
		for _, r := range t {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
			}
		}
		t = b.String()
		if len(t) > 24 {
			t = t[:24]
		}
		t = strings.Trim(t, "-")
		if t != "" && !containsStr(out, t) {
			out = append(out, t)
		}
	}
	for _, t := range tags {
		add(t)
	}
	if flow != "" {
		add(flow)
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func clipRunes(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// Start creates one session for a session step (or fan-out item).
func (s flowSessionRunner) Start(ctx context.Context, req flows.SessionRequest) (string, error) {
	t := session.TaskSpec{Task: req.Task, Agent: req.Agent, Model: req.Model, Count: 1,
		Name: clipRunes(req.Name, 80), Tags: flowSessionTags(req.Tags, req.Flow)}
	switch req.Workspace {
	case flows.WorkspaceRepo:
		repo, err := s.h.resolveRepo(req.Repo)
		if err != nil {
			return "", err
		}
		t.Workspace, t.Dir, t.Repo = session.WorkspaceRepo, repo, repo
	case flows.WorkspaceFolder:
		t.Workspace, t.Dir = session.WorkspaceFolder, req.Folder
	default:
		t.Workspace = session.WorkspaceScratch
	}
	if s.h.d.Users != nil && s.h.d.Users.HasUsers() && movedOwner(s.h.d.Users, req.Owner) {
		return "", errMovedFlow
	}
	t.Owner = s.h.owner(req.Owner)
	if u, ok := s.h.ownerUser(req.Owner); ok && u.Role != users.RoleAdmin {
		if ce := confineTask(ctx, s.h.d.Users, s.h.conf().DefaultAgent, &t); ce != nil {
			return "", ce
		}
	}
	if err := s.h.budgetErr(req.Owner); err != nil {
		return "", err
	}
	infos, err := s.h.d.Sessions.Create(ctx, t)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", errors.New("no session was created")
	}
	return infos[0].ID, nil
}

// resolveRepo takes an absolute path as it is, else a configured repo's
// name (or folder name), else a folder of that name under repo_roots.
func (h *FlowHost) resolveRepo(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("no repository named")
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name), nil
	}
	cfg := h.conf()
	for _, rc := range cfg.Repos {
		if rc.Name == name || filepath.Base(filepath.Clean(rc.Path)) == name {
			return filepath.Clean(rc.Path), nil
		}
	}
	if !strings.ContainsAny(name, `/\`) && name != "." && name != ".." {
		for _, root := range cfg.RepoRoots {
			p := filepath.Join(root, name)
			if st, err := os.Stat(filepath.Join(p, ".git")); err == nil && st != nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("no repository %q (configured repos or a folder under repo_roots)", name)
}

// Wait follows the session event stream until the session is done or
// removed, or ctx ends. A dropped subscription is renewed (and the state
// read again), so nothing is missed and nothing polls.
func (s flowSessionRunner) Wait(ctx context.Context, id string) (flows.SessionResult, error) {
	m := s.h.d.Sessions
	for {
		ch, cancel := m.Subscribe()
		in, ok := m.Get(id)
		if !ok {
			cancel()
			return flows.SessionResult{State: "removed"}, nil
		}
		if flowSessionFinal(in.State) {
			cancel()
			return sessionResult(in), nil
		}
	follow:
		for {
			select {
			case <-ctx.Done():
				cancel()
				return flows.SessionResult{}, ctx.Err()
			case ev, open := <-ch:
				if !open {
					break follow
				}
				if ev.Session.ID != id {
					continue
				}
				if ev.Type == "removed" {
					cancel()
					return flows.SessionResult{State: "removed", Summary: ev.Session.Activity}, nil
				}
				if flowSessionFinal(ev.Session.State) {
					cancel()
					return sessionResult(ev.Session), nil
				}
			}
		}
		cancel()
	}
}

func sessionResult(in session.Info) flows.SessionResult {
	sum := in.Activity
	if sum == "" {
		sum = in.Error
	}
	return flows.SessionResult{State: in.State, Summary: sum}
}

// Stop stops a session the flow started.
func (s flowSessionRunner) Stop(id string) error { return s.h.d.Sessions.Stop(id) }

// Meter hands fn the usage of every model call recorded under session id
// until stop (flows.SessionMeter).
func (s flowSessionRunner) Meter(id string, fn func(flows.ModelReply)) func() {
	return s.h.meters.watch(id, fn)
}

// sessionMeters passes usage records to the flow steps that wait on their
// session. fn runs under mu, so once stop returns it is not called again.
type sessionMeters struct {
	mu   sync.Mutex
	next int
	fns  map[string]map[int]func(flows.ModelReply)
}

func newSessionMeters() *sessionMeters {
	return &sessionMeters{fns: map[string]map[int]func(flows.ModelReply){}}
}

func (m *sessionMeters) watch(id string, fn func(flows.ModelReply)) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	k := m.next
	if m.fns[id] == nil {
		m.fns[id] = map[int]func(flows.ModelReply){}
	}
	m.fns[id][k] = fn
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.fns[id], k)
		if len(m.fns[id]) == 0 {
			delete(m.fns, id)
		}
	}
}

// add is the usage store's hook.
func (m *sessionMeters) add(r usage.Record) {
	if r.SessionID == "" || r.Client == flows.ClientName {
		return // a flow's own call: Complete charges it
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fns := m.fns[r.SessionID]
	if len(fns) == 0 {
		return
	}
	rep := flows.ModelReply{Model: r.Model, PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
		CostUSD: r.CostUSD, EnergyWh: r.EnergyJ / 3600}
	for _, fn := range fns {
		fn(rep)
	}
}

// ---------- notifications ----------

type flowNotifier struct{ h *FlowHost }

func noticeMessage(n flows.Notice) notify.Message {
	urg := "normal"
	if n.Level == "warn" {
		urg = "critical"
	}
	// SessionID keys the notifier's rate limit: one per run and step.
	return notify.Message{Title: n.Title, Body: n.Body, Urgency: urg, Kind: n.Kind, Level: n.Level, Link: n.Link,
		SessionID: "flow:" + n.Run + ":" + n.Step}
}

// Notify sends a desktop/push notification. An approval request's inbox
// item was already made by Post, so it goes to Push (nothing kept);
// anything else goes through Send and is kept in the inbox like every
// notification.
func (f flowNotifier) Notify(n flows.Notice) error {
	if u, ok := f.h.ownerUser(n.Owner); ok && u.Role != users.RoleAdmin {
		// Host notifications are the admin's: other owners get an inbox item.
		if n.Kind == "flow-approve" {
			return nil // Post made it already
		}
		_, err := f.Post(n)
		return err
	}
	msg := noticeMessage(n)
	send := f.h.d.Send
	if n.Kind == "flow-approve" && f.h.d.Push != nil {
		send = f.h.d.Push
	}
	if send == nil {
		if f.h.d.Push != nil {
			send = f.h.d.Push
		} else {
			return errors.New("notifications are not available")
		}
	}
	send(msg)
	return nil
}

// Post puts an item in the inbox and answers its id.
func (f flowNotifier) Post(n flows.Notice) (string, error) {
	if f.h.d.Inbox == nil {
		return "", errors.New("the inbox is not available")
	}
	level := n.Level
	if level == "" {
		level = "info"
	}
	it := f.h.d.Inbox.Add(inbox.Item{Kind: n.Kind, Level: level, Title: n.Title, Body: n.Body, Link: n.Link,
		Owner: f.h.owner(n.Owner)})
	return it.ID, nil
}

// Resolve marks an approval's inbox item read once the run no longer
// waits for it (the bell then stops offering Approve / Reject).
func (f flowNotifier) Resolve(id string) {
	if f.h.d.Inbox != nil && id != "" {
		_, _ = f.h.d.Inbox.MarkRead([]string{id}, false)
	}
}

// ---------- guard ----------

type flowGuard struct{ h *FlowHost }

// CanStart applies the session guard's rule (max sessions, free memory)
// to the sessions running now.
func (g flowGuard) CanStart() (bool, string) {
	cfg := g.h.conf()
	if max := cfg.Guard.MaxSessions; max > 0 && g.h.d.Sessions != nil {
		running := 0
		for _, in := range g.h.d.Sessions.List() {
			if liveStates[in.State] {
				running++
			}
		}
		if running >= max {
			return false, fmt.Sprintf("%d of %d sessions running", running, max)
		}
	}
	if mem := g.h.d.MemAvail(); mem >= 0 && mem < cfg.Guard.MinFreeMiB {
		return false, fmt.Sprintf("low memory: %d MiB available, %d MiB needed", mem, cfg.Guard.MinFreeMiB)
	}
	return true, ""
}

// ---------- catalog ----------

type flowCatalog struct{ h *FlowHost }

func (h *FlowHost) modelList() []engine.ModelInfo {
	if h.d.Models == nil {
		return nil
	}
	h.catMu.Lock()
	defer h.catMu.Unlock()
	if h.catList != nil && time.Since(h.catAt) < h.catEvery {
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

// Model: a model the engine lists is known and installed; one only in the
// config (models[]) is known but not installed now; an id under an enabled
// provider's prefix ("openrouter/…") is a cloud model, known and usable.
func (c flowCatalog) Model(id string) (bool, bool) {
	for _, m := range c.h.modelList() {
		if m.ID == id {
			return true, true
		}
	}
	cfg := c.h.conf()
	for _, p := range cfg.Providers {
		if p.Enabled && p.Name != "" && strings.HasPrefix(id, p.Name+"/") {
			return true, true
		}
	}
	for _, m := range cfg.Models {
		if m.ID == id {
			return true, false
		}
	}
	return false, false
}

// Agent reports whether an agent profile of that name exists.
func (c flowCatalog) Agent(name string) bool {
	_, ok := c.h.conf().AgentProfile(name)
	return ok
}
