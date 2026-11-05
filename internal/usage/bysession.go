package usage

import (
	"slices"
	"sort"
	"time"
)

// SessionModel sums the requests one session (or chat) made to one model
// from one client: the Flow graph's ask edges (Client ClientDelegate) and
// answer edges (any other client).
type SessionModel struct {
	Session          string
	Model            string
	Client           string
	Provider         string // the provider (cloud) or backend (local) of the model
	Requests         int
	PromptTokens     int
	CompletionTokens int
	Last             time.Time // the newest request (its quarter hour's end, capped at now, when it comes from the totals)
}

// BySession sums every request the store still knows of (the ring of recent
// records when it holds them all, else the quarter-hour totals, which reach
// back 62 days) for the given session ids, per session, model, client and
// provider. Records without a session id never match. The result is sorted
// by session, model and client.
func (s *Store) BySession(ids []string) []SessionModel {
	want := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	type key struct{ session, model, client, provider string }
	m := map[key]*SessionModel{}
	add := func(k key, req, prompt, completion int, last time.Time) {
		row := m[k]
		if row == nil {
			row = &SessionModel{Session: k.session, Model: k.model, Client: k.client, Provider: k.provider}
			m[k] = row
		}
		row.Requests += req
		row.PromptTokens += prompt
		row.CompletionTokens += completion
		if last.After(row.Last) {
			row.Last = last
		}
	}
	s.mu.Lock()
	now := s.now()
	if s.ringCovers(time.Time{}) {
		for _, r := range s.ring {
			if want[r.SessionID] {
				add(key{r.SessionID, r.Model, r.Client, r.Provider}, 1, r.PromptTokens, r.CompletionTokens, r.Time)
			}
		}
	} else {
		for k, b := range s.buckets {
			if want[k.session] {
				// The bucket's end, but never later than now: the current
				// quarter hour's end is still in the future, and Flow shows
				// Last as the graph's "updated" time.
				last := k.hour.Add(bucketSpan)
				if last.After(now) {
					last = now
				}
				add(key{k.session, k.model, k.client, k.provider}, b.Requests, b.PromptTokens, b.CompletionTokens, last)
			}
		}
	}
	s.mu.Unlock()
	out := make([]SessionModel, 0, len(m))
	for _, row := range m {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return slices.Compare([]string{a.Session, a.Model, a.Client, a.Provider},
			[]string{b.Session, b.Model, b.Client, b.Provider}) < 0
	})
	return out
}
