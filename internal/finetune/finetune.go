// Package finetune keeps examples for training a local model later:
// delegated jobs (a local model's answer, rated by the agent that asked, with
// a better answer when it had one) and, when asked for, cloud answers that
// went through the gateway (a stronger model as the teacher).
//
// Examples are JSON lines in DIR/examples-YYYY-MM.jsonl and ratings in
// DIR/ratings.jsonl, all 0600 in a 0700 folder. Vault values are masked
// before anything is written. Export turns them into the chat fine-tuning
// format most trainers read: one {"messages": [...]} per line.
//
// review.go reads them back for a person (List, Get), who can rate an
// example (a verdict with By "owner"; the newest verdict wins) or exclude
// it from every export (an exclusion line in ratings.jsonl; nothing is
// deleted). runs.go keeps training runs: DIR/runs/<stamp>/ holds the
// exported dataset.jsonl, run.json and whatever the trainer writes there.
// The formats only ever gain fields, so older files stay readable.
package finetune

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Example kinds.
const (
	KindDelegated = "delegated" // a local model's answer to a cerberus_ask
	KindCloud     = "cloud"     // a cloud model's answer through the gateway
)

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Example is one prompt and the answer it got.
type Example struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Model    string    `json:"model"`
	Session  string    `json:"session,omitempty"`
	Messages []Message `json:"messages"` // the prompt (system and user turns)
	Answer   string    `json:"answer"`
}

// Rating is an agent's (or a person's) verdict on a delegated answer.
//
// A line with Exclude set is not a verdict: it marks the example as kept
// out of (or let back into) every export and leaves the verdict alone.
// Lines written before By and Exclude existed read as an agent's verdict.
type Rating struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Good   bool      `json:"good"`
	Better string    `json:"better,omitempty"` // what the answer should have been
	Note   string    `json:"note,omitempty"`
	// By says who gave the verdict: ByOwner for a person, ByAgent (or ""
	// on older lines) for the agent that asked.
	By      string `json:"by,omitempty"`
	Exclude *bool  `json:"exclude,omitempty"` // an exclusion line (see above)
}

// Who rated.
const (
	ByAgent = "agent"
	ByOwner = "owner"
)

// Store appends examples and ratings.
type Store struct {
	dir     string
	maxRow  int
	secrets func() []string
	now     func() time.Time
	mu      sync.Mutex
}

// ErrTooBig is returned for an example over the size limit (it is skipped).
var ErrTooBig = errors.New("example too large")

// New keeps examples in dir. maxRowKiB bounds one example (<= 0: 256 KiB).
// secrets returns the values to mask (vault values); nil masks nothing.
func New(dir string, maxRowKiB int, secrets func() []string) (*Store, error) {
	if maxRowKiB <= 0 {
		maxRowKiB = 256
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	return &Store{dir: dir, maxRow: maxRowKiB << 10, secrets: secrets, now: time.Now}, nil
}

// Dir is the folder the examples are kept in.
func (s *Store) Dir() string { return s.dir }

// NewID makes an example id.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ex-" + hex.EncodeToString(b)
}

// Add appends one example (its ID and Time are filled in when empty) and
// returns its ID.
func (s *Store) Add(e Example) (string, error) {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.Time.IsZero() {
		e.Time = s.now().UTC()
	}
	e.Answer = s.mask(e.Answer)
	for i := range e.Messages {
		e.Messages[i].Content = s.mask(e.Messages[i].Content)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	if len(raw) > s.maxRow {
		return "", ErrTooBig
	}
	return e.ID, s.appendLine(fmt.Sprintf("examples-%s.jsonl", e.Time.Format("2006-01")), raw)
}

// Rate records a verdict on an example. It does not check that the example
// exists: RateOwner and RateAgent do.
func (s *Store) Rate(r Rating) error {
	raw, err := s.ratingLine(r)
	if err != nil {
		return err
	}
	return s.appendLine(ratingsFile, raw)
}

const ratingsFile = "ratings.jsonl"

// ratingLine checks, masks and encodes one rating line.
func (s *Store) ratingLine(r Rating) ([]byte, error) {
	if !strings.HasPrefix(r.ID, "ex-") {
		return nil, fmt.Errorf("unknown example %q", r.ID)
	}
	if r.Time.IsZero() {
		r.Time = s.now().UTC()
	}
	r.Better, r.Note = s.mask(r.Better), s.mask(r.Note)
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(raw) > s.maxRow {
		return nil, ErrTooBig
	}
	return raw, nil
}

func (s *Store) appendLine(name string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(name, raw)
}

// appendLocked appends one line to a file in the store; s.mu is held.
func (s *Store) appendLocked(name string, raw []byte) error {
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_ = f.Chmod(0o600)
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Store) mask(t string) string {
	if s.secrets == nil || t == "" {
		return t
	}
	vals := s.secrets()
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		if len(v) >= 6 {
			t = strings.ReplaceAll(t, v, "[secret]")
		}
	}
	return t
}

// Stats sums what is collected.
type Stats struct {
	Dir       string         `json:"dir"`
	Examples  int            `json:"examples"`
	ByKind    map[string]int `json:"by_kind"`
	Rated     int            `json:"rated"`
	Good      int            `json:"good"`
	Corrected int            `json:"corrected"` // rated with a better answer
	Excluded  int            `json:"excluded"`  // kept out of every export
	Usable    int            `json:"usable"`    // rows Export would write with default options
	// UsableUnrated is the rows Export would write with Unrated set (the
	// usable rows plus delegated answers nobody rated).
	UsableUnrated int        `json:"usable_unrated"`
	Bytes         int64      `json:"bytes"`
	Oldest        *time.Time `json:"oldest,omitempty"`
	Newest        *time.Time `json:"newest,omitempty"`
}

// ExportOptions chooses the rows.
type ExportOptions struct {
	Since    time.Time
	Kinds    []string // empty: both kinds
	Unrated  bool     // also delegated answers nobody rated (default: only rated good or corrected)
	MinChars int      // skip answers shorter than this
}

// Stats reads everything collected.
func (s *Store) Stats() (Stats, error) {
	st := Stats{Dir: s.dir, ByKind: map[string]int{}}
	exs, rates, excl, size, err := s.load()
	if err != nil {
		return st, err
	}
	st.Bytes = size
	for _, e := range exs {
		st.Examples++
		st.ByKind[e.Kind]++
		t := e.Time
		if st.Oldest == nil || t.Before(*st.Oldest) {
			st.Oldest = &t
		}
		if st.Newest == nil || t.After(*st.Newest) {
			st.Newest = &t
		}
		if excl[e.ID] {
			st.Excluded++
		}
		if r, ok := rates[e.ID]; ok {
			st.Rated++
			if r.Better != "" {
				st.Corrected++
			} else if r.Good {
				st.Good++
			}
		}
	}
	st.Usable = len(rows(exs, rates, excl, ExportOptions{}))
	st.UsableUnrated = len(rows(exs, rates, excl, ExportOptions{Unrated: true}))
	return st, nil
}

// Export writes the chosen examples in chat fine-tuning format and returns
// how many rows it wrote.
func (s *Store) Export(w io.Writer, o ExportOptions) (int, error) {
	exs, rates, excl, _, err := s.load()
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriter(w)
	n := 0
	for _, r := range rows(exs, rates, excl, o) {
		raw, err := json.Marshal(map[string]any{"messages": r})
		if err != nil {
			return n, err
		}
		bw.Write(raw)
		bw.WriteByte('\n')
		n++
	}
	return n, bw.Flush()
}

// rows picks the training conversations: a cloud answer as it is; a
// delegated answer with a better one in its place; a delegated answer rated
// good as it is; unrated ones only when asked; ones rated bad or excluded
// never.
func rows(exs []Example, rates map[string]Rating, excl map[string]bool, o ExportOptions) [][]Message {
	var out [][]Message
	for _, e := range exs {
		if excl[e.ID] {
			continue
		}
		if !o.Since.IsZero() && e.Time.Before(o.Since) {
			continue
		}
		if len(o.Kinds) > 0 && !contains(o.Kinds, e.Kind) {
			continue
		}
		answer := e.Answer
		if e.Kind == KindDelegated {
			r, rated := rates[e.ID]
			switch {
			case rated && r.Better != "":
				answer = r.Better
			case rated && r.Good:
			case !rated && o.Unrated:
			default:
				continue
			}
		}
		if strings.TrimSpace(answer) == "" || len([]rune(answer)) < o.MinChars || len(e.Messages) == 0 {
			continue
		}
		conv := append(append([]Message(nil), e.Messages...), Message{Role: "assistant", Content: answer})
		out = append(out, conv)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// load reads all examples (oldest file first), the latest verdict per id
// and which ids are excluded (the latest exclusion line per id).
func (s *Store) load() ([]Example, map[string]Rating, map[string]bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := filepath.Glob(filepath.Join(s.dir, "examples-*.jsonl"))
	if err != nil {
		return nil, nil, nil, 0, err
	}
	sort.Strings(files)
	var exs []Example
	var size int64
	for _, f := range files {
		n, err := eachLine(f, func(line []byte) {
			var e Example
			if json.Unmarshal(line, &e) == nil && e.ID != "" {
				exs = append(exs, e)
			}
		})
		size += n
		if err != nil {
			return nil, nil, nil, 0, err
		}
	}
	rates := map[string]Rating{}
	excl := map[string]bool{}
	n, err := eachLine(filepath.Join(s.dir, ratingsFile), func(line []byte) {
		var r Rating
		if json.Unmarshal(line, &r) != nil || r.ID == "" {
			return
		}
		if r.Exclude != nil {
			if *r.Exclude {
				excl[r.ID] = true
			} else {
				delete(excl, r.ID)
			}
			return
		}
		rates[r.ID] = r // the newest verdict wins, whoever gave it
	})
	size += n
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, 0, err
	}
	return exs, rates, excl, size, nil
}

func eachLine(path string, fn func([]byte)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, _ := f.Stat()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	return size, sc.Err()
}

// Expand fills the train command's placeholders.
func Expand(argv []string, dataset, baseModel, out string) []string {
	r := strings.NewReplacer("{dataset}", dataset, "{base_model}", baseModel, "{out}", out)
	res := make([]string, len(argv))
	for i, a := range argv {
		res[i] = r.Replace(a)
	}
	return res
}
