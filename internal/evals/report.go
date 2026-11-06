package evals

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Criterion result states.
const (
	StatePass    = "pass"
	StateFail    = "fail"
	StateSkipped = "skipped"
	StateError   = "error"
)

// Result is one criterion's judgement in one session: its gate and the
// iteration it was judged in (1-based; E2 judges a gate again after
// feedback).
type Result struct {
	Criterion string   `json:"criterion"`
	Gate      string   `json:"gate"`
	Iteration int      `json:"iteration"`
	State     string   `json:"state"`
	Rationale string   `json:"rationale"`
	Evidence  []string `json:"evidence"`
	Model     string   `json:"model,omitempty"`
	Tokens    int      `json:"tokens,omitempty"`
}

// GateResult is how one gate went: pass, fail or skipped, and how many
// iterations it took.
type GateResult struct {
	Gate       string `json:"gate"`
	Name       string `json:"name,omitempty"`
	State      string `json:"state"`
	Iterations int    `json:"iterations"`
}

// Report is one session's judgement (runs/<run>/report-<session>.json).
// Results hold the latest iteration of every criterion; History the
// earlier iterations (E2), oldest first; Feedback what was sent to the
// session between them.
type Report struct {
	Run       string `json:"run"`
	Eval      string `json:"eval"`
	Session   string `json:"session"`
	Setup     string `json:"setup"`  // the setup's label
	Repeat    int    `json:"repeat"` // 1-based
	SetupHash string `json:"setup_hash"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`

	SessionState string `json:"session_state"`
	State        string `json:"state"` // judged | error
	Error        string `json:"error,omitempty"`

	Results  []Result     `json:"results"`
	History  []Result     `json:"history,omitempty"`
	Gates    []GateResult `json:"gates"`
	Feedback []Feedback   `json:"feedback,omitempty"` // E2: the feedback sent between iterations, in order

	GatesPassed      int    `json:"gates_passed"`
	GatesTotal       int    `json:"gates_total"`
	FirstFailingGate string `json:"first_failing_gate"`
	CriteriaPassed   int    `json:"criteria_passed"`
	CriteriaTotal    int    `json:"criteria_total"`

	SessionUsage Usage `json:"session_usage"`
	JudgeUsage   Usage `json:"judge_usage"`
	WallMs       int64 `json:"wall_ms"`

	JudgeModel  string    `json:"judge_model"`
	JudgePrompt string    `json:"judge_prompt"` // JudgePromptHash at judging time
	Judged      time.Time `json:"judged"`

	// Trajectory is where the session's trajectory is exported (E4,
	// GET /api/sessions/{id}/trajectory); filled when reports are read.
	Trajectory string `json:"trajectory,omitempty"`
}

// TrajectoryPath is the API path of a session's ATIF trajectory (E4).
func TrajectoryPath(session string) string {
	return "/api/sessions/" + url.PathEscape(session) + "/trajectory?format=atif"
}

// Report states.
const (
	ReportJudged = "judged"
	ReportError  = "error"
)

// tally fills the gate and criterion counts from Results and Gates.
func (r *Report) tally() {
	r.GatesTotal, r.GatesPassed, r.FirstFailingGate = len(r.Gates), 0, ""
	for _, g := range r.Gates {
		if g.State == StatePass {
			r.GatesPassed++
		} else if r.FirstFailingGate == "" {
			r.FirstFailingGate = g.Gate
		}
	}
	r.CriteriaTotal, r.CriteriaPassed = len(r.Results), 0
	for _, x := range r.Results {
		if x.State == StatePass {
			r.CriteriaPassed++
		}
	}
}

// iterations is the sum of the iterations the report's passed gates took.
func (r Report) iterations() int {
	n := 0
	for _, g := range r.Gates {
		if g.State == StatePass {
			n += max(g.Iterations, 1)
		}
	}
	return n
}

// Rank is one setup's line of a run summary. With repeats, counts are
// summed over the setup's reports and WallMs is their mean.
type Rank struct {
	Rank           int      `json:"rank"`
	Setup          string   `json:"setup"`
	Reports        int      `json:"reports"`
	GatesPassed    int      `json:"gates_passed"`
	GatesTotal     int      `json:"gates_total"`
	CriteriaPassed int      `json:"criteria_passed"`
	CriteriaTotal  int      `json:"criteria_total"`
	Iterations     int      `json:"iterations"`
	WallMs         int64    `json:"wall_ms"`
	Session        Usage    `json:"session_usage"`
	Judge          Usage    `json:"judge_usage"`
	SetupHashes    []string `json:"setup_hashes"`
}

// Summarize ranks a run's setups: by gates passed, then criteria passed,
// then (E2) fewer iterations, then shorter wall time. Setups without a
// report yet are listed last, in definition order.
func Summarize(run Run, reports []Report) []Rank {
	by := map[string]*Rank{}
	var order []string
	for _, s := range run.Snapshot.Setups {
		if _, ok := by[s.Label]; !ok {
			by[s.Label] = &Rank{Setup: s.Label, SetupHashes: []string{}}
			order = append(order, s.Label)
		}
	}
	walls := map[string]int64{}
	for _, r := range reports {
		k, ok := by[r.Setup]
		if !ok {
			k = &Rank{Setup: r.Setup, SetupHashes: []string{}}
			by[r.Setup] = k
			order = append(order, r.Setup)
		}
		k.Reports++
		k.GatesPassed += r.GatesPassed
		k.GatesTotal += r.GatesTotal
		k.CriteriaPassed += r.CriteriaPassed
		k.CriteriaTotal += r.CriteriaTotal
		k.Iterations += r.iterations()
		walls[r.Setup] += r.WallMs
		k.Session.add(r.SessionUsage)
		k.Judge.add(r.JudgeUsage)
		if r.SetupHash != "" && !contains(k.SetupHashes, r.SetupHash) {
			k.SetupHashes = append(k.SetupHashes, r.SetupHash)
		}
	}
	out := make([]Rank, 0, len(order))
	pos := map[string]int{}
	for i, l := range order {
		k := by[l]
		if k.Reports > 0 {
			k.WallMs = walls[l] / int64(k.Reports)
		}
		pos[l] = i
		out = append(out, *k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Reports > 0) != (b.Reports > 0) {
			return a.Reports > 0
		}
		if a.GatesPassed != b.GatesPassed {
			return a.GatesPassed > b.GatesPassed
		}
		if a.CriteriaPassed != b.CriteriaPassed {
			return a.CriteriaPassed > b.CriteriaPassed
		}
		if a.Iterations != b.Iterations {
			return a.Iterations < b.Iterations
		}
		if a.WallMs != b.WallMs {
			return a.WallMs < b.WallMs
		}
		return pos[a.Setup] < pos[b.Setup]
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// Flip is one criterion whose state differs between two runs of a setup.
type Flip struct {
	Criterion string `json:"criterion"`
	A         string `json:"a"`
	B         string `json:"b"`
}

// SetupCompare is one setup in a comparison of two runs. Setup hashes
// are the first report's of each run (the CLI diffs those setups).
type SetupCompare struct {
	Setup      string `json:"setup"`
	GatesA     string `json:"gates_a"` // "2/3", or "" when the run has no report of it
	GatesB     string `json:"gates_b"`
	Flips      []Flip `json:"flips"`
	SessionA   string `json:"session_a"`
	SessionB   string `json:"session_b"`
	SetupHashA string `json:"setup_hash_a"`
	SetupHashB string `json:"setup_hash_b"`
}

// Compare lines up two runs' reports by setup label (the first repeat of
// each): gates passed and the criteria whose state flipped.
func Compare(a, b []Report) []SetupCompare {
	first := func(rs []Report) (map[string]Report, []string) {
		m := map[string]Report{}
		var order []string
		for _, r := range rs {
			if old, ok := m[r.Setup]; ok && old.Repeat <= r.Repeat {
				continue
			}
			if _, ok := m[r.Setup]; !ok {
				order = append(order, r.Setup)
			}
			m[r.Setup] = r
		}
		return m, order
	}
	ma, oa := first(a)
	mb, ob := first(b)
	labels := oa
	for _, l := range ob {
		if !contains(labels, l) {
			labels = append(labels, l)
		}
	}
	gates := func(r Report, ok bool) string {
		if !ok {
			return ""
		}
		return strconv.Itoa(r.GatesPassed) + "/" + strconv.Itoa(r.GatesTotal)
	}
	var out []SetupCompare
	for _, l := range labels {
		ra, okA := ma[l]
		rb, okB := mb[l]
		sc := SetupCompare{Setup: l, GatesA: gates(ra, okA), GatesB: gates(rb, okB), Flips: []Flip{},
			SessionA: ra.Session, SessionB: rb.Session, SetupHashA: ra.SetupHash, SetupHashB: rb.SetupHash}
		if okA && okB {
			sa := map[string]string{}
			for _, x := range ra.Results {
				sa[x.Criterion] = x.State
			}
			seen := map[string]bool{}
			for _, x := range rb.Results {
				seen[x.Criterion] = true
				if sa[x.Criterion] != x.State {
					sc.Flips = append(sc.Flips, Flip{Criterion: x.Criterion, A: orDash(sa[x.Criterion]), B: x.State})
				}
			}
			for _, x := range ra.Results {
				if !seen[x.Criterion] {
					sc.Flips = append(sc.Flips, Flip{Criterion: x.Criterion, A: x.State, B: "–"})
				}
			}
		}
		out = append(out, sc)
	}
	return out
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "–"
	}
	return s
}
