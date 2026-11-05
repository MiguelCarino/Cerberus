package flows

import (
	"strings"
	"testing"
	"time"
)

func scheduled(name, sched string, steps ...Step) Flow {
	if len(steps) == 0 {
		steps = []Step{{ID: "a", Kind: KindOutput, To: ToInbox, Template: "{input}"}}
	}
	return Flow{Name: name, Enabled: true, InputHint: "morning", Trigger: Trigger{Kind: TriggerSchedule, Schedule: sched}, Steps: steps}
}

func (h *harness) runsOf(id string) []Run { return h.e.Runs(id, 0) }

func TestSchedulerFiresAtDueTime(t *testing.T) {
	h := newHarness(t) // clock: 2026-11-02 07:59 UTC
	f := h.create(scheduled("Daily", "daily 08:00"))
	manual := h.create(Flow{Name: "Manual", Enabled: true, Steps: f.Steps})
	h.e.Start()
	if d := h.clock.settle(t); d != time.Minute {
		t.Fatalf("first sleep %v, want 1m (until 08:00)", d)
	}
	if l := h.e.List(); l[0].Next == nil || !l[0].Next.Equal(time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)) || l[0].TriggerText != "Daily at 08:00" {
		t.Fatalf("list %+v", l[0])
	}
	h.clock.Advance(time.Minute)
	eventually(t, "the scheduled run", func() bool { return len(h.runsOf(f.ID)) == 1 })
	r := h.wait(h.runsOf(f.ID)[0].ID)
	if r.Trigger != RunSchedule || r.Input != "morning" || r.Actor != ActorSystem || r.State != StatePassed || r.Note != "" {
		t.Fatalf("scheduled run %+v", r)
	}
	if len(h.runsOf(manual.ID)) != 0 {
		t.Fatal("a manual flow was scheduled")
	}
	// The next due time is a day away: the scheduler sleeps the longest
	// step (maxSleep), never polling.
	eventually(t, "the long sleep", func() bool {
		select {
		case d := <-h.clock.made:
			return d == maxSleep
		default:
			return false
		}
	})
	l := h.e.List()
	for _, s := range l {
		if s.ID == f.ID && (s.Next == nil || !s.Next.Equal(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)) || s.LastRun == nil || s.LastRun.ID != r.ID) {
			t.Fatalf("after the run: %+v", s)
		}
	}
	// 15 minutes pass with nothing due: no run.
	h.clock.Advance(maxSleep)
	h.clock.settle(t)
	if n := len(h.runsOf(f.ID)); n != 1 {
		t.Fatalf("%d runs after an idle wake", n)
	}
}

func TestSchedulerSkips(t *testing.T) {
	h := newHarness(t)
	h.guard.why = "memory is low"
	f := h.create(scheduled("Guarded", "every 30m"))
	h.e.Start()
	h.clock.settle(t)
	h.clock.Advance(time.Minute) // 08:00
	eventually(t, "the skip", func() bool {
		l := h.e.List()
		return l[0].LastSkip != nil && strings.Contains(l[0].LastSkip.Reason, "memory is low")
	})
	if len(h.runsOf(f.ID)) != 0 || !h.auditor.has("system flow.run.skip") {
		t.Fatal("guard skip started a run or was not audited")
	}

	// The previous run still going (waiting for approval) skips the next.
	h.guard.mu.Lock()
	h.guard.why = ""
	h.guard.mu.Unlock()
	g := h.create(scheduled("Gate", "every 30m", Step{ID: "ok", Kind: KindApprove}))
	h.clock.settle(t)
	h.clock.Advance(30 * time.Minute) // 08:30
	eventually(t, "the gated run", func() bool { return len(h.runsOf(g.ID)) == 1 })
	h.wait(h.runsOf(g.ID)[0].ID)
	eventually(t, "the guarded flow's run", func() bool { return len(h.runsOf(f.ID)) == 1 })
	h.clock.settle(t)
	h.clock.Advance(30 * time.Minute) // 09:00
	eventually(t, "the going skip", func() bool {
		for _, s := range h.e.List() {
			if s.ID == g.ID && s.LastSkip != nil && s.LastSkip.Reason == "the previous run is still going" {
				return true
			}
		}
		return false
	})
	if n := len(h.runsOf(g.ID)); n != 1 {
		t.Fatalf("gated flow has %d runs", n)
	}
	// A successful start clears the last skip.
	for _, s := range h.e.List() {
		if s.ID == f.ID && s.LastSkip != nil {
			t.Fatalf("skip not cleared: %+v", s.LastSkip)
		}
	}
}

func TestSchedulerMissedRunsOnce(t *testing.T) {
	h := newHarness(t)
	f := h.create(scheduled("Daily", "daily 08:00"))
	h.e.Start()
	h.clock.settle(t) // armed: schedule.json remembers 07:59
	_ = h.e.Close()

	h.clock.Advance(3*24*time.Hour + 2*time.Hour) // 2026-11-05 09:59: four 08:00s missed
	h.open()
	h.e.Start()
	eventually(t, "the catch-up run", func() bool { return len(h.runsOf(f.ID)) == 1 })
	r := h.wait(h.runsOf(f.ID)[0].ID)
	if !strings.Contains(r.Note, "4 scheduled runs were missed") {
		t.Fatalf("note %q", r.Note)
	}
	h.clock.settle(t)
	time.Sleep(20 * time.Millisecond)
	if n := len(h.runsOf(f.ID)); n != 1 {
		t.Fatalf("%d catch-up runs, want 1", n)
	}
}

func TestSchedulerDisabledAndReenabled(t *testing.T) {
	h := newHarness(t)
	f := h.create(scheduled("Hourly", "every 1h"))
	h.e.Start()
	h.clock.settle(t)
	if _, err := h.e.SetFlowEnabled("owner:test", f.ID, false); err != nil {
		t.Fatal(err)
	}
	h.clock.settle(t)
	h.clock.Advance(3 * time.Hour)
	h.clock.settle(t)
	if len(h.runsOf(f.ID)) != 0 {
		t.Fatal("a disabled flow ran")
	}
	// Re-enabled: armed from now, the missed hours are not caught up.
	if _, err := h.e.SetFlowEnabled("owner:test", f.ID, true); err != nil {
		t.Fatal(err)
	}
	h.clock.settle(t)
	time.Sleep(20 * time.Millisecond)
	if len(h.runsOf(f.ID)) != 0 {
		t.Fatal("re-enabling caught up missed runs")
	}
	// flows.enabled off: nothing fires.
	h.e.SetEnabled(false)
	h.clock.settle(t)
	h.clock.Advance(time.Hour)
	h.clock.settle(t)
	time.Sleep(20 * time.Millisecond)
	if len(h.runsOf(f.ID)) != 0 {
		t.Fatal("ran while flows are off")
	}
	h.e.SetEnabled(true)
	h.clock.settle(t)
	h.clock.Advance(time.Hour)
	eventually(t, "the run once on again", func() bool { return len(h.runsOf(f.ID)) == 1 })
}
