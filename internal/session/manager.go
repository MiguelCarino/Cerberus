package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/files/fsafe"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/permits"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/vault"
)

const (
	scheduleEvery  = 2 * time.Second // queue re-check, only while something is queued
	resolveTimeout = 10 * time.Second
	eventBuffer    = 64
	maxFanOut      = 16
)

// session is the manager's record of one session. Every field except out is
// guarded by Manager.mu.
type session struct {
	info          Info
	task          string // the instruction retries are based on
	next          string // the instruction for the next run
	maxAttempts   int
	agentAttempts int // runs with the current agent
	escalated     bool

	dir string  // DATA/sessions/<id>
	out *output // has its own lock

	run         *run
	checkCancel context.CancelFunc
	stopping    bool           // a stop was requested for the current run or checks
	op          bool           // Accept, Discard or Prune in progress
	resume      bool           // the next run continues the agent's own conversation (Resume)
	work        sync.WaitGroup // runs and check passes in flight

	needsApproval bool   // a spawned child held in the queue until Approve
	stopReason    string // why the current run is being stopped ("time limit", "killed"…), shown once stopped

	pendingFiles []string // inbox paths added since the last follow-up (mentioned to agents other than the built-in one)

	// Work previews (preview.go).
	pv     *managed  // the Run preview server (nil = never started)
	pvLive []int     // ports of the live servers found by the last scan
	pvUsed time.Time // last preview request (idle stop of pv)
}

// guard holds the fields of a session that decide how it is confined and
// whose it is. meta.json lives in the session folder, which the session's
// own sandbox can write, so these fields are also kept in a sidecar next to
// the folder (DATA/sessions/<id>.guard.json), out of the agent's reach,
// and load trusts the sidecar over meta.json.
type guard struct {
	Owner     string `json:"owner"`
	Confined  bool   `json:"confined"`
	Workspace string `json:"workspace"`
	Dir       string `json:"dir"`
	Repo      string `json:"repo"`
	Worktree  string `json:"worktree"`
	Parent    string `json:"parent"`
	Client    string `json:"client"`
}

func guardOf(in Info) guard {
	return guard{Owner: in.Owner, Confined: in.Confined, Workspace: in.Workspace, Dir: in.Dir, Repo: in.Repo,
		Worktree: in.Worktree, Parent: in.Parent, Client: in.Client}
}

func (g guard) apply(in *Info) {
	in.Owner, in.Confined, in.Workspace, in.Dir, in.Repo = g.Owner, g.Confined, g.Workspace, g.Dir, g.Repo
	in.Worktree, in.Parent, in.Client = g.Worktree, g.Parent, g.Client
}

// guardPath is the sidecar of session id (outside its folder).
func (m *Manager) guardPath(id string) string { return filepath.Join(m.sessDir, id+".guard.json") }

// meta is the on-disk form of a session (meta.json).
type meta struct {
	Info          Info     `json:"info"`
	Task          string   `json:"task"`
	Next          string   `json:"next,omitempty"`
	MaxAttempts   int      `json:"max_attempts"`
	AgentAttempts int      `json:"agent_attempts"`
	Escalated     bool     `json:"escalated,omitempty"`
	NeedsApproval bool     `json:"needs_approval,omitempty"`
	PendingFiles  []string `json:"pending_files,omitempty"`
}

// Manager owns every session.
type Manager struct {
	cfg      *config.Config                // startup config; the admin token is always read here
	live     atomic.Pointer[config.Config] // set by Reload; settings read through conf()
	eng      *engine.Manager
	gpu      *gpu.Monitor
	notifier *notify.Notifier
	log      *slog.Logger
	vault    *vault.Vault // nil: sessions get no secrets and output is not redacted
	cost     func(id string) (wallJ, cost float64)
	forget   func(id string) // a session was deleted (energy records)

	sessDir    string
	wtDir      string
	batchDir   string
	uploadsDir string // DATA/uploads: staging folders of New task attachments (internal/files)
	home       string // the user's home folder (resolved): folder sessions live under it

	ctx    context.Context // lifetime of the manager (checks, git); cancelled by Shutdown
	cancel context.CancelFunc
	wake   chan struct{}

	memAvail func() int // platform.MemAvailableMiB; swapped in tests

	mu       sync.Mutex
	sessions map[string]*session
	order    []string // ids by creation
	subs     map[chan Event]struct{}
	batches  map[string]*batch
	closing  bool
	wg       sync.WaitGroup // run waiters and check goroutines

	taking   map[string]bool // staging ids a Create is handing out (m.mu)
	placing  map[string]int  // staging ids with an upload linking a file in (m.mu)
	placed   *sync.Cond      // on m.mu: broadcast when a placing count drops
	excluded sync.Map        // worktree → true once its inbox is in info/exclude

	acceptMu sync.Mutex // batch accepts run one at a time, so pushes never race
	pruneMu  sync.Mutex // one prune at a time

	feed     *feed
	feedDone chan struct{}
	feedOnce sync.Once

	noScope atomic.Bool // systemd-run could not reach the user bus: run without a scope from now on

	audit *audit.Logger // nil: nothing is audited (sandbox fallbacks still reach the feed)

	// confineOwner reports whether a session owner (a stored user id) is
	// confined now (not an admin); nil: only Info.Confined counts.
	confineOwner func(owner string) bool

	permits *permits.Store // saved rules ("Always allow"); nil: none

	// Work previews (preview.go).
	pvKey     []byte
	pvKeyOnce sync.Once
	pvScanMu  sync.Mutex
	pvScan    map[string]liveCache

	// Setup snapshots (setup.go).
	version      string                                       // the Cerberus version recorded in setup.json
	versions     versionCache                                 // agent --version lines per binary and mtime
	probeVersion func(ctx context.Context, bin string) string // nil: execVersion (never inside a test binary)
}

// WithVersion is the Cerberus version recorded in each run's setup.json.
func WithVersion(v string) Option { return func(m *Manager) { m.version = v } }

// WithConfine makes every run, checks pass and resume recompute
// confinement from the owner's current role, on top of Info.Confined.
func WithConfine(fn func(owner string) bool) Option { return func(m *Manager) { m.confineOwner = fn } }

// Option configures NewManager.
type Option func(*Manager)

// WithVault gives sessions their SecretEnv from v and redacts v's values
// from all session output.
func WithVault(v *vault.Vault) Option { return func(m *Manager) { m.vault = v } }

// WithCost fills Info.EnergyJ and Info.CostMXN on read (energy accounting).
// fn must not call back into the Manager.
func WithCost(fn func(id string) (wallJ, cost float64)) Option {
	return func(m *Manager) { m.cost = fn }
}

// WithForget is called (outside the Manager's lock) with the id of each
// session Prune deletes, so records kept elsewhere (energy) go with it.
// A discarded session stays listed, with its energy, until it is pruned.
func WithForget(fn func(id string)) Option { return func(m *Manager) { m.forget = fn } }

// NewManager loads existing sessions from DATA/sessions and fleet batches
// from DATA/batches. Sessions that were live when the daemon stopped become
// "stopped" (their worktrees are kept).
func NewManager(cfg *config.Config, eng *engine.Manager, mon *gpu.Monitor, notifier *notify.Notifier, log *slog.Logger, opts ...Option) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	data, err := platform.DataDir()
	if err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, eng: eng, gpu: mon, notifier: notifier, log: log,
		sessDir: filepath.Join(data, "sessions"), wtDir: filepath.Join(data, "worktrees"),
		batchDir: filepath.Join(data, "batches"), uploadsDir: filepath.Join(data, "uploads"),
		wake: make(chan struct{}, 1), memAvail: platform.MemAvailableMiB,
		sessions: map[string]*session{}, subs: map[chan Event]struct{}{}, batches: map[string]*batch{}}
	for _, o := range opts {
		if o != nil {
			o(m)
		}
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	if home, err := os.UserHomeDir(); err == nil {
		if r, err := filepath.EvalSymlinks(home); err == nil {
			home = r
		}
		m.home = home
	}
	if err := os.MkdirAll(m.sessDir, 0o700); err != nil {
		return nil, err
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	m.loadBatches()
	m.feed = newFeed(filepath.Join(data, feedName), m.secretValues)
	m.feedDone = make(chan struct{})
	go m.feed.loop(m.feedDone)
	return m, nil
}

func (m *Manager) load() error {
	entries, err := os.ReadDir(m.sessDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.sessDir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		var md meta
		if err := json.Unmarshal(raw, &md); err != nil || md.Info.ID != e.Name() {
			m.log.Warn("skipping unreadable session", "dir", dir, "err", err)
			continue
		}
		// The sidecar wins over the agent-writable meta.json. A session from
		// before sidecars gets one now, from meta.json (once, at upgrade).
		if graw, gerr := os.ReadFile(m.guardPath(md.Info.ID)); gerr == nil {
			var g guard
			if err := json.Unmarshal(graw, &g); err != nil {
				m.log.Warn("skipping session with an unreadable guard file", "dir", dir, "err", err)
				continue
			}
			g.apply(&md.Info)
		} else if !errors.Is(gerr, fs.ErrNotExist) {
			m.log.Warn("skipping session with an unreadable guard file", "dir", dir, "err", gerr)
			continue
		}
		if md.Info.Workspace == "" { // sessions from before workspaces were all repo sessions
			md.Info.Workspace = WorkspaceRepo
			if md.Info.Dir == "" {
				md.Info.Dir = md.Info.Repo
			}
		}
		s := &session{info: md.Info, task: md.Task, next: md.Next, maxAttempts: md.MaxAttempts,
			agentAttempts: md.AgentAttempts, escalated: md.Escalated, dir: dir, out: m.newOutput(dir),
			needsApproval: md.NeedsApproval, pendingFiles: md.PendingFiles}
		// A spawn awaiting approval survives a restart as it was, and one
		// stopped before approval stays unapproved (it can only be discarded).
		pending := s.needsApproval && s.info.State == StateQueued
		if final(s.info.State) {
			s.needsApproval = false
		}
		if !pending && (live(s.info.State) || s.info.State == StateQueued) {
			if live(s.info.State) && !s.info.RunStarted.IsZero() {
				// The run died with the daemon: it ended when it was last
				// seen alive, not now (the machine may have been off).
				s.info.RunEnded = runEndAfterCrash(s.info)
			}
			s.info.State = StateStopped
			s.info.WaitingReason = "daemon restarted"
			s.info.Updated = time.Now()
			m.persistLocked(s)
		}
		if _, err := os.Stat(m.guardPath(s.info.ID)); errors.Is(err, fs.ErrNotExist) {
			m.persistLocked(s)
		}
		m.sessions[s.info.ID] = s
		m.order = append(m.order, s.info.ID)
	}
	slices.SortFunc(m.order, func(a, b string) int {
		return m.sessions[a].info.Created.Compare(m.sessions[b].info.Created)
	})
	return nil
}

// runEndAfterCrash estimates when a run that was live when the daemon went
// down ended: the latest of its recorded run end, its last output and its
// first start.
func runEndAfterCrash(in Info) time.Time {
	end := in.RunStarted
	for _, t := range []time.Time{in.RunEnded, in.LastOutput} {
		if t.After(end) {
			end = t
		}
	}
	return end
}

func (m *Manager) newOutput(dir string) *output {
	sb := max(m.conf().Sessions.ScrollbackMiB, 1)
	tail := max(m.conf().Sessions.ScrollbackTailKiB, 16)
	return newOutput(filepath.Join(dir, "scrollback.log"), int64(sb)<<20, tail<<10)
}

// Run is the queue scheduler. It sleeps until signalled, and ticks every 2 s
// only while some session is queued (the guard's inputs change on their own).
// It also runs the retention prune: once now and then daily.
func (m *Manager) Run(ctx context.Context) {
	go m.pruneLoop(ctx)
	go m.previewLoop(ctx)
	for {
		queued := m.schedule(ctx)
		var tick <-chan time.Time
		var t *time.Timer
		if queued {
			t = time.NewTimer(scheduleEvery)
			tick = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-m.wake:
		case <-tick:
		}
		if t != nil {
			t.Stop()
		}
	}
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// schedule starts every queued session the guard allows and reports whether
// any remain queued.
func (m *Manager) schedule(ctx context.Context) bool {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return false
	}
	running := 0
	var queued []*session
	for _, id := range m.order {
		s := m.sessions[id]
		switch {
		case live(s.info.State):
			running++
		case s.info.State == StateQueued && !s.needsApproval:
			queued = append(queued, s)
		}
	}
	slices.SortStableFunc(queued, func(a, b *session) int { return queueCompare(a.info, b.info) })
	m.mu.Unlock()
	if len(queued) == 0 {
		return false
	}
	g := guardState{MaxSessions: m.conf().Guard.MaxSessions, MemAvailMiB: m.memAvail(),
		MinFreeMiB: m.conf().Guard.MinFreeMiB, FreeVRAMMiB: -1}
	if m.gpu != nil {
		g.FreeVRAMMiB = m.gpu.FreeVRAMMiB()
	}
	remaining := false
	reserved := map[string]bool{} // models whose VRAM this pass already counted
	for _, s := range queued {
		g.Running = running
		model := m.fillModel(ctx, s, &g)
		if reserved[model] {
			g.ModelLoaded = true // an earlier admission this pass will load it
		}
		ok, reason := g.decide()
		if m.admit(s, ok, reason) {
			running++
			g.reserve(model != "" && !reserved[model])
			if model != "" {
				reserved[model] = true
			}
		} else {
			remaining = true
		}
	}
	return remaining
}

// fillModel sets the guard's model fields for s and returns the resolved
// local model's ID ("" when s uses no local model).
func (m *Manager) fillModel(ctx context.Context, s *session, g *guardState) string {
	g.LocalModel, g.ModelLoaded, g.NeedVRAMMiB = false, false, 0
	m.mu.Lock()
	agent, model := s.info.Agent, s.info.Model
	m.mu.Unlock()
	p, ok := m.conf().AgentProfile(agent)
	if m.eng == nil || !ok || !p.NeedsModel || model == "" {
		return ""
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	info, err := m.eng.Resolve(rctx, model)
	if err != nil {
		return "" // unknown model: the agent will report it
	}
	g.LocalModel, g.ModelLoaded, g.NeedVRAMMiB = info.Local, info.Loaded, info.SizeMiB
	if !info.Local {
		return ""
	}
	return info.ID
}

// admit starts s (when ok) or records why it waits. It reports whether s started.
func (m *Manager) admit(s *session, ok bool, reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.info.State != StateQueued || s.needsApproval || m.closing {
		return false
	}
	if !ok {
		if s.info.WaitingReason != reason {
			s.info.WaitingReason = reason
			m.changedLocked(s)
		}
		return false
	}
	s.info.WaitingReason = ""
	m.setStateLocked(s, StateStarting)
	s.work.Add(1)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer s.work.Done()
		m.start(s)
	}()
	return true
}

// Create validates t and queues Count sessions.
func (m *Manager) Create(ctx context.Context, t TaskSpec) ([]Info, error) {
	t, err := m.normalize(ctx, t)
	if err != nil {
		return nil, err
	}
	group := ""
	if t.Count > 1 {
		group = newID()
	}
	staging := ""
	if t.Staging != "" {
		// Claimed first: a second Create on the same attachments (a double
		// click, a retried POST) fails here, before touching any file.
		release, err := m.claimStaging(t.Staging)
		if err != nil {
			return nil, err
		}
		defer release()
		if staging, err = m.stagingPath(t.Staging); err != nil {
			return nil, err
		}
	}
	out := make([]Info, 0, t.Count)
	for i := range t.Count {
		title := t.Title
		if t.Count > 1 {
			title = fmt.Sprintf("%s (#%d)", t.Title, i+1)
		}
		// The last session takes the staging folder itself; the others
		// get copies. A failed enqueue leaves the staging folder as it was.
		info, err := m.enqueue(ctx, t, title, group, staging, i == t.Count-1)
		if err != nil {
			if staging != "" && len(out) > 0 {
				// Part of the task started with these attachments: a
				// retry would start it again, so they go.
				_ = os.RemoveAll(staging)
			}
			if len(out) > 0 {
				m.signal()
			}
			return out, err
		}
		out = append(out, info)
	}
	m.signal()
	return out, nil
}

// claimStaging marks a staging folder as being handed out by a Create and
// returns the function that releases it. It waits for uploads that are
// linking a file into the folder right now (PlaceStaging), so the hand-out
// sees each of them whole, with its private mark; uploads that try to place
// a file while the claim holds are refused.
func (m *Manager) claimStaging(id string) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.taking == nil {
		m.taking = map[string]bool{}
	}
	if m.taking[id] {
		return nil, errors.New("these attachments are already being handed to another new task")
	}
	m.taking[id] = true
	for m.placing[id] > 0 {
		m.placedCond().Wait()
	}
	return func() {
		m.mu.Lock()
		delete(m.taking, id)
		m.mu.Unlock()
	}, nil
}

// ErrStagingTaken refuses an upload into a staging folder that a Create is
// handing to a new task at this moment.
var ErrStagingTaken = errors.New("these attachments are being handed to a new task; attach the file again")

// PlaceStaging is called by internal/files around the short step that links
// an uploaded file into the staging folder id (and records it as private).
// It refuses with ErrStagingTaken while a Create holds id, and otherwise
// keeps Create's claim waiting until the returned release runs, so a file
// is never handed out half-recorded nor lost to the hand-out's cleanup.
func (m *Manager) PlaceStaging(id string) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.taking[id] {
		return nil, ErrStagingTaken
	}
	if m.placing == nil {
		m.placing = map[string]int{}
	}
	m.placing[id]++
	return func() {
		m.mu.Lock()
		if m.placing[id]--; m.placing[id] <= 0 {
			delete(m.placing, id)
		}
		m.placedCond().Broadcast()
		m.mu.Unlock()
	}, nil
}

// placedCond returns m.placed, made on first use. The caller holds m.mu.
func (m *Manager) placedCond() *sync.Cond {
	if m.placed == nil {
		m.placed = sync.NewCond(&m.mu)
	}
	return m.placed
}

// enqueue records one queued session for a normalized spec. The caller
// signals the scheduler.
//
// With staging set, the attachments go to the session folder first (moved
// when last, else copied) and, for folder and scratch sessions, on into the
// inbox at once; repo sessions get them when their worktree is made.
func (m *Manager) enqueue(ctx context.Context, t TaskSpec, title, group, staging string, last bool) (Info, error) {
	id := newID()
	dir := filepath.Join(m.sessDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Info{}, err
	}
	wt := "" // repo sessions get their worktree when they start
	switch t.Workspace {
	case WorkspaceFolder:
		wt = t.Dir
	case WorkspaceScratch:
		wt = m.scratchDir(id)
		if err := os.MkdirAll(wt, 0o755); err != nil {
			return Info{}, err
		}
	}
	var attached []string
	var moved []fsafe.Moved
	// undo puts everything back as it was: attachments return to the
	// staging folder (a retry gets the full set) and the session folder
	// goes.
	cleanup := func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove(m.guardPath(filepath.Base(dir)))
		if t.Workspace == WorkspaceScratch {
			_ = os.RemoveAll(wt)
		}
	}
	undo := cleanup
	if t.prepare != nil {
		if err := t.prepare(dir, wt); err != nil {
			cleanup()
			return Info{}, err
		}
	}
	if staging != "" {
		err := takeAttachments(staging, dir, m.conf().Files.AllowExec, last)
		if err == nil && wt != "" {
			attached, moved, err = m.deliverAttachments(ctx, dir, t.Workspace, wt)
		}
		undo = func() {
			if rerr := m.returnAttachments(staging, dir, wt, moved, last); rerr != nil {
				m.log.Warn("return attachments to staging", "staging", staging, "err", rerr)
			}
			cleanup()
		}
		if err != nil {
			undo()
			return Info{}, fmt.Errorf("attachments: %w", err)
		}
	}
	now := time.Now()
	s := &session{dir: dir, out: m.newOutput(dir), task: t.Task, next: t.Task, maxAttempts: t.MaxAttempts,
		info: Info{ID: id, Title: title, Repo: t.Repo, Task: t.Task, Agent: t.Agent, Model: t.Model,
			Workspace: t.Workspace, Dir: t.Dir, Worktree: wt, BaseCommit: t.Base,
			EscalateTo: t.EscalateTo, State: StateQueued, Created: now, Updated: now, Group: group,
			Cols: m.conf().Sessions.DefaultCols, Rows: m.conf().Sessions.DefaultRows,
			Name: t.Name, Tags: slices.Clone(t.Tags), Priority: t.Priority, Limits: t.Limits, Parent: t.Parent,
			Client: t.Client, Owner: t.Owner, Confined: t.Confined}}
	info, err := func() (Info, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		kind := FeedCreated
		spawnedBy := ""
		if t.Parent != "" {
			// Checked again under the lock: two spawns racing past normalize
			// must not both take the last slot.
			parent, err := m.canSpawnLocked(t.Parent, 1)
			if err != nil {
				return Info{}, err
			}
			s.info.Depth = parent.info.Depth + 1
			// A child acts for its parent's owner, confined like it.
			s.info.Owner = parent.info.Owner
			s.info.Confined = s.info.Confined || parent.info.Confined
			kind = FeedSpawned
			spawnedBy = parent.info.Name
			if spawnedBy == "" {
				spawnedBy = parent.info.Title
			}
			if m.spawnConfig().RequireApproval {
				s.needsApproval = true
				s.info.WaitingReason = ReasonNeedsApproval
			}
		} else if t.Client != "" {
			if err := m.canClientSpawnLocked(t.Client, 1); err != nil {
				return Info{}, err
			}
			kind = FeedSpawned
			spawnedBy = t.Client
			if m.spawnConfig().RequireApproval {
				s.needsApproval = true
				s.info.WaitingReason = ReasonNeedsApproval
			}
		}
		m.sessions[id] = s
		m.order = append(m.order, id)
		text := s.info.Title
		if t.Parent != "" || t.Client != "" {
			text = "by " + spawnedBy + ": " + text
		}
		m.recordLocked(s, kind, text)
		m.onAttachmentsLocked(s, attached, moved)
		m.persistLocked(s)
		info := m.snapshotLocked(s)
		m.emitLocked(EventCreated, info)
		return info, nil
	}()
	if err != nil {
		undo() // outside the lock: it moves files
		return Info{}, err
	}
	return info, nil
}

// defaultModel is the model a NeedsModel session gets when none is picked:
// engines.default_model, else (a fresh install with it unset) the model the
// engine's "default" alias stands for, the only loaded model or the only
// model there is. It returns "" and the engine's error when there is none.
func (m *Manager) defaultModel(ctx context.Context) (string, error) {
	if def := m.conf().Engines.DefaultModel; def != "" || m.eng == nil {
		return def, nil
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	info, err := m.eng.Resolve(rctx, "default")
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// normalize fills defaults and validates a TaskSpec.
func (m *Manager) normalize(ctx context.Context, t TaskSpec) (TaskSpec, error) {
	t.Task = strings.TrimSpace(t.Task)
	if t.Task == "" {
		return t, errors.New("task is empty")
	}
	if t.Agent == "" {
		t.Agent = m.conf().DefaultAgent
	}
	if internalAgent(t.Agent) || internalAgent(t.EscalateTo) {
		return t, fmt.Errorf("%w: agent %q is internal to Cerberus", ErrAgentArg, FinetuneTrainAgent)
	}
	p, err := m.profile(t.Agent)
	if err != nil {
		return t, err
	}
	var modelErr error
	if p.NeedsModel && t.Model == "" {
		t.Model, modelErr = m.defaultModel(ctx)
	}
	if usesModel(p) && t.Model == "" {
		if modelErr != nil {
			return t, fmt.Errorf("agent %q needs a model: %w", t.Agent, modelErr)
		}
		return t, fmt.Errorf("agent %q needs a model and engines.default_model is not set", t.Agent)
	}
	if t.EscalateTo != "" {
		if _, err := m.profile(t.EscalateTo); err != nil {
			return t, err
		}
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = max(m.conf().Sessions.MaxAttempts, 1)
	}
	t.Count = min(max(t.Count, 1), maxFanOut)
	if t.Title == "" {
		t.Title = titleFrom(t.Task)
	}
	if err := normalizeMeta(&t.Name, &t.Tags, t.Priority, t.Limits); err != nil {
		return t, err
	}
	if t.Parent != "" && t.Client != "" {
		return t, errors.New("a session has a parent session or an MCP client, not both")
	}
	if t.Parent != "" || t.Client != "" {
		if err := agentArgErr("task", t.Task); err != nil {
			return t, err
		}
		if err := agentArgErr("model", t.Model); err != nil {
			return t, err
		}
	}
	if t.Parent != "" {
		if err := m.CanSpawnN(t.Parent, t.Count); err != nil {
			return t, err
		}
		if t.Workspace == WorkspaceSame {
			if done, err := m.sameWorkspace(&t); err != nil || done {
				return t, err
			}
		}
	} else if t.Workspace == WorkspaceSame {
		return t, errors.New(`workspace "same" is only for sessions spawned by another session`)
	}
	if t.Client != "" {
		if err := m.CanClientSpawn(t.Client, t.Count); err != nil {
			return t, err
		}
	}
	if err := m.resolveWorkspace(ctx, &t); err != nil {
		return t, err
	}
	if t.Base != "" {
		if err := m.resolveBase(ctx, &t); err != nil {
			return t, err
		}
	}
	return t, nil
}

// baseRe is the form of TaskSpec.Base: a hex commit id (never an option
// or a ref name, which git would read differently).
var baseRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// resolveBase checks TaskSpec.Base names a commit of the session's repo
// and replaces it with the full id.
func (m *Manager) resolveBase(ctx context.Context, t *TaskSpec) error {
	base := strings.ToLower(strings.TrimSpace(t.Base))
	if t.Workspace != WorkspaceRepo {
		return errors.New("a base commit is for repo sessions only")
	}
	if !baseRe.MatchString(base) {
		return fmt.Errorf("base %q is not a commit id", t.Base)
	}
	sha, err := gitLine(ctx, t.Repo, "rev-parse", "--verify", "-q", base+"^{commit}")
	if err != nil || sha == "" {
		return fmt.Errorf("base %s is not a commit of %s", base, t.Repo)
	}
	t.Base = sha
	return nil
}

// titleFrom is the task's first line, clipped to 72 characters.
func titleFrom(task string) string {
	line, _, _ := strings.Cut(task, "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 72 {
		line = string(r[:71]) + "…"
	}
	return line
}

// newID is a sortable, path-safe id: UTC time plus random hex.
func newID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// List returns every session, oldest first.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.snapshotLocked(m.sessions[id]))
	}
	return out
}

// Get returns one session.
func (m *Manager) Get(id string) (Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Info{}, false
	}
	return m.snapshotLocked(s), true
}

// Subscribe streams session events. A subscriber that falls behind by more
// than its buffer is dropped: its channel is closed, so it reconnects and
// refetches the list instead of silently missing an event.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, eventBuffer)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.subs[ch]; ok { // else emitLocked already dropped it
			delete(m.subs, ch)
			close(ch)
		}
	}
}

// Shutdown stops every live session and waits for them (up to ctx).
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closing = true
	var previews []*session
	for _, s := range m.sessions {
		if s.pv != nil {
			previews = append(previews, s)
		}
	}
	m.mu.Unlock()
	for _, s := range previews {
		m.stopPreview(s, "Cerberus is shutting down")
	}
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.needsApproval && s.info.State == StateQueued {
			continue // a pending spawn waits for a person across restarts
		}
		m.stopLocked(s)
	}
	m.mu.Unlock()
	m.cancel()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		m.log.Warn("sessions did not stop in time")
	}
	m.feedOnce.Do(func() { close(m.feedDone) })
	m.feed.drain()
	m.feed.close()
}

func (m *Manager) lookup(id string) (*session, error) {
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

// snapshotLocked copies s.info (ExitCode gets its own pointer).
func (m *Manager) snapshotLocked(s *session) Info {
	info := s.info
	if info.ExitCode != nil {
		c := *info.ExitCode
		info.ExitCode = &c
	}
	if info.Name == "" {
		info.Name = info.Title
	}
	info.Tags = slices.Clone(info.Tags)
	info.Preview = s.previewInfoLocked()
	if m.cost != nil {
		info.EnergyJ, info.CostMXN = m.cost(info.ID)
	}
	return info
}

// LivePIDs maps each session with a running agent to its process (energy
// attribution; the agent leads its own process group or systemd scope).
func (m *Manager) LivePIDs() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for id, s := range m.sessions {
		if s.run != nil && s.run.cmd != nil && s.run.cmd.Process != nil {
			select {
			case <-s.run.waitDone:
			default:
				out[id] = s.run.cmd.Process.Pid
			}
		}
	}
	return out
}

// setStateLocked moves s to state, persists, emits and notifies. Accepted
// and discarded are final: a waiter or start that finishes late must not
// bring the session back.
func (m *Manager) setStateLocked(s *session, state string) {
	prev := s.info.State
	if final(prev) && state != prev {
		return
	}
	s.info.State = state
	if state != StateWaiting && state != StateQueued && state != StateStopped {
		s.info.WaitingReason = ""
	}
	if state != StateWaiting {
		s.info.WaitingAsk = ""
		s.info.WaitingAlways = nil
	}
	if final(state) {
		s.info.PreviewURL = ""
		if pv := s.pv; pv != nil && !pv.stopping && (pv.state == PreviewStarting || pv.state == PreviewRunning) {
			// Discard and Accept stop it first (endPreview); this is the
			// backstop for any other path to a final state.
			pv.stopping = true
			go platform.Terminate(pv.cmd.Process, previewStopGrace, pv.done)
		}
	} else {
		s.info.PreviewURL = m.previewURL(s.info.ID, s.info.Repo, s.info.Worktree)
	}
	m.changedLocked(s)
	if prev != state {
		kind, text := stateFeed(s.info)
		m.recordLocked(s, kind, text)
	}
	if prev != state && notifyState(state) {
		m.notifyLocked(s)
	}
	if live(prev) && !live(state) {
		m.signal() // a slot freed up for the queue
	}
}

// changedLocked stamps, persists and emits a change to s.
func (m *Manager) changedLocked(s *session) {
	s.info.Updated = time.Now()
	m.persistLocked(s)
	m.emitLocked(EventState, m.snapshotLocked(s))
}

func (m *Manager) emitLocked(typ string, info Info) {
	for ch := range m.subs {
		select {
		case ch <- Event{Type: typ, Session: info}:
		default:
			m.log.Debug("event subscriber lagging; dropped", "session", info.ID)
			delete(m.subs, ch)
			close(ch)
		}
	}
}

// persistLocked rewrites meta.json atomically.
func (m *Manager) persistLocked(s *session) {
	raw, err := json.MarshalIndent(meta{Info: s.info, Task: s.task, Next: s.next, MaxAttempts: s.maxAttempts,
		AgentAttempts: s.agentAttempts, Escalated: s.escalated, NeedsApproval: s.needsApproval,
		PendingFiles: s.pendingFiles}, "", "  ")
	if err != nil {
		return
	}
	if graw, err := json.Marshal(guardOf(s.info)); err == nil {
		if err := safeWrite(m.guardPath(s.info.ID), graw, 0o600); err != nil {
			m.log.Warn("persist session guard", "id", s.info.ID, "err", err)
		}
	}
	p := filepath.Join(s.dir, "meta.json")
	if err := safeWrite(p, raw, 0o600); err != nil {
		m.log.Warn("persist session", "id", s.info.ID, "err", err)
	}
}

func (m *Manager) notifyLocked(s *session) {
	if m.notifier == nil {
		return
	}
	title, body, urgency := notification(s.info)
	if s.info.Confined {
		// Host notifications (desktop, ntfy, the command hook) are the
		// admin's: a confined (non-admin) session's owner gets an inbox
		// item only.
		m.notifier.Keep(notify.Message{Title: title, Body: body, Urgency: urgency, SessionID: s.info.ID}, s.info.Owner)
		return
	}
	url := ""
	if pub := strings.TrimRight(m.conf().Server.PublicURL, "/"); pub != "" {
		url = pub + "/#/s/" + s.info.ID
	}
	m.notifier.Send(notify.Message{Title: title, Body: body, Urgency: urgency, SessionID: s.info.ID, URL: url})
}

// notification is the title, body and urgency for entering info.State.
func notification(info Info) (string, string, string) {
	name := info.Title
	if name == "" {
		name = info.ID
	}
	repo := workspaceLabel(info)
	switch info.State {
	case StateWaiting:
		reason := info.WaitingReason
		if reason == "" {
			reason = "input needed"
		}
		return "Agent waiting: " + name, repo + " · " + info.Agent + ": " + reason, "normal"
	case StatePassed:
		if info.Workspace != WorkspaceRepo {
			return "Finished: " + name, repo + " · ready to review", "normal"
		}
		return "Checks passed: " + name, repo + " · ready to review and accept", "normal"
	case StateFailed:
		return "Session failed: " + name, repo + " · " + firstLine(orDefault(info.Error, "checks failed")), "normal"
	case StateEscalated:
		return "Escalated: " + name, repo + " · handed over to " + info.Agent, "normal"
	default:
		return "Session error: " + name, repo + " · " + firstLine(info.Error), "critical"
	}
}

// workspaceLabel names where a session works, for notifications.
func workspaceLabel(info Info) string {
	switch {
	case info.Workspace == WorkspaceScratch:
		return "scratch"
	case info.Dir != "":
		return filepath.Base(info.Dir)
	default:
		return filepath.Base(info.Repo)
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
