// Package session runs coding-agent sessions. Each session is one agent
// process in a PTY, working in its own detached git worktree. The manager
// streams and keeps the terminal output, notices when the agent waits for
// input, runs the repo's checks when the agent exits, retries or escalates to
// another agent when they fail, queues sessions behind a resource guard, and
// finally accepts the work by fast-forwarding main (never creating a branch)
// or discards it.
//
// A session works in one of three workspaces (see workspace.go): a repo's
// detached worktree, a plain folder edited in place, or a scratch folder
// Cerberus owns.
//
// Sessions live under DATA/sessions/<id>/ (meta.json, task.md,
// scrollback.log, transcript.txt, the built-in agent's conversation.json, and
// workspace/ for scratch sessions) and repo worktrees under
// DATA/worktrees/<id>/.
//
// History (history.go): every session's output is also kept as plain text
// for Search; a finished session can be resumed in place; a daily prune
// removes old finished sessions but never work that is not on main.
// Secrets (secrets.go, redact.go): with WithVault, profile and repo SecretEnv
// are injected at launch and every vault value is masked in all output.
// Fleet tasks (batch.go): one instruction becomes one repo session per repo,
// accepted or discarded together; batches live in DATA/batches/<id>.json.
//
// Control center (control.go, limits.go, feed.go, spawn.go, wall.go): user
// metadata (name, tags, pin, priority) and per-session limits, bulk actions,
// an activity feed (DATA/feed.jsonl), agents spawning child sessions within
// cfg.Agent.Spawn, and one multiplexed output stream for a wall of terminals.
package session

import (
	"errors"
	"time"
)

// Session states.
const (
	StateQueued    = "queued"
	StateStarting  = "starting"
	StateRunning   = "running"
	StateWaiting   = "waiting"
	StateChecking  = "checking"
	StatePassed    = "passed"
	StateFailed    = "failed"
	StateRetrying  = "retrying"
	StateEscalated = "escalated"
	StateAccepted  = "accepted"
	StateDiscarded = "discarded"
	StateStopped   = "stopped"
	StateError     = "error"
)

// Event types sent to Subscribe.
const (
	EventCreated = "created"
	EventState   = "state"
	EventRemoved = "removed"
)

var (
	// ErrNotFound is returned for an unknown session id.
	ErrNotFound = errors.New("session not found")
	// ErrNotRunning is returned when an action needs a live agent process.
	ErrNotRunning = errors.New("session is not running")
	// ErrBusy is returned when the session is running, checking or in the
	// middle of another operation.
	ErrBusy = errors.New("session is busy; stop it first")
	// ErrNoApproval is returned by AnswerApproval when the session no longer
	// waits for that approval (answered already, or it moved on).
	ErrNoApproval = errors.New("the session is not waiting for that approval any more")
	// ErrBadAnswer is an AnswerApproval answer that is not one line of text.
	ErrBadAnswer = errors.New("the answer must be y, a, n or one line of text (at most 500 characters)")
)

// TaskSpec is a request to start one or more sessions.
type TaskSpec struct {
	Workspace   string // "repo" | "folder" | "scratch"; "" = decided from Repo/Dir (nothing = scratch)
	Dir         string // absolute folder; a git repo top level becomes a repo session
	Repo        string // absolute path of a git repo (older clients; same as Workspace "repo" + Dir)
	Task        string // the instruction
	Agent       string // AgentProfile name (default cfg.DefaultAgent)
	Model       string // model id for agents that take one (default engine default)
	EscalateTo  string // profile to hand over to after MaxAttempts failed check runs ("" = none)
	MaxAttempts int    // attempts with Agent before escalating (default cfg.Sessions.MaxAttempts)
	Count       int    // fan-out: N independent sessions with the same task (default 1)
	Title       string

	Name     string   // user label ("" = Title)
	Tags     []string // lowercased [a-z0-9-]{1,24}, at most 8
	Priority int      // -10..10; higher starts first in the queue
	Limits   Limits
	// Parent is the session that spawned this one. Only the MCP/API spawn
	// path sets it: the spawn limits (cfg.Agent.Spawn) apply, and with
	// Workspace "same" the child works in the parent's own workspace.
	Parent string
	// Client is the MCP client (a paired outside agent, devices.ScopeMCP)
	// that started this session through the agent scope. Like Parent, only
	// that path sets it: the spawn rules apply (enabled, approval, at most
	// MaxChildren open sessions per client), the task and model may not
	// start with "-", and the session is a root of its own tree.
	Client string
	// Staging is a staging id from internal/files (files attached on the New
	// task form, in DATA/uploads/<id>/). Its files move into the session's
	// inbox; with Count > 1 every session gets its own copy.
	Staging string

	// Owner is the user id the session belongs to (Phase 4a; "" = from
	// before users, the legacy owner). Spawned children take their
	// parent's owner and confinement.
	Owner string
	// Confined runs the session as a non-admin user's: bwrap is forced
	// (the start fails without it, never a fallback), the home folder is
	// read-only, global sandbox.extra_rw is dropped and host notifications
	// are not sent (docs/SCOPE.md "Confinement of non-admin sessions").
	Confined bool

	// prepare (internal sessions only, see train.go; never set from JSON)
	// runs once the session folder and workspace exist and before the
	// session is queued. An error abandons the session.
	prepare func(dir, workspace string) error
}

// WorkspaceSame is TaskSpec.Workspace for a spawned child that works where
// its parent works: a repo parent's child gets its own worktree of the same
// repo, a folder or scratch parent's child edits that folder in place.
const WorkspaceSame = "same"

// Limits bound one session. Zero values mean the defaults: no time or
// output limit, and Guard.SessionMemoryMax for the memory cap.
type Limits struct {
	MaxMinutes   int    // each run is stopped (reason "time limit") after this long
	MemoryMax    string // systemd MemoryMax for the run's scope, e.g. "4G" (overrides Guard.SessionMemoryMax)
	MaxOutputMiB int    // each run is stopped (reason "output limit") after this much output
}

// Patch changes a session's metadata (Update). Nil fields are kept.
type Patch struct {
	Name     *string
	Tags     *[]string
	Pinned   *bool
	Priority *int
	Limits   *Limits
}

// Info is the public picture of a session.
type Info struct {
	ID, Title, Repo, Task, Agent, Model, EscalateTo string

	Workspace string // "repo" | "folder" | "scratch"
	Dir       string // the chosen folder (repo top level or plain folder); "" for scratch

	State    string
	Attempt  int
	ExitCode *int
	Created  time.Time
	Updated  time.Time // last change of any kind (state, rename, accept...): not when the run ended

	// RunStarted is when the first run's process started (zero until one
	// has); RunEnded is when the latest run's process was reaped, or when
	// it was last seen alive if the daemon stopped under it. Together they
	// bound the time the session actually ran, unlike Created/Updated.
	RunStarted time.Time
	RunEnded   time.Time

	Worktree     string // where the agent works: the repo's worktree, the folder itself, or the scratch folder
	BaseCommit   string // repo sessions only
	ChecksOutput string
	PreviewURL   string
	Preview      *PreviewInfo `json:",omitempty"` // static, live and Run preview summary (filled on read)
	Error        string
	Group        string // fan-out group id

	WaitingReason string
	Cols, Rows    int

	// WaitingAsk is what an approval wait (WaitingReason "approval") asks
	// for, e.g. "write_file: src/x.go" (the built-in agent says it; "" when
	// not known). AskSeq counts approval waits, so an answer given from
	// outside the terminal (AnswerApproval) never lands on a later prompt.
	WaitingAsk string
	AskSeq     int
	// WaitingAlways are the rules "Every time" may save for that approval
	// (the built-in agent's "askrules" marker, validated; AnswerSave).
	WaitingAlways []AlwaysOption `json:",omitempty"`

	EnergyJ float64 // wall-side energy charged to this session (filled on read)
	CostMXN float64 // that energy at the current marginal electricity price

	Name     string   // user label; reads fall back to Title
	Tags     []string // lowercased labels for filtering
	Pinned   bool     // listed and queued first
	Priority int      // -10..10; higher starts first in the queue (ties by creation)
	Limits   Limits
	Parent   string // the session that spawned this one ("" = started by a person)
	Depth    int    // spawn depth: 0 for a person's session, parent's + 1 for a child
	Client   string // the MCP client that started it ("" = a person or a parent session)

	LastOutput  time.Time // when the agent last printed anything
	OutputBytes int64     // output bytes over every run
	Activity    string    // last non-empty output line, plain text, at most 160 characters

	UploadBytes int64 // bytes uploaded into the workspace (attachments and file drops), limited by files.max_total_mib

	// Users and roles (Phase 4a). Owner is the user id ("" = from before
	// users); OwnerName is filled by the API on read. Confined: see
	// TaskSpec.Confined.
	Owner     string
	OwnerName string
	Confined  bool `json:",omitempty"`
}

// Event is a change to a session, for live UIs.
type Event struct {
	Type    string // "state" | "created" | "removed"
	Session Info
}

// AcceptOpts controls Accept.
type AcceptOpts struct {
	Message string
	Push    bool
}

// AgentStatus describes one agent profile for pickers.
type AgentStatus struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Available   bool   `json:"available"`
	NeedsModel  bool   `json:"needs_model"`
	Interactive bool   `json:"interactive"`
}

// live reports whether a session in state holds (or is about to hold) a
// running slot for the guard.
func live(state string) bool {
	switch state {
	case StateStarting, StateRunning, StateWaiting, StateChecking, StateRetrying, StateEscalated:
		return true
	}
	return false
}

// final reports whether a session in state can no longer change.
func final(state string) bool {
	return state == StateAccepted || state == StateDiscarded
}

// notifyState reports whether entering state sends a notification.
func notifyState(state string) bool {
	switch state {
	case StateWaiting, StatePassed, StateFailed, StateEscalated, StateError:
		return true
	}
	return false
}
