package flows

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func planReply(req ModelRequest) ModelReply {
	return ModelReply{Text: `Sure! [{"title":"Page","kind":"html","task":"write a"},{"title":"Look","kind":"CSS","task":"write b"}] hope that helps`,
		PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.01}
}

func TestRunEveryModelStep(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if req.Model == "big" {
			return planReply(req), nil
		}
		last := req.Messages[len(req.Messages)-1].Content
		return ModelReply{Text: "[" + req.Model + "] " + last, Model: req.Model, PromptTokens: 10, CompletionTokens: 5, EnergyWh: 0.5}, nil
	}
	f := h.create(Flow{Name: "Site", Trigger: Trigger{Kind: TriggerManual}, Steps: []Step{
		{ID: "plan", Kind: KindPlan, Model: "big", Template: "Plan: {input}"},
		{ID: "fan", Kind: KindFanOut, From: "plan", DefaultModel: "small", Routes: []Route{{Kind: "css", Model: "cloud/x"}},
			Template: "{item.kind}:{item.title}:{item.task}"},
		{ID: "merge", Kind: KindPrompt, Model: "small", System: "You merge for {flow.name}.", Template: "Merge {steps.fan.output}"},
		{ID: "has", Kind: KindIf, Test: TestContains, On: "merge", Value: "WRITE A", Then: "save", Else: End},
		{ID: "skipme", Kind: KindOutput, To: ToNotify, Template: "never"},
		{ID: "save", Kind: KindOutput, To: ToFile, File: "../site {date}.html", Template: "<p>{steps.merge.output}</p>"},
	}})
	r := h.wait(h.run(f.ID, "a bakery").ID)
	if r.State != StatePassed {
		t.Fatalf("state %s (%s)", r.State, r.Reason)
	}
	// plan
	plan := h.stepRec(r.ID, "plan")
	items, _ := plan.Output.([]any)
	if len(items) != 2 || plan.Model != "big" || plan.Calls != 1 || plan.Input != "Plan: a bakery" {
		t.Fatalf("plan record %+v", plan)
	}
	// fan-out routes: html → default small, CSS → cloud/x (case-insensitive)
	fan := h.stepRec(r.ID, "fan")
	if len(fan.Items) != 2 || fan.Items[0].Model != "small" || fan.Items[1].Model != "cloud/x" {
		t.Fatalf("fan items %+v", fan.Items)
	}
	outs, _ := fan.Output.([]any)
	if len(outs) != 2 || outs[0] != "[small] html:Page:write a" {
		t.Fatalf("fan output %#v", fan.Output)
	}
	// merge got the system prompt and the fan-out output JSON-encoded
	var mergeReq ModelRequest
	for _, c := range h.models.Calls() {
		if c.Step == "merge" {
			mergeReq = c
		}
		if c.Client != ClientName || c.Run != r.ID || c.Flow != f.ID {
			t.Fatalf("usage identity %+v", c)
		}
	}
	if mergeReq.Messages[0].Role != "system" || mergeReq.Messages[0].Content != "You merge for Site." ||
		!strings.Contains(mergeReq.Messages[1].Content, `["[small] html:Page:write a"`) {
		t.Fatalf("merge request %+v", mergeReq.Messages)
	}
	// if: contains is case-insensitive → then: save; skipme skipped
	if rec := h.stepRec(r.ID, "has"); rec.Output != true || rec.Jump != "then → save" {
		t.Fatalf("if record %+v", rec)
	}
	for _, s := range h.detail(r.ID).Steps {
		if s.ID == "skipme" {
			t.Fatal("skipped step ran")
		}
	}
	// file output: sanitised name inside the run folder
	save := h.stepRec(r.ID, "save")
	if save.File != "files/site_2026-11-02.html" {
		t.Fatalf("file %q", save.File)
	}
	dir, _ := h.e.RunDir(r.ID)
	b, err := os.ReadFile(filepath.Join(dir, save.File))
	if err != nil || !strings.HasPrefix(string(b), "<p>[small] Merge") {
		t.Fatalf("file content %q %v", b, err)
	}
	// totals
	if r.Tokens != 150+3*15 || r.CostUSD != 0.01 || r.EnergyWh != 1.5 {
		t.Fatalf("totals tokens=%d cost=%v energy=%v", r.Tokens, r.CostUSD, r.EnergyWh)
	}
	if !slices.Equal(r.Models, []string{"big", "small", "cloud/x"}) && !slices.Equal(r.Models, []string{"big", "cloud/x", "small"}) {
		t.Fatalf("models %v", r.Models)
	}
	if st := h.events.states(r.ID); st[0] != StateQueued || st[len(st)-1] != StatePassed {
		t.Fatalf("events %v", st)
	}
	if !h.auditor.has("owner:test flow.run.start") || !h.auditor.has("owner:test flow.create") {
		t.Fatalf("audit %v", h.auditor.events)
	}
	// run records on disk
	if _, err := os.Stat(filepath.Join(dir, "steps", "001-plan.json")); err != nil {
		t.Fatal(err)
	}

	// graph
	g, err := h.e.Graph(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	node := map[string]Node{}
	for _, n := range g.Nodes {
		node[n.ID] = n
	}
	if node["st:plan"].State != StepPassed || node["st:skipme"].State != StepSkipped || node["end"].State != StatePassed {
		t.Fatalf("graph states %+v", g.Nodes)
	}
	if node["it:fan:1"].Sub != "cloud/x" || node["m:big"].Kind != NodeModel || node["input"].Sub != "a bakery" {
		t.Fatalf("graph nodes %+v", g.Nodes)
	}
	var ask, then int
	for _, e := range g.Edges {
		if e.Kind == EdgeAsk && e.From == "st:plan" && e.To == "m:big" && e.Tokens == 150 && e.Count == 1 {
			ask++
		}
		if e.Kind == EdgeThen && e.From == "st:has" && e.To == "st:save" {
			then++
		}
	}
	if ask != 1 || then != 1 || g.Updated.IsZero() {
		t.Fatalf("graph edges %+v", g.Edges)
	}
	if raw, _ := json.Marshal(g); !strings.Contains(string(raw), `"corrected":0`) {
		t.Fatal("graph JSON shape")
	}
}

func TestPlanAndJSONRetry(t *testing.T) {
	h := newHarness(t)
	var n int
	var mu sync.Mutex
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if req.Step == "plan" {
			if k == 1 {
				return ModelReply{Text: "I cannot do arrays", PromptTokens: 1}, nil
			}
			return ModelReply{Text: "```json\n[{\"title\":\"x\",\"task\":\"y\"}]\n```"}, nil
		}
		return ModelReply{Text: `{"verdict": {"ok": true}, "n": 3}`}, nil
	}
	f := h.create(Flow{Name: "Retry", Steps: []Step{
		{ID: "plan", Kind: KindPlan, Model: "big", Template: "go"},
		{ID: "judge", Kind: KindPrompt, Model: "small", JSON: true, Template: "judge"},
		{ID: "t", Kind: KindIf, Test: TestJSON, On: "judge", Path: "verdict.ok", Value: "true", Then: End},
		{ID: "never", Kind: KindOutput, To: ToInbox, Template: "x"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %s", r.State, r.Reason)
	}
	plan := h.stepRec(r.ID, "plan")
	if plan.Calls != 2 {
		t.Fatalf("plan calls %d", plan.Calls)
	}
	calls := h.models.Calls()
	if len(calls[1].Messages) != 4 || calls[1].Messages[2].Role != "assistant" || !strings.Contains(calls[1].Messages[3].Content, "not a usable plan") {
		t.Fatalf("retry messages %+v", calls[1].Messages)
	}
	if out, _ := h.stepRec(r.ID, "judge").Output.(map[string]any); out["n"] != 3.0 {
		t.Fatalf("judge output %#v", out)
	}
	if _, posted := h.notifier.counts(); posted != 0 {
		t.Fatal("jump to end did not stop")
	}

	// Two bad replies fail the plan.
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: "[1, 2]"}, nil
	}
	r = h.wait(h.run(f.ID, "").ID)
	if r.State != StateFailed || !strings.Contains(r.Reason, "step plan") {
		t.Fatalf("bad plan: %s %q", r.State, r.Reason)
	}
	if rec := h.stepRec(r.ID, "plan"); rec.State != StepFailed || rec.Calls != 2 || rec.Error == "" {
		t.Fatalf("failed plan record %+v", rec)
	}
}

func TestModelErrorFailsRun(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) { return ModelReply{}, errBoom }
	f := h.create(Flow{Name: "E", Steps: []Step{{ID: "a", Kind: KindPrompt, Model: "small", Template: "x"}}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateFailed || r.Reason != "step a: boom" {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	g, _ := h.e.Graph(r.ID)
	if g.Nodes[1].State != StepFailed || g.Nodes[len(g.Nodes)-1].ID != "end" {
		t.Fatalf("graph %+v", g.Nodes)
	}
}

func TestSessionStepAndStateTest(t *testing.T) {
	h := newHarness(t)
	h.sessions.result = SessionResult{State: "failed", Summary: "tests red"}
	f := h.create(Flow{Name: "S", Steps: []Step{
		{ID: "work", Kind: KindSession, Agent: "cerberus", Workspace: WorkspaceRepo, Repo: "/r", Task: "fix {input}"},
		{ID: "ok", Kind: KindIf, Test: TestState, On: "work", Value: "passed", Else: End},
		{ID: "tell", Kind: KindOutput, To: ToNotify, Template: "{steps.work.output}"},
	}})
	r := h.wait(h.run(f.ID, "bug 7").ID)
	if r.State != StatePassed || !slices.Equal(r.Sessions, []string{"s-1"}) {
		t.Fatalf("%s %v", r.State, r.Sessions)
	}
	req := h.sessions.reqs[0]
	if req.Task != "fix bug 7" || req.Workspace != WorkspaceRepo || req.Repo != "/r" || !slices.Equal(req.Tags, []string{"flow:" + f.ID}) || req.Run != r.ID {
		t.Fatalf("session request %+v", req)
	}
	out, _ := h.stepRec(r.ID, "work").Output.(map[string]any)
	if out["session"] != "s-1" || out["state"] != "failed" || out["summary"] != "tests red" {
		t.Fatalf("session output %#v", out)
	}
	if notified, _ := h.notifier.counts(); notified != 0 {
		t.Fatal("else: end did not stop")
	}
	g, _ := h.e.Graph(r.ID)
	if g.Nodes[1].Href != "#/s/s-1" {
		t.Fatalf("session href %+v", g.Nodes[1])
	}
	// passed → notify
	h.sessions.result = SessionResult{State: "passed"}
	r = h.wait(h.run(f.ID, "x").ID)
	if notified, _ := h.notifier.counts(); notified != 1 || r.State != StatePassed {
		t.Fatalf("notified=%d state=%s", notified, r.State)
	}
	// no session runner
	h.sessions.failErr = errBoom
	r = h.wait(h.run(f.ID, "x").ID)
	if r.State != StateFailed || !strings.Contains(r.Reason, "session not started") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
}

func approveFlow() Flow {
	return Flow{Name: "Gate", Steps: []Step{
		{ID: "draft", Kind: KindPrompt, Model: "small", Template: "draft {input}"},
		{ID: "gate", Kind: KindApprove, Message: "Ship {steps.draft.output}?"},
		{ID: "out", Kind: KindOutput, To: ToInbox, Template: "shipped {steps.draft.output}"},
	}}
}

func TestApproveAndReject(t *testing.T) {
	h := newHarness(t)
	f := h.create(approveFlow())
	r := h.wait(h.run(f.ID, "it").ID)
	if r.State != StateWaiting || r.Approval == nil || r.Approval.Message != "Ship ok:draft it?" || r.Approval.Inbox != "in-1" {
		t.Fatalf("waiting run %+v %+v", r, r.Approval)
	}
	notified, posted := h.notifier.counts()
	if notified != 1 || posted != 1 || h.notifier.posted[0].Kind != "flow-approve" || h.notifier.posted[0].Link != "#/flows/runs/"+r.ID {
		t.Fatalf("approval notices %+v", h.notifier.posted)
	}
	g, _ := h.e.Graph(r.ID)
	if g.Nodes[2].State != StepWaiting {
		t.Fatalf("graph waiting %+v", g.Nodes[2])
	}
	// waiting runs hold no slot
	h.e.mu.Lock()
	active := h.e.active
	h.e.mu.Unlock()
	if active != 0 {
		t.Fatalf("active = %d while waiting", active)
	}
	if _, err := h.e.Approve("owner:test", "fr-20260101-000000-zzzzzz", true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if got := h.notifier.resolvedIDs(); len(got) != 0 {
		t.Fatalf("resolved before a decision: %v", got)
	}
	if _, err := h.e.Approve("owner:test", r.ID, true, "go"); err != nil {
		t.Fatal(err)
	}
	if got := h.notifier.resolvedIDs(); !slices.Equal(got, []string{"in-1"}) {
		t.Fatalf("approval inbox item not resolved: %v", got)
	}
	r = h.wait(r.ID)
	if r.State != StatePassed {
		t.Fatalf("after approve: %s %s", r.State, r.Reason)
	}
	if out, _ := h.stepRec(r.ID, "gate").Output.(map[string]any); out["ok"] != true || out["note"] != "go" {
		t.Fatalf("gate output %#v", out)
	}
	if h.notifier.posted[1].Body != "shipped ok:draft it" {
		t.Fatalf("output after approve %+v", h.notifier.posted[1])
	}
	if _, err := h.e.Approve("owner:test", r.ID, true, ""); !errors.Is(err, ErrState) {
		t.Fatalf("approve a passed run: %v", err)
	}
	if !h.auditor.has("owner:test flow.run.approve") {
		t.Fatal("approve not audited")
	}

	// reject
	r = h.wait(h.run(f.ID, "no").ID)
	r, err := h.e.Approve("owner:phone", r.ID, false, "not now")
	if err != nil || r.State != StateRejected || r.Reason != "rejected by owner:phone: not now" {
		t.Fatalf("reject: %+v %v", r, err)
	}
	if rec := h.stepRec(r.ID, "gate"); rec.State != StepRejected {
		t.Fatalf("gate record %+v", rec)
	}
	if got := h.notifier.resolvedIDs(); len(got) != 2 || got[1] != r.Approval.Inbox {
		t.Fatalf("rejected approval not resolved: %v", got)
	}
	if !h.auditor.has("owner:phone flow.run.reject") {
		t.Fatal("reject not audited")
	}
}

func TestRestartRecovery(t *testing.T) {
	h := newHarness(t)
	f := h.create(approveFlow())
	waiting := h.wait(h.run(f.ID, "keep").ID)

	h.sessions.block = true
	sf := h.create(Flow{Name: "Long", Steps: []Step{{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "t"}}})
	running := h.run(sf.ID, "")
	<-h.sessions.started
	_ = h.e.Close()
	if st := h.sessions.Stopped(); len(st) != 0 {
		t.Fatalf("shutdown stopped sessions %v", st)
	}

	// A crash leaves a run "running" on disk.
	crashed := running
	crashed.ID = "fr-20261102-075800-crash1"
	crashed.State = StateRunning
	if err := (runStore{dir: filepath.Join(h.dir, "runs")}).write(&crashed); err != nil {
		t.Fatal(err)
	}
	h.sessions.block = false
	h.open()
	for _, id := range []string{running.ID, crashed.ID} {
		d := h.detail(id)
		if d.Run.State != StateFailed || d.Run.Reason != ReasonRestarted {
			t.Fatalf("%s after restart: %s %q", id, d.Run.State, d.Run.Reason)
		}
	}
	if d := h.detail(waiting.ID); d.Run.State != StateWaiting {
		t.Fatalf("waiting run after restart: %s", d.Run.State)
	}
	// It resumes after the approve step with the earlier outputs.
	if _, err := h.e.Approve("owner:test", waiting.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	r := h.wait(waiting.ID)
	if r.State != StatePassed {
		t.Fatalf("resumed: %s %s", r.State, r.Reason)
	}
	_, posted := h.notifier.counts()
	if h.notifier.posted[posted-1].Body != "shipped ok:draft keep" {
		t.Fatalf("resumed output %+v", h.notifier.posted[posted-1])
	}
	steps := h.detail(waiting.ID).Steps
	if len(steps) != 3 || steps[0].N != 1 || steps[1].N != 2 || steps[2].N != 3 {
		t.Fatalf("step numbering %+v", steps)
	}
}

func TestCancel(t *testing.T) {
	h := newHarness(t)
	h.sessions.block = true
	h.e.SetMaxRuns(1)
	f := h.create(Flow{Name: "C", Steps: []Step{{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "t"}}})
	r1 := h.run(f.ID, "")
	<-h.sessions.started
	r2 := h.run(f.ID, "")
	if d := h.detail(r2.ID); d.Run.State != StateQueued {
		t.Fatalf("second run %s, want queued", d.Run.State)
	}
	// cancel the queued one
	c2, err := h.e.Cancel("owner:test", r2.ID)
	if err != nil || c2.State != StateCanceled {
		t.Fatalf("cancel queued: %+v %v", c2, err)
	}
	// cancel the running one: its session is stopped
	if _, err := h.e.Cancel("owner:test", r1.ID); err != nil {
		t.Fatal(err)
	}
	c1 := h.wait(r1.ID)
	if c1.State != StateCanceled || c1.Reason != "canceled by owner:test" {
		t.Fatalf("cancel running: %s %q", c1.State, c1.Reason)
	}
	if !slices.Equal(h.sessions.Stopped(), []string{"s-1"}) {
		t.Fatalf("stopped %v", h.sessions.Stopped())
	}
	if rec := h.stepRec(r1.ID, "w"); rec.State != StepCanceled || rec.Session != "s-1" {
		t.Fatalf("canceled record %+v", rec)
	}
	if _, err := h.e.Cancel("owner:test", r1.ID); !errors.Is(err, ErrState) {
		t.Fatalf("cancel twice: %v", err)
	}
	// cancel a waiting one
	h.sessions.block = false
	af := h.create(approveFlow())
	w := h.wait(h.run(af.ID, "").ID)
	if c, err := h.e.Cancel("owner:test", w.ID); err != nil || c.State != StateCanceled {
		t.Fatalf("cancel waiting: %+v %v", c, err)
	}
	if !h.auditor.has("owner:test flow.run.cancel") {
		t.Fatal("cancel not audited")
	}
}

func TestMaxRunsQueue(t *testing.T) {
	h := newHarness(t)
	h.sessions.block = true
	f := h.create(Flow{Name: "Q", Steps: []Step{{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "t"}}})
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, h.run(f.ID, "").ID)
	}
	<-h.sessions.started
	<-h.sessions.started
	if d := h.detail(ids[2]); d.Run.State != StateQueued {
		t.Fatalf("third run %s with max_runs 2", d.Run.State)
	}
	select {
	case id := <-h.sessions.started:
		t.Fatalf("a third session started: %s", id)
	case <-time.After(50 * time.Millisecond):
	}
	h.sessions.mu.Lock()
	h.sessions.block = false
	h.sessions.mu.Unlock()
	close(h.sessions.release)
	for _, id := range ids {
		if r := h.wait(id); r.State != StatePassed {
			t.Fatalf("%s: %s %s", id, r.State, r.Reason)
		}
	}
}

func TestLimits(t *testing.T) {
	h := newHarness(t)
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: "x", PromptTokens: 60, CompletionTokens: 20, CostUSD: 0.30}, nil
	}
	steps := []Step{
		{ID: "a", Kind: KindPrompt, Model: "small", Template: "1"},
		{ID: "b", Kind: KindPrompt, Model: "small", Template: "2"},
		{ID: "c", Kind: KindPrompt, Model: "small", Template: "3"},
	}
	tok := h.create(Flow{Name: "Tokens", Limits: Limits{MaxTokens: 100}, Steps: steps})
	r := h.wait(h.run(tok.ID, "").ID)
	if r.State != StateLimited || !strings.Contains(r.Reason, "tokens") || r.Tokens != 160 {
		t.Fatalf("tokens: %s %q %d", r.State, r.Reason, r.Tokens)
	}
	if rec := h.stepRec(r.ID, "b"); rec.State != StepLimited {
		t.Fatalf("limited record %+v", rec)
	}
	cost := h.create(Flow{Name: "Cost", Limits: Limits{MaxCostUSD: 0.5}, Steps: steps})
	r = h.wait(h.run(cost.ID, "").ID)
	if r.State != StateLimited || !strings.Contains(r.Reason, "cost") {
		t.Fatalf("cost: %s %q", r.State, r.Reason)
	}
	// max steps counts fan-out items
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: `[{"title":"1","task":"a"},{"title":"2","task":"b"},{"title":"3","task":"c"}]`}, nil
	}
	ms := h.create(Flow{Name: "Steps", Limits: Limits{MaxSteps: 4}, Steps: []Step{
		{ID: "p", Kind: KindPlan, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}"},
	}})
	r = h.wait(h.run(ms.ID, "").ID)
	if r.State != StateLimited || !strings.Contains(r.Reason, "more than 4 steps") {
		t.Fatalf("steps: %s %q", r.State, r.Reason)
	}
	// timeout (unit shortened for the test)
	old := timeoutUnit
	timeoutUnit = 20 * time.Millisecond
	defer func() { timeoutUnit = old }()
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		<-ctx.Done()
		return ModelReply{}, ctx.Err()
	}
	to := h.create(Flow{Name: "Slow", Limits: Limits{TimeoutMin: 1}, Steps: steps[:1]})
	r = h.wait(h.run(to.ID, "").ID)
	if r.State != StateLimited || r.Reason != "timeout after 1 min" {
		t.Fatalf("timeout: %s %q", r.State, r.Reason)
	}
}

func TestFanOutParallelismAndFailure(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if req.Step == "p" {
			var b strings.Builder
			b.WriteString("[")
			for i := 0; i < 6; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"title":"t","kind":"k","task":"x"}`)
			}
			b.WriteString("]")
			return ModelReply{Text: b.String()}, nil
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ModelReply{}, ctx.Err()
		}
		return ModelReply{Text: "done"}, nil
	}
	f := h.create(Flow{Name: "Par", Limits: Limits{MaxParallel: 2}, Steps: []Step{
		{ID: "p", Kind: KindPlan, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", MaxParallel: 3, Template: "{item.task}"},
	}})
	r := h.run(f.ID, "")
	eventually(t, "two items running", func() bool {
		h.models.mu.Lock()
		defer h.models.mu.Unlock()
		return h.models.cur == 2
	})
	// The live graph shows the items while the step runs.
	g, _ := h.e.Graph(r.ID)
	running := 0
	for _, n := range g.Nodes {
		if n.Kind == NodeItem && n.State == StepRunning {
			running++
		}
	}
	if running != 2 {
		t.Fatalf("live items running = %d: %+v", running, g.Nodes)
	}
	close(gate)
	if r = h.wait(r.ID); r.State != StatePassed {
		t.Fatalf("%s %s", r.State, r.Reason)
	}
	if h.models.peak != 2 {
		t.Fatalf("peak parallel %d, want 2", h.models.peak)
	}

	// One failing item fails the step and cancels the rest.
	h.models.peak = 0
	var mu sync.Mutex
	n := 0
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if req.Step == "p" {
			return ModelReply{Text: `["a","b","c","d"]`}, nil
		}
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if k == 1 {
			return ModelReply{}, errBoom
		}
		<-ctx.Done()
		return ModelReply{}, ctx.Err()
	}
	jf := h.create(Flow{Name: "Par JSON", Steps: []Step{
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}"},
	}})
	r = h.wait(h.run(jf.ID, "").ID)
	if r.State != StateFailed || !strings.Contains(r.Reason, "boom") {
		t.Fatalf("failing item: %s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "f")
	failed := 0
	for _, it := range rec.Items {
		if it.State == StepFailed && it.Error == "boom" {
			failed++
		}
		if it.Title == "" {
			t.Fatalf("string items keep their text as title: %+v", it)
		}
	}
	if failed != 1 || len(rec.Items) != 4 {
		t.Fatalf("items %+v", rec.Items)
	}
}

func TestOutputsAndDisabled(t *testing.T) {
	h := newHarness(t)
	f := h.create(Flow{Name: "Out", Steps: []Step{
		{ID: "n", Kind: KindOutput, To: ToNotify, Template: "hello {input} at {time}"},
		{ID: "i", Kind: KindOutput, To: ToInbox, Template: "inbox {flow.name}"},
		{ID: "f1", Kind: KindOutput, To: ToFile, Template: "a"},
		{ID: "f2", Kind: KindOutput, To: ToFile, Template: "b"},
	}})
	r := h.wait(h.run(f.ID, "you").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %s", r.State, r.Reason)
	}
	if h.notifier.notified[0].Body != "hello you at 07:59" || h.notifier.posted[0].Body != "inbox Out" {
		t.Fatalf("notices %+v %+v", h.notifier.notified, h.notifier.posted)
	}
	if a, b := h.stepRec(r.ID, "f1").File, h.stepRec(r.ID, "f2").File; a != "files/output.txt" || b != "files/output-2.txt" {
		t.Fatalf("files %q %q", a, b)
	}
	h.e.SetEnabled(false)
	if _, err := h.e.RunFlow("owner:test", f.ID, ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	h.e.SetEnabled(true)
	if _, err := h.e.RunFlow("owner:test", "fl-missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing flow: %v", err)
	}
	// test runs of drafts: invalid refused, valid marked test
	var ve *ValidationError
	if _, err := h.e.TestRun("owner:test", Flow{Name: "x"}, ""); !errors.As(err, &ve) {
		t.Fatalf("invalid draft: %v", err)
	}
	tr, err := h.e.TestRun("owner:test", Flow{Name: "Draft", Steps: []Step{{ID: "a", Kind: KindPrompt, Model: "small", Template: "{input}"}}}, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if tr = h.wait(tr.ID); !tr.Test || tr.Flow != "fl-draft" || tr.Trigger != RunTest || tr.State != StatePassed {
		t.Fatalf("test run %+v", tr)
	}
	if runs := h.e.Runs("fl-draft", 0); len(runs) != 1 {
		t.Fatalf("draft runs %d", len(runs))
	}
}

func TestEngineFlowCRUD(t *testing.T) {
	h := newHarness(t)
	f := h.create(Flow{Name: "Warn", Steps: []Step{{ID: "a", Kind: KindPrompt, Model: "absent", Template: "x"}}})
	_, ps, err := h.e.Update("owner:test", f)
	if err != nil || len(ps) != 1 || ps[0].Severity != SevWarning {
		t.Fatalf("update warnings %+v %v", ps, err)
	}
	if _, _, err := h.e.Update("owner:test", f); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	bad := f
	bad.Steps[0].Model = "nope"
	if _, ps, err := h.e.Create("owner:test", bad); err == nil || !HasErrors(ps) {
		t.Fatal("unknown model accepted")
	}
	on, err := h.e.SetFlowEnabled("owner:test", f.ID, true)
	if err != nil || !on.Enabled || on.Version != 3 {
		t.Fatalf("enable %+v %v", on, err)
	}
	if l := h.e.List(); len(l) != 1 || l[0].TriggerText != "Manual" || l[0].Next != nil {
		t.Fatalf("list %+v", l)
	}
	if err := h.e.Delete("owner:test", f.ID); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"flow.create", "flow.update", "flow.delete"} {
		if !h.auditor.has("owner:test " + ev) {
			t.Errorf("%s not audited", ev)
		}
	}
	if len(h.e.Templates()) != 4 {
		t.Fatal("templates")
	}
}

func TestRetention(t *testing.T) {
	h := newHarness(t)
	h.e.o.KeepRuns = 2
	f := h.create(Flow{Name: "R", Steps: []Step{{ID: "a", Kind: KindOutput, To: ToInbox, Template: "x"}}})
	var ids []string
	for i := 0; i < 4; i++ {
		h.clock.Advance(time.Second)
		ids = append(ids, h.wait(h.run(f.ID, "").ID).ID)
	}
	runs := h.e.Runs(f.ID, 0)
	if len(runs) != 2 || runs[0].ID != ids[3] || runs[1].ID != ids[2] {
		t.Fatalf("kept %v", runs)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "runs", ids[0])); !os.IsNotExist(err) {
		t.Fatalf("old run folder left: %v", err)
	}
}

func TestStaticFlowGraph(t *testing.T) {
	g := FlowGraph(validFlow())
	ids := []string{}
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
		if n.State != "" {
			t.Fatalf("static state %+v", n)
		}
	}
	want := []string{"input", "st:plan", "st:fan", "st:work", "st:check", "st:ok", "st:out", "end", "m:big", "m:cloud/x", "m:small"}
	if !slices.Equal(ids, want) {
		t.Fatalf("nodes %v", ids)
	}
	var thenEnd, elseEnd bool
	for _, e := range g.Edges {
		if e.From == "st:check" && e.Kind == EdgeThen && e.To == "st:ok" {
			thenEnd = true
		}
		if e.From == "st:check" && e.Kind == EdgeElse && e.To == "end" {
			elseEnd = true
		}
	}
	if !thenEnd || !elseEnd {
		t.Fatalf("if edges %+v", g.Edges)
	}
}
