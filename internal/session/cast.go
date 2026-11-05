package session

// Session recording: every run also writes an asciicast v2 file,
// DATA/sessions/<id>/runs/<attempt>.cast, for replay in the web UI.
//
// The file holds a JSON header line and then one `[t, "o", data]` event per
// chunk of output, the same bytes that reach the scrollback: the pump has
// already masked vault values in them. Input is never recorded (there are no
// "i" events). The recording is capped by Sessions.RecordMaxMiB; once the
// next event would pass the cap a final notice is written (an "o" line people
// see plus an "m" marker event players can jump to) and recording stops.
// The folder lives inside the session folder, so pruning the session removes
// it.
//
// A sandboxed agent can write in its session folder, so it could plant
// symlinks there. The runs folder is opened through an os.Root (a symlink
// leading out of the session folder is refused) and each cast is created
// fresh with O_EXCL after removing whatever sat at its name, so Cerberus
// never writes through a planted link.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	runsDir = "runs"
	castExt = ".cast"
	// castReserve keeps room under the cap for the final notice.
	castReserve = 512
	castNotice  = "\r\n\x1b[0m[cerberus: recording stopped, size limit reached]\r\n"
	castMarker  = "recording stopped: size limit"
)

var castName = regexp.MustCompile(`^([1-9][0-9]{0,8})\.cast$`)

// castHeader is the first line of an asciicast v2 file.
type castHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// castWriter appends output events to one run's recording. It is used under
// output.mu, so it needs no lock of its own. Disk errors stop the recording
// quietly; the live stream never depends on it.
type castWriter struct {
	f       *os.File
	start   time.Time
	now     func() time.Time
	size    int64
	cap     int64
	stopped bool
	pending []byte // an incomplete UTF-8 sequence held for the next chunk
	line    []byte // scratch
}

// openCast creates runs/<attempt>.cast under sessDir and writes the header.
// capBytes <= 0 means recording is off: it returns nil, nil.
func openCast(sessDir string, attempt, cols, rows int, title string, capBytes int64, now func() time.Time) (*castWriter, error) {
	if capBytes <= 0 {
		return nil, nil
	}
	if attempt < 1 {
		return nil, fmt.Errorf("record: bad attempt %d", attempt)
	}
	if now == nil {
		now = time.Now
	}
	runs, err := openRunsRoot(sessDir, true)
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	name := strconv.Itoa(attempt) + castExt
	// Whatever is at the name (an older recording of a reused attempt
	// number, or a planted symlink) goes; Remove unlinks a link itself.
	if err := runs.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("record: %w", err)
	}
	f, err := runs.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|oNoFollow, 0o600)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	start := now()
	c := &castWriter{f: f, start: start, now: now, cap: capBytes}
	hdr, _ := json.Marshal(castHeader{Version: 2, Width: max(cols, 1), Height: max(rows, 1),
		Timestamp: start.Unix(), Title: title, Env: map[string]string{"TERM": "xterm-256color"}})
	hdr = append(hdr, '\n')
	if int64(len(hdr))+castReserve > capBytes {
		c.close()
		return nil, nil // a cap this small records nothing useful
	}
	c.put(hdr)
	return c, nil
}

// openRunsRoot opens sessDir/runs as an os.Root, creating it when create is
// set. A runs entry that leads outside the session folder is refused.
func openRunsRoot(sessDir string, create bool) (*os.Root, error) {
	sess, err := os.OpenRoot(sessDir)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	if create {
		if st, err := sess.Lstat(runsDir); err == nil && !st.IsDir() {
			// Not ours: a file or link planted where the folder goes.
			if err := sess.Remove(runsDir); err != nil {
				return nil, fmt.Errorf("record: %w", err)
			}
		}
		if err := sess.Mkdir(runsDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("record: %w", err)
		}
	}
	st, err := sess.Lstat(runsDir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("record: %s/%s is not a folder", sessDir, runsDir)
	}
	return sess.OpenRoot(runsDir)
}

func (c *castWriter) put(b []byte) {
	n, err := c.f.Write(b)
	c.size += int64(n)
	if err != nil {
		c.stopped = true
	}
}

// write records p as one output event at the current time.
func (c *castWriter) write(p []byte) {
	if c == nil || c.stopped || len(p) == 0 {
		return
	}
	data := p
	if len(c.pending) > 0 {
		data = append(c.pending, p...)
		c.pending = nil
	}
	if cut := incompleteTail(data); cut > 0 {
		c.pending = slices.Clone(data[len(data)-cut:])
		data = data[:len(data)-cut]
	}
	if len(data) == 0 {
		return
	}
	c.line = appendEvent(c.line[:0], c.elapsed(), "o", data)
	if c.size+int64(len(c.line)) > c.cap-castReserve {
		c.finish()
		return
	}
	c.put(c.line)
}

// finish writes the final notice and marker and stops recording.
func (c *castWriter) finish() {
	t := c.elapsed()
	c.put(appendEvent(nil, t, "o", []byte(castNotice)))
	c.put(appendEvent(nil, t, "m", []byte(castMarker)))
	c.stopped = true
	c.pending = nil
}

func (c *castWriter) elapsed() float64 { return c.now().Sub(c.start).Seconds() }

// close writes out a held partial character (as U+FFFD) and closes the file.
func (c *castWriter) close() {
	if c == nil || c.f == nil {
		return
	}
	if len(c.pending) > 0 && !c.stopped {
		c.put(appendEvent(nil, c.elapsed(), "o", c.pending))
	}
	c.pending = nil
	_ = c.f.Close()
	c.f = nil
	c.stopped = true
}

// appendEvent appends `[t, "kind", "data"]\n`. json.Marshal of a string
// replaces invalid UTF-8 with U+FFFD, as asciicast players expect.
func appendEvent(dst []byte, t float64, kind string, data []byte) []byte {
	dst = append(dst, '[')
	dst = strconv.AppendFloat(dst, t, 'f', 6, 64)
	dst = append(dst, ", \""...)
	dst = append(dst, kind...)
	dst = append(dst, "\", "...)
	s, _ := json.Marshal(string(data))
	dst = append(dst, s...)
	return append(dst, "]\n"...)
}

// incompleteTail is the length of an unfinished UTF-8 sequence at the end of
// b (0 when b ends on a whole character or on bytes that can never become
// one).
func incompleteTail(b []byte) int {
	for i := 1; i <= 3 && i <= len(b); i++ {
		c := b[len(b)-i]
		switch {
		case c < 0x80:
			return 0 // ASCII ends any sequence
		case c < 0xC0:
			continue // a continuation byte: look further back
		}
		need := 2
		if c >= 0xF0 {
			need = 4
		} else if c >= 0xE0 {
			need = 3
		}
		if i < need && !utf8.FullRune(b[len(b)-i:]) {
			return i
		}
		return 0
	}
	return 0
}

// RunCast describes one recorded run.
type RunCast struct {
	Attempt   int       `json:"attempt"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Recording bool      `json:"recording"` // the run is still being recorded
}

// Runs lists the recordings of session id, oldest attempt first. A session
// without recordings yields an empty list.
func (m *Manager) Runs(id string) ([]RunCast, error) {
	m.mu.Lock()
	s, err := m.lookup(id)
	var live int
	if err == nil && s.run != nil && s.out.recording() {
		live = s.info.Attempt
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	runs, err := openRunsRoot(s.dir, false)
	if errors.Is(err, fs.ErrNotExist) {
		return []RunCast{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	f, err := runs.Open(".")
	if err != nil {
		return nil, err
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	out := []RunCast{}
	for _, e := range ents {
		mt := castName.FindStringSubmatch(e.Name())
		if mt == nil || !e.Type().IsRegular() {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		n, _ := strconv.Atoi(mt[1])
		out = append(out, RunCast{Attempt: n, Size: st.Size(), Modified: st.ModTime(), Recording: n == live})
	}
	slices.SortFunc(out, func(a, b RunCast) int { return a.Attempt - b.Attempt })
	return out, nil
}

// OpenRun opens the recording of one attempt of session id for reading (the
// caller closes it). A live run's file is complete up to its last event. It
// returns fs.ErrNotExist when there is no such recording, and refuses
// anything that is not a regular file inside the runs folder.
func (m *Manager) OpenRun(id string, attempt int) (*os.File, error) {
	if attempt < 1 {
		return nil, fs.ErrNotExist
	}
	m.mu.Lock()
	s, err := m.lookup(id)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	runs, err := openRunsRoot(s.dir, false)
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	name := strconv.Itoa(attempt) + castExt
	st, err := runs.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fs.ErrNotExist
	}
	f, err := runs.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	if fst, err := f.Stat(); err != nil || !fst.Mode().IsRegular() {
		f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}

// ParseRunName turns "<attempt>.cast" into the attempt number (for routes
// such as /api/sessions/{id}/runs/{attempt}.cast).
func ParseRunName(name string) (int, bool) {
	mt := castName.FindStringSubmatch(name)
	if mt == nil {
		return 0, false
	}
	n, err := strconv.Atoi(mt[1])
	return n, err == nil
}

// startRecording opens the recording of the run r is about to become
// (attempt info.Attempt+1) and hands it to the session's output. The title
// is masked like the output. Failures are logged and the run goes on
// unrecorded. Called from launch before the pump starts.
func (m *Manager) startRecording(s *session, r *run, info Info) {
	capBytes := int64(m.conf().Sessions.RecordMaxMiB) << 20
	if capBytes <= 0 {
		return
	}
	title := info.Name
	if title == "" {
		title = info.Title
	}
	title = string(r.redact.mask(nil, []byte(title)))
	c, err := openCast(s.dir, info.Attempt+1, max(info.Cols, 20), max(info.Rows, 5), title, capBytes, nil)
	if err != nil {
		m.log.Warn("session recording", "id", info.ID, "err", err)
		return
	}
	if c != nil {
		s.out.startCast(c)
	}
}
