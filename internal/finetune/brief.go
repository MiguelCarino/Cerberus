package finetune

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Brief is an example without its text: who asked which model, when. The
// Flow graph reads these (with the verdicts) to put ratings on its edges,
// so it never holds the prompts and answers in memory.
type Brief struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Model   string    `json:"model"`
	Session string    `json:"session,omitempty"`
}

// Briefs reads the examples of the given sessions (every example when
// sessions is empty) without their text, oldest file first, and the newest
// verdict for each of them by example id. A rating line that only marks an
// example excluded or included (it has an "exclude" key) is not a verdict
// and is skipped. The verdicts carry no text: Note is empty and Better is
// BetterGiven when a better answer was written. The files are only read:
// their format does not change.
//
// A file is parsed again only when its size or modification time changed
// since the last call (appends touch the current month's file and
// ratings.jsonl), so a live view that asks often reads little. The cache
// holds the briefs and, for example ids that exist, the newest verdict
// without its text: never a prompt, an answer or a better answer.
func (s *Store) Briefs(sessions []string) ([]Brief, map[string]Rating, error) {
	want := map[string]bool{}
	for _, id := range sessions {
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cacheOf(s)
	all, err := c.allBriefs(s.dir)
	if err != nil {
		return nil, nil, err
	}
	var out []Brief
	for _, b := range all {
		if len(want) == 0 || want[b.Session] {
			out = append(out, b)
		}
	}
	known, err := c.ratings(filepath.Join(s.dir, ratingsFile), all)
	if err != nil {
		return nil, nil, err
	}
	rates := map[string]Rating{}
	for _, b := range out {
		if r, ok := known[b.ID]; ok {
			rates[b.ID] = r
		}
	}
	return out, rates, nil
}

// BetterGiven stands in for a better answer in the verdicts Briefs returns.
const BetterGiven = "…"

// briefCache keeps the parsed files of one Store; it is used under s.mu.
type briefCache struct {
	files    map[string]briefFile
	rateStat fileStamp
	rateIDs  int               // how many examples existed when rates was built
	rates    map[string]Rating // the newest verdict per existing example, without text
	agentN   map[string]int    // verdicts by the asking agent per existing example
}

type fileStamp struct {
	size int64
	mod  time.Time
}

type briefFile struct {
	stamp  fileStamp
	briefs []Brief
}

var briefCaches sync.Map // *Store → *briefCache

func cacheOf(s *Store) *briefCache {
	if c, ok := briefCaches.Load(s); ok {
		return c.(*briefCache)
	}
	c, _ := briefCaches.LoadOrStore(s, &briefCache{files: map[string]briefFile{}})
	return c.(*briefCache)
}

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{size: fi.Size(), mod: fi.ModTime()}, nil
}

// allBriefs returns every example's brief, oldest file first, and forgets
// files removed by hand.
func (c *briefCache) allBriefs(dir string) ([]Brief, error) {
	files, err := filepath.Glob(filepath.Join(dir, "examples-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var all []Brief
	seen := map[string]bool{}
	for _, f := range files {
		seen[f] = true
		briefs, err := c.examples(f)
		if err != nil {
			return nil, err
		}
		all = append(all, briefs...)
	}
	for f := range c.files {
		if !seen[f] {
			delete(c.files, f) // removed by hand
		}
	}
	return all, nil
}

func (c *briefCache) examples(path string) ([]Brief, error) {
	st, err := stampOf(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			delete(c.files, path)
			return nil, nil
		}
		return nil, err
	}
	if f, ok := c.files[path]; ok && f.stamp == st {
		return f.briefs, nil
	}
	var briefs []Brief
	if _, err := eachLine(path, func(line []byte) {
		var b Brief
		if json.Unmarshal(line, &b) == nil && b.ID != "" {
			briefs = append(briefs, b)
		}
	}); err != nil {
		return nil, err
	}
	c.files[path] = briefFile{stamp: st, briefs: briefs}
	return briefs, nil
}

// ratings returns the newest verdict, without text, for each example in
// all, and fills c.agentN. Lines for ids that are not examples are skipped,
// so a flood of made-up ids costs no memory. It parses ratings.jsonl again
// only when the file or the number of examples changed.
func (c *briefCache) ratings(path string, all []Brief) (map[string]Rating, error) {
	st, err := stampOf(path)
	if errors.Is(err, fs.ErrNotExist) {
		c.rates, c.agentN, c.rateStat, c.rateIDs = map[string]Rating{}, map[string]int{}, fileStamp{}, len(all)
		return c.rates, nil
	}
	if err != nil {
		return nil, err
	}
	if c.rates != nil && c.rateStat == st && c.rateIDs == len(all) {
		return c.rates, nil
	}
	known := make(map[string]bool, len(all))
	for _, b := range all {
		known[b.ID] = true
	}
	rates := map[string]Rating{}
	agentN := map[string]int{}
	if _, err := eachLine(path, func(line []byte) {
		var r struct {
			ID      string    `json:"id"`
			Time    time.Time `json:"time"`
			Good    bool      `json:"good"`
			Better  string    `json:"better"`
			By      string    `json:"by"`
			Exclude *bool     `json:"exclude"`
		}
		if json.Unmarshal(line, &r) != nil || !known[r.ID] || r.Exclude != nil {
			return
		}
		v := Rating{ID: r.ID, Time: r.Time, Good: r.Good, By: r.By}
		if r.Better != "" {
			v.Better = BetterGiven
		}
		rates[r.ID] = v // the newest verdict wins
		if r.By == "" || r.By == ByAgent {
			agentN[r.ID]++
		}
	}); err != nil {
		return nil, err
	}
	c.rates, c.agentN, c.rateStat, c.rateIDs = rates, agentN, st, len(all)
	return rates, nil
}
