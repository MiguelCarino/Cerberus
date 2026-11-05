package flows

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// Run states.
const (
	StateQueued   = "queued"
	StateRunning  = "running"
	StateWaiting  = "waiting"
	StatePassed   = "passed"
	StateFailed   = "failed"
	StateRejected = "rejected"
	StateLimited  = "limited"
	StateCanceled = "canceled"
)

// Step record states (graph nodes add "pending", "running" and
// "queued": a step or item whose model call the engine is holding).
const (
	StepQueued   = "queued"
	StepPending  = "pending"
	StepRunning  = "running"
	StepWaiting  = "waiting"
	StepPassed   = "passed"
	StepFailed   = "failed"
	StepSkipped  = "skipped"
	StepRejected = "rejected"
	StepLimited  = "limited"
	StepCanceled = "canceled"
)

// Final reports whether a run state is final.
func Final(state string) bool {
	switch state {
	case StatePassed, StateFailed, StateRejected, StateLimited, StateCanceled:
		return true
	}
	return false
}

// Going reports whether a run is queued, running or waiting.
func Going(state string) bool { return !Final(state) }

// Run triggers.
const (
	RunManual   = "manual"
	RunSchedule = "schedule"
	RunTest     = "test"
)

// Reasons the engine writes.
const (
	ReasonRestarted = "daemon restarted"
)

// Run is one execution: DATA/flows/runs/<id>/run.json. Snapshot is the
// flow as it was when the run started (edits do not change a run).
type Run struct {
	ID          string    `json:"id"`
	Flow        string    `json:"flow"`
	FlowName    string    `json:"flow_name"`
	FlowVersion int       `json:"flow_version"`
	Trigger     string    `json:"trigger"`              // manual | schedule | test
	Owner       string    `json:"owner,omitempty"`      // the flow's owner when the run started (Phase 4a)
	OwnerName   string    `json:"owner_name,omitempty"` // filled by the API on read
	Test        bool      `json:"test"`
	Actor       string    `json:"actor"`
	Input       string    `json:"input"`
	State       string    `json:"state"`
	Reason      string    `json:"reason"` // why it failed, was limited or canceled
	Note        string    `json:"note"`   // e.g. missed scheduled runs
	Step        string    `json:"step"`   // the current (or last) step id
	PC          int       `json:"pc"`     // index of the next step to run
	StepsRun    int       `json:"steps_run"`
	Created     time.Time `json:"created"`
	Started     time.Time `json:"started"` // zero while queued
	Ended       time.Time `json:"ended"`   // zero until final
	Updated     time.Time `json:"updated"`
	Tokens      int       `json:"tokens"`
	CostUSD     float64   `json:"cost_usd"`
	EnergyWh    float64   `json:"energy_wh"`
	ActiveMS    int64     `json:"active_ms"` // running time counted against timeout_min
	Sessions    []string  `json:"sessions"`
	Models      []string  `json:"models"`
	Approval    *Approval `json:"approval"`
	Snapshot    Flow      `json:"snapshot"`
	// Waiting is why the current step's model call is queued by the
	// engine right now ("waiting for qwen: 1 of 1 slots busy"; for a
	// fan-out step, a summary of its waiting items); "" when it is not.
	Waiting string `json:"waiting,omitempty"`
}

// Approval is the pending (or decided) approval of an approve step.
type Approval struct {
	Step    string    `json:"step"`
	Message string    `json:"message"`
	Inbox   string    `json:"inbox"` // the inbox item id
	Asked   time.Time `json:"asked"`
	Decided time.Time `json:"decided"`
	OK      bool      `json:"ok"`
	Note    string    `json:"note"`
	By      string    `json:"by"`
}

// StepRecord is one finished step: steps/<n>-<step id>.json, n counting
// executed steps from 1.
type StepRecord struct {
	N                int          `json:"n"`
	ID               string       `json:"id"`
	Kind             string       `json:"kind"`
	Name             string       `json:"name"`
	State            string       `json:"state"`
	Input            string       `json:"input"` // as rendered
	Output           any          `json:"output"`
	Model            string       `json:"model"`
	PromptTokens     int          `json:"prompt_tokens"`
	CompletionTokens int          `json:"completion_tokens"`
	Calls            int          `json:"calls"` // model calls (a retry counts)
	CostUSD          float64      `json:"cost_usd"`
	EnergyWh         float64      `json:"energy_wh"`
	Started          time.Time    `json:"started"`
	Ended            time.Time    `json:"ended"`
	DurationMS       int64        `json:"duration_ms"`
	Error            string       `json:"error"`
	Session          string       `json:"session"`
	Jump             string       `json:"jump"` // if: the branch taken ("then"/"else") and target
	File             string       `json:"file"` // output to file: path relative to the run folder
	Items            []ItemRecord `json:"items"`
	Retries          int          `json:"retries,omitempty"`     // model calls retried after a transient engine failure
	RetryError       string       `json:"retry_error,omitempty"` // the error that caused the retry
}

// ItemRecord is one fan-out item's result.
type ItemRecord struct {
	Index            int       `json:"index"`
	Title            string    `json:"title"`
	Kind             string    `json:"kind"`
	Model            string    `json:"model"`
	State            string    `json:"state"`
	Input            string    `json:"input"`
	Output           any       `json:"output"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Calls            int       `json:"calls"`
	CostUSD          float64   `json:"cost_usd"`
	EnergyWh         float64   `json:"energy_wh"`
	Session          string    `json:"session"`
	Error            string    `json:"error"`
	Started          time.Time `json:"started"`
	Ended            time.Time `json:"ended"`
	Retries          int       `json:"retries,omitempty"`     // model calls retried after a transient engine failure
	RetryError       string    `json:"retry_error,omitempty"` // the error that caused the retry
	Waiting          string    `json:"waiting,omitempty"`     // why its model call is queued now (live only)
}

// RunDetail is a run with its finished steps (in execution order). Live
// is the running fan-out step's items so far (the step has no record
// until it ends).
type RunDetail struct {
	Run   Run          `json:"run"`
	Steps []StepRecord `json:"steps"`
	Live  []ItemRecord `json:"live,omitempty"`
}

// newRunID is fr-<local yyyymmdd-hhmmss>-<6 random base36>.
func newRunID(t time.Time) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [6]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return "fr-" + t.Format("20060102-150405") + "-" + string(b[:])
}

// runStore is the on-disk side of runs.
type runStore struct{ dir string }

func (rs runStore) runDir(id string) string { return filepath.Join(rs.dir, id) }

func (rs runStore) write(r *Run) error {
	d := rs.runDir(r.ID)
	if err := os.MkdirAll(filepath.Join(d, "steps"), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return platform.WritePrivate(filepath.Join(d, "run.json"), append(b, '\n'))
}

func (rs runStore) read(id string) (*Run, error) {
	b, err := os.ReadFile(filepath.Join(rs.runDir(id), "run.json"))
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, errors.New("run id mismatch")
	}
	return &r, nil
}

func (rs runStore) writeStep(runID string, rec StepRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%03d-%s.json", rec.N, rec.ID)
	return platform.WritePrivate(filepath.Join(rs.runDir(runID), "steps", name), append(b, '\n'))
}

var stepFileRe = regexp.MustCompile(`^([0-9]{3,})-([a-z][a-z0-9_-]*)\.json$`)

// steps reads a run's step records in execution order.
func (rs runStore) steps(runID string) ([]StepRecord, error) {
	ents, err := os.ReadDir(filepath.Join(rs.runDir(runID), "steps"))
	if errors.Is(err, os.ErrNotExist) {
		return []StepRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []StepRecord{}
	for _, e := range ents {
		if !stepFileRe.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(rs.runDir(runID), "steps", e.Name()))
		if err != nil {
			continue
		}
		var rec StepRecord
		if json.Unmarshal(b, &rec) == nil {
			out = append(out, rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

// list reads every run.json under the runs folder.
func (rs runStore) list() []*Run {
	ents, err := os.ReadDir(rs.dir)
	if err != nil {
		return nil
	}
	var out []*Run
	for _, e := range ents {
		if !e.IsDir() || !ValidRunID(e.Name()) {
			continue
		}
		if r, err := rs.read(e.Name()); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func (rs runStore) remove(id string) error {
	if !ValidRunID(id) {
		return ErrNotFound
	}
	return os.RemoveAll(rs.runDir(id))
}

// writeFile writes an output file under <run>/files/ and answers its
// path relative to the run folder.
func (rs runStore) writeFile(runID, name string, data []byte) (string, error) {
	d := filepath.Join(rs.runDir(runID), "files")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	if err := platform.WritePrivate(filepath.Join(d, name), data); err != nil {
		return "", err
	}
	return "files/" + name, nil
}

// sanitizeFile is a safe base file name: letters, digits, ".", "-", "_"
// (others become "_"), no leading dots, at most 100 bytes; "" when nothing
// usable is left.
func sanitizeFile(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.TrimLeft(b.String(), "._")
	if len(s) > 100 {
		s = s[:100]
	}
	if strings.Trim(s, "_.-") == "" {
		return ""
	}
	return s
}

// uniqueFile is name, or name-2.ext, -3, ... when taken in the run's
// files folder.
func (rs runStore) uniqueFile(runID, name string) string {
	d := filepath.Join(rs.runDir(runID), "files")
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	cand := name
	for n := 2; ; n++ {
		if _, err := os.Lstat(filepath.Join(d, cand)); errors.Is(err, os.ErrNotExist) {
			return cand
		}
		cand = base + "-" + strconv.Itoa(n) + ext
	}
}
