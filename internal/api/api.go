// Package api is Cerberus' HTTP front door. It serves the JSON and SSE
// control API under /api, mounts the OpenAI-compatible gateway at /v1, serves
// session previews at /preview/<id>/ (or, with server.preview_ports, each on
// an origin of its own: previewsrv.go) and the embedded web UI at /.
//
// Every route except /healthz, /login and /assets/* needs the bearer token
// (header, cookie, or a one-time ?token= on GET that becomes a cookie). See
// auth.go for the rules, including the CSRF header for cookie-authenticated
// writes.
//
// Phase 2 adds the vault (/api/secrets: names and metadata only, values go in
// and never come out), spend, budgets and subscription plans on /api/usage,
// session history (/api/search, resume, prune, transcripts), fleet tasks
// (/api/repos/find, /api/batches) and voice transcription (/api/voice).
//
// The control center (control.go) adds session metadata (PATCH), bulk
// actions, kill, spawn approval, the activity feed and a multiplexed wall
// stream. agentscope.go is the restricted /api/agent/* scope through which
// an agent (via `cerberus mcp` or the built-in agent's cerberus_* tools)
// spawns and steers its own child sessions, and lists and reads its own
// session's files, and nothing else.
//
// File drop (files.go, day phase 2) adds uploads into a session's workspace
// (streamed multipart), the workspace browser's list, raw, delete and mkdir,
// and staged New task attachments (/api/uploads, TaskSpec.Staging).
//
// Safety (day phase 3, safety.go and security.go): every request must name
// this server in its Host header (DNS rebinding); paired devices have their
// own revocable tokens (subpackage devices) and the admin token can be
// rotated; auth failures are rate limited per address and SSE streams are
// capped per actor; every state-changing request and auth failure goes to
// the audit log (GET /api/audit); GET /api/doctor runs the safety checklist
// (subpackage doctor). Subpackage qr draws pairing QR codes for the CLI.
//
// Startup and configuration (day phase 4, config.go): GET and PATCH
// /api/config (atomic, validated, saved, applied live through Reloaders
// where possible, else restart_pending), the schema, a clean self-restart
// and the first-run flag.
//
// LLM chat (day phase 5, chat.go): the /api/chats routes over
// internal/chat (create, list, read, delete, send, the event stream with
// replay, permission answers, stop, search, session rules, Atrium and
// claude CLI imports, the model picker) and the quick answer, POST
// /api/quick. People only: the agent scope has no chat access.
//
// Extras (day phase 6, extras.go): the notification inbox (/api/inbox, and
// inbox events on /api/events), session recordings (asciicast runs, also
// for an agent's own session under /api/agent/runs), the model download
// manager (/api/downloads, progress on /api/events), the leaderboard
// (/api/leaderboard) and the PWA files (/manifest.webmanifest, /sw.js).
//
// Flows (flows.go, flowhost.go): saved recipes of model calls and agent
// sessions run by hand, by the CLI or on a schedule (/api/flows: flows,
// runs, approvals, run graphs, templates; flowrun events on /api/events),
// and FlowHost, the daemon's implementations of the flow engine's
// dependencies.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/files"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/gateway"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/permits"
	"carino.systems/cerberus/internal/records"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/subs"
	"carino.systems/cerberus/internal/sysmon"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
	"carino.systems/cerberus/internal/vault"
	"carino.systems/cerberus/internal/voice"
	"carino.systems/cerberus/web"
)

const (
	maxBodyBytes  = 1 << 20
	pingEvery     = 20 * time.Second
	acceptTimeout = 5 * time.Minute
)

// Deps are the services the API exposes. Any of them may be nil; the routes
// that need a missing one answer 503.
type Deps struct {
	Cfg      *config.Config
	Engine   *engine.Manager
	GPU      *gpu.Monitor
	Usage    *usage.Store
	Gateway  *gateway.Gateway
	Sessions *session.Manager
	Log      *slog.Logger
	Version  string
	Build    string // the short commit (and "-dirty"), shown beside the version

	// Phase 2.
	Vault *vault.Vault
	Subs  PlanTracker // usually a *subs.Tracker
	Voice *voice.Transcriber

	// Monitor phase.
	Sysmon *sysmon.Monitor
	Energy *energy.Energy

	// File drop (day phase 2).
	Files *files.Store

	// Safety (day phase 3). Audit records every state-changing request and
	// auth failure; Devices holds the paired devices; DataDir is where the
	// doctor looks for audit.jsonl and measures free space.
	// Delegation and fine-tuning examples (finetune may be nil: capture off
	// and no export).
	Finetune *finetune.Store
	// Records are the gateway's request records (G3; nil = off).
	Records *records.Store

	Audit   *audit.Logger
	Devices *devices.Store
	DataDir string

	// Startup and configuration (day phase 4, config.go). Reloaders apply a
	// changed config live; Restart (nil: no restart endpoint) shuts the
	// daemon down cleanly and starts it again. It is called once, after the
	// reply is sent.
	Reloaders []Reloader
	Restart   func()
	// RestartCheck (nil: none) runs before a restart is accepted: an error
	// means the daemon would not come back with the saved config (a listen
	// address it refuses or cannot bind), so the restart is refused (409)
	// and the daemon keeps running.
	RestartCheck func() error

	// LLM chat (day phase 5, chat.go). Nil: the chat routes answer 503.
	Chat *chat.Manager

	// Extras (day phase 6, extras.go). Inbox is the notification centre;
	// Downloads the model download manager, whose OnChange should be
	// DownloadFeed.Publish so progress reaches the event stream. Nil: those
	// routes answer 503. Recordings and the leaderboard come from Sessions,
	// Usage and Energy.
	Inbox        *inbox.Store
	Downloads    *downloads.Manager
	DownloadFeed *DownloadFeed

	// Flows (flows.go). Flows is the engine (nil: the routes answer 503,
	// except the templates); FlowFeed carries its run events to
	// /api/events and must be the engine's Publisher (FlowHost does that).
	Flows    *flows.Engine
	FlowFeed *FlowFeed

	// Users and roles (Phase 4a). Nil or empty: single-owner mode, as
	// before users existed.
	Users *users.Store

	// Permits are the saved rules ("Always allow", permits.go); nil: the
	// routes answer 503.
	Permits *permits.Store

	// Previews owns the preview ports (previewsrv.go); nil: previews stay
	// under /preview/<id>/ and live servers are not offered.
	Previews *Previews
}

// PlanTracker is what the API needs of subscription tracking: the usage
// snapshot, and a switch to scan often while someone is looking.
type PlanTracker interface {
	Snapshot() []subs.PlanUsage
	SetActive(on bool)
}

// The interfaces below are what the handlers use, so tests can supply fakes.

type sessionService interface {
	List() []session.Info
	Get(id string) (session.Info, bool)
	Create(ctx context.Context, t session.TaskSpec) ([]session.Info, error)
	Input(id string, data []byte) error
	AnswerApproval(id string, seq int, answer string) (session.Info, error)
	AnswerSave(id string, seq, option int) (session.Info, permits.Saved, error)
	Resize(id string, cols, rows int) error
	Attach(id string) ([]byte, <-chan []byte, func(), error)
	FollowUp(ctx context.Context, id, text string) error
	RunChecks(ctx context.Context, id string) error
	Stop(id string) error
	Diff(id string) (string, error)
	Accept(ctx context.Context, id string, o session.AcceptOpts) (string, error)
	Discard(id string) error
	Delete(ctx context.Context, id string, force bool) error
	Subscribe() (<-chan session.Event, func())
	Agents() []session.AgentStatus
	PreviewDir(id string) (string, bool)
	SaveTo(id, dir string, overwrite bool) (int, error)
	// SaveInto saves only inside root, resolving every step through os.Root
	// (a confined caller's own folder).
	SaveInto(id, root, dir string, overwrite bool) (int, error)
	Archive(id string, w io.Writer) error
	Browse(dir string, hidden bool) (session.Listing, error)
}

// historyService and fleetService are the Phase 2 session features.
type historyService interface {
	Search(q session.SearchQuery) []session.SearchHit
	Resume(ctx context.Context, id string, text string) error
	Prune(ctx context.Context) (session.PruneReport, error)
	Transcript(id string) ([]byte, error)
}

type fleetService interface {
	FindRepos(sel session.RepoSelector) []session.RepoMatch
	CreateBatch(ctx context.Context, b session.BatchSpec) (session.BatchInfo, []session.Info, error)
	Batches() []session.BatchInfo
	Batch(id string) (session.BatchInfo, []session.Info, bool)
	AcceptBatch(ctx context.Context, id string, o session.AcceptOpts) []session.BatchResult
	DiscardBatch(id string) []session.BatchResult
}

// secretStore never hands values out: the API has no use for Get.
type secretStore interface {
	Backend() string
	List() ([]vault.Item, error)
	Set(name, value, note string) error
	Delete(name string) error
}

type spendSource interface {
	SpendByProvider(since time.Time) map[string]float64
}

type budgetSource interface {
	Budgets() []gateway.BudgetStatus
}

type voiceService interface {
	Available() (bool, string)
	MaxSeconds() int
	Transcribe(ctx context.Context, audio io.Reader, mime string) (string, error)
}

type modelService interface {
	Models(ctx context.Context) []engine.ModelInfo
	Load(ctx context.Context, id string) error
	Unload(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
}

type gpuSource interface{ Snapshot() gpu.Snapshot }

type usageSource interface {
	Recent(n int) []usage.Record
	Aggregate(since time.Time) []usage.Agg
}

// server holds the resolved dependencies and the UI assets.
type server struct {
	cfg      *config.Config
	log      *slog.Logger
	version  string
	build    string
	started  time.Time
	sessions sessionService
	models   modelService
	gpu      gpuSource
	usage    usageSource
	gateway  http.Handler
	memAvail func() int
	ping     time.Duration

	history historyService
	fleet   fleetService
	secrets secretStore
	spend   spendSource
	budgets budgetSource
	plans   PlanTracker
	voice   voiceService
	watch   watchState
	sysmon  monitorService
	energy  energyService
	masker  secretMasker

	control controlService // control center (day phase 1)
	inputs  session.InputLog

	files *files.Store // file drop (day phase 2)

	chats *chat.Manager // LLM chat (day phase 5)

	// Extras (day phase 6, extras.go).
	inbox     inboxService
	downloads downloadService
	dlFeed    *DownloadFeed
	runs      runService
	board     boardFunc
	pwa       map[string]pwaFile

	// Safety (day phase 3, safety.go and security.go).
	audit      *audit.Logger
	devices    *devices.Store
	dataDir    string
	limiter    *failLimiter
	conns      *connTracker
	mux        *http.ServeMux
	routes     []string                    // every pattern registered on mux, in order (routeMux)
	doctorEnv  func(doctor.Env) doctor.Env // tests: fixture injection
	ifaceIPs   func() []string             // interface addresses for pairing URLs (nil: real)
	inAudit    *inputAudit                 // coalesces terminal input into one audit record per line
	tokMu      sync.RWMutex                // guards cfg.Server.Token and prevTokens
	prevTokens []prevToken                 // admin tokens rotated away by this process

	// Configuration (day phase 4, config.go).
	cfgMu     sync.Mutex                    // one settings change (or rotation) at a time; taken before tokMu
	live      atomic.Pointer[config.Config] // the applied config (nil: cfg); never changed once stored
	boot      *config.Config                // the config at start, for restart_pending
	reloaders []Reloader
	restart   func()
	restartOK func() error // RestartCheck
	diskSum   [32]byte     // sha256 of config.json as the daemon last loaded or wrote it; guarded by cfgMu

	// Delegation (delegate.go).
	delegate   delegateState
	delegation delegationSource
	finetune   *finetune.Store
	records    *records.Store
	busy       func() map[string]gateway.Busy // G5: in-flight age and stalls per model
	guardStats func() gateway.GuardStats      // G2: the agent guard's counters
	bench      benchJobs                      // G6: the bench run going now
	usageFeed  *usageFeed                     // "usage" hints on /api/events (usagefeed.go)

	// Flows (flows.go).
	flows    *flows.Engine
	flowFeed *FlowFeed

	// Users and roles (Phase 4a; login.go, me.go, users.go, owner.go).
	users     *users.Store
	permits   *permits.Store
	notFound  []byte // web/dist/404.html (nil: the built-in page)
	previews  *Previews
	budgetHit budgetNotes

	ui    fs.FS
	index []byte
	etags map[string]string
}

// New builds the whole HTTP handler.
func New(d Deps) http.Handler {
	s := newServer(d)
	if d.Sessions != nil {
		s.sessions, s.history, s.fleet, s.masker = d.Sessions, d.Sessions, d.Sessions, d.Sessions
		s.control = d.Sessions
	}
	if d.Engine != nil {
		s.models = d.Engine
	}
	if d.GPU != nil {
		s.gpu = d.GPU
	}
	if d.Usage != nil {
		s.usage, s.spend, s.delegation = d.Usage, d.Usage, d.Usage
		s.usageFeed = newUsageFeed()
		d.Usage.OnAdd(s.usageFeed.publish)
	}
	s.finetune = d.Finetune
	s.records = d.Records
	if d.Gateway != nil {
		s.gateway, s.budgets = d.Gateway.Handler(), d.Gateway
		s.busy = d.Gateway.Busy
		s.guardStats = d.Gateway.GuardStats
	}
	if d.Vault != nil {
		s.secrets = d.Vault
	}
	if d.Subs != nil {
		s.plans = d.Subs
	}
	if d.Voice != nil {
		s.voice = d.Voice
	}
	if d.Sysmon != nil {
		s.sysmon = d.Sysmon
	}
	if d.Energy != nil {
		s.energy = d.Energy
	}
	s.files = d.Files
	s.chats = d.Chat
	if d.Inbox != nil {
		s.inbox = d.Inbox
	}
	if d.Downloads != nil {
		s.downloads = d.Downloads
	}
	s.dlFeed = d.DownloadFeed
	s.flows, s.flowFeed = d.Flows, d.FlowFeed
	if d.Sessions != nil {
		s.runs = d.Sessions
	}
	if d.Usage != nil {
		var list func() []session.Info
		if d.Sessions != nil {
			list = d.Sessions.List
		}
		s.board = newBoard(d.Usage, list, d.Energy)
	}
	return s.handler()
}

// newServer fills everything except the services (which New or tests set).
func newServer(d Deps) *server {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	cfg := d.Cfg
	if cfg == nil {
		cfg = config.Defaults()
	}
	s := &server{cfg: cfg, log: log, version: d.Version, build: d.Build, started: time.Now(),
		memAvail: memAvailable, ping: pingEvery,
		audit: d.Audit, devices: d.Devices, dataDir: d.DataDir,
		limiter: newFailLimiter(), conns: newConnTracker(), inAudit: newInputAudit(),
		boot: cfg.Clone(), reloaders: d.Reloaders, restart: d.Restart, restartOK: d.RestartCheck,
		users: d.Users, permits: d.Permits, previews: d.Previews}
	if d.Previews != nil {
		d.Previews.srv.Store(s)
	}
	s.applyUsersConfig(cfg)
	s.noteDisk()
	s.loadUI(web.FS)
	return s
}

// handler wires the routes and wraps them, outside in: security headers,
// the Host check, auth (with the failure rate limit, stream cap and
// revocation) and the audit log.
func (s *server) handler() http.Handler {
	mux := &routeMux{ServeMux: http.NewServeMux(), wrap: s.gate}
	s.mux = mux.ServeMux
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	s.apiRoutes(mux)
	mux.Handle("/v1/", http.StripPrefix("/v1", http.HandlerFunc(s.handleGateway)))
	mux.HandleFunc("GET /preview/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /preview/{id}/{path...}", s.handlePreview)
	s.uiRoutes(mux)
	s.userRoutes(mux)
	mux.HandleFunc("/", s.handleNotFound)
	s.routes = mux.patterns
	return s.withHeaders(s.withHost(s.withAuth(s.withAudit(mux))))
}

// routeMux is the ServeMux plus a record of every pattern registered on
// it, in order, so the route table can be enumerated (server.routes; the
// route matrix test classifies every entry).
type routeMux struct {
	*http.ServeMux
	patterns []string
	// wrap puts the route class check (roles.go) around every handler;
	// it panics for a pattern without a class.
	wrap func(pattern string, h http.Handler) http.Handler
}

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}

func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	if m.wrap != nil {
		h = m.wrap(pattern, h)
	}
	m.ServeMux.Handle(pattern, h)
}

func (s *server) apiRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("POST /api/models/{rest...}", s.handleModelAction)
	mux.HandleFunc("GET /api/usage", s.handleUsage)
	mux.HandleFunc("POST /api/usage/watch", s.handleUsageWatch)
	mux.HandleFunc("GET /api/repos", s.handleRepos)
	mux.HandleFunc("GET /api/repos/find", s.handleRepoFind)
	mux.HandleFunc("GET /api/bench", s.handleBenchList)
	mux.HandleFunc("POST /api/bench", s.handleBenchStart)
	mux.HandleFunc("GET /api/bench/{id}", s.handleBenchGet)
	mux.HandleFunc("GET /api/records", s.handleRecords)
	mux.HandleFunc("GET /api/records/{day}/{file}/{offset}", s.handleRecord)
	mux.HandleFunc("DELETE /api/records", s.handleRecordsDelete)
	mux.HandleFunc("GET /api/secrets", s.handleSecretList)
	mux.HandleFunc("PUT /api/secrets/{name}", s.handleSecretPut)
	mux.HandleFunc("DELETE /api/secrets/{name}", s.handleSecretDelete)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("POST /api/prune", s.handlePrune)
	mux.HandleFunc("GET /api/sessions/{id}/transcript", s.handleTranscript)
	mux.HandleFunc("GET /api/batches", s.handleBatchList)
	mux.HandleFunc("POST /api/batches", s.handleBatchCreate)
	mux.HandleFunc("GET /api/batches/{id}", s.handleBatchGet)
	mux.HandleFunc("POST /api/batches/{id}/{action}", s.handleBatchAction)
	mux.HandleFunc("GET /api/voice", s.handleVoice)
	mux.HandleFunc("POST /api/voice/transcribe", s.handleVoiceTranscribe)
	mux.HandleFunc("GET /api/sessions", s.handleSessionList)
	mux.HandleFunc("POST /api/sessions", s.handleSessionCreate)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSessionGet)
	mux.HandleFunc("GET /api/sessions/{id}/diff", s.handleSessionDiff)
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleSessionStream)
	mux.HandleFunc("GET /api/sessions/{id}/archive", s.handleSessionArchive)
	mux.HandleFunc("GET /api/sessions/{id}/preview", s.handlePreviewGet)
	mux.HandleFunc("POST /api/sessions/{id}/preview/start", s.handlePreviewStart)
	mux.HandleFunc("POST /api/sessions/{id}/preview/stop", s.handlePreviewStop)
	mux.HandleFunc("GET /api/fs", s.handleFS)
	mux.HandleFunc("POST /api/sessions/{id}/{action}", s.handleSessionAction)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionDelete)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	s.controlRoutes(mux)
	s.fileRoutes(mux)
	s.securityRoutes(mux)
	s.configRoutes(mux)
	s.chatRoutes(mux)
	s.extrasRoutes(mux)
	s.flowRoutes(mux)
	s.flowsRoutes(mux)
	s.bundleRoutes(mux)
	mux.HandleFunc("GET /api/monitor", s.handleMonitor)
	mux.HandleFunc("GET /api/monitor/processes", s.handleProcesses)
	mux.HandleFunc("GET /api/monitor/series", s.handleSeries)
	mux.HandleFunc("GET /api/monitor/stream", s.handleMonitorStream)
	mux.HandleFunc("POST /api/monitor/processes/{pid}/{action}", s.handleProcessAction)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown endpoint "+r.Method+" "+r.URL.Path)
	})
}

func (s *server) handleGateway(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"gateway not available","type":"unavailable"}}`))
		return
	}
	pr := principalOf(r)
	if id := r.Header.Get(sessionHeader); id != "" && !s.mayTagSession(pr, id) {
		// A person's key may tag usage only onto their own session or chat.
		r.Header.Del(sessionHeader)
	}
	if owner := s.gatewayOwner(r, pr); owner != "" {
		r.Header.Set(userHeader, owner)
		if msg, over := s.overBudget(owner); over {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"message": msg, "type": "budget_exceeded", "code": "user_budget"}})
			return
		}
	}
	s.gateway.ServeHTTP(w, r)
}

// mayTagSession reports whether a /v1 caller may charge its request to the
// session, chat or flow run id (X-Cerberus-Session): session agent tokens
// (withAuth pins the header to their own session) and admin-role callers
// always; everyone else only to an object they own.
func (s *server) mayTagSession(pr principal, id string) bool {
	if !s.hasUsers() || pr.Kind == kindAgent {
		return true
	}
	switch pr.Kind {
	case kindAdmin, kindLocal, kindAgentShared:
		return true
	}
	o, ok := s.objectOwner(id)
	return ok && s.sameOwner(pr, o)
}

// gatewayOwner is the user a /v1 request is charged to (docs/SCOPE.md
// "Budgets"): the caller's user for a personal key, cookie or device; the
// session's owner for a session agent token; for the admin token, the
// owner of the chat, session or flow run named by X-Cerberus-Session (or
// X-Cerberus-Owner, which daemon-internal callers set); else the legacy
// owner. "" before users exist.
func (s *server) gatewayOwner(r *http.Request, pr principal) string {
	if !s.hasUsers() {
		return ""
	}
	switch pr.Kind {
	case kindLogin, kindDevice, kindUserKey, kindAgent:
		return pr.User
	case kindAdmin:
		if o := r.Header.Get("X-Cerberus-Owner"); o != "" {
			if u, ok := s.users.Get(s.users.Owner(o)); ok {
				return u.ID
			}
		}
		if id := r.Header.Get(sessionHeader); id != "" {
			if o, ok := s.objectOwner(id); ok {
				return s.users.Owner(o)
			}
		}
	}
	return s.legacyOwner()
}

// objectOwner is the stored owner of a chat, session or flow run by id.
func (s *server) objectOwner(id string) (string, bool) {
	if s.chats != nil {
		if o, ok := s.chats.Owner(id); ok {
			return o, true
		}
	}
	if s.sessions != nil {
		if info, ok := s.sessions.Get(id); ok {
			return info.Owner, true
		}
	}
	if s.flows != nil {
		if d, err := s.flows.GetRun(id); err == nil {
			return d.Run.Owner, true
		}
	}
	return "", false
}

// ---------- JSON helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// bodyErrStatus is 413 for a body over its limit, else 400.
func bodyErrStatus(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func unavailable(w http.ResponseWriter, what string) {
	writeError(w, http.StatusServiceUnavailable, what+" not available")
}

// decodeBody reads a JSON body (bounded). An empty body leaves v untouched.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	return decodeBodyLimit(w, r, v, maxBodyBytes)
}

// decodeBodyLimit is decodeBody with its own size limit.
func decodeBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
