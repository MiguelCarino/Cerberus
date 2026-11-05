package usage

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Leaderboard periods, groupings and metrics (GET /api/leaderboard).
var (
	Periods = []string{"today", "7d", "30d", "month"}
	ByKeys  = []string{"session", "agent", "model", "repo"}
	Metrics = []string{"cost", "tokens", "energy", "time"}
)

// Errors for a bad leaderboard query (the API answers 400).
var (
	ErrPeriod = errors.New("period must be today, 7d, 30d or month")
	ErrBy     = errors.New("by must be session, agent, model or repo")
	ErrMetric = errors.New("metric must be cost, tokens, energy or time")
)

// Row labels for gateway traffic that no session made, and sessions with
// no repo or a forgotten agent.
const (
	LabelDirect  = "(direct API)"
	LabelNoRepo  = "(no repo)"
	LabelUnknown = "(unknown)"
)

// SessionInfo is what the leaderboard needs to know about one session. It
// comes from the session manager; the usage package does not import it.
type SessionInfo struct {
	ID, Label, Agent, Repo string
	// Start and End bound the session's life (End = now while it runs).
	Start, End time.Time
	// WallJ is the energy charged to the session over its whole life, at
	// the wall (CPU share of its cgroup plus its requests' GPU energy).
	WallJ float64
}

// Sources feeds Leaderboard. Only Usage is required.
type Sources struct {
	Usage    *Store
	Sessions []SessionInfo
	// WallJ turns DC joules (the gateway's per-request GPU energy) into wall
	// joules (PSU losses); nil leaves them as they are.
	WallJ func(dcJ float64) float64
	// PricePerKWh is the marginal electricity price per wall kWh in
	// Currency (MXN by default); 0 leaves energy unpriced.
	PricePerKWh float64
	Currency    string
	Now         time.Time // zero: time.Now()
	Limit       int       // rows kept after sorting; <= 0 keeps all
	// Keep, when set, limits the board to the records and sessions whose
	// session id it accepts ("" is direct traffic, "chat-…" a chat): a
	// caller's own work only (Phase 4a owner scoping).
	Keep func(session string) bool
}

// Row is one ranked entry. Value is the ranked metric (USD, tokens, kWh or
// seconds).
type Row struct {
	Key              string  `json:"key"`
	Label            string  `json:"label"`
	Agent            string  `json:"agent,omitempty"`
	Repo             string  `json:"repo,omitempty"`
	Requests         int     `json:"requests"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Tokens           int     `json:"tokens"`
	CostUSD          float64 `json:"cost_usd"`
	EnergyKWh        float64 `json:"energy_kwh"`
	EnergyCost       float64 `json:"energy_cost"` // EnergyKWh at the marginal price, in Board.Currency
	WallSec          float64 `json:"wall_sec"`
	Sessions         int     `json:"sessions,omitempty"`
	Value            float64 `json:"value"`
}

// Board is a ranked leaderboard. CostCurrency is "USD": Cerberus has no
// exchange-rate source, so API cost is never converted. Total sums every
// row before Limit.
type Board struct {
	Period       string    `json:"period"`
	By           string    `json:"by"`
	Metric       string    `json:"metric"`
	Since        time.Time `json:"since"`
	Until        time.Time `json:"until"`
	CostCurrency string    `json:"cost_currency"`
	Currency     string    `json:"currency"` // energy cost currency
	PricePerKWh  float64   `json:"price_per_kwh"`
	Rows         []Row     `json:"rows"`
	Total        Row       `json:"total"`
}

// PeriodStart is when period begins, in now's location: local midnight for
// today, the 1st at midnight for month, 7 or 30 days back otherwise.
func PeriodStart(period string, now time.Time) (time.Time, error) {
	switch period {
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	case "7d":
		return now.Add(-7 * 24 * time.Hour), nil
	case "30d":
		return now.Add(-30 * 24 * time.Hour), nil
	case "month":
		y, m, _ := now.Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, now.Location()), nil
	}
	return time.Time{}, ErrPeriod
}

// group is the usage of one session (or none), model and client.
type group struct {
	session, model, client string
	acc
}

// groups sums the records at or after since by session, model and client:
// exact from the ring when it reaches back, else from the quarter-hour
// totals (like Aggregate).
func (s *Store) groups(since time.Time) []group {
	type gk struct{ session, model, client string }
	m := map[gk]*acc{}
	get := func(k gk) *acc {
		a := m[k]
		if a == nil {
			a = &acc{}
			m[k] = a
		}
		return a
	}
	s.mu.Lock()
	if s.ringCovers(since) {
		for _, r := range s.ring {
			if !r.Time.Before(since) {
				get(gk{r.SessionID, r.Model, r.Client}).add(r)
			}
		}
	} else {
		from := since.Truncate(bucketSpan)
		for k, b := range s.buckets {
			if !k.hour.Before(from) {
				get(gk{k.session, k.model, k.client}).merge(b)
			}
		}
	}
	s.mu.Unlock()
	out := make([]group, 0, len(m))
	for k, a := range m {
		out = append(out, group{session: k.session, model: k.model, client: k.client, acc: *a})
	}
	return out
}

// overlap is how long [a0,a1] and [b0,b1] share.
func overlap(a0, a1, b0, b1 time.Time) time.Duration {
	lo, hi := a0, a1
	if b0.After(lo) {
		lo = b0
	}
	if b1.Before(hi) {
		hi = b1
	}
	if hi.After(lo) {
		return hi.Sub(lo)
	}
	return 0
}

// Leaderboard ranks sessions, agents, models or repos over a period by API
// cost (USD), tokens, energy (wall kWh) or wall time.
//
// Tokens and cost come from the usage records. Energy of a model is its
// requests' GPU energy; energy of a session is its lifetime attributed
// energy prorated by how much of its life falls in the period (at least its
// requests' GPU energy in the period). Wall time of a model is its summed
// request time; of a session, its life inside the period. Agents and repos
// sum their sessions; gateway traffic no session made is one row labelled
// LabelDirect (by agent, one per client name). Rows whose ranked value is
// zero are left out.
func Leaderboard(src Sources, period, by, metric string) (Board, error) {
	now := src.Now
	if now.IsZero() {
		now = time.Now()
	}
	since, err := PeriodStart(period, now)
	if err != nil {
		return Board{}, err
	}
	if !contains(ByKeys, by) {
		return Board{}, ErrBy
	}
	if !contains(Metrics, metric) {
		return Board{}, ErrMetric
	}
	wall := src.WallJ
	if wall == nil {
		wall = func(j float64) float64 { return j }
	}
	cur := src.Currency
	if cur == "" {
		cur = "MXN"
	}
	b := Board{Period: period, By: by, Metric: metric, Since: since, Until: now, CostCurrency: "USD",
		Currency: cur, PricePerKWh: src.PricePerKWh, Rows: []Row{}}

	var gs []group
	if src.Usage != nil {
		gs = src.Usage.groups(since)
	}
	meta := make(map[string]SessionInfo, len(src.Sessions))
	for _, si := range src.Sessions {
		if src.Keep != nil && !src.Keep(si.ID) {
			continue
		}
		meta[si.ID] = si
	}

	// Per session: usage, GPU wall joules in the period.
	type sessAcc struct {
		acc
		gpuWallJ float64
	}
	perSess := map[string]*sessAcc{}
	rows := map[string]*Row{}
	row := func(key, label string) *Row {
		r := rows[key]
		if r == nil {
			r = &Row{Key: key, Label: label}
			rows[key] = r
		}
		return r
	}
	addUsage := func(r *Row, a *acc) {
		r.Requests += a.Requests
		r.PromptTokens += a.PromptTokens
		r.CompletionTokens += a.CompletionTokens
		r.CostUSD += a.CostUSD
	}

	for i := range gs {
		g := &gs[i]
		if src.Keep != nil && !src.Keep(g.session) {
			continue
		}
		if _, known := meta[g.session]; !known && strings.HasPrefix(g.session, "chat-") {
			// A chat labels its requests with its chat id (for the Flow
			// graph); it is not a session, so it ranks like direct traffic.
			g.session = ""
		}
		if by == "model" {
			r := row(g.model, g.model)
			addUsage(r, &g.acc)
			r.EnergyKWh += wall(g.EnergyJ) / 3.6e6
			r.WallSec += float64(g.durMs) / 1000
			continue
		}
		if g.session == "" {
			key, label := "", LabelDirect
			if by == "agent" && g.client != "" {
				key, label = "client:"+g.client, g.client
			}
			r := row(key, label)
			addUsage(r, &g.acc)
			r.EnergyKWh += wall(g.EnergyJ) / 3.6e6
			r.WallSec += float64(g.durMs) / 1000
			continue
		}
		sa := perSess[g.session]
		if sa == nil {
			sa = &sessAcc{}
			perSess[g.session] = sa
		}
		sa.merge(&g.acc)
		sa.gpuWallJ += wall(g.EnergyJ)
	}

	if by != "model" {
		// Every session that lived in the period or made requests in it.
		ids := map[string]bool{}
		for id := range perSess {
			ids[id] = true
		}
		for id, si := range meta {
			end := si.End
			if end.IsZero() || end.After(now) {
				end = now
			}
			if overlap(si.Start, end, since, now) > 0 {
				ids[id] = true
			}
		}
		for id := range ids {
			si, known := meta[id]
			sa := perSess[id]
			if sa == nil {
				sa = &sessAcc{}
			}
			var life, in time.Duration
			if known {
				end := si.End
				if end.IsZero() || end.After(now) {
					end = now
				}
				life, in = end.Sub(si.Start), overlap(si.Start, end, since, now)
			}
			energyJ := sa.gpuWallJ
			if known && si.WallJ > 0 {
				share := 1.0
				if life > 0 {
					share = float64(in) / float64(life)
				}
				energyJ = max(energyJ, si.WallJ*share)
			}

			var r *Row
			switch by {
			case "session":
				label := si.Label
				if label == "" {
					label = id
				}
				r = row(id, label)
				r.Agent, r.Repo = si.Agent, si.Repo
			case "agent":
				key, label := si.Agent, si.Agent
				if key == "" {
					key, label = "?", LabelUnknown
				}
				r = row(key, label)
			case "repo":
				key, label := si.Repo, si.Repo
				if key == "" {
					key, label = "-", LabelNoRepo
				}
				r = row(key, label)
			}
			addUsage(r, &sa.acc)
			r.EnergyKWh += energyJ / 3.6e6
			r.WallSec += in.Seconds()
			r.Sessions++
		}
	}

	for _, r := range rows {
		r.Tokens = r.PromptTokens + r.CompletionTokens
		r.EnergyCost = r.EnergyKWh * src.PricePerKWh
		r.Value = value(r, metric)
		addRow(&b.Total, r)
		if r.Value > 0 {
			b.Rows = append(b.Rows, *r)
		}
	}
	b.Total.Key, b.Total.Label = "total", "Total"
	b.Total.Value = value(&b.Total, metric)
	sort.Slice(b.Rows, func(i, j int) bool {
		if b.Rows[i].Value != b.Rows[j].Value {
			return b.Rows[i].Value > b.Rows[j].Value
		}
		return b.Rows[i].Label < b.Rows[j].Label
	})
	if src.Limit > 0 && len(b.Rows) > src.Limit {
		b.Rows = b.Rows[:src.Limit]
	}
	return b, nil
}

func value(r *Row, metric string) float64 {
	switch metric {
	case "cost":
		return r.CostUSD
	case "tokens":
		return float64(r.Tokens)
	case "energy":
		return r.EnergyKWh
	}
	return r.WallSec
}

func addRow(t *Row, r *Row) {
	t.Requests += r.Requests
	t.PromptTokens += r.PromptTokens
	t.CompletionTokens += r.CompletionTokens
	t.Tokens += r.Tokens
	t.CostUSD += r.CostUSD
	t.EnergyKWh += r.EnergyKWh
	t.EnergyCost += r.EnergyCost
	t.WallSec += r.WallSec
	t.Sessions += r.Sessions
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
