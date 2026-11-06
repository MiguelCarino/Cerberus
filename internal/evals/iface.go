package evals

import (
	"context"
	"time"
)

// ClientName is the usage client the judge's calls are recorded under;
// ModelRequest.Run (the eval run id) is recorded as their session.
const ClientName = "cerberus-eval"

// EventType is the event-stream type of a run Event.
const EventType = "evalrun"

// Message is one chat message ("system", "user" or "assistant").
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ModelRequest is one judge call. The host sends it through the gateway
// at temperature 0 with thinking off and at most MaxTokens of answer.
type ModelRequest struct {
	Model     string
	Messages  []Message
	MaxTokens int
	Client    string // ClientName
	Run       string // the run id: usage is recorded with it as the session
	Owner     string // the run's owner: charged for the call
}

// ModelReply is a completion and its usage.
type ModelReply struct {
	Text             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	EnergyWh         float64
}

// ModelCaller runs one chat completion. It honours ctx.
type ModelCaller interface {
	Complete(ctx context.Context, req ModelRequest) (ModelReply, error)
}

// StartRequest starts one agent session of a run. The session is owned by
// Owner, tagged "eval" and the eval id, and never auto-accepted. Base is
// the pinned commit of a repo eval.
type StartRequest struct {
	Agent     string
	Model     string
	Workspace string // scratch | repo
	Repo      string // as the eval names it (the host resolves it)
	Base      string
	Task      string
	Name      string
	Run       string
	Eval      string
	Owner     string
}

// SessionInfo is what judging needs to know of a session.
type SessionInfo struct {
	ID         string
	State      string
	Activity   string // the last output line or the error
	Workspace  string // scratch | repo | folder
	Worktree   string // where the agent worked
	BaseCommit string
	Checks     string // the checks output
	SetupHash  string
	Agent      string
	Model      string
	RunStarted time.Time // the first attempt's start
	RunEnded   time.Time // the latest attempt's end
	// AttemptStarted is the latest attempt's start (zero = not known:
	// judging measures a resumed attempt itself).
	AttemptStarted time.Time
}

// Usage is tokens, money and energy.
type Usage struct {
	Tokens   int     `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	EnergyWh float64 `json:"energy_wh"`
	// Prompt and Completion split Tokens for a judge or feedback call
	// (what Job.Charge hands a flow step to record); not stored in
	// reports, and zero where only the total is known (session usage).
	Prompt     int `json:"-"`
	Completion int `json:"-"`
}

// callUsage is the usage of one model call.
func callUsage(r ModelReply) Usage {
	return Usage{Tokens: r.PromptTokens + r.CompletionTokens, CostUSD: r.CostUSD, EnergyWh: r.EnergyWh,
		Prompt: r.PromptTokens, Completion: r.CompletionTokens}
}

func (u *Usage) add(o Usage) {
	u.Tokens += o.Tokens
	u.Prompt += o.Prompt
	u.Completion += o.Completion
	u.CostUSD += o.CostUSD
	u.EnergyWh += o.EnergyWh
}

// Sessions starts and follows the agent sessions of a run.
//
// Wait blocks until the session reaches a final state (passed, failed,
// stopped, error, accepted, discarded or removed) or ctx ends. Transcript
// is the masked plain-text transcript, Diff the diff against the base of a
// repo session. Mask hides secrets the session knows of in text. Usage is
// what the session's model calls cost so far (tokens and money through the
// gateway, energy measured at the wall).
type Sessions interface {
	Start(ctx context.Context, req StartRequest) (id string, err error)
	Wait(ctx context.Context, id string) (SessionInfo, error)
	Stop(id string) error
	Info(id string) (SessionInfo, bool)
	Transcript(id string) ([]byte, error)
	Diff(id string) (string, error)
	Mask(text string) string
	Usage(id string) Usage
}

// Resumer is optional on Sessions (E2): Resume starts a new attempt of an
// ended session in its workspace and conversation, with text (the judge's
// feedback) as the instruction, and records it in the session feed as a
// follow-up prefixed FeedbackPrefix. Without it every gate is judged once.
type Resumer interface {
	Resume(ctx context.Context, id, text string) error
}

// BaseResolver is optional on Sessions: ResolveBase answers the full
// commit a repo eval's sessions start from (base "" = the repo's main
// head), so every setup of a run starts from the same tree. owner is the
// run's owner: a repo not shared with a confined owner is refused before
// it is looked at, so a run cannot probe repos the owner may not use.
type BaseResolver interface {
	ResolveBase(ctx context.Context, owner, repo, base string) (string, error)
}

// Catalog answers validation questions. Model: known (configured, listed
// by an engine, or a cloud model) and installed now. Agent: a profile of
// that name exists, and its binary is found. Persona: evals.personas has
// that name.
type Catalog interface {
	Model(id string) (known, installed bool)
	Agent(name string) (known, installed bool)
	Persona(name string) bool
}

// Auditor is the audit log (*audit.Logger satisfies it). Events are
// "eval.create", "eval.update", "eval.delete", "eval.run.start",
// "eval.run.cancel" and "eval.run.rejudge".
type Auditor interface {
	Event(actor, event, detail string, targets ...string) error
}

// Event is one run change, published as EventType. Session is set when
// the change concerns one session of the run.
type Event struct {
	Run     string `json:"run"`
	Eval    string `json:"eval"`
	State   string `json:"state"`
	Session string `json:"session,omitempty"`
	Owner   string `json:"owner,omitempty"` // the event stream sends it to the owner and admins only
}

// Publisher puts events on the event stream without blocking for long.
type Publisher interface {
	Publish(ev Event)
}

// Settings are the evals.* config keys the engine reads at each run (and
// engines.default_model for the judge fallback).
type Settings struct {
	Enabled      bool
	JudgeModel   string
	DefaultModel string
	MaxParallel  int
	EvidenceKB   int
	Personas     map[string]string
}

// withDefaults fills the zero (or out of range) keys.
func (s Settings) withDefaults() Settings {
	if s.MaxParallel < 1 {
		s.MaxParallel = DefaultMaxParallel
	}
	if s.EvidenceKB < 1 {
		s.EvidenceKB = DefaultEvidenceKB
	}
	return s
}

// Defaults of the evals.* keys.
const (
	DefaultMaxParallel = 1
	DefaultEvidenceKB  = 64
	// KeepRuns is how many finished runs of one eval are kept.
	KeepRuns = 50
	// JudgeParallel is how many judge calls of one session run at once.
	JudgeParallel = 2
	// JudgeMaxTokens bounds a judge answer.
	JudgeMaxTokens = 600
)
