package flows

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// schedEntry is the scheduler's memory of one scheduled flow, kept in
// DATA/flows/schedule.json so a restart knows what was missed.
type schedEntry struct {
	Schedule   string    `json:"schedule"`
	Last       time.Time `json:"last"` // when it last fired (or was armed)
	SkipTime   time.Time `json:"skip_time,omitzero"`
	SkipReason string    `json:"skip_reason,omitempty"`
	next       time.Time // the next due time (computed)
}

func (e *Engine) loadSchedule() map[string]*schedEntry {
	out := map[string]*schedEntry{}
	b, err := os.ReadFile(e.schedPath)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		e.log.Warn("flows: schedule state not read", "err", err)
		return map[string]*schedEntry{}
	}
	for id, en := range out {
		if en == nil || !ValidID(id) {
			delete(out, id)
		}
	}
	return out
}

func (e *Engine) saveScheduleLocked() {
	b, err := json.MarshalIndent(e.sched, "", "  ")
	if err == nil {
		err = platform.WritePrivate(e.schedPath, append(b, '\n'))
	}
	if err != nil {
		e.log.Warn("flows: schedule state not saved", "err", err)
	}
}

// reschedule wakes the scheduler to recompute (flows changed).
func (e *Engine) reschedule() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// dueFlow is a flow to fire now.
type dueFlow struct {
	flow Flow
	note string
}

// scheduleLoop sleeps until the next due time (at most maxSleep, so a
// changed wall clock or a suspend is noticed), fires what is due, and
// wakes early when flows change. It never polls tightly.
func (e *Engine) scheduleLoop() {
	defer e.wg.Done()
	for {
		now := e.clock.Now()
		due, next := e.plan(now)
		for _, d := range due {
			e.fire(d)
		}
		wait := maxSleep
		if !next.IsZero() {
			if d := next.Sub(e.clock.Now()); d < wait {
				wait = max(d, time.Second)
			}
		}
		t := e.clock.NewTimer(wait)
		select {
		case <-e.base.Done():
			t.Stop()
			return
		case <-e.kick:
		case <-t.C():
		}
		t.Stop()
	}
}

// plan brings the schedule up to date at now: it answers the flows due
// (each at most once, however many times were missed) and the earliest
// next due time (zero when none).
func (e *Engine) plan(now time.Time) ([]dueFlow, time.Time) {
	flows := e.store.List()
	e.mu.Lock()
	defer e.mu.Unlock()
	var due []dueFlow
	var earliest time.Time
	dirty := false
	keep := map[string]bool{}
	for _, f := range flows {
		if !f.Enabled || f.Trigger.Kind != TriggerSchedule {
			continue
		}
		sc, err := ParseSchedule(f.Trigger.Schedule)
		if err != nil {
			continue
		}
		keep[f.ID] = true
		en := e.sched[f.ID]
		switch {
		case en == nil || en.Schedule != f.Trigger.Schedule:
			skip := en
			en = &schedEntry{Schedule: f.Trigger.Schedule, Last: now.UTC(), next: sc.Next(now, e.loc)}
			if skip != nil {
				en.SkipTime, en.SkipReason = skip.SkipTime, skip.SkipReason
			}
			e.sched[f.ID] = en
			dirty = true
		case en.next.IsZero(): // loaded from disk: what was missed since Last
			en.next = sc.Next(en.Last, e.loc)
		}
		if !en.next.IsZero() && !en.next.After(now) {
			missed := sc.Missed(en.Last, now, e.loc, 1000)
			note := ""
			late := now.Sub(en.next)
			switch {
			case missed > 1:
				note = fmt.Sprintf("%d scheduled runs were missed while the daemon was down or asleep; this one run stands in for them", missed)
			case late > 2*time.Minute:
				note = fmt.Sprintf("ran late: it was due at %s (the daemon was down or asleep)", en.next.In(e.loc).Format("2006-01-02 15:04"))
			}
			if !e.disabled {
				due = append(due, dueFlow{flow: f, note: note})
			}
			en.Last, en.next = now.UTC(), sc.Next(now, e.loc)
			dirty = true
		}
		if !en.next.IsZero() && (earliest.IsZero() || en.next.Before(earliest)) {
			earliest = en.next
		}
	}
	for id := range e.sched {
		if !keep[id] {
			delete(e.sched, id)
			dirty = true
		}
	}
	if dirty {
		e.saveScheduleLocked()
	}
	return due, earliest
}

// fire starts a scheduled run, or records why it was skipped: the guard
// refuses new sessions, or the previous run of the flow is still going.
func (e *Engine) fire(d dueFlow) {
	f := d.flow
	if e.o.OwnerActive != nil && !e.o.OwnerActive(f.Owner) {
		e.skip(f.ID, "the flow's owner is disabled or removed")
		return
	}
	if e.o.Guard != nil {
		if ok, why := e.o.Guard.CanStart(); !ok {
			e.skip(f.ID, "the session guard refuses new sessions: "+why)
			return
		}
	}
	e.mu.Lock()
	going := false
	for _, r := range e.index {
		if r.Flow == f.ID && !r.Test && Going(r.State) {
			going = true
			break
		}
	}
	e.mu.Unlock()
	if going {
		e.skip(f.ID, "the previous run is still going")
		return
	}
	if _, err := e.start(ActorSystem, f, f.InputHint, RunSchedule, d.note); err != nil {
		e.skip(f.ID, "not started: "+err.Error())
		return
	}
	e.mu.Lock()
	if en := e.sched[f.ID]; en != nil && en.SkipReason != "" {
		en.SkipTime, en.SkipReason = time.Time{}, ""
		e.saveScheduleLocked()
	}
	e.mu.Unlock()
}

func (e *Engine) skip(id, reason string) {
	e.log.Info("flows: scheduled run skipped", "flow", id, "reason", reason)
	e.audit(ActorSystem, "flow.run.skip", reason, id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if en := e.sched[id]; en != nil {
		en.SkipTime, en.SkipReason = e.clock.Now().UTC(), reason
		e.saveScheduleLocked()
	}
}
