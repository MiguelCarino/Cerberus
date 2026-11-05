package flows

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// An approval that lands the moment the run turns waiting starts a new
// execution; the old one must not drop that execution's live entry, or the
// run could no longer be canceled.
func TestApproveAtOnceKeepsRunCancelable(t *testing.T) {
	h := newHarness(t)
	h.sessions.block = true
	f := h.create(Flow{Name: "G", Steps: []Step{
		{ID: "gate", Kind: KindApprove, Message: "go?"},
		{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "t"},
	}})
	for i := 0; i < 30; i++ {
		r := h.wait(h.run(f.ID, "").ID)
		if r.State != StateWaiting {
			t.Fatalf("run %d: %s", i, r.State)
		}
		if _, err := h.e.Approve("owner:test", r.ID, true, ""); err != nil {
			t.Fatal(err)
		}
		sid := <-h.sessions.started
		if _, err := h.e.Cancel("owner:test", r.ID); err != nil {
			t.Fatal(err)
		}
		c := h.wait(r.ID)
		if c.State != StateCanceled || c.Reason != "canceled by owner:test" {
			t.Fatalf("run %d after cancel: %s %q", i, c.State, c.Reason)
		}
		eventually(t, "session stopped", func() bool { return slices.Contains(h.sessions.Stopped(), sid) })
	}
	eventually(t, "slots free", func() bool {
		h.e.mu.Lock()
		defer h.e.mu.Unlock()
		return h.e.active == 0 && len(h.e.live) == 0
	})
}

// A run that waited while newer runs of its flow finished is kept when it
// ends, with its steps and files.
func TestPruneKeepsTheRunThatJustEnded(t *testing.T) {
	h := newHarness(t)
	h.e.o.KeepRuns = 2
	f := h.create(Flow{Name: "P", Steps: []Step{
		{ID: "gate", Kind: KindApprove, Message: "go?"},
		{ID: "save", Kind: KindOutput, To: ToFile, File: "out.txt", Template: "{input}"},
	}})
	a := h.wait(h.run(f.ID, "a").ID)
	var later []string
	for i := 0; i < 2; i++ {
		h.clock.Advance(time.Second)
		r := h.wait(h.run(f.ID, "x").ID)
		if _, err := h.e.Approve("owner:test", r.ID, true, ""); err != nil {
			t.Fatal(err)
		}
		later = append(later, h.wait(r.ID).ID)
	}
	if _, err := h.e.Approve("owner:test", a.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if r := h.wait(a.ID); r.State != StatePassed {
		t.Fatalf("a: %s %s", r.State, r.Reason)
	}
	if rec := h.stepRec(a.ID, "save"); rec.File == "" {
		t.Fatalf("a's output %+v", rec)
	}
	runs := h.e.Runs(f.ID, 0)
	if len(runs) != 2 || runs[0].ID != later[1] || runs[1].ID != a.ID {
		t.Fatalf("kept %v", runs)
	}
	// A run rejected (or canceled) late is kept too.
	b := h.wait(h.run(f.ID, "b").ID)
	h.clock.Advance(time.Second)
	c := h.wait(h.run(f.ID, "c").ID)
	if _, err := h.e.Cancel("owner:test", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Approve("owner:test", b.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.GetRun(b.ID); err != nil {
		t.Fatalf("rejected run pruned: %v", err)
	}
}

// An approval posted while the run is being canceled is resolved, so the
// inbox does not keep offering Approve for a run that no longer waits.
func TestApprovalPostedDuringCancelIsResolved(t *testing.T) {
	h := newHarness(t)
	f := h.create(Flow{Name: "G", Steps: []Step{{ID: "gate", Kind: KindApprove, Message: "go?"}}})
	h.notifier.onPost = func(n Notice) {
		if _, err := h.e.Cancel("owner:test", n.Run); err != nil {
			t.Error(err)
		}
	}
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateCanceled || r.Approval != nil {
		t.Fatalf("%s %+v", r.State, r.Approval)
	}
	if got := h.notifier.resolvedIDs(); !slices.Equal(got, []string{"in-1"}) {
		t.Fatalf("orphan approval not resolved: %v", got)
	}
}

// Model usage inside a session step counts toward the run's limits; a
// session that passes one is stopped and the run ends limited.
func TestSessionUsageCountsTowardLimits(t *testing.T) {
	h := newHarness(t)
	h.sessions.block = true
	f := h.create(Flow{Name: "S", Limits: Limits{MaxCostUSD: 0.5}, Steps: []Step{
		{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "t"},
	}})
	r := h.run(f.ID, "")
	sid := <-h.sessions.started
	eventually(t, "meter", func() bool { return h.sessions.metered(sid) })
	h.sessions.use(sid, ModelReply{Model: "cloud/x", PromptTokens: 100, CompletionTokens: 20, CostUSD: 0.3})
	h.sessions.use(sid, ModelReply{Model: "cloud/x", PromptTokens: 100, CompletionTokens: 20, CostUSD: 0.3})
	done := h.wait(r.ID)
	if done.State != StateLimited || !strings.Contains(done.Reason, "over the limit") {
		t.Fatalf("%s %q", done.State, done.Reason)
	}
	if done.Tokens != 240 || done.CostUSD < 0.59 || !slices.Contains(done.Models, "cloud/x") {
		t.Fatalf("usage %d %v %v", done.Tokens, done.CostUSD, done.Models)
	}
	if !slices.Contains(h.sessions.Stopped(), sid) {
		t.Fatalf("session over the limit not stopped: %v", h.sessions.Stopped())
	}
	if rec := h.stepRec(r.ID, "w"); rec.State != StepLimited || rec.Calls != 2 || rec.CostUSD < 0.59 {
		t.Fatalf("step record %+v", rec)
	}
	if h.sessions.metered(sid) {
		t.Fatal("meter left running")
	}

	// Under the limit the session's usage is still charged, and fan-out
	// items run as sessions are metered as well.
	g := h.create(Flow{Name: "F", Steps: []Step{
		{ID: "list", Kind: KindPrompt, Model: "small", JSON: true, Template: "x"},
		{ID: "fan", Kind: KindFanOut, From: "list", As: KindSession, Agent: "cerberus", Template: "{item.task}"},
	}})
	h.models.fn = func(_ context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: `["one"]`, PromptTokens: 1, CompletionTokens: 1}, nil
	}
	r = h.run(g.ID, "")
	sid = <-h.sessions.started
	eventually(t, "fan meter", func() bool { return h.sessions.metered(sid) })
	h.sessions.use(sid, ModelReply{PromptTokens: 7, CompletionTokens: 3, CostUSD: 0.01})
	close(h.sessions.release)
	done = h.wait(r.ID)
	if done.State != StatePassed || done.Tokens != 12 {
		t.Fatalf("fan-out session usage: %s %q tokens=%d", done.State, done.Reason, done.Tokens)
	}
	if it := h.stepRec(r.ID, "fan").Items[0]; it.PromptTokens != 7 || it.Calls != 1 {
		t.Fatalf("item usage %+v", it)
	}
}
