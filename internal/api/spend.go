package api

import (
	"net/http"
	"sync"
	"time"

	"carino.systems/cerberus/internal/gateway"
	"carino.systems/cerberus/internal/subs"
)

// watchFor is how long one POST /api/usage/watch keeps subscription
// tracking in its active (frequent) mode.
const watchFor = 10 * time.Minute

// watchState holds the timer that turns tracking back to idle. Each watch
// call pushes the expiry out; nothing runs between calls.
type watchState struct {
	mu    sync.Mutex
	timer *time.Timer
	until time.Time
}

// Spend is the "spend" object of GET /api/usage: USD per provider.
type Spend struct {
	Today map[string]float64 `json:"today"`
	Month map[string]float64 `json:"month"`
}

func (s *server) spendSummary(now time.Time) Spend {
	sp := Spend{Today: map[string]float64{}, Month: map[string]float64{}}
	if s.spend == nil {
		return sp
	}
	y, m, d := now.Date()
	if v := s.spend.SpendByProvider(time.Date(y, m, d, 0, 0, 0, 0, now.Location())); v != nil {
		sp.Today = v
	}
	if v := s.spend.SpendByProvider(time.Date(y, m, 1, 0, 0, 0, 0, now.Location())); v != nil {
		sp.Month = v
	}
	return sp
}

func (s *server) budgetList() []gateway.BudgetStatus {
	if s.budgets == nil {
		return []gateway.BudgetStatus{}
	}
	return nonNil(s.budgets.Budgets())
}

func (s *server) planList() []subs.PlanUsage {
	if s.plans == nil {
		return []subs.PlanUsage{}
	}
	return nonNil(s.plans.Snapshot())
}

// handleUsageWatch marks subscription tracking active for watchFor. The Usage
// view calls it when it opens (and again while it stays open).
func (s *server) handleUsageWatch(w http.ResponseWriter, r *http.Request) {
	if s.plans == nil {
		unavailable(w, "subscription tracking")
		return
	}
	until := s.watchUsage(watchFor)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "until": until})
}

// watchUsage turns tracking active now and idle after d, unless watched again.
func (s *server) watchUsage(d time.Duration) time.Time {
	s.plans.SetActive(true)
	ws := &s.watch
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.until = time.Now().Add(d)
	if ws.timer != nil {
		ws.timer.Stop()
	}
	plans := s.plans
	var self *time.Timer
	self = time.AfterFunc(d, func() {
		ws.mu.Lock()
		current := ws.timer == self
		if current {
			ws.timer = nil
		}
		ws.mu.Unlock()
		if current {
			plans.SetActive(false)
		}
	})
	ws.timer = self
	return ws.until
}
