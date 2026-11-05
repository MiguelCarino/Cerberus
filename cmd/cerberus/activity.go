package main

import (
	"context"
	"sync"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/subs"
)

// planActivity keeps subscription tracking in its active mode while either
// the Usage view is watched (the API's SetActive, which expires after 10
// minutes) or a session of a subscription's agent is live. Without it, the
// API's expiry would turn tracking idle in the middle of a Claude session.
type planActivity struct {
	t api.PlanTracker

	mu      sync.Mutex
	watched bool
	live    bool
}

func newPlanActivity(t api.PlanTracker) *planActivity { return &planActivity{t: t} }

// Snapshot passes through to the tracker.
func (a *planActivity) Snapshot() []subs.PlanUsage { return a.t.Snapshot() }

// SetActive records whether the Usage view is being watched.
func (a *planActivity) SetActive(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.watched = on
	a.t.SetActive(a.watched || a.live)
}

func (a *planActivity) setLive(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live == on {
		return
	}
	a.live = on
	a.t.SetActive(a.watched || a.live)
}

// sessionFeed is what followSessions needs of the session manager.
type sessionFeed interface {
	Subscribe() (<-chan session.Event, func())
	List() []session.Info
}

// followSessions updates the live flag on every session event until ctx
// ends. A subscriber the manager drops for being slow resubscribes.
func (a *planActivity) followSessions(ctx context.Context, src sessionFeed, agents map[string]bool) {
	if len(agents) == 0 {
		return
	}
	for ctx.Err() == nil {
		ch, cancel := src.Subscribe()
		a.setLive(anyLive(src.List(), agents))
		func() {
			defer cancel()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
					a.setLive(anyLive(src.List(), agents))
				}
			}
		}()
	}
}

// liveStates are the states in which an agent may be spending a plan.
var liveStates = map[string]bool{
	session.StateStarting: true, session.StateRunning: true, session.StateWaiting: true,
	session.StateChecking: true, session.StateRetrying: true, session.StateEscalated: true,
}

func anyLive(infos []session.Info, agents map[string]bool) bool {
	for _, in := range infos {
		if liveStates[in.State] && agents[in.Agent] {
			return true
		}
	}
	return false
}

// subscriptionAgents is the set of agent profiles that consume a plan.
func subscriptionAgents(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, sc := range cfg.Subscriptions {
		for _, a := range sc.Agents {
			out[a] = true
		}
	}
	return out
}
