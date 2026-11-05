package finetune

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ReserveRunDir makes a new, empty run folder DIR/runs/<stamp> (or
// <stamp>-2, -3… when that one exists) exclusively, so two runs started in
// the same second, from the server or the CLI, never share a folder. It
// returns the run id and the folder.
func (s *Store) ReserveRunDir() (id, dir string, err error) {
	return s.reserveRun(s.now())
}

func (s *Store) reserveRun(now time.Time) (id, dir string, err error) {
	if err := os.MkdirAll(s.RunsDir(), 0o700); err != nil {
		return "", "", err
	}
	stamp := now.Format(runStamp)
	for i := 1; i < 100; i++ {
		id = stamp
		if i > 1 {
			id = fmt.Sprintf("%s-%d", stamp, i)
		}
		dir = filepath.Join(s.RunsDir(), id)
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return id, dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
	}
	return "", "", errors.New("too many runs started in one second")
}

// Run is one training run: DIR/runs/<ID>/ holds dataset.jsonl (the
// exported rows), run.json (this record) and whatever the trainer writes
// there (the folder is its {out}). Runs started from the CLI have no
// run.json; Runs still lists them, with what the folder tells.
type Run struct {
	ID      string    `json:"id"`   // the folder name: a local time stamp, "-2"… when two start in one second
	Time    time.Time `json:"time"` // when the run was exported
	Rows    int       `json:"rows"` // rows in dataset.jsonl
	Base    string    `json:"base"` // base model ({base_model}); "" when none was set
	Unrated bool      `json:"unrated,omitempty"`
	Session string    `json:"session,omitempty"` // the session that runs the trainer ("" from the CLI)
	Dir     string    `json:"dir"`               // the run folder
	Dataset string    `json:"dataset"`           // DIR/runs/<ID>/dataset.jsonl
	Out     string    `json:"out"`               // the trainer's {out} (the run folder)
	Command []string  `json:"command,omitempty"` // the expanded trainer command line, vault values masked
}

// ErrNoRows is returned by NewRun when no example is usable.
var ErrNoRows = errors.New("no usable examples yet")

const (
	runFile     = "run.json"
	datasetFile = "dataset.jsonl"
	runStamp    = "20060102-150405"
)

// RunsDir is where runs are kept.
func (s *Store) RunsDir() string { return filepath.Join(s.dir, "runs") }

// NewRun exports the usable examples into a new run folder and records the
// run (without a session yet). With no usable rows it leaves nothing behind
// and returns ErrNoRows.
func (s *Store) NewRun(base string, o ExportOptions) (Run, error) {
	now := s.now()
	id, dir, err := s.reserveRun(now)
	if err != nil {
		return Run{}, err
	}
	r := Run{ID: id, Time: now.UTC(), Base: base, Unrated: o.Unrated, Dir: dir,
		Dataset: filepath.Join(dir, datasetFile), Out: dir}
	n, err := s.exportFile(r.Dataset, o)
	if err == nil && n == 0 {
		err = ErrNoRows
	}
	if err == nil {
		r.Rows = n
		err = s.SaveRun(r)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return Run{}, err
	}
	return r, nil
}

func (s *Store) exportFile(path string, o ExportOptions) (int, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := s.Export(f, o)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// SaveRun writes r's run.json (replacing it whole). The command line is
// masked like everything else the store writes.
func (s *Store) SaveRun(r Run) error {
	if r.ID == "" || filepath.Base(r.ID) != r.ID || r.ID == "." || r.ID == ".." {
		return fmt.Errorf("bad run id %q", r.ID)
	}
	r.Command = append([]string(nil), r.Command...)
	for i := range r.Command {
		r.Command[i] = s.mask(r.Command[i])
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(s.RunsDir(), r.ID)
	tmp, err := os.CreateTemp(dir, ".run-*.json")
	if err != nil {
		return err
	}
	_ = tmp.Chmod(0o600)
	_, err = tmp.Write(append(raw, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, runFile))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// Runs lists the run folders, newest first. A folder without run.json (a
// CLI run) is described from its name and dataset.
func (s *Store) Runs() ([]Run, error) {
	ents, err := os.ReadDir(s.RunsDir())
	if errors.Is(err, os.ErrNotExist) {
		return []Run{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Run{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.RunsDir(), e.Name())
		var r Run
		raw, err := os.ReadFile(filepath.Join(dir, runFile))
		if err == nil && json.Unmarshal(raw, &r) == nil {
			r.ID, r.Dir = e.Name(), dir // the folder is the truth if it was moved
			if r.Dataset == "" {
				r.Dataset = filepath.Join(dir, datasetFile)
			}
			out = append(out, r)
			continue
		}
		ds := filepath.Join(dir, datasetFile)
		if _, err := os.Stat(ds); err != nil {
			continue // not a run folder
		}
		r = Run{ID: e.Name(), Dir: dir, Dataset: ds, Out: dir}
		if t, err := time.ParseInLocation(runStamp, e.Name(), time.Local); err == nil {
			r.Time = t.UTC()
		} else if fi, err := e.Info(); err == nil {
			r.Time = fi.ModTime().UTC()
		}
		r.Rows = countLines(ds)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.After(out[j].Time)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	n := 0
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n
}
