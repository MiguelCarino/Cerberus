package session

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
)

const (
	// subBuffer is how many chunks a slow subscriber may lag before it is cut off.
	subBuffer = 256
	// transcriptCap is the size at which transcript.txt keeps its last half.
	transcriptCap  = 4 << 20
	transcriptName = "transcript.txt"
)

// output is a session's terminal output: an append-only scrollback.log
// (capped; truncated to its last half), a plain-text transcript.txt beside
// it for search (escape sequences and control characters removed, also
// capped), an in-memory tail while the session is active, and live
// subscribers.
//
// Memory is held only while active: release closes the files and frees the
// tail, and the next Write or attach loads them back from disk.
type output struct {
	tailMax int

	mu    sync.Mutex
	sb    capFile // scrollback.log
	tx    capFile // transcript.txt
	strip textStripper
	txr   redactor // masks the stripped text again before it reaches transcript.txt
	txBuf []byte   // scratch for txr
	tail  []byte   // nil = not loaded
	subs  map[*subscriber]struct{}
	cast  *castWriter // the current run's recording (nil = none)
}

type subscriber struct {
	ch   chan []byte
	once sync.Once
}

func (s *subscriber) close() { s.once.Do(func() { close(s.ch) }) }

func newOutput(path string, capBytes int64, tailMax int) *output {
	if capBytes < 2*int64(tailMax) {
		capBytes = 2 * int64(tailMax)
	}
	return &output{tailMax: tailMax, subs: map[*subscriber]struct{}{},
		sb: capFile{path: path, cap: capBytes},
		tx: capFile{path: filepath.Join(filepath.Dir(path), transcriptName), cap: transcriptCap}}
}

// Write appends p to the scrollback, the transcript and the tail and fans it
// out. A subscriber whose buffer is full is dropped (its channel is closed)
// so the pump never blocks; a client then reattaches and gets a fresh
// backlog.
func (o *output) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loadTailLocked()
	o.sb.write(p)
	o.cast.write(p)
	if lines := o.strip.feed(p); len(lines) > 0 {
		o.tx.write(o.maskTextLocked(lines))
	}
	o.tail = append(o.tail, p...)
	if len(o.tail) > 2*o.tailMax {
		n := copy(o.tail, o.tail[len(o.tail)-o.tailMax:])
		o.tail = o.tail[:n]
	}
	if len(o.subs) > 0 {
		chunk := bytes.Clone(p) // shared, read-only for every subscriber
		for s := range o.subs {
			select {
			case s.ch <- chunk:
			default:
				delete(o.subs, s)
				s.close()
			}
		}
	}
	return len(p), nil
}

// setSecrets sets the values the transcript pass masks. The pump redacts
// the raw stream already; this second pass runs on the plain text, where
// stripping can join a value that something in the raw stream kept apart.
func (o *output) setSecrets(values []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.txr.set(values)
}

// maskTextLocked masks secret values in complete plain-text lines.
func (o *output) maskTextLocked(text []byte) []byte {
	if len(o.txr.values) == 0 {
		return text
	}
	o.txBuf = o.txr.mask(o.txBuf[:0], text)
	return o.txBuf
}

func (o *output) loadTailLocked() {
	if o.tail == nil {
		o.tail = make([]byte, 0, o.tailMax)
		o.tail = append(o.tail, readLast(o.sb.path, int64(o.tailMax))...)
	}
}

// attach returns the last tailMax bytes and a live channel. Both are taken
// under one lock, so no output is lost or repeated between them.
func (o *output) attach() ([]byte, <-chan []byte, func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var backlog []byte
	if o.tail != nil {
		backlog = bytes.Clone(o.tail[max(len(o.tail)-o.tailMax, 0):])
	} else {
		backlog = readLast(o.sb.path, int64(o.tailMax))
	}
	s := &subscriber{ch: make(chan []byte, subBuffer)}
	o.subs[s] = struct{}{}
	detach := func() {
		o.mu.Lock()
		delete(o.subs, s)
		o.mu.Unlock()
		s.close()
	}
	return backlog, s.ch, detach
}

// transcript is the whole plain-text transcript, including the line still
// being written.
func (o *output) transcript() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, _ := os.ReadFile(o.tx.path)
	if p := o.strip.pending(); len(p) > 0 {
		b = append(append(b, o.maskTextLocked(p)...), '\n')
	}
	return b
}

// startCast makes c the recording that receives output from now on,
// closing an earlier one.
func (o *output) startCast(c *castWriter) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cast.close()
	o.cast = c
}

// stopCast ends the current recording.
func (o *output) stopCast() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cast.close()
	o.cast = nil
}

// recording reports whether a recording is open and still taking output.
func (o *output) recording() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cast != nil && !o.cast.stopped
}

// release closes the files and frees the tail while the session is idle.
// An unfinished transcript line is written out first.
func (o *output) release() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if rest := o.strip.flush(); len(rest) > 0 {
		o.tx.write(o.maskTextLocked(rest))
	}
	o.sb.close()
	o.tx.close()
	o.tail = nil
}

// closeSubs ends every live subscription (the session is final).
func (o *output) closeSubs() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for s := range o.subs {
		delete(o.subs, s)
		s.close()
	}
}

// capFile is an append-only file, opened lazily, that keeps its last half
// once it grows past cap. Disk errors are not fatal: the live stream keeps
// working without it.
type capFile struct {
	path string
	cap  int64
	f    *os.File
	size int64
}

func (c *capFile) write(p []byte) {
	if c.f == nil {
		// O_NOFOLLOW: a sandboxed agent can write in its session folder and
		// must not redirect Cerberus' own logs through a symlink.
		f, err := os.OpenFile(c.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|oNoFollow, 0o600)
		if err != nil {
			return
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return
		}
		c.f, c.size = f, st.Size()
	}
	n, _ := c.f.Write(p)
	c.size += int64(n)
	if c.size > c.cap {
		c.truncate()
	}
}

// truncate keeps the last half of the file, starting at a line break when
// one is near so the kept part begins cleanly.
func (c *capFile) truncate() {
	keep := readLast(c.path, c.cap/2)
	if i := bytes.IndexByte(keep, '\n'); i >= 0 && i < 4096 {
		keep = keep[i+1:]
	}
	if err := c.f.Truncate(0); err != nil {
		return
	}
	n, _ := c.f.Write(keep) // O_APPEND: lands at offset 0 after truncation
	c.size = int64(n)
}

func (c *capFile) close() {
	if c.f != nil {
		c.f.Close()
		c.f = nil
	}
}

// readLast returns up to n bytes from the end of the file at path.
func readLast(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(st.Size()-n, 0)
	buf := make([]byte, st.Size()-off)
	k, _ := f.ReadAt(buf, off) // a short read just yields less backlog
	return buf[:k]
}
