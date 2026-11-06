// Package evals judges an agent's work against criteria (docs/SCOPE.md
// "Evals phase specification", E1). An eval is a saved definition: a task,
// a criteria graph split into ordered gates, and the agent setups to try.
// A run starts one agent session per setup and repeat, waits for each to
// end, then asks a judge model, gate by gate and criterion by criterion in
// topological order, whether the session's workspace meets each criterion.
// The judge only reads: evidence is built from the workspace (files,
// checks output, transcript tail, diff), masked and capped, and handed to
// the model as data.
//
// Gates carry an iteration budget (max_iterations). Every result records
// its gate and iteration, so the feedback loop of E2 (resume the session
// with the failed criteria, judge the gate again) adds iterations without
// changing the data. With max_iterations 1 a gate is judged once.
//
// Files (all under DATA/evals, 0700 folders and 0600 files, written
// atomically):
//
//	<id>.json                     an eval definition (optimistic versions)
//	runs/<run>/run.json           a run: the eval as it ran and its sessions
//	runs/<run>/report-<sess>.json one session's report
//
// The package knows nothing about the daemon: sessions, the judge model,
// the catalog, the audit log and the event stream come in through the
// interfaces in iface.go, so tests use fakes and never load a model.
package evals

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Workspaces an eval's sessions start in.
const (
	WorkspaceScratch = "scratch"
	WorkspaceRepo    = "repo"
)

// Evidence kinds a criterion may ask for.
const (
	EvidenceFiles      = "files"
	EvidenceChecks     = "checks"
	EvidenceTranscript = "transcript"
	EvidenceDiff       = "diff"
)

// EvidenceKinds are every evidence kind, in the order evidence is built.
var EvidenceKinds = []string{EvidenceFiles, EvidenceChecks, EvidenceTranscript, EvidenceDiff}

// ImplicitGate is the one gate of an eval that names none.
const ImplicitGate = "main"

// Bounds of a definition.
const (
	MaxCriteria      = 64
	MaxGates         = 10
	MaxSetups        = 16
	MaxRepeat        = 10
	MaxIterations    = 5
	MaxPromptRunes   = 2000
	MaxTaskBytes     = 32 << 10
	MaxNameRunes     = 120
	MaxLabelRunes    = 60
	MaxPersonaRunes  = 64
	maxEvalFileBytes = 1 << 20
)

// Gate is one stage of an eval: its criteria are judged together, and a
// gate that fails stops the later ones. MaxIterations is how many attempts
// the session gets at this gate (E2 resumes it with feedback in between).
type Gate struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	MaxIterations int    `json:"max_iterations"`
}

// Criterion is one thing the judge decides, pass or fail. DependsOn are
// criteria that must pass first (same gate or an earlier one); Evidence
// the kinds of evidence the judge sees (empty: all four).
type Criterion struct {
	ID        string   `json:"id"`
	Prompt    string   `json:"prompt"`
	Gate      string   `json:"gate,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
}

// Setup is one agent configuration to try: an agent profile and, for
// agents that take one, a model ("" = as the profile says).
type Setup struct {
	Label string `json:"label"`
	Agent string `json:"agent"`
	Model string `json:"model,omitempty"`
}

// Eval is a saved definition (DATA/evals/<id>.json).
type Eval struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	Updated   time.Time `json:"updated"`
	Owner     string    `json:"owner,omitempty"`
	OwnerName string    `json:"owner_name,omitempty"` // filled by the API on read, never stored

	Task      string `json:"task"`
	Workspace string `json:"workspace"`      // scratch | repo
	Repo      string `json:"repo,omitempty"` // repo: a configured name, a folder under repo_roots, or an absolute path
	Base      string `json:"base,omitempty"` // repo: the commit every setup starts from ("" = main when the run starts)

	Gates    []Gate      `json:"gates"`
	Criteria []Criterion `json:"criteria"`
	Setups   []Setup     `json:"setups"`

	Repeat     int    `json:"repeat"`
	JudgeModel string `json:"judge_model"` // "" = evals.judge_model, then engines.default_model
	Persona    string `json:"persona"`     // E2: the voice of judge feedback ("" = the built-in reviewer)
}

func (e Eval) clone() Eval {
	out := e
	out.Gates = append([]Gate(nil), e.Gates...)
	out.Setups = append([]Setup(nil), e.Setups...)
	out.Criteria = make([]Criterion, len(e.Criteria))
	for i, c := range e.Criteria {
		c.DependsOn = append([]string(nil), c.DependsOn...)
		c.Evidence = append([]string(nil), c.Evidence...)
		out.Criteria[i] = c
	}
	if out.Gates == nil {
		out.Gates = []Gate{}
	}
	if out.Setups == nil {
		out.Setups = []Setup{}
	}
	return out
}

// Clone is a deep copy.
func (e Eval) Clone() Eval { return e.clone() }

var (
	idRe        = regexp.MustCompile(`^ev-[a-z0-9][a-z0-9-]{0,62}$`)
	runIDRe     = regexp.MustCompile(`^er-[0-9]{8}-[0-9]{6}-[a-z0-9]{6}$`)
	criterionRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	commitRe    = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	personaRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// ValidID reports whether id has the form of an eval id (ev-<slug>).
func ValidID(id string) bool { return idRe.MatchString(id) }

// ValidRunID reports whether id has the form of a run id.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// ValidCriterionID reports whether id may name a criterion or a gate.
func ValidCriterionID(id string) bool { return criterionRe.MatchString(id) }

// ValidCommit reports whether s looks like a commit id (hex, 7 to 64).
func ValidCommit(s string) bool { return commitRe.MatchString(s) }

// Slug is an eval id made from a name: ev-<lowercase words joined by ->.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "eval"
	}
	return "ev-" + s
}

// EffectiveGates are the gates an eval runs: its own, or the implicit
// "main" gate (max_iterations 1) when it names none. Iterations below 1
// count as 1.
func EffectiveGates(e Eval) []Gate {
	if len(e.Gates) == 0 {
		return []Gate{{ID: ImplicitGate, Name: "Main", MaxIterations: 1}}
	}
	out := make([]Gate, len(e.Gates))
	for i, g := range e.Gates {
		if g.MaxIterations < 1 {
			g.MaxIterations = 1
		}
		out[i] = g
	}
	return out
}

// GateOf is the gate a criterion belongs to: its own, else the first
// gate.
func GateOf(e Eval, c Criterion) string {
	if c.Gate != "" {
		return c.Gate
	}
	return EffectiveGates(e)[0].ID
}

// EvidenceOf is the evidence kinds a criterion asks for (all when empty),
// in the order they are built.
func EvidenceOf(c Criterion) []string {
	if len(c.Evidence) == 0 {
		return append([]string(nil), EvidenceKinds...)
	}
	var out []string
	for _, k := range EvidenceKinds {
		for _, want := range c.Evidence {
			if want == k {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// normalize trims text and fills the stored defaults.
func normalize(e *Eval) {
	e.Name = strings.TrimSpace(e.Name)
	e.Task = strings.TrimSpace(e.Task)
	e.Workspace = strings.ToLower(strings.TrimSpace(e.Workspace))
	if e.Workspace == "" {
		e.Workspace = WorkspaceScratch
	}
	e.Repo = strings.TrimSpace(e.Repo)
	e.Base = strings.ToLower(strings.TrimSpace(e.Base))
	if e.Workspace != WorkspaceRepo {
		e.Repo, e.Base = "", ""
	}
	e.JudgeModel = strings.TrimSpace(e.JudgeModel)
	e.Persona = strings.TrimSpace(e.Persona)
	if e.Repeat == 0 {
		e.Repeat = 1
	}
	for i := range e.Gates {
		e.Gates[i].ID = strings.TrimSpace(e.Gates[i].ID)
		e.Gates[i].Name = strings.TrimSpace(e.Gates[i].Name)
		if e.Gates[i].MaxIterations == 0 {
			e.Gates[i].MaxIterations = 1
		}
	}
	for i := range e.Criteria {
		c := &e.Criteria[i]
		c.ID = strings.TrimSpace(c.ID)
		c.Prompt = strings.TrimSpace(c.Prompt)
		c.Gate = strings.TrimSpace(c.Gate)
		for j := range c.DependsOn {
			c.DependsOn[j] = strings.TrimSpace(c.DependsOn[j])
		}
		for j := range c.Evidence {
			c.Evidence[j] = strings.ToLower(strings.TrimSpace(c.Evidence[j]))
		}
	}
	for i := range e.Setups {
		s := &e.Setups[i]
		s.Label = strings.TrimSpace(s.Label)
		s.Agent = strings.TrimSpace(s.Agent)
		s.Model = strings.TrimSpace(s.Model)
		if s.Label == "" {
			s.Label = s.Agent
			if s.Model != "" {
				s.Label += " " + s.Model
			}
		}
	}
	if e.Gates == nil {
		e.Gates = []Gate{}
	}
	if e.Criteria == nil {
		e.Criteria = []Criterion{}
	}
	if e.Setups == nil {
		e.Setups = []Setup{}
	}
}

// Normalize is normalize for callers outside the package (the API's
// validate route shows a draft as it would be saved).
func Normalize(e Eval) Eval {
	e = e.clone()
	normalize(&e)
	return e
}
