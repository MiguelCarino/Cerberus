// Package downloads is the model download manager: it fetches local models
// without the command line, for the three local engines.
//
//   - LM Studio: runs `lms get <query> --yes` and parses progress from its
//     output.
//   - Ollama: POST /api/pull with streaming progress.
//   - llama.cpp: a plain GGUF file from a Hugging Face https URL, into the
//     first llama.cpp models folder (<dir>/<owner>/<repo>/<file>.gguf). The
//     download goes to <file>.gguf.part and resumes with an HTTP Range
//     request; it is checked against the SHA-256 the Hugging Face repo
//     publishes for the file (X-Linked-Etag) when there is one, and refused
//     when free disk is below what is left to fetch plus 2 GiB.
//
// One download runs at a time per engine; the others wait in that engine's
// queue. Each item reports bytes, total, rate and ETA. A runner goroutine
// exists only while its engine has work, so an idle manager costs nothing.
// Finished downloads are reported through Options.OnDone (the daemon turns
// that into an inbox item and a notification).
//
// The manager never loads a model: a finished download is only a file (or a
// backend's own store entry) that the engines list on their next scan.
package downloads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
)

// Engine names, as in engine.Backend* and config.
const (
	EngineLMStudio = "lmstudio"
	EngineOllama   = "ollama"
	EngineLlamaCpp = "llamacpp"
)

// Item states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateVerifying = "verifying"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

const (
	// DiskHeadroom is kept free on top of a GGUF download.
	DiskHeadroom = 2 << 30
	// keepFinished is how many finished items List still shows.
	keepFinished = 50
	// emitEvery throttles progress callbacks per item.
	emitEvery = 500 * time.Millisecond
	// rateWindow is the shortest interval a rate sample spans.
	rateWindow = time.Second
)

var (
	// ErrNotFound is returned for an unknown download ID.
	ErrNotFound = errors.New("no such download")
	// ErrNoSpace is returned when free disk is below size + DiskHeadroom.
	ErrNoSpace = errors.New("not enough free disk space")
	// ErrChecksum is returned when a file does not match its published SHA-256.
	ErrChecksum = errors.New("checksum mismatch")
	// ErrUnavailable is returned when the engine cannot download (disabled,
	// lms missing, no models folder).
	ErrUnavailable = errors.New("engine cannot download")
	// ErrBadRef is returned for a reference the engine refuses.
	ErrBadRef = errors.New("invalid model reference")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("download manager closed")
)

// Request asks for one download.
type Request struct {
	Engine string `json:"engine"` // lmstudio | ollama | llamacpp; "" = guess from Ref
	Ref    string `json:"ref"`    // LM Studio query, Ollama model name, or Hugging Face URL
	SHA256 string `json:"sha256,omitempty"`
}

// Item is one download as the API and the UI see it.
type Item struct {
	ID       string  `json:"id"`
	Engine   string  `json:"engine"`
	Ref      string  `json:"ref"`
	Name     string  `json:"name"` // short display name
	State    string  `json:"state"`
	Status   string  `json:"status,omitempty"` // the engine's own words ("pulling manifest"…)
	Bytes    int64   `json:"bytes"`
	Total    int64   `json:"total"`   // 0 = unknown
	Percent  float64 `json:"percent"` // 0..100 (from Bytes/Total, or reported directly)
	Rate     float64 `json:"rate"`    // bytes per second
	ETASec   int64   `json:"eta_sec"` // -1 = unknown
	Path     string  `json:"path,omitempty"`
	SHA256   string  `json:"sha256,omitempty"`
	Verified bool    `json:"verified"`
	Resumed  bool    `json:"resumed,omitempty"`
	Error    string  `json:"error,omitempty"`
	// Suggest lists references to try instead after a failure (GGUF builds
	// on Hugging Face of a model LM Studio could not find), as typed in the
	// Get models box.
	Suggest []string  `json:"suggest,omitempty"`
	Created time.Time `json:"created"`
	Started time.Time `json:"started,omitzero"`
	Ended   time.Time `json:"ended,omitzero"`
}

// Final reports whether the item has stopped for good.
func (it Item) Final() bool {
	return it.State == StateDone || it.State == StateFailed || it.State == StateCanceled
}

// Notice is the inbox entry for a finished item: kind "download", a level
// (info|warn), a title, a body and the in-app route.
func (it Item) Notice() (kind, level, title, body, link string) {
	kind, link = "download", "#/models"
	if it.State == StateDone {
		body = it.Name + " (" + engineLabel(it.Engine) + ") is ready. It was not loaded."
		if it.Path != "" {
			body += " Saved to " + it.Path + "."
		}
		return kind, "info", "Model downloaded", body, link
	}
	return kind, "warn", "Model download failed", it.Name + " (" + engineLabel(it.Engine) + "): " + it.Error, link
}

func engineLabel(e string) string {
	switch e {
	case EngineLMStudio:
		return "LM Studio"
	case EngineOllama:
		return "Ollama"
	case EngineLlamaCpp:
		return "llama.cpp"
	}
	return e
}

// Options configures a Manager.
type Options struct {
	// Config returns the live configuration (required).
	Config func() *config.Config
	Log    *slog.Logger
	// OnChange receives every state change and throttled progress (for the
	// event stream). It must not block.
	OnChange func(Item)
	// OnDone receives items that finished done or failed (not canceled).
	OnDone func(Item)
	// FreeSpace reports the bytes available under a path. An error means
	// unknown, and the disk check is skipped. Default: the file system's.
	FreeSpace func(path string) (uint64, error)
	// HTTP is the client for Ollama. Default: no overall timeout (pulls are
	// long); requests are bound to the download's context.
	HTTP *http.Client
	// LMSBinary resolves the lms executable ("" = none). Default: the
	// configured binary on PATH, then ~/.lmstudio/bin/lms.
	LMSBinary func() string
}

// Manager runs downloads.
type Manager struct {
	opt Options
	log *slog.Logger
	now func() time.Time

	// Hugging Face: the transport and the URL rule (tests swap both).
	hfTransport http.RoundTripper
	checkURL    func(u string, redirect bool) error

	mu      sync.Mutex
	closed  bool
	seq     int
	jobs    map[string]*job
	order   []string // creation order
	queues  map[string][]*job
	running map[string]*job
	wg      sync.WaitGroup
}

type job struct {
	it        Item
	req       Request
	cancel    context.CancelFunc // set while running
	canceled  bool
	discard   bool // canceled by the user: drop partial files (not on Close)
	lastEmit  time.Time
	emitTimer *time.Timer // a held-back progress update
	// rate sampling
	sampleAt    time.Time
	sampleBytes int64
	sampled     bool // the first byte count is known (a resume starts above 0)
}

// New returns a manager. Nothing runs until Add.
func New(opt Options) *Manager {
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	if opt.FreeSpace == nil {
		opt.FreeSpace = freeSpace
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{}
	}
	if opt.LMSBinary == nil {
		opt.LMSBinary = func() string { return findLMS(opt.Config().Engines.LMStudio.Binary) }
	}
	return &Manager{opt: opt, log: opt.Log, now: time.Now,
		hfTransport: http.DefaultTransport, checkURL: checkHFURL,
		jobs: map[string]*job{}, queues: map[string][]*job{}, running: map[string]*job{}}
}

// ParseRef turns a `cerberus pull` reference into a request: an https URL is
// a Hugging Face GGUF for llama.cpp; "ollama:<name>", "lms:<query>" or
// "lmstudio:<query>" pick the engine; anything else leaves Engine empty
// (Add then prefers LM Studio when lms exists, else Ollama).
func ParseRef(ref string) Request {
	ref = strings.TrimSpace(ref)
	low := strings.ToLower(ref)
	switch {
	case strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://"):
		return Request{Engine: EngineLlamaCpp, Ref: ref}
	case strings.HasPrefix(low, "ollama:"):
		return Request{Engine: EngineOllama, Ref: strings.TrimSpace(ref[len("ollama:"):])}
	case strings.HasPrefix(low, "lmstudio:"):
		return Request{Engine: EngineLMStudio, Ref: strings.TrimSpace(ref[len("lmstudio:"):])}
	case strings.HasPrefix(low, "lms:"):
		return Request{Engine: EngineLMStudio, Ref: strings.TrimSpace(ref[len("lms:"):])}
	}
	return Request{Ref: ref}
}

// Add validates req and queues it. A request equal to one still queued or
// running returns that item instead of a second copy.
func (m *Manager) Add(req Request) (Item, error) {
	req.Ref = strings.TrimSpace(req.Ref)
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	if req.Engine == "" {
		req = m.guessEngine(req)
	}
	name, err := m.validate(&req)
	if err != nil {
		return Item{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Item{}, ErrClosed
	}
	for _, id := range m.order {
		j := m.jobs[id]
		// A job being canceled is still "running" until its runner stops;
		// re-adding it queues a fresh one behind it instead.
		if !j.it.Final() && !j.canceled && j.req.Engine == req.Engine && j.req.Ref == req.Ref {
			return j.it, nil
		}
	}
	m.seq++
	now := m.now()
	j := &job{req: req, it: Item{ID: "dl-" + strconv.FormatInt(now.UnixMilli(), 36) + "-" + strconv.Itoa(m.seq),
		Engine: req.Engine, Ref: req.Ref, Name: name, State: StateQueued, ETASec: -1, SHA256: req.SHA256, Created: now}}
	m.jobs[j.it.ID] = j
	m.order = append(m.order, j.it.ID)
	m.queues[req.Engine] = append(m.queues[req.Engine], j)
	m.emitLocked(j, true)
	if m.running[req.Engine] == nil {
		m.startNextLocked(req.Engine)
	}
	m.pruneLocked()
	return j.it, nil
}

// guessEngine picks LM Studio when lms exists, else Ollama, for a reference
// without an engine.
func (m *Manager) guessEngine(req Request) Request {
	r := ParseRef(req.Ref)
	r.SHA256 = req.SHA256
	if r.Engine != "" {
		return r
	}
	c := m.opt.Config()
	if c.Engines.LMStudio.Enabled && m.opt.LMSBinary() != "" {
		r.Engine = EngineLMStudio
	} else {
		r.Engine = EngineOllama
	}
	return r
}

// validate checks req for its engine and returns the display name.
func (m *Manager) validate(req *Request) (string, error) {
	c := m.opt.Config()
	if req.SHA256 != "" && !isSHA256(req.SHA256) {
		return "", fmt.Errorf("%w: sha256 must be 64 hex characters", ErrBadRef)
	}
	if req.SHA256 != "" && req.Engine != EngineLlamaCpp {
		return "", fmt.Errorf("%w: sha256 is only checked for Hugging Face GGUF downloads", ErrBadRef)
	}
	switch req.Engine {
	case EngineLMStudio:
		if err := validLMSQuery(req.Ref); err != nil {
			return "", err
		}
		if !c.Engines.LMStudio.Enabled || m.opt.LMSBinary() == "" {
			return "", fmt.Errorf("%w: LM Studio's lms command was not found", ErrUnavailable)
		}
		// A Hugging Face repo link shows as owner/repo.
		if rest, ok := strings.CutPrefix(req.Ref, "https://huggingface.co/"); ok && rest != "" {
			return rest, nil
		}
		return req.Ref, nil
	case EngineOllama:
		if err := validOllamaName(req.Ref); err != nil {
			return "", err
		}
		if !c.Engines.Ollama.Enabled || c.Engines.Ollama.URL == "" {
			return "", fmt.Errorf("%w: Ollama is disabled", ErrUnavailable)
		}
		return req.Ref, nil
	case EngineLlamaCpp:
		src, err := parseHFURL(req.Ref)
		if err != nil {
			return "", err
		}
		if err := m.checkURL(src.url, false); err != nil {
			return "", err
		}
		if len(c.Engines.LlamaCpp.ModelDirs) == 0 || c.Engines.LlamaCpp.ModelDirs[0] == "" {
			return "", fmt.Errorf("%w: no llama.cpp models folder is configured", ErrUnavailable)
		}
		req.Ref = src.url
		return src.file, nil
	}
	return "", fmt.Errorf("%w: unknown engine %q", ErrBadRef, req.Engine)
}

// startNextLocked starts the runner for engine when its queue has work.
func (m *Manager) startNextLocked(engine string) {
	q := m.queues[engine]
	if len(q) == 0 || m.closed {
		return
	}
	j := q[0]
	m.queues[engine] = q[1:]
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.it.State = StateRunning
	j.it.Started = m.now()
	j.sampleAt = j.it.Started
	m.running[engine] = j
	m.emitLocked(j, true)
	m.wg.Add(1)
	go m.runJob(ctx, j)
}

// runJob runs one download, records the outcome and starts the engine's
// next item.
func (m *Manager) runJob(ctx context.Context, j *job) {
	defer m.wg.Done()
	var err error
	switch j.req.Engine {
	case EngineLMStudio:
		err = m.runLMS(ctx, j)
	case EngineOllama:
		err = m.runOllama(ctx, j)
	case EngineLlamaCpp:
		err = m.runHF(ctx, j)
	}
	j.cancel()

	m.mu.Lock()
	j.it.Ended = m.now()
	j.it.Rate, j.it.ETASec = 0, -1
	switch {
	case j.canceled && err != nil:
		j.it.State, j.it.Error = StateCanceled, ""
	case err != nil:
		j.it.State, j.it.Error = StateFailed, err.Error()
	default:
		j.it.State = StateDone
		if j.it.Total > 0 {
			j.it.Bytes, j.it.Percent = j.it.Total, 100
		}
	}
	it := j.it
	delete(m.running, j.req.Engine)
	m.emitLocked(j, true)
	m.startNextLocked(j.req.Engine)
	m.pruneLocked()
	m.mu.Unlock()

	if it.State == StateFailed {
		m.log.Warn("model download failed", "engine", it.Engine, "ref", it.Ref, "err", it.Error)
	}
	if it.State != StateCanceled && m.opt.OnDone != nil {
		m.opt.OnDone(it)
	}
}

// Cancel stops a queued or running download, or forgets a finished one.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}
	switch {
	case j.it.Final():
		delete(m.jobs, id)
		m.order = slices.DeleteFunc(m.order, func(s string) bool { return s == id })
	case j.it.State == StateQueued:
		m.queues[j.req.Engine] = slices.DeleteFunc(m.queues[j.req.Engine], func(x *job) bool { return x == j })
		j.canceled = true
		j.it.State, j.it.Ended, j.it.ETASec = StateCanceled, m.now(), -1
		m.emitLocked(j, true)
	default:
		j.canceled, j.discard = true, true
		j.cancel()
	}
	return nil
}

// Get returns one item.
func (m *Manager) Get(id string) (Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		return j.it, nil
	}
	return Item{}, ErrNotFound
}

// List returns every item, newest first.
func (m *Manager) List() []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		out = append(out, m.jobs[m.order[i]].it)
	}
	return out
}

// Close cancels everything and waits for the runners to stop.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, q := range m.queues {
		for _, j := range q {
			j.canceled = true
			j.it.State, j.it.Ended = StateCanceled, m.now()
		}
	}
	m.queues = map[string][]*job{}
	for _, j := range m.running {
		j.canceled = true
		j.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// pruneLocked drops the oldest finished items past keepFinished.
func (m *Manager) pruneLocked() {
	n := 0
	for _, id := range m.order {
		if m.jobs[id].it.Final() {
			n++
		}
	}
	for i := 0; n > keepFinished && i < len(m.order); {
		id := m.order[i]
		if m.jobs[id].it.Final() {
			delete(m.jobs, id)
			m.order = slices.Delete(m.order, i, i+1)
			n--
			continue
		}
		i++
	}
}

// emitLocked sends j to OnChange. Progress (force false) goes out at most
// every emitEvery per item; a held-back update is sent when the interval
// ends, so the last figure always arrives.
func (m *Manager) emitLocked(j *job, force bool) {
	if m.opt.OnChange == nil {
		return
	}
	now := m.now()
	if wait := emitEvery - now.Sub(j.lastEmit); !force && wait > 0 {
		if j.emitTimer == nil {
			j.emitTimer = time.AfterFunc(wait, func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if j.emitTimer != nil {
					j.emitTimer = nil
					m.emitLocked(j, true)
				}
			})
		}
		return
	}
	if j.emitTimer != nil {
		j.emitTimer.Stop()
		j.emitTimer = nil
	}
	j.lastEmit = now
	m.opt.OnChange(j.it)
}

// progress records bytes done of total (total <= 0 keeps the known total)
// and updates rate and ETA.
func (m *Manager) progress(j *job, done, total int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if total > 0 {
		j.it.Total = total
	}
	j.it.Bytes = done
	if j.it.Total > 0 {
		j.it.Percent = min(100, float64(done)*100/float64(j.it.Total))
	}
	now := m.now()
	if !j.sampled {
		// The first figure may include bytes already on disk (a resumed
		// .part, Ollama layers present): count only what moves from here.
		j.sampled = true
		j.sampleAt, j.sampleBytes = now, done
	} else if dt := now.Sub(j.sampleAt); dt >= rateWindow {
		inst := float64(done-j.sampleBytes) / dt.Seconds()
		if inst < 0 {
			inst = 0
		}
		if j.it.Rate == 0 {
			j.it.Rate = inst
		} else {
			j.it.Rate = 0.7*j.it.Rate + 0.3*inst // smoothed
		}
		j.sampleAt, j.sampleBytes = now, done
	}
	j.it.ETASec = -1
	if j.it.Rate > 0 && j.it.Total > 0 && j.it.Total >= done {
		j.it.ETASec = int64(float64(j.it.Total-done) / j.it.Rate)
	}
	m.emitLocked(j, false)
}

// percent records a progress figure the engine gives only as a percentage.
func (m *Manager) percent(j *job, pct float64) {
	m.mu.Lock()
	total := j.it.Total
	if total == 0 {
		j.it.Percent = min(100, max(0, pct))
		m.emitLocked(j, false)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.progress(j, int64(float64(total)*pct/100), total)
}

// update changes fields of j under the lock and emits the change.
func (m *Manager) update(j *job, force bool, f func(it *Item)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&j.it)
	m.emitLocked(j, force)
}

// discarded reports whether the user canceled j.
func (m *Manager) discarded(j *job) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return j.discard
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
