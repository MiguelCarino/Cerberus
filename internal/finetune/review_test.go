package finetune

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newClockStore is a store whose clock moves one minute per call.
func newClockStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "ft"), 0, nil)
	must(t, err)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { at = at.Add(time.Minute); return at }
	return s
}

func TestListFiltersAndPaging(t *testing.T) {
	s := newClockStore(t)
	p := func(q string) []Message { return []Message{{Role: "user", Content: q}} }
	var ids []string
	for i := range 7 {
		id, err := s.Add(Example{Kind: KindDelegated, Model: "qwen", Messages: p("question " + string(rune('a'+i))), Answer: "answer"})
		must(t, err)
		ids = append(ids, id)
	}
	cloud, _ := s.Add(Example{Kind: KindCloud, Model: "p/big", Messages: p("cloudy needle"), Answer: "teacher"})
	must(t, s.Rate(Rating{ID: ids[0], Good: true}))
	must(t, s.Rate(Rating{ID: ids[1], Good: false}))
	must(t, s.Rate(Rating{ID: ids[2], Better: "the Needle answer"}))

	page, err := s.List(ListOptions{})
	must(t, err)
	if page.Total != 8 || len(page.Examples) != 8 || page.Next != "" || page.Examples[0].ID != cloud {
		t.Fatalf("all: total %d, %d listed, next %q, first %s", page.Total, len(page.Examples), page.Next, page.Examples[0].ID)
	}
	// Paging: 3 at a time, newest first, nothing lost or repeated.
	var seen []string
	before := ""
	for range 5 {
		pg, err := s.List(ListOptions{Limit: 3, Before: before})
		must(t, err)
		for _, it := range pg.Examples {
			seen = append(seen, it.ID)
		}
		if pg.Next == "" {
			break
		}
		before = pg.Next
	}
	if len(seen) != 8 || seen[0] != cloud || seen[7] != ids[0] {
		t.Fatalf("paged %v", seen)
	}
	if _, err := s.List(ListOptions{Before: "ex-nope"}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("bad cursor: %v", err)
	}

	for rated, want := range map[string][]string{
		RatedGood: {ids[0]}, RatedBad: {ids[1]}, RatedCorrected: {ids[2]},
	} {
		pg, err := s.List(ListOptions{Rated: rated})
		must(t, err)
		if len(pg.Examples) != len(want) || pg.Examples[0].ID != want[0] {
			t.Errorf("rated=%s: %+v", rated, pg.Examples)
		}
	}
	pg, _ := s.List(ListOptions{Rated: RatedUnrated, Kind: KindDelegated})
	if pg.Total != 4 {
		t.Errorf("unrated delegated: %d", pg.Total)
	}
	pg, _ = s.List(ListOptions{Kind: KindCloud})
	if pg.Total != 1 || pg.Examples[0].Rating != nil {
		t.Errorf("cloud: %+v", pg)
	}
	pg, _ = s.List(ListOptions{Q: "NEEDLE"}) // the prompt of one, the better answer of another
	if pg.Total != 2 {
		t.Errorf("q: %d", pg.Total)
	}
	if _, err := s.List(ListOptions{Rated: "meh"}); err == nil {
		t.Error("bad rated accepted")
	}
	if _, err := s.List(ListOptions{Kind: "meh"}); err == nil {
		t.Error("bad kind accepted")
	}
}

func TestListClipsAndGetIsWhole(t *testing.T) {
	s := newClockStore(t)
	long := strings.Repeat("é", ListClip) // 2 bytes a rune: clipped mid-way
	id, err := s.Add(Example{Kind: KindCloud, Model: "m", Messages: []Message{{Role: "user", Content: long}}, Answer: "short"})
	must(t, err)
	pg, _ := s.List(ListOptions{})
	it := pg.Examples[0]
	if !it.Truncated || len(it.Messages[0].Content) > ListClip || !strings.HasPrefix(long, it.Messages[0].Content) || it.Answer != "short" {
		t.Errorf("clip: truncated %v, %d bytes", it.Truncated, len(it.Messages[0].Content))
	}
	full, err := s.Get(id)
	must(t, err)
	if full.Truncated || full.Messages[0].Content != long {
		t.Error("Get clipped")
	}
	if _, err := s.Get("ex-nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown: %v", err)
	}
}

func TestOwnerRatingOverridesAgent(t *testing.T) {
	s := newClockStore(t)
	id, _ := s.Add(Example{Kind: KindDelegated, Model: "m", Messages: []Message{{Role: "user", Content: "q"}}, Answer: "wrong"})
	must(t, s.Rate(Rating{ID: id, Good: true, By: ByAgent}))
	must(t, s.RateOwner(id, false, "right", "the agent missed it"))
	it, err := s.Get(id)
	must(t, err)
	if it.Rating == nil || it.Rating.By != ByOwner || it.Rating.Better != "right" || it.Rating.Good {
		t.Fatalf("rating %+v", it.Rating)
	}
	var buf bytes.Buffer
	n, _ := s.Export(&buf, ExportOptions{})
	if n != 1 || !strings.Contains(buf.String(), "right") || strings.Contains(buf.String(), "wrong") {
		t.Errorf("export %d: %s", n, buf.String())
	}
	if err := s.RateOwner("ex-nope", true, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("rate unknown: %v", err)
	}
}

func TestExcludeKeepsOutOfExport(t *testing.T) {
	s := newClockStore(t)
	p := []Message{{Role: "user", Content: "q"}}
	a, _ := s.Add(Example{Kind: KindCloud, Model: "m", Messages: p, Answer: "keep me"})
	b, _ := s.Add(Example{Kind: KindDelegated, Model: "m", Messages: p, Answer: "leave me"})
	must(t, s.Rate(Rating{ID: b, Good: true}))
	must(t, s.Exclude(b, true))

	it, _ := s.Get(b)
	if !it.Excluded || it.Rating == nil || !it.Rating.Good {
		t.Fatalf("excluded item %+v (the verdict must survive)", it)
	}
	var buf bytes.Buffer
	if n, _ := s.Export(&buf, ExportOptions{Unrated: true}); n != 1 || strings.Contains(buf.String(), "leave me") {
		t.Errorf("export with an excluded row: %d %s", n, buf.String())
	}
	st, _ := s.Stats()
	if st.Excluded != 1 || st.Usable != 1 || st.Examples != 2 {
		t.Errorf("stats %+v", st)
	}
	must(t, s.Exclude(b, false))
	buf.Reset()
	if n, _ := s.Export(&buf, ExportOptions{}); n != 2 {
		t.Errorf("after re-including: %d rows", n)
	}
	must(t, s.Exclude(a, true))
	if err := s.Exclude("ex-nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("exclude unknown: %v", err)
	}
	// Exclusion lines are rating lines: old readers see ids and verdicts.
	raw, _ := os.ReadFile(filepath.Join(s.Dir(), "ratings.jsonl"))
	if !strings.Contains(string(raw), `"exclude":true`) || !strings.Contains(string(raw), `"by":"owner"`) {
		t.Errorf("ratings.jsonl:\n%s", raw)
	}
}

// Lines written before by/exclude existed still read.
func TestOldRatingLinesRead(t *testing.T) {
	s := newClockStore(t)
	id, _ := s.Add(Example{Kind: KindDelegated, Model: "m", Messages: []Message{{Role: "user", Content: "q"}}, Answer: "a"})
	line := `{"id":"` + id + `","time":"2026-10-01T00:00:00Z","good":true}` + "\n"
	must(t, os.WriteFile(filepath.Join(s.Dir(), "ratings.jsonl"), []byte(line), 0o600))
	it, _ := s.Get(id)
	if it.Rating == nil || !it.Rating.Good || it.Rating.By != "" || it.Excluded {
		t.Errorf("old line read as %+v excluded=%v", it.Rating, it.Excluded)
	}
}

func TestRuns(t *testing.T) {
	s := newClockStore(t)
	if _, err := s.NewRun("qwen", ExportOptions{}); !errors.Is(err, ErrNoRows) {
		t.Fatalf("empty run: %v", err)
	}
	if ents, _ := os.ReadDir(s.RunsDir()); len(ents) != 0 {
		t.Fatalf("an empty run left %d folders", len(ents))
	}
	s.Add(Example{Kind: KindCloud, Model: "m", Messages: []Message{{Role: "user", Content: "q"}}, Answer: "a"})
	// Two runs in the same second get their own folders.
	fixed := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	s.now = func() time.Time { return fixed }
	r1, err := s.NewRun("qwen", ExportOptions{})
	must(t, err)
	r2, err := s.NewRun("", ExportOptions{Unrated: true})
	must(t, err)
	if r1.ID == r2.ID || r1.Rows != 1 || r1.Out != r1.Dir || !strings.HasSuffix(r2.ID, "-2") {
		t.Fatalf("runs %+v %+v", r1, r2)
	}
	r1.Session, r1.Command = "sess-1", []string{"train", r1.Dataset}
	must(t, s.SaveRun(r1))
	if err := s.SaveRun(Run{ID: "../x"}); err == nil {
		t.Error("SaveRun took a path")
	}
	// A CLI run: a folder with a dataset and no run.json.
	cli := filepath.Join(s.RunsDir(), "20261101-080000")
	must(t, os.MkdirAll(cli, 0o700))
	must(t, os.WriteFile(filepath.Join(cli, "dataset.jsonl"), []byte("{}\n{}\n"), 0o600))
	must(t, os.MkdirAll(filepath.Join(s.RunsDir(), "junk"), 0o700))

	runs, err := s.Runs()
	must(t, err)
	if len(runs) != 3 || runs[0].ID != r2.ID || runs[1].Session != "sess-1" || runs[2].ID != "20261101-080000" || runs[2].Rows != 2 {
		raw, _ := json.Marshal(runs)
		t.Fatalf("runs %s", raw)
	}
	data, _ := os.ReadFile(r1.Dataset)
	if !strings.Contains(string(data), `"messages"`) {
		t.Errorf("dataset %s", data)
	}
}
