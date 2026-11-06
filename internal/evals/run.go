package evals

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// Run states.
const (
	RunRunning  = "running"  // sessions going or being judged
	RunJudging  = "judging"  // a rejudge of ended sessions
	RunDone     = "done"     // every session judged (or failed to start)
	RunCanceled = "canceled" // stopped by a person
	RunFailed   = "failed"   // could not go on (the daemon stopped during it)
)

// Final reports whether a run state is an end state.
func Final(state string) bool {
	return state == RunDone || state == RunCanceled || state == RunFailed
}

// Slot states: one session of a run (a setup and a repeat).
const (
	SlotPending  = "pending"  // waiting for a free place (evals.max_parallel)
	SlotRunning  = "running"  // the session is going
	SlotJudging  = "judging"  // the session ended; the judge is at work
	SlotJudged   = "judged"   // its report is written
	SlotError    = "error"    // the session could not start, or judging failed
	SlotCanceled = "canceled" // the run was canceled first
)

// Slot is one session of a run. Live is the session's own state now
// (filled on read, never stored).
type Slot struct {
	Setup   string `json:"setup"`
	Repeat  int    `json:"repeat"`
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	Session string `json:"session"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Live    string `json:"live,omitempty"`
}

// Run is one run of an eval (runs/<run>/run.json): the eval as it ran
// (Snapshot), its sessions and the judge's usage.
type Run struct {
	ID          string    `json:"id"`
	Eval        string    `json:"eval"`
	EvalName    string    `json:"eval_name"`
	EvalVersion int       `json:"eval_version"`
	Owner       string    `json:"owner,omitempty"`
	OwnerName   string    `json:"owner_name,omitempty"`
	Actor       string    `json:"actor"`
	State       string    `json:"state"`
	Reason      string    `json:"reason,omitempty"`
	SetupFilter string    `json:"setup_filter,omitempty"`
	Base        string    `json:"base,omitempty"` // the commit a repo eval's sessions start from
	JudgeModel  string    `json:"judge_model"`
	JudgePrompt string    `json:"judge_prompt"`
	MaxParallel int       `json:"max_parallel"`
	Created     time.Time `json:"created"`
	Started     time.Time `json:"started"`
	Ended       time.Time `json:"ended"`
	Updated     time.Time `json:"updated"`
	Slots       []Slot    `json:"sessions"`
	JudgeUsage  Usage     `json:"judge_usage"`
	Snapshot    Eval      `json:"snapshot"`
}

func (r Run) clone() Run {
	out := r
	out.Slots = append([]Slot(nil), r.Slots...)
	out.Snapshot = r.Snapshot.clone()
	return out
}

// RunDetail is a run with its reports and the ranked summary.
type RunDetail struct {
	Run     Run      `json:"run"`
	Reports []Report `json:"reports"`
	Summary []Rank   `json:"summary"`
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// newRunID is er-<local yyyymmdd-hhmmss>-<6 base36>.
func newRunID(now time.Time) string {
	var b [6]byte
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(36))
		if err != nil {
			n = big.NewInt(int64(now.UnixNano()+int64(i)) % 36)
		}
		b[i] = base36[n.Int64()]
	}
	return "er-" + now.Local().Format("20060102-150405") + "-" + string(b[:])
}

func (e *Engine) runDir(id string) string { return filepath.Join(e.runsDir, id) }

func (e *Engine) saveRunLocked(r *Run) error {
	r.Updated = e.now().UTC()
	dir := e.runDir(r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return platform.WritePrivate(filepath.Join(dir, "run.json"), append(b, '\n'))
}

func readRun(p string) (Run, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Run{}, err
	}
	if len(b) > 4*maxEvalFileBytes {
		return Run{}, fmt.Errorf("%s: too big", p)
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return Run{}, fmt.Errorf("%s: %w", p, err)
	}
	normalize(&r.Snapshot)
	r.OwnerName = ""
	if r.Slots == nil {
		r.Slots = []Slot{}
	}
	return r, nil
}

func (e *Engine) reportPath(run, session string) string {
	return filepath.Join(e.runDir(run), "report-"+session+".json")
}

func (e *Engine) saveReport(rep Report) error {
	if !sessionIDRe.MatchString(rep.Session) {
		return fmt.Errorf("session id %q cannot name a report file", rep.Session)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return platform.WritePrivate(e.reportPath(rep.Run, rep.Session), append(b, '\n'))
}

// readReport reads the stored report of one session of a run.
func (e *Engine) readReport(run, session string) (Report, bool) {
	if !sessionIDRe.MatchString(session) {
		return Report{}, false
	}
	b, err := os.ReadFile(e.reportPath(run, session))
	if err != nil {
		return Report{}, false
	}
	var rep Report
	if json.Unmarshal(b, &rep) != nil || rep.Session != session {
		return Report{}, false
	}
	return rep, true
}

// carryOver keeps what a rejudge cannot know from the report it replaces
// (prev): a rejudge judges each gate once on the session as it ended, so
// the feedback loop's record (the feedback sent, the earlier iterations'
// results, how many iterations each gate took) and the wall time summed
// over the attempts come from prev. A gate judged now takes prev's
// iteration count (and its results that iteration number); a gate now
// skipped stays at 0.
func carryOver(prev, rep Report) Report {
	if rep.State == ReportError {
		return rep
	}
	rep.Feedback = prev.Feedback
	rep.History = prev.History
	if prev.WallMs > 0 {
		rep.WallMs = prev.WallMs
	}
	iters := map[string]int{}
	for _, g := range prev.Gates {
		if g.Iterations > 1 {
			iters[g.Gate] = g.Iterations
		}
	}
	for i, g := range rep.Gates {
		if n := iters[g.Gate]; n > 0 && g.State != StateSkipped {
			rep.Gates[i].Iterations = n
		}
	}
	for i, r := range rep.Results {
		if n := iters[r.Gate]; n > 0 && r.Iteration > 0 {
			rep.Results[i].Iteration = n
		}
	}
	return rep
}

// readReports reads every report of a run, in the order of its slots.
func (e *Engine) readReports(r Run) []Report {
	out := []Report{}
	seen := map[string]bool{}
	for _, s := range r.Slots {
		if s.Session == "" || seen[s.Session] || !sessionIDRe.MatchString(s.Session) {
			continue
		}
		seen[s.Session] = true
		b, err := os.ReadFile(e.reportPath(r.ID, s.Session))
		if err != nil {
			continue
		}
		var rep Report
		if json.Unmarshal(b, &rep) != nil || rep.Session != s.Session {
			continue
		}
		if rep.Results == nil {
			rep.Results = []Result{}
		}
		if rep.Gates == nil {
			rep.Gates = []GateResult{}
		}
		rep.Trajectory = TrajectoryPath(rep.Session)
		out = append(out, rep)
	}
	return out
}

// loadRuns reads every run.json under runs/. A run the daemon stopped in
// the middle of is recorded as failed; its ended sessions can be judged
// again with Rejudge.
func (e *Engine) loadRuns() error {
	if err := os.MkdirAll(e.runsDir, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(e.runsDir)
	if err != nil {
		return err
	}
	for _, d := range ents {
		if !d.IsDir() || !ValidRunID(d.Name()) {
			continue
		}
		r, err := readRun(filepath.Join(e.runsDir, d.Name(), "run.json"))
		if err != nil || r.ID != d.Name() {
			continue
		}
		if !Final(r.State) {
			r.State, r.Reason = RunFailed, "the daemon stopped during the run (rejudge judges the sessions that ended)"
			if r.Ended.IsZero() {
				r.Ended = e.now().UTC()
			}
			for i := range r.Slots {
				switch r.Slots[i].State {
				case SlotPending:
					r.Slots[i].State = SlotCanceled
				case SlotRunning, SlotJudging:
					r.Slots[i].State, r.Slots[i].Error = SlotError, "interrupted: the daemon stopped"
				}
			}
			_ = e.saveRunLocked(&r)
		}
		rr := r
		e.runs[r.ID] = &rr
	}
	return nil
}

// sortNewest orders runs newest first (created, then id).
func sortNewest(list []*Run) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Created.Equal(list[j].Created) {
			return list[i].Created.After(list[j].Created)
		}
		return list[i].ID > list[j].ID
	})
}

// pruneLocked keeps the newest KeepRuns runs of an eval; runs still going
// are never removed.
func (e *Engine) pruneLocked(eval string) {
	var list []*Run
	for _, r := range e.runs {
		if r.Eval == eval {
			list = append(list, r)
		}
	}
	if len(list) <= KeepRuns {
		return
	}
	sortNewest(list)
	for _, r := range list[KeepRuns:] {
		if !Final(r.State) || e.active[r.ID] != nil {
			continue
		}
		if !ValidRunID(r.ID) || strings.ContainsAny(r.ID, `/\`) {
			continue
		}
		if err := os.RemoveAll(e.runDir(r.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		delete(e.runs, r.ID)
	}
}
