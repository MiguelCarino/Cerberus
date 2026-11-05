package finetune

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Rating states for ListOptions.Rated.
const (
	RatedGood      = "good"      // rated good, no better answer
	RatedBad       = "bad"       // rated bad, no better answer
	RatedCorrected = "corrected" // a better answer was written
	RatedUnrated   = "unrated"   // nobody rated it
)

// ListClip is how much of each message and of the answer List returns.
const ListClip = 4 << 10

// ErrNotFound is returned for an example id the store does not hold.
var ErrNotFound = errors.New("no such example")

// ErrBadCursor is returned for a Before that names no example.
var ErrBadCursor = errors.New("before must be the id of a listed example")

// Item is one example as a person reviews it: the example, its current
// verdict (nil: unrated) and whether it is excluded from exports.
type Item struct {
	Example
	Rating    *Rating `json:"rating"`
	Excluded  bool    `json:"excluded"`
	Truncated bool    `json:"truncated,omitempty"` // a message or the answer was clipped (List only)
}

// ListOptions filters and pages List.
type ListOptions struct {
	Kind   string // "" or KindDelegated / KindCloud
	Rated  string // "" or RatedGood / RatedBad / RatedCorrected / RatedUnrated
	Q      string // case-insensitive text in the prompt, answer, better answer, note or model
	Limit  int    // <= 0: 20; at most 200
	Before string // an example id: only examples listed after it (the previous page's Next)
}

// Page is one page of List, newest first.
type Page struct {
	Examples []Item `json:"examples"`
	Total    int    `json:"total"` // examples matching the filters, over all pages
	Next     string `json:"next"`  // pass as Before for the next page; "" on the last
}

// ValidRated reports whether v is a Rated filter value ("" included).
func ValidRated(v string) bool {
	switch v {
	case "", RatedGood, RatedBad, RatedCorrected, RatedUnrated:
		return true
	}
	return false
}

// ValidKind reports whether v is a Kind filter value ("" included).
func ValidKind(v string) bool { return v == "" || v == KindDelegated || v == KindCloud }

// ratedState is the Rated filter value a verdict falls under.
func ratedState(r *Rating) string {
	switch {
	case r == nil:
		return RatedUnrated
	case r.Better != "":
		return RatedCorrected
	case r.Good:
		return RatedGood
	}
	return RatedBad
}

// items joins examples with their verdicts, newest first (ties by id,
// newest id first).
func (s *Store) items() ([]Item, error) {
	exs, rates, excl, _, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(exs))
	for _, e := range exs {
		it := Item{Example: e, Excluded: excl[e.ID]}
		if r, ok := rates[e.ID]; ok {
			it.Rating = &r
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return newer(out[i].Example, out[j].Example) })
	return out, nil
}

func newer(a, b Example) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return a.ID > b.ID
}

// List returns one page of examples, newest first, with every message and
// the answer clipped to ListClip bytes (Truncated says when).
func (s *Store) List(o ListOptions) (Page, error) {
	if !ValidKind(o.Kind) {
		return Page{}, fmt.Errorf("kind must be %s or %s", KindDelegated, KindCloud)
	}
	if !ValidRated(o.Rated) {
		return Page{}, errors.New("rated must be good, bad, corrected or unrated")
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 200)
	all, err := s.items()
	if err != nil {
		return Page{}, err
	}
	var cursor *Example
	if o.Before != "" {
		for i := range all {
			if all[i].ID == o.Before {
				cursor = &all[i].Example
				break
			}
		}
		if cursor == nil {
			return Page{}, ErrBadCursor
		}
	}
	q := strings.ToLower(strings.TrimSpace(o.Q))
	page := Page{Examples: []Item{}}
	for _, it := range all {
		if (o.Kind != "" && it.Kind != o.Kind) || (o.Rated != "" && ratedState(it.Rating) != o.Rated) ||
			(q != "" && !matches(it, q)) {
			continue
		}
		page.Total++
		if cursor != nil && !newer(*cursor, it.Example) {
			continue
		}
		if len(page.Examples) < limit {
			page.Examples = append(page.Examples, clipItem(it))
		} else if page.Next == "" {
			page.Next = page.Examples[len(page.Examples)-1].ID
		}
	}
	return page, nil
}

// matches reports whether the lowercased query q is in the item's text.
func matches(it Item, q string) bool {
	has := func(t string) bool { return t != "" && strings.Contains(strings.ToLower(t), q) }
	if has(it.Answer) || has(it.Model) || has(it.Session) {
		return true
	}
	for _, m := range it.Messages {
		if has(m.Content) {
			return true
		}
	}
	return it.Rating != nil && (has(it.Rating.Better) || has(it.Rating.Note))
}

// clipItem copies it with long texts clipped.
func clipItem(it Item) Item {
	it.Messages = append([]Message(nil), it.Messages...)
	for i := range it.Messages {
		var cut bool
		it.Messages[i].Content, cut = clip(it.Messages[i].Content, ListClip)
		it.Truncated = it.Truncated || cut
	}
	var cut bool
	it.Answer, cut = clip(it.Answer, ListClip)
	it.Truncated = it.Truncated || cut
	if it.Rating != nil {
		r := *it.Rating
		r.Better, cut = clip(r.Better, ListClip)
		it.Truncated = it.Truncated || cut
		it.Rating = &r
	}
	return it
}

// clip shortens t to at most n bytes on a rune boundary.
func clip(t string, n int) (string, bool) {
	if len(t) <= n {
		return t, false
	}
	for n > 0 && !utf8.RuneStart(t[n]) {
		n--
	}
	return t[:n], true
}

// Get returns one example in full.
func (s *Store) Get(id string) (Item, error) {
	all, err := s.items()
	if err != nil {
		return Item{}, err
	}
	for _, it := range all {
		if it.ID == id {
			return it, nil
		}
	}
	return Item{}, ErrNotFound
}

// RateOwner records a person's verdict on example id (which must exist):
// a rating with By "owner". Like any verdict, the newest one wins.
func (s *Store) RateOwner(id string, good bool, better, note string) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	return s.Rate(Rating{ID: id, Good: good, Better: better, Note: note, By: ByOwner})
}

// MaxAgentRatings is how many verdicts the agent that asked may give one
// example. The newest verdict wins, so a few are plenty (a second thought,
// a correction); the cap keeps a looping agent from growing ratings.jsonl
// without bound, since each line may hold a long better answer.
const MaxAgentRatings = 3

// ErrNotYours is returned when an agent rates an example its session did
// not ask for.
var ErrNotYours = errors.New("that example is not this session's")

// ErrRatedEnough is returned when an agent has already given an example
// MaxAgentRatings verdicts.
var ErrRatedEnough = errors.New("that example has enough verdicts from its agent")

// RateAgent records the verdict of the agent of session on example id. The
// example must exist and have been asked for by that session, and the
// agent may rate it at most MaxAgentRatings times. The check and the append
// happen under one lock, so concurrent calls cannot pass the cap.
func (s *Store) RateAgent(session, id string, good bool, better, note string) error {
	raw, err := s.ratingLine(Rating{ID: id, Good: good, Better: better, Note: note, By: ByAgent})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cacheOf(s)
	all, err := c.allBriefs(s.dir)
	if err != nil {
		return err
	}
	owner, found := "", false
	for _, b := range all {
		if b.ID == id {
			owner, found = b.Session, true
			break
		}
	}
	if !found {
		return ErrNotFound
	}
	if session == "" || owner != session {
		return ErrNotYours
	}
	if _, err := c.ratings(filepath.Join(s.dir, ratingsFile), all); err != nil {
		return err
	}
	if c.agentN[id] >= MaxAgentRatings {
		return ErrRatedEnough
	}
	return s.appendLocked(ratingsFile, raw)
}

// Exclude keeps example id (which must exist) out of every export, or lets
// it back in. The example stays on disk and its verdict is unchanged.
func (s *Store) Exclude(id string, exclude bool) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	return s.Rate(Rating{ID: id, By: ByOwner, Exclude: &exclude})
}
