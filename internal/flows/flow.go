package flows

import (
	"regexp"
	"strings"
	"time"

	"carino.systems/cerberus/internal/evals"
)

// Trigger kinds.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
)

// Step kinds.
const (
	KindPrompt  = "prompt"
	KindPlan    = "plan"
	KindFanOut  = "fan_out"
	KindSession = "session"
	KindIf      = "if"
	KindApprove = "approve"
	KindOutput  = "output"
	KindJudge   = "judge"
)

// Kinds lists every step kind in the order the designer offers them.
var Kinds = []string{KindPrompt, KindPlan, KindFanOut, KindSession, KindJudge, KindIf, KindApprove, KindOutput}

// Test operators of an `if` step.
const (
	TestContains = "contains"
	TestMatches  = "matches"
	TestEquals   = "equals"
	TestJSON     = "json"
	TestState    = "state"
)

// Output targets of an `output` step.
const (
	ToNotify = "notify"
	ToInbox  = "inbox"
	ToFile   = "file"
)

// Workspaces of a session step (and of a fan-out run as sessions).
// WorkspaceStep is for a session step only: it works where the session(s)
// of an earlier step (field from) worked.
const (
	WorkspaceScratch = "scratch"
	WorkspaceRepo    = "repo"
	WorkspaceFolder  = "folder"
	WorkspaceStep    = "step"
)

// End is the jump target of an `if` branch that stops the run (as passed).
const End = "end"

// Limit defaults (SCOPE: limits).
const (
	DefaultMaxSteps    = 50
	DefaultMaxTokens   = 200000
	DefaultMaxCostUSD  = 1.00
	DefaultMaxParallel = 2
	DefaultTimeoutMin  = 60
	DefaultMaxItems    = 8

	maxMaxItems    = 50
	maxMaxParallel = 16
	maxMaxSteps    = 500
	maxTimeoutMin  = 7 * 24 * 60
	maxNameLen     = 120
	maxDescLen     = 2000
	maxHintLen     = 2000
	maxTemplateLen = 64 << 10
)

// Flow is a saved recipe: DATA/flows/<id>.json. Every field is always
// written; Version and Updated are set by the store.
type Flow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	Trigger     Trigger   `json:"trigger"`
	InputHint   string    `json:"input_hint"`
	Limits      Limits    `json:"limits"`
	Steps       []Step    `json:"steps"`
	Version     int       `json:"version"`
	Updated     time.Time `json:"updated"`
	// Owner is the user id the flow belongs to (Phase 4a; "" = from before
	// users). Its runs, sessions and gateway calls act for that user.
	// OwnerName is filled by the API on read.
	Owner     string `json:"owner,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
}

// Trigger says when a flow runs by itself. Kind "manual" runs only when
// asked (button, CLI, API); "schedule" also runs on Schedule (see
// ParseSchedule), with the flow's InputHint as input.
type Trigger struct {
	Kind     string `json:"kind"`
	Schedule string `json:"schedule,omitempty"`
}

// Limits bound one run. A zero field means its default (Effective).
type Limits struct {
	MaxSteps    int     `json:"max_steps,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	MaxCostUSD  float64 `json:"max_cost_usd,omitempty"`
	MaxParallel int     `json:"max_parallel,omitempty"`
	TimeoutMin  int     `json:"timeout_min,omitempty"`
}

// Effective is l with every zero (or negative) field set to its default.
func (l Limits) Effective() Limits {
	if l.MaxSteps <= 0 {
		l.MaxSteps = DefaultMaxSteps
	}
	if l.MaxTokens <= 0 {
		l.MaxTokens = DefaultMaxTokens
	}
	if l.MaxCostUSD <= 0 {
		l.MaxCostUSD = DefaultMaxCostUSD
	}
	if l.MaxParallel <= 0 {
		l.MaxParallel = DefaultMaxParallel
	}
	if l.TimeoutMin <= 0 {
		l.TimeoutMin = DefaultTimeoutMin
	}
	return l
}

// Step is one node. It is a flat record: each kind reads only its own
// fields (listed per kind below) and the others stay empty.
//
//	prompt   model, system?, template, json?, temperature?, max_tokens?, thinking?
//	plan     model, template, max_items, temperature?, max_tokens?, thinking?
//	fan_out  from, routes[], default_model, as ("prompt"|"session"), template,
//	         agent?, workspace?, repo?, folder?, max_parallel, min_ok?,
//	         temperature?, max_tokens?, thinking? (prompt items only),
//	         max_attempts?, escalate_to?, max_minutes? (session items only)
//	session  agent, workspace (scratch|repo|folder|step), repo?, folder?,
//	         from? (workspace step: an earlier session step, or a fan-out
//	         run as sessions), task, max_attempts?, escalate_to?, max_minutes?
//	judge    on (an earlier session, prompt or fan-out-as-prompt step id),
//	         criteria[], gates[]?, persona?,
//	         model? ("" = evals.judge_model, then engines.default_model)
//	if       test (contains|matches|equals|json|state), on (an earlier step id),
//	         value, path (json only), then, else (a later step id, "end" or "")
//	approve  message
//	output   to (notify|inbox|file), template, file? (a file name template)
//
// Every step but if and approve may also set on_error ("fail" (default),
// "continue" or a later step id); prompt, plan and fan-out steps may set
// retry {max 0-3, backoff_s}.
type Step struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`

	Model    string `json:"model,omitempty"`
	System   string `json:"system,omitempty"`
	Template string `json:"template,omitempty"`
	JSON     bool   `json:"json,omitempty"`
	MaxItems int    `json:"max_items,omitempty"`

	// Model settings of a prompt, plan or fan-out (prompt items) call.
	// Unset means the model's defaults. Temperature is 0 to 2; MaxTokens
	// the answer's token budget (the gateway clips it to the model's cap);
	// Thinking is "on", "off" or "" (as the model does), sent to local
	// models only.
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Thinking    string   `json:"thinking,omitempty"`

	From         string  `json:"from,omitempty"`
	Routes       []Route `json:"routes,omitempty"`
	DefaultModel string  `json:"default_model,omitempty"`
	As           string  `json:"as,omitempty"`
	MaxParallel  int     `json:"max_parallel,omitempty"`

	Agent     string `json:"agent,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Folder    string `json:"folder,omitempty"`
	Task      string `json:"task,omitempty"`

	Test  string `json:"test,omitempty"`
	On    string `json:"on,omitempty"`
	Value string `json:"value,omitempty"`
	Path  string `json:"path,omitempty"`
	Then  string `json:"then,omitempty"`
	Else  string `json:"else,omitempty"`

	Message string `json:"message,omitempty"`

	To   string `json:"to,omitempty"`
	File string `json:"file,omitempty"`

	// judge (E2): the criteria graph and gates of an eval, judged against
	// the workspace of the session step On, with feedback in Persona's voice.
	Criteria []evals.Criterion `json:"criteria,omitempty"`
	Gates    []evals.Gate      `json:"gates,omitempty"`
	Persona  string            `json:"persona,omitempty"`

	// Session settings of a session step (and of a fan-out run as
	// sessions), passed to the session manager: MaxAttempts runs of the
	// agent before it hands over to the profile EscalateTo, and the time
	// limit of each run. They act when the workspace has checks (a failed
	// check run is what starts the next attempt). 0 and "" = the defaults.
	MaxAttempts int    `json:"max_attempts,omitempty"`
	EscalateTo  string `json:"escalate_to,omitempty"`
	MaxMinutes  int    `json:"max_minutes,omitempty"`

	// Retry runs a prompt or plan step (or one fan-out item) again when it
	// fails for any reason but a limit, a cancel or the timeout.
	Retry *Retry `json:"retry,omitempty"`
	// OnError is what a failed step does: OnErrorFail (the default) ends
	// the run, OnErrorContinue goes on to the next step, and a later step
	// id jumps there. The step's output is then {"error": "..."}. A limit,
	// a cancel or the timeout always ends the run.
	OnError string `json:"on_error,omitempty"`
	// MinOK (fan-out) is how many items must pass. 0 = all of them. With
	// it set, a failed item does not stop the others, and its result is
	// null.
	MinOK int `json:"min_ok,omitempty"`
}

// Retry is a step's retry policy: Max more tries (0 to 3), BackoffS
// seconds apart (0 to 600).
type Retry struct {
	Max      int `json:"max"`
	BackoffS int `json:"backoff_s,omitempty"`
}

// OnError values (besides a later step id).
const (
	OnErrorFail     = "fail"
	OnErrorContinue = "continue"
)

// Bounds of the step error settings.
const (
	maxRetry       = 3
	maxBackoffS    = 600
	maxAttemptsCap = 5
)

// retries is the number of extra tries a step (or item) gets.
func (s Step) retries() int {
	if s.Retry == nil || s.Retry.Max < 0 {
		return 0
	}
	return min(s.Retry.Max, maxRetry)
}

// backoff is the pause before a retry.
func (s Step) backoff() time.Duration {
	if s.Retry == nil || s.Retry.BackoffS <= 0 {
		return 0
	}
	return time.Duration(min(s.Retry.BackoffS, maxBackoffS)) * time.Second
}

// handlesErrors reports whether a failure of the step lets the run go on.
func (s Step) handlesErrors() bool { return s.OnError != "" && s.OnError != OnErrorFail }

// Route sends fan-out items of one kind to one model.
type Route struct {
	Kind  string `json:"kind"`
	Model string `json:"model"`
}

// Label is the step's name, else its id.
func (s Step) Label() string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return s.ID
}

// fanAs is the fan-out mode with its default.
func (s Step) fanAs() string {
	if s.As == "" {
		return KindPrompt
	}
	return s.As
}

// workspace is the session workspace with its default.
func (s Step) workspace() string {
	if s.Workspace == "" {
		return WorkspaceScratch
	}
	return s.Workspace
}

// Thinking settings of a step.
const (
	ThinkingOn  = "on"
	ThinkingOff = "off"
)

// Bounds of a step's model settings.
const (
	maxTemperature   = 2.0
	maxStepMaxTokens = 262144
)

// shape is the model settings of the step's calls, with the answer's
// JSON schema ("" = none).
func (s Step) shape(wantJSON bool, schema string) callShape {
	sh := callShape{json: wantJSON, maxTokens: s.MaxTokens, thinking: s.Thinking}
	if s.Temperature != nil {
		t := *s.Temperature
		sh.temperature = &t
	}
	if schema != "" {
		sh.schema = []byte(schema)
	}
	return sh
}

// maxItems is the plan's item cap with its default.
func (s Step) maxItems() int {
	if s.MaxItems <= 0 {
		return DefaultMaxItems
	}
	if s.MaxItems > maxMaxItems {
		return maxMaxItems
	}
	return s.MaxItems
}

// clone is a deep copy of f (the slices are not shared).
func (f Flow) clone() Flow {
	f.Steps = append([]Step(nil), f.Steps...)
	for i := range f.Steps {
		st := &f.Steps[i]
		st.Routes = append([]Route(nil), st.Routes...)
		if st.Retry != nil {
			rt := *st.Retry
			st.Retry = &rt
		}
		if st.Temperature != nil {
			t := *st.Temperature
			st.Temperature = &t
		}
		if st.Criteria != nil {
			cs := make([]evals.Criterion, len(st.Criteria))
			for j, c := range st.Criteria {
				c.DependsOn = append([]string(nil), c.DependsOn...)
				c.Evidence = append([]string(nil), c.Evidence...)
				cs[j] = c
			}
			st.Criteria = cs
		}
		if st.Gates != nil {
			st.Gates = append([]evals.Gate(nil), st.Gates...)
		}
	}
	return f
}

// stepIndex is the index of step id in f, or -1.
func (f Flow) stepIndex(id string) int {
	for i, s := range f.Steps {
		if s.ID == id {
			return i
		}
	}
	return -1
}

var (
	idRe     = regexp.MustCompile(`^fl-[a-z0-9][a-z0-9-]{0,47}$`)
	stepIDRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	runIDRe  = regexp.MustCompile(`^fr-[0-9]{8}-[0-9]{6}-[a-z0-9]{4,8}$`)
)

// ValidID reports whether id is a well-formed flow id (fl-<slug>).
func ValidID(id string) bool { return idRe.MatchString(id) }

// ValidRunID reports whether id is a well-formed run id.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// ValidStepID reports whether id is a well-formed step id: a short slug
// (a lower-case letter, then letters, digits, "-" or "_"; at most 32).
// "end" and the on_error keywords "continue" and "fail" are reserved: a
// jump to a step of that name would read as the keyword.
func ValidStepID(id string) bool {
	return stepIDRe.MatchString(id) && id != End && id != OnErrorContinue && id != OnErrorFail
}

// Slug is name as a flow id: "fl-" and the lower-case letters and digits
// of name with every other run of characters as one "-" (at most 40
// characters of slug; "flow" when nothing is left).
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			if b.Len() >= 40 {
				break
			}
			continue
		}
		dash = true
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		s = "flow"
	}
	return "fl-" + s
}
