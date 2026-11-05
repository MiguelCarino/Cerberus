package flows

import (
	"strings"
	"testing"
	"time"
)

// A flow's owner rides on its runs, events, model calls, session steps and
// notices, survives updates, and a disabled owner's schedule is skipped.
func TestFlowOwner(t *testing.T) {
	h := newHarness(t)
	f := scheduled("Owned", "every 30m",
		Step{ID: "p", Kind: KindPrompt, Model: "big", Template: "hi {input}"},
		Step{ID: "s", Kind: KindSession, Agent: "cerberus", Task: "do it"},
		Step{ID: "o", Kind: KindOutput, To: ToInbox, Template: "{input}"})
	f.Owner = "u-bob"
	out := h.create(f)
	if out.Owner != "u-bob" {
		t.Fatalf("created %+v", out)
	}
	// An update keeps the owner even when the body names another.
	upd := out
	upd.Owner = "u-mallory"
	upd.Description = "changed"
	got, _, err := h.e.Update("owner:test", upd)
	if err != nil || got.Owner != "u-bob" {
		t.Fatalf("update %+v %v", got, err)
	}
	r := h.wait(h.run(out.ID, "x").ID)
	if r.Owner != "u-bob" {
		t.Fatalf("run %+v", r)
	}
	for _, c := range h.models.Calls() {
		if c.Owner != "u-bob" {
			t.Fatalf("model call %+v", c)
		}
	}
	h.sessions.mu.Lock()
	reqs := append([]SessionRequest(nil), h.sessions.reqs...)
	h.sessions.mu.Unlock()
	if len(reqs) == 0 || reqs[0].Owner != "u-bob" {
		t.Fatalf("session requests %+v", reqs)
	}
	h.events.mu.Lock()
	for _, ev := range h.events.evs {
		if ev.Run == r.ID && ev.Owner != "u-bob" {
			t.Fatalf("event %+v", ev)
		}
	}
	h.events.mu.Unlock()

	// A disabled owner's scheduled runs are skipped with a note.
	h2 := newHarness(t)
	_ = h2.e.Close()
	h2.dir = t.TempDir()
	e, err := Open(Options{Dir: h2.dir, Models: h2.models, Notifier: h2.notifier, Auditor: h2.auditor,
		Events: h2.events, Guard: h2.guard, Catalog: fakeCatalog{}, Clock: h2.clock, Location: time.UTC,
		OwnerActive: func(o string) bool { return o != "u-gone" }})
	if err != nil {
		t.Fatal(err)
	}
	h2.e = e
	t.Cleanup(func() { _ = e.Close() })
	g := scheduled("Gone", "every 30m")
	g.Owner = "u-gone"
	gf := h2.create(g)
	e.Start()
	h2.clock.settle(t)
	h2.clock.Advance(time.Minute)
	eventually(t, "the skip", func() bool {
		for _, s := range e.List() {
			if s.ID == gf.ID && s.LastSkip != nil && strings.Contains(s.LastSkip.Reason, "owner is disabled") {
				return true
			}
		}
		return false
	})
	if len(h2.runsOf(gf.ID)) != 0 {
		t.Fatal("a disabled owner's flow ran")
	}
}
