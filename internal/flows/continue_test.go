package flows

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// A run canceled inside a session step continues in that session; the
// steps before it are not run again; a passed run cannot be continued.
func TestContinueResumesSession(t *testing.T) {
	h := newHarness(t)
	h.sessions.block = true
	f := h.create(Flow{Name: "K", Steps: []Step{
		{ID: "a", Kind: KindPrompt, Model: "small", Template: "plan {input}"},
		{ID: "w", Kind: KindSession, Agent: "cerberus", Task: "do {steps.a.output}"},
		{ID: "o", Kind: KindOutput, To: ToInbox, Template: "{steps.w.output}"},
	}})
	r := h.run(f.ID, "x")
	<-h.sessions.started
	if _, err := h.e.Cancel("owner:test", r.ID); err != nil {
		t.Fatal(err)
	}
	if c := h.wait(r.ID); c.State != StateCanceled || c.PC != 1 {
		t.Fatalf("canceled: %s pc=%d", c.State, c.PC)
	}
	h.sessions.mu.Lock()
	h.sessions.block = false
	h.sessions.mu.Unlock()
	c, err := h.e.Continue("owner:test", r.ID)
	if err != nil || c.Continued != 1 || c.Resume != "s-1" {
		t.Fatalf("continue: %+v %v", c, err)
	}
	done := h.wait(r.ID)
	if done.State != StatePassed || done.Reason != "" {
		t.Fatalf("after continue: %s %q", done.State, done.Reason)
	}
	h.sessions.mu.Lock()
	resumed, started := append([]string(nil), h.sessions.resumed...), h.sessions.n
	h.sessions.mu.Unlock()
	if !slices.Equal(resumed, []string{"s-1"}) || started != 1 {
		t.Fatalf("resumed %v, started %d", resumed, started)
	}
	if n := len(h.models.Calls()); n != 1 {
		t.Fatalf("step a ran %d times", n)
	}
	// both attempts of w keep a record: canceled, then passed in s-1
	var ws []string
	for _, rec := range h.detail(r.ID).Steps {
		if rec.ID == "w" && rec.Session == "s-1" {
			ws = append(ws, rec.State)
		}
	}
	if !slices.Equal(ws, []string{StepCanceled, StepPassed}) {
		t.Fatalf("records of w %v", ws)
	}
	if g, _ := h.e.Graph(r.ID); g.Nodes[2].State != StepPassed {
		t.Fatalf("graph %+v", g.Nodes[2])
	}
	if _, err := h.e.Continue("owner:test", r.ID); !errors.Is(err, ErrState) {
		t.Fatalf("continue a passed run: %v", err)
	}
	if !h.auditor.has("owner:test flow.run.continue") {
		t.Fatal("continue not audited")
	}
}

// A run that failed at a prompt step runs that step again, and a run
// limited by tokens gets a fresh allowance.
func TestContinueFailedAndLimited(t *testing.T) {
	h := newHarness(t)
	fail := true
	h.models.fn = func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		if fail {
			return ModelReply{}, errBoom
		}
		return ModelReply{Text: "fine", PromptTokens: 60, CompletionTokens: 0}, nil
	}
	f := h.create(Flow{Name: "F", Limits: Limits{MaxTokens: 100}, Steps: []Step{
		{ID: "a", Kind: KindPrompt, Model: "small", Template: "a"},
		{ID: "b", Kind: KindPrompt, Model: "small", Template: "b"},
	}})
	r := h.wait(h.run(f.ID, "").ID)
	if r.State != StateFailed || r.PC != 0 {
		t.Fatalf("first: %s pc=%d %q", r.State, r.PC, r.Reason)
	}
	fail = false
	if _, err := h.e.Continue("owner:test", r.ID); err != nil {
		t.Fatal(err)
	}
	r = h.wait(r.ID)
	if r.State != StateLimited || r.PC != 1 {
		t.Fatalf("second: %s pc=%d %q", r.State, r.PC, r.Reason)
	}
	if _, err := h.e.Continue("owner:test", r.ID); err != nil {
		t.Fatal(err)
	}
	r = h.wait(r.ID)
	if r.State != StatePassed || r.Tokens != 180 || r.Continued != 2 {
		t.Fatalf("third: %s tokens=%d continued=%d %q", r.State, r.Tokens, r.Continued, r.Reason)
	}
}
