package gateway

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/notify"
)

// spender is optionally implemented by the recorder (*usage.Store does);
// without it budgets cannot be measured and are not enforced.
type spender interface {
	Spend(provider string, since time.Time) float64
}

// BudgetStatus is a configured budget with the spend of its current period.
type BudgetStatus struct {
	config.Budget
	Spent float64 `json:"spent"`
	State string  `json:"state"` // "ok" | "soft" | "hard"
}

// SetPricing replaces the price table and budgets (both copied). Safe while
// serving.
func (g *Gateway) SetPricing(prices map[string]config.Price, budgets []config.Budget) {
	g.priceMu.Lock()
	defer g.priceMu.Unlock()
	g.prices = maps.Clone(prices)
	g.budgets = slices.Clone(budgets)
}

// providerOf names who bills a model: the ID prefix ("anthropic/…") for a
// cloud model, else the backend.
func providerOf(id string, local bool, backend string) string {
	if !local {
		if p, _, ok := strings.Cut(id, "/"); ok && p != "" {
			return p
		}
	}
	return backend
}

// price finds the price of a model: its own entry, else "<provider>/*".
func (g *Gateway) price(model, provider string) (config.Price, bool) {
	g.priceMu.Lock()
	defer g.priceMu.Unlock()
	if p, ok := g.prices[model]; ok {
		return p, true
	}
	p, ok := g.prices[provider+"/*"]
	return p, ok
}

// costUSD prices a request. Cached prompt tokens use the CachedIn rate, or
// the In rate when the table has no cached price.
func costUSD(p config.Price, prompt, cached, completion int) float64 {
	cached = min(max(cached, 0), max(prompt, 0))
	cin := p.CachedIn
	if cin == 0 {
		cin = p.In
	}
	return (float64(prompt-cached)*p.In + float64(cached)*cin + float64(completion)*p.Out) / 1e6
}

// periodStart is the local start of the budget period containing now.
func periodStart(period string, now time.Time) (time.Time, bool) {
	y, m, d := now.Date()
	switch period {
	case "day":
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), true
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, now.Location()), true
	}
	return time.Time{}, false
}

func budgetMatches(b config.Budget, provider string) bool {
	return b.Provider == provider || b.Provider == "*"
}

// usd formats dollars with cents, or with enough significant digits that a
// sub-cent amount (a test cap, a cheap model) does not print as $0.00.
func usd(v float64) string {
	if v == 0 || v >= 0.01 || v <= -0.01 {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 2, 64), 64)
	return strconv.FormatFloat(r, 'f', -1, 64)
}

func budgetLabel(b config.Budget) string {
	if b.Provider == "*" {
		return "total"
	}
	return b.Provider
}

func budgetState(b config.Budget, spent float64) string {
	switch {
	case b.HardUSD > 0 && spent >= b.HardUSD:
		return "hard"
	case b.SoftUSD > 0 && spent >= b.SoftUSD:
		return "soft"
	}
	return "ok"
}

// Budgets reports every configured budget with its current spend.
func (g *Gateway) Budgets() []BudgetStatus {
	g.priceMu.Lock()
	budgets := slices.Clone(g.budgets)
	g.priceMu.Unlock()
	sp, _ := g.store.(spender)
	now := g.now()
	out := make([]BudgetStatus, 0, len(budgets))
	for _, b := range budgets {
		st := BudgetStatus{Budget: b, State: "ok"}
		if from, ok := periodStart(b.Period, now); ok && sp != nil {
			st.Spent = sp.Spend(b.Provider, from)
			st.State = budgetState(b, st.Spent)
		}
		out = append(out, st)
	}
	return out
}

// checkBudgets measures every budget matching provider, announces soft caps
// crossed in this period (once each) and returns the first budget at its
// hard cap, if any.
func (g *Gateway) checkBudgets(provider string) (hit config.Budget, spent float64, over bool) {
	sp, ok := g.store.(spender)
	if !ok || provider == "" {
		return
	}
	g.priceMu.Lock()
	var budgets []config.Budget
	for _, b := range g.budgets {
		if budgetMatches(b, provider) {
			budgets = append(budgets, b)
		}
	}
	g.priceMu.Unlock()

	now := g.now()
	var msgs []notify.Message
	for _, b := range budgets {
		from, ok := periodStart(b.Period, now)
		if !ok {
			continue
		}
		s := sp.Spend(b.Provider, from)
		if b.SoftUSD > 0 && s >= b.SoftUSD && g.markNotified(b, from) {
			body := fmt.Sprintf("%s spend this %s is $%s, past the soft cap of $%s.", budgetLabel(b), b.Period, usd(s), usd(b.SoftUSD))
			if b.HardUSD > 0 {
				body += fmt.Sprintf(" Requests stop at $%s.", usd(b.HardUSD))
			}
			msgs = append(msgs, notify.Message{Title: "Cerberus: " + budgetLabel(b) + " budget", Body: body, Urgency: "normal", Kind: "budget", Link: "#/usage"})
		}
		if !over && b.HardUSD > 0 && s >= b.HardUSD {
			hit, spent, over = b, s, true
		}
	}
	if g.notifier != nil {
		for _, m := range msgs {
			g.notifier(m)
		}
	}
	return
}

// markNotified records that b's soft cap was announced for the period
// starting at from; it reports false when it already was.
func (g *Gateway) markNotified(b config.Budget, from time.Time) bool {
	key := b.Provider + "|" + b.Period
	g.priceMu.Lock()
	defer g.priceMu.Unlock()
	if prev, ok := g.notified[key]; ok && prev.Equal(from) {
		return false
	}
	g.notified[key] = from
	return true
}

// refuseOverBudget answers 429 budget_exceeded when c's provider is at a
// hard cap, before anything is sent upstream.
func (g *Gateway) refuseOverBudget(w http.ResponseWriter, c *call) bool {
	b, _, over := g.checkBudgets(c.rec.Provider)
	if !over {
		return false
	}
	g.fail(w, c, http.StatusTooManyRequests, "budget_exceeded",
		fmt.Sprintf("%s %s budget of $%s reached", budgetLabel(b), b.Period, usd(b.HardUSD)))
	return true
}

// priceCall fills c's CostUSD for a cloud request from the price table
// (local models and unpriced models stay at 0).
func (g *Gateway) priceCall(c *call) {
	if c.rec.Local || c.rec.Backend == "" {
		return
	}
	if p, ok := g.price(c.rec.Model, c.rec.Provider); ok {
		c.rec.CostUSD = costUSD(p, c.rec.PromptTokens, c.rec.CachedTokens, c.rec.CompletionTokens)
	}
}
