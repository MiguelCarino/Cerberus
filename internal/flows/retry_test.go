package flows

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errTransient is how LM Studio answers when llama.cpp died under the
// request (the 2026-11-03 failure).
var errTransient = &ModelError{Status: 400, Message: "terminated"}

// autoAdvance moves the fake clock past every retry backoff as soon as it
// is armed, so retries happen without waiting.
func autoAdvance(t *testing.T, h *harness) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			case d := <-h.clock.made:
				if d == retryBackoff {
					h.clock.Advance(d)
				}
			}
		}
	}()
}

func itemByTitle(t *testing.T, items []ItemRecord, title string) ItemRecord {
	t.Helper()
	for _, it := range items {
		if it.Title == title {
			return it
		}
	}
	t.Fatalf("no item %q in %+v", title, items)
	return ItemRecord{}
}

func fanFlow(h *harness) Flow {
	return h.create(Flow{Name: "Fan", Limits: Limits{MaxParallel: 3}, Steps: []Step{
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}"},
	}})
}

func TestTransientClassification(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errTransient, true},
		{&ModelError{Status: 502, Message: "Channel Error"}, true},
		{&ModelError{Status: 400, Message: "messages: required"}, false},
		{&ModelError{Status: 429, Message: "over the limit"}, false},
		{&limitError{"used 10 tokens, over the limit of 5"}, false},
		{context.Canceled, false},
		{errBoom, false},
	} {
		if got := transientModelErr(c.err); got != c.want {
			t.Errorf("transientModelErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestFanOutTransientRetrySucceeds(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var bCalls atomic.Int32
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		task := req.Messages[len(req.Messages)-1].Content
		switch {
		case req.Step == "p":
			return ModelReply{Text: `["a","b","c"]`}, nil
		case task == "b" && bCalls.Add(1) == 1:
			return ModelReply{}, errTransient
		}
		return ModelReply{Text: "done " + task, PromptTokens: 3, CompletionTokens: 2}, nil
	}
	r := h.wait(h.run(fanFlow(h).ID, "").ID)
	if r.State != StatePassed {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "f")
	b := itemByTitle(t, rec.Items, "b")
	if b.State != StepPassed || b.Retries != 1 || !strings.Contains(b.RetryError, "terminated") || b.Output != "done b" {
		t.Fatalf("item b %+v", b)
	}
	if a := itemByTitle(t, rec.Items, "a"); a.Retries != 0 || a.RetryError != "" {
		t.Fatalf("item a %+v", a)
	}
	if rec.Retries != 1 || bCalls.Load() != 2 {
		t.Fatalf("step retries %d, b called %d times", rec.Retries, bCalls.Load())
	}
	g, _ := h.e.Graph(r.ID)
	for _, n := range g.Nodes {
		if n.ID == "it:f:1" && !strings.HasPrefix(n.Detail, "retried once") {
			t.Fatalf("retried item node %+v", n)
		}
	}
}

func TestFanOutTransientTwiceKeepsSiblings(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var bCalls atomic.Int32
	bDone := make(chan struct{})
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		task := req.Messages[len(req.Messages)-1].Content
		switch task {
		case "x":
			return ModelReply{Text: `["a","b","c"]`}, nil
		case "b":
			if bCalls.Add(1) == 2 {
				defer close(bDone)
			}
			return ModelReply{}, &ModelError{Status: 502, Message: "Channel Error"}
		}
		// The siblings are still running when b fails for good.
		select {
		case <-bDone:
		case <-ctx.Done():
			return ModelReply{}, ctx.Err()
		}
		return ModelReply{Text: "done " + task}, nil
	}
	r := h.wait(h.run(fanFlow(h).ID, "").ID)
	if r.State != StateFailed || !strings.Contains(r.Reason, "1 of 3 items failed") || !strings.Contains(r.Reason, "Channel Error") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "f")
	if !strings.Contains(rec.Error, "1 of 3 items failed") {
		t.Fatalf("step error %q", rec.Error)
	}
	for _, title := range []string{"a", "c"} {
		if it := itemByTitle(t, rec.Items, title); it.State != StepPassed || it.Output != "done "+title {
			t.Fatalf("sibling %s %+v", title, it)
		}
	}
	b := itemByTitle(t, rec.Items, "b")
	if b.State != StepFailed || b.Retries != 1 || bCalls.Load() != 2 || !strings.Contains(b.Error, "Channel Error") {
		t.Fatalf("item b %+v (calls %d)", b, bCalls.Load())
	}
}

func TestFanOutNonTransientFailsFast(t *testing.T) {
	h := newHarness(t)
	var bCalls atomic.Int32
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		task := req.Messages[len(req.Messages)-1].Content
		switch task {
		case "x":
			return ModelReply{Text: `["a","b","c"]`}, nil
		case "b":
			bCalls.Add(1)
			return ModelReply{}, &ModelError{Status: 400, Message: "messages: invalid"}
		}
		<-ctx.Done()
		return ModelReply{}, ctx.Err()
	}
	r := h.wait(h.run(fanFlow(h).ID, "").ID)
	if r.State != StateFailed || !strings.Contains(r.Reason, "1 of 3 items failed") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
	rec := h.stepRec(r.ID, "f")
	for _, title := range []string{"a", "c"} {
		if it := itemByTitle(t, rec.Items, title); it.State != StepCanceled {
			t.Fatalf("sibling %s not canceled: %+v", title, it)
		}
	}
	if b := itemByTitle(t, rec.Items, "b"); b.State != StepFailed || b.Retries != 0 || bCalls.Load() != 1 {
		t.Fatalf("item b %+v (calls %d)", b, bCalls.Load())
	}
}

func TestPromptAndPlanRetryOnce(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var calls sync.Map // step → *atomic.Int32
	count := func(step string) int32 {
		v, _ := calls.LoadOrStore(step, new(atomic.Int32))
		return v.(*atomic.Int32).Add(1)
	}
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		n := count(req.Step)
		switch req.Step {
		case "a":
			if n == 1 {
				return ModelReply{}, &ModelError{Status: 503, Message: "local engine failed: connection reset"}
			}
			return ModelReply{Text: "hello"}, nil
		case "p":
			if n == 1 {
				return ModelReply{}, errTransient
			}
			return ModelReply{Text: `[{"title":"t","task":"x"}]`}, nil
		}
		return ModelReply{}, errTransient // always
	}
	f := h.create(Flow{Name: "Retry", Steps: []Step{
		{ID: "a", Kind: KindPrompt, Model: "small", Template: "x"},
		{ID: "p", Kind: KindPlan, Model: "small", Template: "x"},
		{ID: "z", Kind: KindPrompt, Model: "small", Template: "x"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if a := h.stepRec(r.ID, "a"); a.State != StepPassed || a.Retries != 1 || !strings.Contains(a.RetryError, "connection reset") || a.Output != "hello" {
		t.Fatalf("step a %+v", a)
	}
	if p := h.stepRec(r.ID, "p"); p.State != StepPassed || p.Retries != 1 {
		t.Fatalf("step p %+v", p)
	}
	// A transient failure that persists fails the step after one retry.
	z := h.stepRec(r.ID, "z")
	if r.State != StateFailed || z.State != StepFailed || z.Retries != 1 || count("z")-1 != 2 {
		t.Fatalf("run %s %q, step z %+v", r.State, r.Reason, z)
	}
}

func TestSessionItemsNotRetried(t *testing.T) {
	h := newHarness(t)
	h.sessions.failErr = errTransient
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		return ModelReply{Text: `["a"]`}, nil
	}
	f := h.create(Flow{Name: "S", Steps: []Step{
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", As: KindSession, Agent: "cerberus", Template: "{item.task}"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if it := h.stepRec(r.ID, "f").Items[0]; r.State != StateFailed || it.Retries != 0 {
		t.Fatalf("%s %q %+v", r.State, r.Reason, it)
	}
}

// hookKey carries the wait hook to the fake model caller in tests.
type hookKey struct{}

func captureHooks(t *testing.T) {
	old := withWaitHook
	withWaitHook = func(ctx context.Context, f func(string)) context.Context {
		return context.WithValue(ctx, hookKey{}, f)
	}
	t.Cleanup(func() { withWaitHook = old })
}

func hookOf(ctx context.Context) func(string) {
	f, _ := ctx.Value(hookKey{}).(func(string))
	if f == nil {
		return func(string) {}
	}
	return f
}

func nodeByID(g Graph, id string) Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return Node{}
}

func (f *fakeEvents) count(run string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.evs {
		if e.Run == run {
			n++
		}
	}
	return n
}

func TestStepWaitHook(t *testing.T) {
	captureHooks(t)
	old := waitGap
	waitGap = 0
	t.Cleanup(func() { waitGap = old })
	h := newHarness(t)
	const why = "waiting for qwen/qwen3.8-27b: 1 of 1 slots busy"
	waiting, running, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		hook := hookOf(ctx)
		hook(why)
		waiting <- struct{}{}
		<-running
		hook("")
		running <- struct{}{}
		<-release
		return ModelReply{Text: "ok"}, nil
	}
	f := h.create(Flow{Name: "W", Steps: []Step{{ID: "a", Kind: KindPrompt, Model: "small", Template: "x"}}})
	r := h.run(f.ID, "")
	<-waiting
	before := h.events.count(r.ID)
	if got := h.detail(r.ID).Run.Waiting; got != why {
		t.Fatalf("run waiting %q", got)
	}
	g, _ := h.e.Graph(r.ID)
	if n := nodeByID(g, "st:a"); n.State != StepQueued || n.Detail != why {
		t.Fatalf("waiting node %+v", n)
	}
	running <- struct{}{}
	<-running
	if got := h.detail(r.ID).Run.Waiting; got != "" {
		t.Fatalf("still waiting %q", got)
	}
	if h.events.count(r.ID) <= before {
		t.Fatal("the cleared wait was not published")
	}
	g, _ = h.e.Graph(r.ID)
	if n := nodeByID(g, "st:a"); n.State != StepRunning || n.Detail != "" {
		t.Fatalf("proceeding node %+v", n)
	}
	close(release)
	if r = h.wait(r.ID); r.State != StatePassed || r.Waiting != "" {
		t.Fatalf("%s %q waiting %q", r.State, r.Reason, r.Waiting)
	}
}

func TestFanOutWaitHookAndThrottle(t *testing.T) {
	captureHooks(t)
	old := waitGap
	waitGap = time.Hour // only the first change is sent at once
	t.Cleanup(func() { waitGap = old })
	h := newHarness(t)
	const why = "waiting for room: gemma busy"
	waiting, release := make(chan struct{}), make(chan struct{})
	var flipEvents int
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if req.Step == "p" {
			return ModelReply{Text: `["a"]`}, nil
		}
		hook := hookOf(ctx)
		before := h.events.count(req.Run)
		for i := 0; i < 50; i++ { // an engine that flip-flops
			hook(why)
			hook("")
		}
		hook(why)
		flipEvents = h.events.count(req.Run) - before
		waiting <- struct{}{}
		<-release
		hook("")
		return ModelReply{Text: "ok"}, nil
	}
	r := h.run(fanFlow(h).ID, "")
	<-waiting
	d := h.detail(r.ID)
	if len(d.Live) != 1 || d.Live[0].Waiting != why || d.Run.Waiting != "item 1: "+why {
		t.Fatalf("live %+v run waiting %q", d.Live, d.Run.Waiting)
	}
	g, _ := h.e.Graph(r.ID)
	if n := nodeByID(g, "it:f:0"); n.State != StepQueued || n.Detail != why {
		t.Fatalf("item node %+v", n)
	}
	if n := nodeByID(g, "st:f"); n.State != StepQueued || n.Detail != "item 1: "+why {
		t.Fatalf("fan-out node %+v", n)
	}
	// 101 hook changes: one event at once, the rest held for the gap.
	if flipEvents != 1 {
		t.Fatalf("%d events for the flip-flopping hook, want 1", flipEvents)
	}
	close(release)
	if r = h.wait(r.ID); r.State != StatePassed || r.Waiting != "" {
		t.Fatalf("%s %q waiting %q", r.State, r.Reason, r.Waiting)
	}
	if it := h.stepRec(r.ID, "f").Items[0]; it.Waiting != "" {
		t.Fatalf("a finished item keeps waiting: %+v", it)
	}
}

// A limit hit by one item after another item already failed for good with
// a transient error still ends the run as limited, with the limit's reason.
func TestFanOutLimitAfterTransientKeepsLimit(t *testing.T) {
	h := newHarness(t)
	autoAdvance(t, h)
	var bCalls atomic.Int32
	bDone := make(chan struct{})
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		task := req.Messages[len(req.Messages)-1].Content
		switch task {
		case "x":
			return ModelReply{Text: `["a","b","c"]`}, nil
		case "b":
			if bCalls.Add(1) == 2 {
				defer close(bDone)
			}
			return ModelReply{}, &ModelError{Status: 502, Message: "Channel Error"}
		case "c":
			select {
			case <-bDone:
			case <-ctx.Done():
				return ModelReply{}, ctx.Err()
			}
			return ModelReply{Text: "big", PromptTokens: 5000}, nil
		}
		select {
		case <-ctx.Done():
			return ModelReply{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		return ModelReply{Text: "done " + task}, nil
	}
	f := h.create(Flow{Name: "Fan", Limits: Limits{MaxParallel: 3, MaxTokens: 1000}, Steps: []Step{
		{ID: "p", Kind: KindPrompt, JSON: true, Model: "big", Template: "x"},
		{ID: "f", Kind: KindFanOut, From: "p", DefaultModel: "small", Template: "{item.task}"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateLimited || !strings.Contains(r.Reason, "tokens") {
		t.Fatalf("%s %q", r.State, r.Reason)
	}
}
