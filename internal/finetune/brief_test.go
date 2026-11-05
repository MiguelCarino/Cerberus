package finetune

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBriefs(t *testing.T) {
	s, err := New(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, r, err := s.Briefs(nil); err != nil || len(b) != 0 || len(r) != 0 {
		t.Fatalf("empty store: %v %v %v", b, r, err)
	}
	msgs := []Message{{Role: "user", Content: "q"}}
	a, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Session: "s1", Messages: msgs, Answer: "x"})
	b, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Session: "s2", Messages: msgs, Answer: "y"})
	c, _ := s.Add(Example{Kind: KindCloud, Model: "big", Session: "s1", Messages: msgs, Answer: "z"})
	_ = s.Rate(Rating{ID: a, Good: false})
	_ = s.Rate(Rating{ID: a, Good: true}) // newest wins
	_ = s.Rate(Rating{ID: b, Good: false, Better: "w"})
	// an exclusion line is not a verdict
	f, err := os.OpenFile(filepath.Join(s.Dir(), "ratings.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":"` + a + `","time":"2026-11-02T00:00:00Z","good":false,"exclude":true}` + "\n")
	f.WriteString("not json\n")
	f.Close()

	briefs, rates, err := s.Briefs([]string{"s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(briefs) != 2 || briefs[0].ID != a || briefs[1].ID != c || briefs[1].Kind != KindCloud || briefs[0].Model != "qwen" {
		t.Fatalf("briefs %+v", briefs)
	}
	if len(rates) != 1 || !rates[a].Good {
		t.Fatalf("rates %+v", rates)
	}
	all, rates, _ := s.Briefs(nil)
	if len(all) != 3 || len(rates) != 2 || rates[b].Better != BetterGiven {
		t.Fatalf("all %+v rates %+v", all, rates)
	}
	// the cache sees appends: a new example and a newer verdict
	d, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Session: "s1", Messages: msgs, Answer: "v"})
	_ = s.Rate(Rating{ID: a, Good: false})
	briefs, rates, _ = s.Briefs([]string{"s1"})
	if len(briefs) != 3 || briefs[2].ID != d || rates[a].Good {
		t.Fatalf("after appends: %+v %+v", briefs, rates)
	}
	// and a file removed by hand
	files, _ := filepath.Glob(filepath.Join(s.Dir(), "examples-*.jsonl"))
	for _, f := range files {
		os.Remove(f)
	}
	os.Remove(filepath.Join(s.Dir(), "ratings.jsonl"))
	if briefs, rates, err := s.Briefs(nil); err != nil || len(briefs) != 0 || len(rates) != 0 {
		t.Fatalf("after removal: %+v %+v %v", briefs, rates, err)
	}
}

func TestRateAgent(t *testing.T) {
	s, err := New(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []Message{{Role: "user", Content: "q"}}
	a, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Session: "s1", Messages: msgs, Answer: "x"})
	b, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Session: "s2", Messages: msgs, Answer: "y"})
	if err := s.RateAgent("s1", "ex-nope", true, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("made-up id: %v", err)
	}
	if err := s.RateAgent("s1", b, true, "", ""); !errors.Is(err, ErrNotYours) {
		t.Errorf("another session's example: %v", err)
	}
	if err := s.RateAgent("", a, true, "", ""); !errors.Is(err, ErrNotYours) {
		t.Errorf("no session: %v", err)
	}
	for i := 0; i < MaxAgentRatings; i++ {
		if err := s.RateAgent("s1", a, i%2 == 0, "better", ""); err != nil {
			t.Fatalf("rating %d: %v", i, err)
		}
	}
	if err := s.RateAgent("s1", a, true, "", ""); !errors.Is(err, ErrRatedEnough) {
		t.Errorf("over the cap: %v", err)
	}
	// The owner is not capped, and an owner verdict does not use the agent's.
	if err := s.RateOwner(a, true, "", ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir(), "ratings.jsonl"))
	if n := strings.Count(string(data), "\n"); n != MaxAgentRatings+1 {
		t.Errorf("%d rating lines", n)
	}
}

func TestBriefsSkipUnknownRatings(t *testing.T) {
	s, err := New(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []Message{{Role: "user", Content: "q"}}
	a, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Session: "s1", Messages: msgs, Answer: "x"})
	for i := 0; i < 50; i++ {
		_ = s.Rate(Rating{ID: "ex-fake" + strings.Repeat("x", i), Good: true, Better: strings.Repeat("b", 1000)})
	}
	_ = s.Rate(Rating{ID: a, Good: false, Better: "long better answer", Note: "n"})
	_, rates, err := s.Briefs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[a].Better != BetterGiven || rates[a].Note != "" {
		t.Fatalf("rates %+v", rates)
	}
	c := cacheOf(s)
	if len(c.rates) != 1 || len(c.agentN) != 1 {
		t.Errorf("the cache kept %d verdicts, %d counts", len(c.rates), len(c.agentN))
	}
	// A rating that arrived before its example (by hand) is picked up once
	// the example exists, even though ratings.jsonl did not change.
	pre := NewID()
	_ = s.Rate(Rating{ID: pre, Good: true})
	if _, rates, _ := s.Briefs(nil); len(rates) != 1 {
		t.Fatalf("unknown id kept: %+v", rates)
	}
	_, _ = s.Add(Example{ID: pre, Kind: KindDelegated, Model: "qwen", Session: "s1", Messages: msgs, Answer: "z"})
	if _, rates, _ := s.Briefs(nil); len(rates) != 2 || !rates[pre].Good {
		t.Errorf("late example: %+v", rates)
	}
}
