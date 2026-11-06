package evals

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSessions starts "sessions" that end when the test says so (or at
// once with auto), each with a workspace folder of its own.
type fakeSessions struct {
	t    *testing.T
	root string
	auto bool // sessions end (passed) as soon as they start

	mu       sync.Mutex
	n        int
	infos    map[string]SessionInfo
	done     map[string]chan struct{}
	reqs     []StartRequest
	stopped  []string
	running  int
	peak     int
	startErr error
	write    func(dir string, req StartRequest) // fills a new workspace

	resumes   []string // "id: text" of every Resume
	resumeErr error
	onResume  func(dir string) // changes the workspace for the new attempt
	// Like the session manager, RunStarted is the first attempt's start
	// only; attempt k (from 0) runs 1000+100k .. 1000+100k+12 s. With
	// noAttemptStart AttemptStarted is left zero (an older host).
	attempts       map[string]int
	noAttemptStart bool
}

func newFakeSessions(t *testing.T) *fakeSessions {
	return &fakeSessions{t: t, root: t.TempDir(), infos: map[string]SessionInfo{}, done: map[string]chan struct{}{}}
}

func (f *fakeSessions) Start(ctx context.Context, req StartRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.n++
	id := fmt.Sprintf("s%d", f.n)
	dir := filepath.Join(f.root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if f.write != nil {
		f.write(dir, req)
	}
	ws := req.Workspace
	f.infos[id] = SessionInfo{ID: id, State: "running", Workspace: ws, Worktree: dir, Agent: req.Agent, Model: req.Model,
		SetupHash: "hash-" + req.Agent, BaseCommit: req.Base, Checks: "ok all tests pass"}
	f.done[id] = make(chan struct{})
	f.reqs = append(f.reqs, req)
	f.running++
	f.peak = max(f.peak, f.running)
	if f.auto {
		f.endLocked(id, "passed")
	}
	return id, nil
}

func (f *fakeSessions) endLocked(id, state string) {
	in := f.infos[id]
	if in.State != "running" {
		return
	}
	in.State = state
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	start := time.Unix(1000+100*int64(f.attempts[id]), 0)
	f.attempts[id]++
	if in.RunStarted.IsZero() {
		in.RunStarted = start
	}
	if !f.noAttemptStart {
		in.AttemptStarted = start
	}
	in.RunEnded = start.Add(12 * time.Second)
	f.infos[id] = in
	f.running--
	close(f.done[id])
}

func (f *fakeSessions) end(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endLocked(id, state)
}

func (f *fakeSessions) Wait(ctx context.Context, id string) (SessionInfo, error) {
	f.mu.Lock()
	ch, ok := f.done[id]
	f.mu.Unlock()
	if !ok {
		return SessionInfo{ID: id, State: "removed"}, nil
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return SessionInfo{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.infos[id], nil
}

func (f *fakeSessions) Stop(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	f.endLocked(id, "stopped")
	return nil
}

// Resume starts a new attempt of an ended session; with auto it ends
// (passed) at once.
func (f *fakeSessions) Resume(ctx context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return f.resumeErr
	}
	in, ok := f.infos[id]
	if !ok {
		return errors.New("no such session")
	}
	if in.State == "running" {
		return errors.New("session is running")
	}
	f.resumes = append(f.resumes, id+": "+text)
	if f.onResume != nil {
		f.onResume(in.Worktree)
	}
	in.State = "running"
	f.infos[id] = in
	f.done[id] = make(chan struct{})
	f.running++
	if f.auto {
		f.endLocked(id, "passed")
	}
	return nil
}

func (f *fakeSessions) resumed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resumes...)
}

func (f *fakeSessions) Info(id string) (SessionInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.infos[id]
	return in, ok
}

func (f *fakeSessions) Transcript(id string) ([]byte, error) {
	return []byte("agent: I wrote main.go\nsecret=hunter2-value\n"), nil
}

func (f *fakeSessions) Diff(id string) (string, error) { return "+func main() {}\n", nil }

func (f *fakeSessions) Mask(s string) string {
	return strings.ReplaceAll(s, "hunter2-value", "••••")
}

func (f *fakeSessions) Usage(id string) Usage {
	return Usage{Tokens: 100, CostUSD: 0.01, EnergyWh: 0.5}
}

func (f *fakeSessions) ResolveBase(ctx context.Context, owner, repo, base string) (string, error) {
	if base == "" {
		return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	}
	return base + strings.Repeat("0", 40-len(base)), nil
}

func (f *fakeSessions) started() []StartRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StartRequest(nil), f.reqs...)
}

// fakeJudge answers per criterion id: verdicts maps id → "pass" / "fail"
// / "garbage" (not JSON, every time) / "garbage-once" / "err". failTimes
// fails a criterion that many times, then passes it. A feedback call
// (FeedbackSystemPrompt) answers "Feedback: " and the criteria it names,
// or fails with feedbackErr.
type fakeJudge struct {
	mu          sync.Mutex
	verdicts    map[string]string
	failTimes   map[string]int
	feedbackErr error
	calls       []ModelRequest
	once        map[string]bool
	inFlight    int
	peak        int
	block       chan struct{} // non-nil: calls wait for it (or ctx)
}

var critRe = regexp.MustCompile(`Criterion ([a-z0-9_]+):`)

func (j *fakeJudge) Complete(ctx context.Context, req ModelRequest) (ModelReply, error) {
	j.mu.Lock()
	j.calls = append(j.calls, req)
	j.inFlight++
	j.peak = max(j.peak, j.inFlight)
	block := j.block
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.inFlight--
		j.mu.Unlock()
	}()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ModelReply{}, ctx.Err()
		}
	} else {
		time.Sleep(2 * time.Millisecond) // lets parallel calls overlap
	}
	user := req.Messages[1].Content
	if strings.HasPrefix(req.Messages[0].Content, FeedbackSystemPrompt) {
		j.mu.Lock()
		ferr := j.feedbackErr
		j.mu.Unlock()
		if ferr != nil {
			return ModelReply{}, ferr
		}
		ids := regexp.MustCompile(`(?m)^- ([a-z0-9_]+):`).FindAllStringSubmatch(user, -1)
		var names []string
		for _, x := range ids {
			names = append(names, x[1])
		}
		return ModelReply{Text: "Feedback: fix " + strings.Join(names, ", "), Model: req.Model, PromptTokens: 30,
			CompletionTokens: 10, CostUSD: 0.0005}, nil
	}
	m := critRe.FindStringSubmatch(user)
	if m == nil {
		return ModelReply{}, errors.New("no criterion in the message")
	}
	j.mu.Lock()
	v := j.verdicts[m[1]]
	if n := j.failTimes[m[1]]; n > 0 {
		j.failTimes[m[1]] = n - 1
		v = "fail"
	}
	if v == "garbage-once" {
		if j.once == nil {
			j.once = map[string]bool{}
		}
		if !j.once[m[1]] {
			j.once[m[1]] = true
			j.mu.Unlock()
			return ModelReply{Text: "I think it passes.", PromptTokens: 10, CompletionTokens: 2}, nil
		}
		v = "pass"
	}
	j.mu.Unlock()
	switch v {
	case "garbage":
		return ModelReply{Text: "sure!", PromptTokens: 10, CompletionTokens: 2}, nil
	case "err":
		return ModelReply{}, errors.New("model exploded")
	case "":
		v = "pass"
	}
	return ModelReply{Text: fmt.Sprintf("```json\n{\"verdict\": %q, \"rationale\": \"because %s\", \"evidence\": [\"main.go:1\"]}\n```", v, m[1]),
		Model: req.Model, PromptTokens: 50, CompletionTokens: 20, CostUSD: 0.001, EnergyWh: 0.01}, nil
}

func (j *fakeJudge) callCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.calls)
}

type fakeCatalog struct{}

func (fakeCatalog) Model(id string) (bool, bool) {
	switch id {
	case "qwen", "judge":
		return true, true
	case "absent":
		return true, false
	}
	return false, false
}

func (fakeCatalog) Agent(name string) (bool, bool) {
	switch name {
	case "cerberus", "claude":
		return true, true
	case "codex":
		return true, false
	}
	return false, false
}

func (fakeCatalog) Persona(name string) bool { return name == "strict" }

type fakeEvents struct {
	mu  sync.Mutex
	evs []Event
	ch  chan Event
}

func (f *fakeEvents) Publish(ev Event) {
	f.mu.Lock()
	f.evs = append(f.evs, ev)
	ch := f.ch
	f.mu.Unlock()
	if ch != nil {
		select {
		case ch <- ev:
		default:
		}
	}
}

type fakeAudit struct {
	mu     sync.Mutex
	events []string
}

func (a *fakeAudit) Event(actor, event, detail string, targets ...string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	return nil
}

// sample is a valid eval with two gates and a dependency.
func sample() Eval {
	return Eval{Name: "Health endpoint", Task: "Add a GET /health endpoint", Workspace: WorkspaceScratch,
		Gates: []Gate{{ID: "build", Name: "Build", MaxIterations: 1}, {ID: "test", Name: "Test", MaxIterations: 1}},
		Criteria: []Criterion{
			{ID: "has_server", Prompt: "There is an HTTP server entry point in main.go", Gate: "build"},
			{ID: "health_route", Prompt: "GET /health returns 200", Gate: "build", DependsOn: []string{"has_server"}},
			{ID: "tests_pass", Prompt: "The tests pass", Gate: "test", Evidence: []string{"checks", "files"}},
		},
		Setups: []Setup{{Label: "qwen", Agent: "cerberus", Model: "qwen"}, {Label: "claude", Agent: "claude"}},
		Repeat: 1, JudgeModel: "judge"}
}

type env struct {
	t      *testing.T
	e      *Engine
	sess   *fakeSessions
	judge  *fakeJudge
	events *fakeEvents
	audit  *fakeAudit
	dir    string
	set    Settings
}

func newEnv(t *testing.T) *env {
	t.Helper()
	en := &env{t: t, sess: newFakeSessions(t), judge: &fakeJudge{verdicts: map[string]string{}}, events: &fakeEvents{},
		audit: &fakeAudit{}, dir: t.TempDir(), set: Settings{Enabled: true, MaxParallel: 1, EvidenceKB: 16}}
	en.open()
	return en
}

func (en *env) open() {
	en.t.Helper()
	e, err := Open(Options{Dir: en.dir, Sessions: en.sess, Models: en.judge, Catalog: fakeCatalog{}, Auditor: en.audit,
		Events: en.events, Settings: func() Settings { return en.set }})
	if err != nil {
		en.t.Fatal(err)
	}
	en.e = e
	en.t.Cleanup(func() { _ = e.Close() })
}

// waitRun waits until the run reaches a final state.
func (en *env) waitRun(id string) RunDetail {
	en.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, err := en.e.GetRun(id)
		if err != nil {
			en.t.Fatal(err)
		}
		if Final(d.Run.State) {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	d, _ := en.e.GetRun(id)
	en.t.Fatalf("run %s did not end: %+v", id, d.Run)
	return RunDetail{}
}

// waitFor polls cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
