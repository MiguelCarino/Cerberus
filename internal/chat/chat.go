// Package chat is Cerberus' LLM chat (day phase 5): Atrium's desktop chat
// (llm-chat.py + llm_chat_engine.py) as a server-side service for the web
// UI, so the same conversation works from any browser, a phone included.
//
// A chat runs on the server: its tools execute on this host, and closing
// the browser tab does not stop it. Each chat is a file
// DATA/chats/<id>.json (0600, written atomically) in Atrium's native
// session shape, so the two can read each other's conversations. Two
// backends run a message:
//
//   - native: any model through the gateway, with the built-in agent's
//     tool loop (agent.Run), its permission modes and the phase-3 command
//     policy;
//   - claude: the claude CLI, only when it is installed, one
//     `claude -p --output-format stream-json …` process per message with
//     the stdio permission protocol (can_use_tool → a card → the answer).
//
// Everything a run does becomes an event in Atrium's wire protocol (see
// agent.Event) plus a few chat events (user, queued, dropped, answered,
// state, status, switched, error). Events are kept in a per-chat log with
// increasing indices, so a browser that reconnects replays what it missed
// (Subscribe, ServeSSE).
//
// Permission cards are answered once, for the session (a session rule,
// which then answers every covered card by itself), or denied with a note.
// The 600 ms arming and the two-press confirm for commands are the UI's.
// A message sent while a chat is busy queues; Stop cancels the stream, ends
// the running command's process group, denies the open cards and drops
// the queue. When a chat finishes, fails or waits for an answer while no
// browser watches it, the notifier says so.
package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/permits"
)

// Notifier sends notifications (*notify.Notifier).
type Notifier interface {
	Send(notify.Message)
}

// Options configure a Manager. Only DataDir and Config are required.
type Options struct {
	DataDir string                // chats live in DataDir/chats
	Config  func() *config.Config // the live config
	// Gateway returns the OpenAI-compatible base URL (ending in /v1) and the
	// key to send. Nil: http://<server.listen>/v1 with the admin token.
	Gateway func() (base, key string)
	// Notifier tells the owner about finished and waiting chats nobody
	// watches (nil: no notifications).
	Notifier Notifier
	// ContextLength is a model's context window in tokens (0 = unknown).
	ContextLength func(model string) int
	// ResolveModel maps an alias ("default") to the model the gateway would
	// use now; a chat on "default" keeps that model from its first message
	// on, so a later unload cannot leave "default" pointing nowhere.
	ResolveModel func(ctx context.Context, id string) (string, error)
	// DefaultModel is the model a chat uses when none is named.
	DefaultModel func() string
	// Claude finds the claude CLI ("" = not installed). Nil looks on $PATH
	// and in ~/.local/bin.
	Claude func() string
	// Home is the default working directory (default: the user's home).
	Home string
	// AtriumDir holds Atrium's sessions (default
	// $XDG_STATE_HOME/hypr/llm-chat/sessions).
	AtriumDir string
	// ClaudeProjects holds the claude CLI's sessions (default ~/.claude/projects).
	ClaudeProjects string
	Log            *slog.Logger
	Now            func() time.Time
	// Access says what a chat owner's runs may do (Phase 4a, docs/SCOPE.md
	// "Chats of non-admin users"); it is asked at every run, so a role
	// change applies to the next message. Nil: full access (admin).
	Access func(owner string) Access
	// IsAdmin is a cheap form of Access(owner).Admin (no sandbox probe),
	// used under the manager's lock. Nil: Access is asked.
	IsAdmin func(owner string) bool
	// Permits are the saved rules ("Always allow"): they answer the cards
	// of their owner's chats (nil: none saved, none offered).
	Permits *permits.Store
}

// Access is what one chat owner may do.
type Access struct {
	Admin   bool                         // the claude backend and any working folder
	NoTools bool                         // plain chat only (role chat)
	Cwd     string                       // forced working folder ("" = the chat's own)
	Jail    bool                         // nothing outside Cwd (agent.RunOptions.Jail)
	Wrap    func(argv []string) []string // run_command in a sandbox (nil with Jail: no commands)
}

// ErrBackend refuses the claude backend to a non-admin.
var ErrBackend = errors.New("the claude chat backend is for admins only")

// access is Options.Access for owner (full access when unset).
func (m *Manager) access(owner string) Access {
	if m.o.Access == nil {
		return Access{Admin: true}
	}
	return m.o.Access(owner)
}

// Owner is the stored owner of a chat (false: no such chat).
func (m *Manager) Owner(id string) (string, bool) {
	m.mu.Lock()
	if l := m.lives[id]; l != nil {
		o := l.chat.Owner
		m.mu.Unlock()
		return o, true
	}
	m.mu.Unlock()
	c, err := m.store.load(id)
	if err != nil {
		return "", false
	}
	return c.Owner, true
}

// Manager owns every chat.
type Manager struct {
	o     Options
	store store

	// epoch names this process's event logs: indices restart at 0 with
	// the daemon, so a browser that sees another epoch reloads the chat.
	epoch string

	mu      sync.Mutex
	lives   map[string]*live
	closing bool // Stopping or Close ran: no new runs
	wg      sync.WaitGroup

	contMu sync.Mutex // one "continue" at a time (bySource + create + save)

	askSubs map[chan struct{}]struct{} // SubscribeAsks (guarded by mu)
}

// maxIdleLives is how many idle chats stay in memory (their messages,
// images included, and event logs); older idle ones are loaded again from
// disk when opened. Busy, watched or waiting chats, and chats with session
// rules or a queue, always stay.
const maxIdleLives = 16

// maxEvents is how many events a chat's log keeps for replay.
const maxEvents = 5000

// live is a chat in memory: its conversation, its run and its event log.
type live struct {
	chat *Chat

	events []Event // the log, oldest first; events[0].Index == base
	base   int
	next   int // the next event's index
	subs   map[*sub]struct{}

	busy    bool
	cancel  context.CancelFunc
	started time.Time
	elapsed float64 // the last run's seconds
	state   string  // idle | thinking | writing | tool | waiting | done | error | stopped
	queue   []queued
	asks    []*pendingAsk
	rules   []Rule
	ctxMax  int // a context window the backend reported (claude)
	stopped bool
	// chans holds every card's answer channel from its ask event until the
	// run's wait takes the answer, so an answer given before the waiter
	// looks is never lost.
	chans    map[string]chan answer
	runStart int       // the running message's "user" event index
	used     time.Time // last looked up (idle eviction)
}

// queued is a message waiting for the chat to be free.
type queued struct {
	Text   string
	Images []string
	Model  string
	Mode   string
}

// pendingAsk is an open permission card.
type pendingAsk struct {
	req agent.AskRequest
	ch  chan answer // buffered (1): the answer is never lost
}

type answer struct {
	d       agent.Decision
	updated []map[string]any // claude: updatedPermissions for a session rule
}

// sub is one stream subscriber.
type sub struct{ ch chan Event }

// Event is one entry of a chat's log. Raw is the JSON sent to browsers:
// the agent event's wire fields (or a chat event's) plus "i", the index.
type Event struct {
	Index int
	Type  string
	Raw   json.RawMessage
}

// Errors.
var (
	ErrBusy      = errors.New("the chat is busy")
	ErrNoRequest = errors.New("no such permission request (answered already?)")
	ErrNoClaude  = errors.New("the claude CLI is not installed: pick another model")
	ErrDisabled  = errors.New("the LLM chat is turned off (chat.enabled)")
	ErrEmpty     = errors.New("nothing to send: write a message or attach an image")
	ErrClosing   = errors.New("Cerberus is shutting down: send it again in a moment")
)

// New makes a Manager. It creates DataDir/chats.
func New(o Options) (*Manager, error) {
	if o.DataDir == "" || o.Config == nil {
		return nil, errors.New("chat: DataDir and Config are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	dir := filepath.Join(o.DataDir, "chats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return &Manager{o: o, store: store{dir: dir}, lives: map[string]*live{}, epoch: hex.EncodeToString(b[:])}, nil
}

// Stopping stops every running chat and refuses new messages, without
// waiting. The daemon calls it before its listeners go down, so a native
// run whose loopback /v1 request is cut ends as stopped (no "failed"
// notification, no queued message started against a dead gateway).
func (m *Manager) Stopping() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closing = true
	for _, l := range m.lives {
		m.stopLocked(l)
	}
}

// Close stops every running chat and waits for the runs to end.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.Stopping()
	m.wg.Wait()
}

// Epoch names this process's event logs (see View.Epoch).
func (m *Manager) Epoch() string { return m.epoch }

func (m *Manager) cfg() *config.Config {
	if c := m.o.Config(); c != nil {
		return c
	}
	return config.Defaults()
}

func (m *Manager) now() time.Time { return m.o.Now() }

// claudePath is the claude CLI, or "".
func (m *Manager) claudePath() string {
	if m.o.Claude != nil {
		return m.o.Claude()
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	if m.o.Home != "" {
		p := filepath.Join(m.o.Home, ".local", "bin", "claude")
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// ClaudeAvailable reports whether the claude backend can run.
func (m *Manager) ClaudeAvailable() bool { return m.claudePath() != "" }

// Home is the default working directory, so the UI can show paths under it as ~/….
func (m *Manager) Home() string { return m.o.Home }

// BackendOf names the backend a model runs on: "claude" or "claude:<model>"
// is the claude CLI, anything else the gateway.
func BackendOf(model string) string {
	low := strings.ToLower(strings.TrimSpace(model))
	if low == "claude" || strings.HasPrefix(low, "claude:") {
		return BackendClaude
	}
	return BackendNative
}

// CreateOptions start a chat.
type CreateOptions struct {
	Cwd   string `json:"cwd"`   // working directory (default: home)
	Model string `json:"model"` // default: the default model
	Mode  string `json:"mode"`  // default: chat.default_mode
	Owner string `json:"-"`     // the creating user's id (set by the API, never from the body)
}

// Create starts an empty chat and saves it.
func (m *Manager) Create(o CreateOptions) (*Chat, error) {
	cfg := m.cfg()
	if !cfg.Chat.Enabled {
		return nil, ErrDisabled
	}
	acc := m.access(o.Owner)
	var cwd string
	var err error
	if acc.Cwd != "" {
		if err := os.MkdirAll(acc.Cwd, 0o700); err != nil {
			return nil, err
		}
		cwd = acc.Cwd
	} else if cwd, err = m.checkCwd(o.Cwd); err != nil {
		return nil, err
	}
	mode, err := parseMode(orStr(o.Mode, cfg.Chat.DefaultMode))
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(o.Model)
	if model == "" {
		model = m.defaultModel(cfg)
	}
	backend := BackendOf(model)
	if backend == BackendClaude && !acc.Admin {
		return nil, ErrBackend
	}
	if backend == BackendClaude && !m.ClaudeAvailable() {
		return nil, ErrNoClaude
	}
	now := m.now()
	c := &Chat{ID: newID(now), Cwd: cwd, Model: model, Created: unix(now), Updated: unix(now),
		Messages: []agent.Message{}, Backend: backend, Mode: mode, Owner: o.Owner}
	if err := m.store.save(c); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.addLocked(newLive(c))
	m.mu.Unlock()
	return clone(c), nil
}

func newLive(c *Chat) *live {
	return &live{chat: c, subs: map[*sub]struct{}{}, state: "idle", runStart: -1}
}

// addLocked keeps l in memory and evicts the least recently used idle
// chats beyond maxIdleLives.
func (m *Manager) addLocked(l *live) {
	l.used = m.now()
	m.lives[l.chat.ID] = l
	m.evictLocked()
}

// evictable: nothing in memory would be lost but the log and the images,
// which a reload from disk replaces with the saved file.
func evictable(l *live) bool {
	return !l.busy && len(l.subs) == 0 && len(l.asks) == 0 && len(l.queue) == 0 && len(l.rules) == 0
}

func (m *Manager) evictLocked() {
	var idle []*live
	for _, l := range m.lives {
		if evictable(l) {
			idle = append(idle, l)
		}
	}
	if len(idle) <= maxIdleLives {
		return
	}
	slices.SortFunc(idle, func(a, b *live) int { return a.used.Compare(b.used) })
	for _, l := range idle[:len(idle)-maxIdleLives] {
		delete(m.lives, l.chat.ID)
	}
}

func (m *Manager) defaultModel(cfg *config.Config) string {
	if m.o.DefaultModel != nil {
		if s := m.o.DefaultModel(); s != "" {
			return s
		}
	}
	return orStr(cfg.Engines.DefaultModel, "default")
}

// checkCwd resolves a chat's working directory: absolute, existing.
func (m *Manager) checkCwd(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		cwd = m.o.Home
	}
	cwd = expandHome(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("the working folder must be an absolute path, not %q", cwd)
	}
	cwd = filepath.Clean(cwd)
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return "", fmt.Errorf("the working folder %s does not exist", cwd)
	}
	return cwd, nil
}

func parseMode(s string) (string, error) {
	md, ok := agent.ParseMode(s)
	if !ok || md == agent.ModeBypass {
		return "", fmt.Errorf("unknown mode %q: ask, auto, acceptEdits or plan", s)
	}
	return string(md), nil
}

// lookup returns the live chat, loading it from disk. Call with m.mu held.
func (m *Manager) lookup(id string) (*live, error) {
	if l := m.lives[id]; l != nil {
		l.used = m.now()
		return l, nil
	}
	c, err := m.store.load(id)
	if err != nil {
		return nil, err
	}
	c.ID = id
	l := newLive(c)
	m.addLocked(l)
	return l, nil
}

// clone copies c deeply enough for a caller to read while runs go on.
func clone(c *Chat) *Chat {
	cc := *c
	cc.Messages = slices.Clone(c.Messages)
	return &cc
}

// Summary is one row of the chat list.
type Summary struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"` // "cerberus"
	Title   string  `json:"title"`
	Cwd     string  `json:"cwd"`
	Model   string  `json:"model"`
	Backend string  `json:"backend"`
	Created float64 `json:"created"`
	Updated float64 `json:"updated"`
	Busy    bool    `json:"busy"`
	Waiting int     `json:"waiting"`
	Source  string  `json:"source,omitempty"`
	// Owner is the chat's user id; OwnerName is filled by the API.
	Owner     string `json:"owner,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
}

// BusyOwners lists the owners of the chats running now (cheap: no disk).
func (m *Manager) BusyOwners() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, l := range m.lives {
		if l.busy {
			out = append(out, l.chat.Owner)
		}
	}
	return out
}

// List returns the saved chats, newest first.
func (m *Manager) List() []Summary {
	chats := listDir(m.store.dir)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Summary, 0, len(chats))
	for _, c := range chats {
		if l := m.lives[c.ID]; l != nil {
			c = l.chat
		}
		s := Summary{ID: c.ID, Kind: "cerberus", Title: orStr(c.Title, "(no words)"), Cwd: c.Cwd, Model: c.Model,
			Backend: orStr(c.Backend, BackendNative), Created: c.Created, Updated: c.Updated, Source: c.Source,
			Owner: c.Owner}
		if l := m.lives[c.ID]; l != nil {
			s.Busy, s.Waiting = l.busy, len(l.asks)
		}
		out = append(out, s)
	}
	return out
}

// Status is a chat's status line: context use, time, cost and state.
type Status struct {
	Model    string  `json:"model"`
	Backend  string  `json:"backend"`
	Mode     string  `json:"mode"`
	Cwd      string  `json:"cwd"`
	Used     int     `json:"used"`    // tokens in the context
	Context  int     `json:"context"` // the model's context length (0 = unknown)
	Elapsed  float64 `json:"elapsed"` // seconds of the running (or last) run
	CostUSD  float64 `json:"cost"`
	Busy     bool    `json:"busy"`
	State    string  `json:"state"`
	Waiting  int     `json:"waiting"`
	Queue    int     `json:"queue"`
	Watchers int     `json:"watchers"`
}

// View is a chat with its live state (GET /api/chats/{id}).
type View struct {
	Chat   *Chat              `json:"chat"`
	Status Status             `json:"status"`
	Asks   []agent.AskRequest `json:"asks"`
	Queue  []string           `json:"queue"`
	Rules  []Rule             `json:"rules"`
	Next   int                `json:"next"` // the next event index: stream from here
	// Base is the oldest event the log still holds; RunFrom the running
	// message's "user" event (-1 when idle). A busy chat replays from
	// RunFrom when RunFrom >= Base, else the log lost the run's start and
	// the saved messages are all there is.
	Base    int    `json:"base"`
	RunFrom int    `json:"run_from"`
	Epoch   string `json:"epoch"` // changes when the daemon restarts (indices start at 0 again)
}

// Get returns a chat and its state.
func (m *Manager) Get(id string) (*View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	v := &View{Chat: clone(l.chat), Status: m.statusLocked(l), Asks: []agent.AskRequest{}, Queue: []string{},
		Rules: slices.Clone(l.rules), Next: l.next, Base: l.base, RunFrom: -1, Epoch: m.epoch}
	if l.busy {
		v.RunFrom = l.runStart
	}
	if v.Rules == nil {
		v.Rules = []Rule{}
	}
	for _, a := range l.asks {
		v.Asks = append(v.Asks, a.req)
	}
	for _, q := range l.queue {
		v.Queue = append(v.Queue, q.Text)
	}
	return v, nil
}

func (m *Manager) statusLocked(l *live) Status {
	c := l.chat
	s := Status{Model: c.Model, Backend: orStr(c.Backend, BackendNative), Mode: c.Mode, Cwd: c.Cwd,
		Used: c.ContextUsed, CostUSD: c.CostUSD, Busy: l.busy, State: l.state, Waiting: len(l.asks),
		Queue: len(l.queue), Watchers: len(l.subs), Elapsed: l.elapsed}
	if l.busy {
		s.Elapsed = m.now().Sub(l.started).Seconds()
	}
	s.Context = l.ctxMax
	if s.Context == 0 && m.o.ContextLength != nil && s.Backend == BackendNative {
		s.Context = m.o.ContextLength(c.Model)
	}
	return s
}

// Delete removes a chat that is not running.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.lives[id]; l != nil {
		if l.busy {
			return ErrBusy
		}
		for s := range l.subs {
			close(s.ch)
			delete(l.subs, s)
		}
		delete(m.lives, id)
	}
	return m.store.remove(id)
}

// Rules lists a chat's session rules; ForgetRules drops them.
func (m *Manager) ForgetRules(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.lookup(id)
	if err != nil {
		return err
	}
	l.rules = nil
	m.emitLocked(l, "rules", map[string]any{"rules": []Rule{}})
	return nil
}

// SetOptions changes a chat's model, mode or working directory for the
// next message. A model on the other backend is refused for a chat that
// has messages: send with that model instead, which starts a new chat.
func (m *Manager) SetOptions(id string, o CreateOptions) (*Chat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	c := l.chat
	if o.Mode != "" {
		mode, err := parseMode(o.Mode)
		if err != nil {
			return nil, err
		}
		c.Mode = mode
	}
	if o.Model != "" {
		if b := BackendOf(o.Model); b != orStr(c.Backend, BackendNative) && len(c.Messages) > 0 {
			return nil, fmt.Errorf("%s runs on the %s backend: send a message with it to start a new chat", o.Model, b)
		}
		c.Model, c.Backend = strings.TrimSpace(o.Model), BackendOf(o.Model)
	}
	if o.Cwd != "" {
		if l.busy {
			return nil, ErrBusy
		}
		cwd, err := m.checkCwd(o.Cwd)
		if err != nil {
			return nil, err
		}
		c.Cwd = cwd
	}
	c.Updated = unix(m.now())
	if err := m.store.save(c); err != nil {
		return nil, err
	}
	m.emitLocked(l, "status", m.statusLocked(l))
	return clone(c), nil
}

// ---- the event log

// emitLocked appends a chat event (data's fields beside "type" and "i").
func (m *Manager) emitLocked(l *live, typ string, data any) {
	fields := map[string]any{}
	if data != nil {
		raw, err := json.Marshal(data)
		if err == nil {
			_ = json.Unmarshal(raw, &fields)
		}
		if fields == nil {
			fields = map[string]any{"data": data}
		}
	}
	fields["type"] = typ
	raw, err := json.Marshal(fields)
	if err != nil {
		return
	}
	m.appendLocked(l, typ, raw)
}

// emitAgentLocked appends an engine event and tracks the chat's state.
func (m *Manager) emitAgentLocked(l *live, ev agent.Event) {
	switch ev.Type {
	case agent.EvStart, agent.EvReasoning, agent.EvToolDone:
		l.state = "thinking"
	case agent.EvText:
		l.state = "writing"
	case agent.EvTool:
		l.state = "tool"
	case agent.EvAsk:
		l.state = "waiting"
		if ev.Req != nil {
			m.offerLocked(l, ev.Req)
			l.asks = append(l.asks, &pendingAsk{req: *ev.Req, ch: l.chanFor(ev.Req.ID)})
		}
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	m.appendLocked(l, ev.Type, raw)
}

// appendLocked stamps raw with the next index, keeps it and fans it out.
// A subscriber that cannot keep up is dropped; it reconnects from its last
// index.
func (m *Manager) appendLocked(l *live, typ string, raw []byte) {
	idx := l.next
	l.next++
	stamped := append([]byte(fmt.Sprintf(`{"i":%d,`, idx)), raw[1:]...)
	if len(raw) == 2 { // "{}"
		stamped = []byte(fmt.Sprintf(`{"i":%d}`, idx))
	}
	ev := Event{Index: idx, Type: typ, Raw: stamped}
	if typ == agent.EvAsk || typ == "answered" {
		defer m.pokeAsksLocked()
	}
	l.events = append(l.events, ev)
	if len(l.events) > maxEvents {
		drop := len(l.events) - maxEvents
		l.events = slices.Delete(l.events, 0, drop)
		l.base += drop
	}
	for s := range l.subs {
		select {
		case s.ch <- ev:
		default:
			close(s.ch)
			delete(l.subs, s)
		}
	}
}

// Subscribe returns the events from index since on (the backlog the log
// still holds) and a channel for the ones to come. The channel closes when
// the subscriber falls behind or the chat is deleted; cancel ends the
// subscription. While subscribed, the chat counts as watched.
func (m *Manager) Subscribe(id string, since int) (backlog []Event, ch <-chan Event, cancel func(), err error) {
	backlog, ch, cancel, _, err = m.subscribe(id, since)
	return
}

// Hello is what a stream says first: the log's epoch and range, so a
// browser notices a restart (another epoch, or next below what it has) or
// a gap (base above the index it asked for) and reloads the chat.
type Hello struct {
	Epoch string `json:"epoch"`
	Base  int    `json:"base"`
	Next  int    `json:"next"`
}

func (m *Manager) subscribe(id string, since int) (backlog []Event, ch <-chan Event, cancel func(), hello Hello, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.lookup(id)
	if err != nil {
		return nil, nil, nil, Hello{}, err
	}
	hello = Hello{Epoch: m.epoch, Base: l.base, Next: l.next}
	if since > l.next { // an index from before a restart: replay what there is
		since = l.base
	}
	from := max(since, l.base) - l.base
	if from < len(l.events) {
		backlog = slices.Clone(l.events[from:])
	}
	s := &sub{ch: make(chan Event, 512)}
	l.subs[s] = struct{}{}
	var once sync.Once
	cancel = func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if _, ok := l.subs[s]; ok {
				delete(l.subs, s)
				close(s.ch)
			}
		})
	}
	return backlog, s.ch, cancel, hello, nil
}

// chanFor is req's answer channel, made on first use.
func (l *live) chanFor(id string) chan answer {
	if l.chans == nil {
		l.chans = map[string]chan answer{}
	}
	ch := l.chans[id]
	if ch == nil {
		ch = make(chan answer, 1)
		l.chans[id] = ch
	}
	return ch
}

// watchedLocked reports whether a browser follows the chat.
func watchedLocked(l *live) bool { return len(l.subs) > 0 }

// notifyLocked tells the owner about a chat nobody watches.
func (m *Manager) notifyLocked(l *live, title, body, urgency string) {
	if m.o.Notifier == nil || watchedLocked(l) {
		return
	}
	link := ""
	if base := strings.TrimRight(m.cfg().Server.PublicURL, "/"); base != "" {
		link = base + "/#/chat/" + l.chat.ID
	}
	msg := notify.Message{Title: title, Body: truncRunes(body, 200), Urgency: urgency, SessionID: l.chat.ID, URL: link}
	admin := m.o.IsAdmin == nil && m.access(l.chat.Owner).Admin || m.o.IsAdmin != nil && m.o.IsAdmin(l.chat.Owner)
	if !admin {
		// A non-admin's chat: an inbox item for them, never a host notification.
		if k, ok := m.o.Notifier.(interface{ Keep(notify.Message, string) }); ok {
			k.Keep(msg, l.chat.Owner)
		}
		return
	}
	m.o.Notifier.Send(msg)
}
