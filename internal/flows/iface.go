package flows

import (
	"context"
	"time"
)

// ClientName is the usage client a ModelCaller should record flow
// requests under; ModelRequest.Run is the session id to record.
const ClientName = "cerberus-flow"

// EventType is the event-stream type of a run Event.
const EventType = "flowrun"

// Message is one chat message ("system", "user" or "assistant").
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ModelRequest is one chat completion for a prompt, plan or fan-out step.
type ModelRequest struct {
	Model    string    // a model id, a cloud model, or "default"
	Messages []Message // system first when there is one
	JSON     bool      // the step wants JSON back (a hint; the runner parses)
	Client   string    // always ClientName
	Run      string    // run id: record usage with it as the session
	Flow     string    // flow id
	Step     string    // step id
	Owner    string    // the run's owner (Phase 4a): charged for the call
}

// ModelReply is a completion with its usage. CostUSD is the money cost
// at the existing prices (0 for local models); EnergyWh the energy the
// call used when known.
type ModelReply struct {
	Text             string
	Model            string // the model that answered (else the request's)
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	EnergyWh         float64
}

// ModelCaller runs chat completions through the gateway (local or cloud).
// It must honour ctx cancellation.
type ModelCaller interface {
	Complete(ctx context.Context, req ModelRequest) (ModelReply, error)
}

// SessionRequest starts one agent session for a session step (or a
// fan-out item run as a session). The session has no parent, carries
// Tags (always "flow:<flow id>") and is never auto-accepted.
type SessionRequest struct {
	Agent     string // agent profile
	Model     string // "" = as the profile says (fan-out routes may set it)
	Workspace string // scratch | repo | folder
	Repo      string
	Folder    string
	Task      string // the rendered task
	Name      string // a display name ("<flow> · <step>")
	Tags      []string
	Run       string
	Flow      string
	Step      string
	Owner     string // the run's owner (Phase 4a): the session acts for them, confined unless admin
}

// SessionResult is a session's final state ("passed", "failed",
// "stopped", "error", ...) and its last activity line.
type SessionResult struct {
	State   string
	Summary string
}

// SessionRunner starts agent sessions and waits for them. Wait blocks
// until the session reaches a final state or ctx ends (then it returns
// ctx's error); it must not poll tightly. Stop stops a session the runner
// started (on cancel and on limits).
type SessionRunner interface {
	Start(ctx context.Context, req SessionRequest) (id string, err error)
	Wait(ctx context.Context, id string) (SessionResult, error)
	Stop(id string) error
}

// SessionMeter is optional on a SessionRunner. Meter calls fn with the
// usage of every model call recorded under session id from now until stop
// is called; after stop returns, fn is not called again. The engine
// charges that usage to the run, so a run's max_tokens and max_cost_usd
// cover its session steps too (and a session that passes a limit is
// stopped). Usage an agent spends outside the daemon (an agent CLI that
// calls its own vendor directly) is not seen, so it is not counted. fn
// may be called from any goroutine; it does not block for long and never
// calls back into the runner.
type SessionMeter interface {
	Meter(id string, fn func(ModelReply)) (stop func())
}

// Notice is a notification or an inbox entry. Kind is "flow" (output
// steps) or "flow-approve" (approval requests); Link an in-app route
// ("#/flows/runs/<run>").
type Notice struct {
	Kind  string
	Level string // "info" | "warn"
	Title string
	Body  string
	Link  string
	Run   string
	Flow  string
	Step  string
	Owner string // the run's owner (Phase 4a): non-admins get inbox items, never host notifications
}

// Notifier delivers notifications (Notify: desktop / push) and inbox
// entries (Post, which answers the inbox item id).
type Notifier interface {
	Notify(n Notice) error
	Post(n Notice) (id string, err error)
}

// Resolver is optional on a Notifier: Resolve(id) is called with an
// approval's inbox item id once the approval no longer waits (approved,
// rejected or the run canceled), so the inbox can mark it read. It is
// called without the engine's lock held and must not call back into the
// engine.
type Resolver interface {
	Resolve(id string)
}

// Auditor is the audit log; *audit.Logger satisfies it. Events are
// "flow.create", "flow.update", "flow.delete", "flow.run.start",
// "flow.run.approve", "flow.run.reject", "flow.run.cancel" and
// "flow.run.skip"; targets are the flow id and, for runs, the run id.
type Auditor interface {
	Event(actor, event, detail string, targets ...string) error
}

// Guard is the session guard: CanStart answers false and a reason when
// new sessions are refused now (scheduled runs are then skipped).
type Guard interface {
	CanStart() (ok bool, reason string)
}

// Catalog answers validation questions: whether a model id is known
// (configured, listed by an engine or a cloud model) and installed now,
// and whether an agent profile exists. "default" is never asked.
type Catalog interface {
	Model(id string) (known, installed bool)
	Agent(name string) bool
}

// Event is one run or step change, published as EventType.
type Event struct {
	Run   string `json:"run"`
	Flow  string `json:"flow"`
	State string `json:"state"`
	Step  string `json:"step"`
	Owner string `json:"owner,omitempty"` // the run's owner: the event stream sends it to them and admins only
}

// Publisher puts events on the event stream. Publish must not block for
// long (drop or buffer).
type Publisher interface {
	Publish(ev Event)
}

// Clock is the time source; tests use a fake. NewTimer works like
// time.NewTimer.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a stoppable one-shot timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now is time.Now.
func (SystemClock) Now() time.Time { return time.Now() }

// NewTimer is time.NewTimer.
func (SystemClock) NewTimer(d time.Duration) Timer { return sysTimer{time.NewTimer(d)} }

type sysTimer struct{ t *time.Timer }

func (s sysTimer) C() <-chan time.Time { return s.t.C }
func (s sysTimer) Stop() bool          { return s.t.Stop() }
