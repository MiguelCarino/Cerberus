package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Feed item kinds.
const (
	FeedCreated  = "created"
	FeedState    = "state" // any other state change (stopped, error, queued, approved…)
	FeedWaiting  = "waiting"
	FeedPassed   = "passed"
	FeedFailed   = "failed"
	FeedAccepted = "accepted"
	FeedFollowUp = "followup"
	FeedInput    = "input"
	FeedSpawned  = "spawned"
	FeedLimit    = "limit"
	FeedUpdate   = "update" // metadata changed (name, tags, pin, priority, limits)
)

const (
	feedName      = "feed.jsonl"
	feedRing      = 2000 // items kept in memory
	feedSubBuffer = 128  // items a slow feed subscriber may lag before it is cut off
	inputClip     = 80   // characters of typed input recorded
	feedTextClip  = 240  // characters of any other text recorded
	// feedPendingClip bounds queued text before masking; drain clips to
	// inputClip or feedTextClip after masking.
	feedPendingClip = 16 << 10
	feedLoadBytes   = 1 << 20 // how much of feed.jsonl is read back at start
)

// feedRotateBytes is the size at which feed.jsonl is rotated to feed.jsonl.1
// (a variable so tests can make it small).
var feedRotateBytes int64 = 4 << 20

// FeedItem is one entry of the activity feed.
type FeedItem struct {
	Time      time.Time
	SessionID string
	Name      string
	Kind      string // created|state|waiting|passed|failed|accepted|followup|input|spawned|limit|update
	Text      string
}

// feed is the activity log: a ring of the last feedRing items in memory,
// appended to DATA/feed.jsonl, and fanned out to live subscribers.
//
// record never blocks and is safe with Manager.mu held: it only queues the
// item. drain redacts queued items through the vault values (which may run
// secret-tool, so never under Manager.mu), then stores and fans them out.
// A goroutine drains when woken; Feed drains too, so reads are current.
type feed struct {
	path    string
	secrets func() []string

	mu      sync.Mutex // pending and subs
	pending []FeedItem
	subs    map[chan FeedItem]struct{}
	wake    chan struct{}

	procMu sync.Mutex // one drain at a time; guards ring, f, size
	ring   []FeedItem
	f      *os.File
	size   int64
}

func newFeed(path string, secrets func() []string) *feed {
	fd := &feed{path: path, secrets: secrets, subs: map[chan FeedItem]struct{}{}, wake: make(chan struct{}, 1)}
	fd.ring = loadFeed(path)
	return fd
}

// loadFeed reads back the newest items of feed.jsonl.
func loadFeed(path string) []FeedItem {
	raw := readLast(path, feedLoadBytes)
	if len(raw) == int(feedLoadBytes) { // the first line is probably cut
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
	}
	var out []FeedItem
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var it FeedItem
		if json.Unmarshal(sc.Bytes(), &it) == nil && !it.Time.IsZero() {
			out = append(out, it)
		}
	}
	if len(out) > feedRing {
		out = slices.Clone(out[len(out)-feedRing:])
	}
	return out
}

// record queues an item. Its text is clipped only in drain, after vault
// values are masked: clipping first could cut a secret in two, and the
// redactor masks whole values only, so the first part would leak. Here it
// is only bounded (far beyond any clip plus any secret) to cap memory.
func (fd *feed) record(it FeedItem) {
	if it.Time.IsZero() {
		it.Time = time.Now()
	}
	it.Text = clipRunes(strings.TrimSpace(it.Text), feedPendingClip)
	fd.mu.Lock()
	fd.pending = append(fd.pending, it)
	fd.mu.Unlock()
	select {
	case fd.wake <- struct{}{}:
	default:
	}
}

// loop drains whenever woken until done is closed.
func (fd *feed) loop(done <-chan struct{}) {
	for {
		select {
		case <-done:
			fd.drain()
			return
		case <-fd.wake:
			fd.drain()
		}
	}
}

// drain redacts, stores and fans out the queued items.
func (fd *feed) drain() {
	fd.procMu.Lock()
	defer fd.procMu.Unlock()
	fd.mu.Lock()
	items := fd.pending
	fd.pending = nil
	fd.mu.Unlock()
	if len(items) == 0 {
		return
	}
	var vals []string
	if fd.secrets != nil {
		vals = fd.secrets()
	}
	var r redactor
	r.set(vals)
	var buf bytes.Buffer
	for i := range items {
		if len(r.values) > 0 {
			items[i].Text = string(r.mask(nil, []byte(items[i].Text)))
			items[i].Name = string(r.mask(nil, []byte(items[i].Name)))
		}
		items[i].Text = clipFeedText(items[i].Kind, items[i].Text)
		if raw, err := json.Marshal(items[i]); err == nil {
			buf.Write(raw)
			buf.WriteByte('\n')
		}
	}
	fd.ring = append(fd.ring, items...)
	if len(fd.ring) > 2*feedRing {
		n := copy(fd.ring, fd.ring[len(fd.ring)-feedRing:])
		fd.ring = fd.ring[:n]
	}
	fd.appendFile(buf.Bytes())

	fd.mu.Lock()
	defer fd.mu.Unlock()
	for ch := range fd.subs {
		for _, it := range items {
			select {
			case ch <- it:
				continue
			default:
			}
			delete(fd.subs, ch) // lagging: the client reconnects and refetches
			close(ch)
			break
		}
	}
}

// appendFile writes lines to feed.jsonl, rotating it first when it would
// pass feedRotateBytes. Disk errors only cost the on-disk copy.
func (fd *feed) appendFile(p []byte) {
	if fd.path == "" || len(p) == 0 {
		return
	}
	if fd.f == nil {
		f, err := os.OpenFile(fd.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return
		}
		fd.f, fd.size = f, st.Size()
	}
	if fd.size > 0 && fd.size+int64(len(p)) > feedRotateBytes {
		fd.f.Close()
		fd.f = nil
		_ = os.Rename(fd.path, fd.path+".1")
		f, err := os.OpenFile(fd.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return
		}
		fd.f, fd.size = f, 0
	}
	n, _ := fd.f.Write(p)
	fd.size += int64(n)
}

func (fd *feed) close() {
	fd.procMu.Lock()
	defer fd.procMu.Unlock()
	if fd.f != nil {
		fd.f.Close()
		fd.f = nil
	}
}

// items returns up to limit items newer than since, oldest first.
func (fd *feed) items(since time.Time, limit int) []FeedItem {
	fd.drain()
	fd.procMu.Lock()
	defer fd.procMu.Unlock()
	start := len(fd.ring) - min(len(fd.ring), feedRing)
	out := []FeedItem{}
	for _, it := range fd.ring[start:] {
		if it.Time.After(since) {
			out = append(out, it)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (fd *feed) subscribe() (<-chan FeedItem, func()) {
	ch := make(chan FeedItem, feedSubBuffer)
	fd.mu.Lock()
	fd.subs[ch] = struct{}{}
	fd.mu.Unlock()
	return ch, func() {
		fd.mu.Lock()
		defer fd.mu.Unlock()
		if _, ok := fd.subs[ch]; ok {
			delete(fd.subs, ch)
			close(ch)
		}
	}
}

// clipFeedText clips masked text for the feed: typed input and follow-ups
// harder than the rest.
func clipFeedText(kind, text string) string {
	if kind == FeedInput || kind == FeedFollowUp {
		return clipRunes(text, inputClip)
	}
	return clipRunes(text, feedTextClip)
}

// clipRunes shortens s to at most n characters, ending in "…" when cut.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Feed returns up to limit activity items newer than since (zero = all
// kept), oldest first. limit <= 0 means every kept item (at most 2000).
func (m *Manager) Feed(since time.Time, limit int) []FeedItem {
	return m.feed.items(since, limit)
}

// SubscribeFeed streams new feed items as they are recorded. A subscriber
// that falls more than its buffer behind is dropped (its channel closed);
// it then refetches with Feed and subscribes again.
func (m *Manager) SubscribeFeed() (<-chan FeedItem, func()) { return m.feed.subscribe() }

// Record adds a user action to the feed, for actions the API takes outside
// the session manager's own methods (for example typed terminal input).
// Input and follow-up text is clipped to 80 characters, and vault values
// are masked before anything is stored.
func (m *Manager) Record(id, kind, text string) {
	m.mu.Lock()
	name := id
	if s, ok := m.sessions[id]; ok {
		name = displayName(s.info)
	}
	m.mu.Unlock()
	m.feed.record(FeedItem{SessionID: id, Name: name, Kind: kind, Text: text})
}

// recordLocked adds an item about s to the feed (m.mu held).
func (m *Manager) recordLocked(s *session, kind, text string) {
	m.feed.record(FeedItem{SessionID: s.info.ID, Name: displayName(s.info), Kind: kind, Text: text})
}

// displayName is the session's label: Name, else Title, else its id.
func displayName(info Info) string {
	switch {
	case info.Name != "":
		return info.Name
	case info.Title != "":
		return info.Title
	}
	return info.ID
}

// stateFeed is the feed kind and text for a session entering info.State.
func stateFeed(info Info) (string, string) {
	switch info.State {
	case StateWaiting:
		return FeedWaiting, orDefault(info.WaitingReason, "input needed")
	case StatePassed:
		return FeedPassed, ""
	case StateFailed:
		return FeedFailed, firstLine(info.Error)
	case StateAccepted:
		return FeedAccepted, ""
	}
	text := info.State
	switch {
	case info.State == StateError && info.Error != "":
		text += ": " + firstLine(info.Error)
	case info.WaitingReason != "":
		text += ": " + info.WaitingReason
	}
	return FeedState, text
}
