// Package usage records one entry per gateway request in an append-only JSONL
// file and keeps the most recent entries in memory for the UI, plus
// quarter-hour per-model totals (tokens, energy and API cost) so aggregates
// and spend cover a time window, not a record count. The totals reach back
// 62 days, enough for "this month" in any month, and are rebuilt from the
// file on Open.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// tailBytes is how much of an existing file Open always reads back. A
// larger file is read from where its records enter the bucketKeep window
// (found by binary search), so budgets stay exact after a restart however
// busy the month was, without reading records too old to count.
var tailBytes int64 = 32 << 20

const (
	defaultKeep = 1000
	// bucketSpan is a quarter hour: every real UTC offset (+5:30, +5:45,
	// -3:30) is a multiple of it, so a bucket never straddles local midnight
	// and day or month totals never include the previous period.
	bucketSpan = 15 * time.Minute
	bucketKeep = 62 * 24 * time.Hour // totals older than this are dropped
)

// Record is one request's usage. TokPerSec and TokPerJ are filled by the
// caller; zero means unknown. Provider is the model's provider name for
// cloud models (the ID prefix) or the backend for local ones; CostUSD is the
// API price of the request (always 0 for local models). CachedTokens is the
// part of PromptTokens the upstream served from its prompt cache.
type Record struct {
	Time             time.Time
	Model            string
	Backend          string
	Client           string
	Local            bool
	PromptTokens     int
	CompletionTokens int
	TTFTMs           int64
	DurationMs       int64
	TokPerSec        float64
	EnergyJ          float64
	TokPerJ          float64
	Status           int
	Error            string
	SessionID        string
	Provider         string
	CostUSD          float64
	CachedTokens     int
	User             string `json:",omitempty"` // the user id charged (Phase 4a; "" = from before users)
	// Finish is how the reply ended: the upstream finish_reason (stop,
	// length, tool_calls, ...) or "length_in_thinking" when the budget ran
	// out inside the model's thinking (gateway phase G4).
	Finish string `json:",omitempty"`
	// API is the client API: chat, completions, embeddings, messages or
	// responses (gateway phase G1; "" = from before it).
	API string `json:",omitempty"`
	// ReasoningTokens are the completion tokens spent thinking, when the
	// upstream reports them.
	ReasoningTokens int `json:",omitempty"`
	// GuardFlagged and GuardHeld count the agent guard's flagged tool
	// results and held tool calls in this request (gateway phase G2).
	GuardFlagged int `json:",omitempty"`
	GuardHeld    int `json:",omitempty"`
}

// Agg summarises the records of one model.
type Agg struct {
	Model            string
	Requests         int
	PromptTokens     int
	CompletionTokens int
	EnergyJ          float64
	CostUSD          float64
	AvgTokPerSec     float64 // over records with TokPerSec > 0
	AvgTokPerJ       float64 // over records with TokPerJ > 0
}

// Store appends records to a JSONL file and keeps a ring of the last ones.
type Store struct {
	mu      sync.Mutex
	f       *os.File
	ring    []Record // capacity keep; ordered by insertion starting at head once full
	head    int
	keep    int
	buckets map[bucketKey]*acc // per-bucketSpan, per-model, per-provider totals of every record
	// truncatedLoad: Open skipped the start of the file (records older than
	// bucketKeep), so a ring that is not full may still miss older records.
	truncatedLoad bool
	now           func() time.Time
	onAdd         []func(Record) // called after Add, outside the lock
}

type bucketKey struct {
	hour     time.Time
	model    string
	provider string
	session  string // day phase 6: per-session totals for the leaderboard
	client   string
	user     string // Phase 4a: per-user totals for budgets
}

// acc sums records; the averages are computed from it at the end.
type acc struct {
	Agg
	tps, tpj   float64
	ntps, ntpj int
	durMs      int64 // summed request wall time (leaderboard)
}

func (a *acc) add(r Record) {
	a.Requests++
	a.PromptTokens += r.PromptTokens
	a.CompletionTokens += r.CompletionTokens
	a.EnergyJ += r.EnergyJ
	a.CostUSD += r.CostUSD
	a.durMs += max(r.DurationMs, 0)
	if r.TokPerSec > 0 {
		a.tps += r.TokPerSec
		a.ntps++
	}
	if r.TokPerJ > 0 {
		a.tpj += r.TokPerJ
		a.ntpj++
	}
}

func (a *acc) merge(b *acc) {
	a.Requests += b.Requests
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.EnergyJ += b.EnergyJ
	a.CostUSD += b.CostUSD
	a.durMs += b.durMs
	a.tps, a.ntps = a.tps+b.tps, a.ntps+b.ntps
	a.tpj, a.ntpj = a.tpj+b.tpj, a.ntpj+b.ntpj
}

// Open loads the tail of path (if it exists) into memory and opens it for
// appending. keep <= 0 uses a default of 1000.
func Open(path string, keep int) (*Store, error) {
	if keep <= 0 {
		keep = defaultKeep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{keep: keep, ring: make([]Record, 0, min(keep, 1024)), buckets: map[bucketKey]*acc{}, now: time.Now}
	if err := s.loadTail(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

// loadTail reads path back from where its records enter the bucketKeep
// window (at least its last tailBytes); a missing file is fine and malformed
// lines are skipped.
func (s *Store) loadTail(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	off := int64(0)
	if size := st.Size(); size > tailBytes {
		off = min(windowStart(f, size, s.now().Add(-bucketKeep)), size-tailBytes)
	}
	s.truncatedLoad = off > 0
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	if off > 0 {
		// Drop the partial first line.
		if _, err := r.ReadSlice('\n'); err != nil && err != bufio.ErrBufferFull {
			return nil
		}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if json.Unmarshal(line, &rec) == nil {
			s.push(rec)
		}
	}
	s.prune()
	return nil
}

// maxLine bounds one JSONL record when reading the file back.
const maxLine = 1 << 20

// windowStart finds, by binary search over byte offsets, an offset at or a
// little before the first record at or after cut. Records are appended in
// time order, so one parsed line per probe is enough; a probe that finds no
// readable record counts as "too new", which only makes the read longer.
func windowStart(f *os.File, size int64, cut time.Time) int64 {
	const grain = 64 << 10
	lo, hi := int64(0), size
	for hi-lo > grain {
		mid := lo + (hi-lo)/2
		if t, ok := recordTimeAfter(f, mid, size); ok && t.Before(cut) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

// recordTimeAfter returns the Time of the first whole record after off.
func recordTimeAfter(f *os.File, off, size int64) (time.Time, bool) {
	r := bufio.NewReaderSize(io.NewSectionReader(f, off, size-off), 16<<10)
	if off > 0 {
		if _, err := r.ReadSlice('\n'); err != nil && err != bufio.ErrBufferFull {
			return time.Time{}, false
		}
	}
	for range 8 { // skip a few malformed lines
		line, err := r.ReadSlice('\n')
		if err != nil && err != io.EOF {
			return time.Time{}, false
		}
		var rec struct{ Time time.Time }
		if l := bytes.TrimSpace(line); len(l) > 0 && json.Unmarshal(l, &rec) == nil {
			return rec.Time, true
		}
		if err == io.EOF {
			break
		}
	}
	return time.Time{}, false
}

// Add appends r to the file and the ring. Write errors are not reported:
// usage tracking must never fail a request.
func (s *Store) Add(r Record) {
	line, err := json.Marshal(r)
	s.mu.Lock()
	if err == nil && s.f != nil {
		_, _ = s.f.Write(append(line, '\n'))
	}
	s.push(r)
	if r.Time.Sub(r.Time.Truncate(bucketSpan)) < time.Minute {
		s.prune() // roughly once per bucket of traffic
	}
	hooks := s.onAdd
	s.mu.Unlock()
	for _, fn := range hooks {
		fn(r)
	}
}

// OnAdd registers fn to be called with every new record (energy attribution).
func (s *Store) OnAdd(fn func(Record)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onAdd = append(s.onAdd, fn)
}

// push adds r to the ring and to its bucket.
func (s *Store) push(r Record) {
	if s.now().Sub(r.Time) <= bucketKeep {
		k := bucketKey{hour: r.Time.Truncate(bucketSpan), model: r.Model, provider: r.Provider,
			session: r.SessionID, client: r.Client, user: r.User}
		b := s.buckets[k]
		if b == nil {
			b = &acc{}
			s.buckets[k] = b
		}
		b.add(r)
	}
	s.pushRing(r)
}

// prune drops totals older than bucketKeep.
func (s *Store) prune() {
	cut := s.now().Add(-bucketKeep)
	for k := range s.buckets {
		if k.hour.Add(bucketSpan).Before(cut) {
			delete(s.buckets, k)
		}
	}
}

func (s *Store) pushRing(r Record) {
	if len(s.ring) < s.keep {
		s.ring = append(s.ring, r)
		return
	}
	s.ring[s.head] = r
	s.head = (s.head + 1) % s.keep
}

// Recent returns up to n records, newest first. n <= 0 returns all kept.
func (s *Store) Recent(n int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := len(s.ring)
	if n <= 0 || n > l {
		n = l
	}
	out := make([]Record, n)
	for i := range n {
		// The newest record sits just before head (or at the end before the ring fills).
		out[i] = s.ring[(s.head-1-i+2*l)%l]
	}
	return out
}

// Aggregate summarises the records at or after since, by model, busiest
// first. When the ring of recent records reaches back to since the result is
// exact; otherwise it comes from the quarter-hour totals, which include the
// whole quarter hour since falls in (and reach back 62 days at most). Period
// starts in local time (midnight, the 1st) fall on a bucket boundary in
// every time zone, so day and month figures are exact either way.
func (s *Store) Aggregate(since time.Time) []Agg {
	byModel := map[string]*acc{}
	get := func(model string) *acc {
		a := byModel[model]
		if a == nil {
			a = &acc{Agg: Agg{Model: model}}
			byModel[model] = a
		}
		return a
	}
	s.mu.Lock()
	if s.ringCovers(since) {
		for _, r := range s.ring {
			if !r.Time.Before(since) {
				get(r.Model).add(r)
			}
		}
	} else {
		from := since.Truncate(bucketSpan)
		for k, b := range s.buckets {
			if !k.hour.Before(from) {
				get(k.model).merge(b)
			}
		}
	}
	s.mu.Unlock()

	out := make([]Agg, 0, len(byModel))
	for _, a := range byModel {
		if a.ntps > 0 {
			a.AvgTokPerSec = a.tps / float64(a.ntps)
		}
		if a.ntpj > 0 {
			a.AvgTokPerJ = a.tpj / float64(a.ntpj)
		}
		out = append(out, a.Agg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Spend is the API cost in USD of provider's requests at or after since.
// Provider "*" or "" sums every provider. Like Aggregate, it is exact while
// the ring of recent records reaches back to since and otherwise comes from
// the quarter-hour totals.
func (s *Store) Spend(provider string, since time.Time) float64 {
	all := provider == "" || provider == "*"
	var sum float64
	s.eachCost(since, func(p string, usd float64) {
		if all || p == provider {
			sum += usd
		}
	})
	return sum
}

// SpendByProvider is Spend for every provider with a non-zero cost since
// since (local models cost nothing, so they do not appear).
func (s *Store) SpendByProvider(since time.Time) map[string]float64 {
	out := map[string]float64{}
	s.eachCost(since, func(p string, usd float64) {
		if usd != 0 {
			out[p] += usd
		}
	})
	return out
}

// eachCost calls fn with the provider and cost of every record (or
// bucket) at or after since, under the lock.
func (s *Store) eachCost(since time.Time, fn func(provider string, usd float64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ringCovers(since) {
		for _, r := range s.ring {
			if !r.Time.Before(since) {
				fn(r.Provider, r.CostUSD)
			}
		}
		return
	}
	from := since.Truncate(bucketSpan)
	for k, b := range s.buckets {
		if !k.hour.Before(from) {
			fn(k.provider, b.CostUSD)
		}
	}
}

// UserSum is one user's spend and tokens over a period.
type UserSum struct {
	CostUSD float64
	Tokens  int64 // prompt + completion
}

// ByUser sums cost and tokens per Record.User at or after since (exact
// while the ring reaches back, else from the quarter-hour totals, like
// Spend).
func (s *Store) ByUser(since time.Time) map[string]UserSum {
	out := map[string]UserSum{}
	s.mu.Lock()
	defer s.mu.Unlock()
	add := func(user string, usd float64, tok int64) {
		u := out[user]
		u.CostUSD += usd
		u.Tokens += tok
		out[user] = u
	}
	if s.ringCovers(since) {
		for _, r := range s.ring {
			if !r.Time.Before(since) {
				add(r.User, r.CostUSD, int64(r.PromptTokens+r.CompletionTokens))
			}
		}
		return out
	}
	from := since.Truncate(bucketSpan)
	for k, b := range s.buckets {
		if !k.hour.Before(from) {
			add(k.user, b.CostUSD, int64(b.PromptTokens+b.CompletionTokens))
		}
	}
	return out
}

// ringCovers reports whether the ring holds every record since since: it
// has never dropped one, or its oldest record is older than since.
func (s *Store) ringCovers(since time.Time) bool {
	if len(s.ring) < s.keep {
		return !s.truncatedLoad
	}
	return s.ring[s.head].Time.Before(since)
}

// Close closes the file. Later Adds still update memory.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
