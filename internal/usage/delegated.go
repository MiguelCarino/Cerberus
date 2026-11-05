package usage

import (
	"sort"
	"time"
)

// ClientDelegate is the Client of gateway requests made for cerberus_ask:
// work an agent handed to a local model.
const ClientDelegate = "cerberus-delegate"

// DelegatedRow sums the delegated requests of one session (or model).
type DelegatedRow struct {
	Key              string  `json:"key"` // session id or model id
	Requests         int     `json:"requests"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	EnergyJ          float64 `json:"energy_j"`
	Seconds          float64 `json:"seconds"`
	SavedUSD         float64 `json:"saved_usd"` // at the reference price; 0 without one
}

// Delegated is the delegated work since a time: totals, per session and per
// model. price gives what the reference cloud model charges per million
// prompt and completion tokens (ok false: no saving is shown). session, when
// set, keeps only that session's requests.
type Delegated struct {
	Total     DelegatedRow   `json:"total"`
	BySession []DelegatedRow `json:"by_session"`
	ByModel   []DelegatedRow `json:"by_model"`
	Reference string         `json:"reference,omitempty"`
}

// Delegated sums the requests made through cerberus_ask since a time.
func (s *Store) Delegated(since time.Time, session string, reference string, inPerM, outPerM float64) Delegated {
	out := Delegated{Total: DelegatedRow{Key: "total"}, Reference: reference}
	bySes, byMod := map[string]*DelegatedRow{}, map[string]*DelegatedRow{}
	row := func(m map[string]*DelegatedRow, k string) *DelegatedRow {
		r := m[k]
		if r == nil {
			r = &DelegatedRow{Key: k}
			m[k] = r
		}
		return r
	}
	for _, g := range s.groups(since) {
		if g.client != ClientDelegate || (session != "" && g.session != session) {
			continue
		}
		saved := (float64(g.PromptTokens)*inPerM + float64(g.CompletionTokens)*outPerM) / 1e6
		for _, r := range []*DelegatedRow{&out.Total, row(bySes, g.session), row(byMod, g.model)} {
			r.Requests += g.Requests
			r.PromptTokens += g.PromptTokens
			r.CompletionTokens += g.CompletionTokens
			r.EnergyJ += g.EnergyJ
			r.Seconds += float64(g.durMs) / 1000
			r.SavedUSD += saved
		}
	}
	flat := func(m map[string]*DelegatedRow) []DelegatedRow {
		rows := make([]DelegatedRow, 0, len(m))
		for _, r := range m {
			rows = append(rows, *r)
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Requests != rows[j].Requests {
				return rows[i].Requests > rows[j].Requests
			}
			return rows[i].Key < rows[j].Key
		})
		return rows
	}
	out.BySession, out.ByModel = flat(bySes), flat(byMod)
	return out
}
