// Package voice turns recorded speech into task text, on this machine.
//
// The browser's MediaRecorder output (webm/opus, ogg/opus, mp4/aac) or a WAV
// is converted by ffmpeg into a 16 kHz mono WAV in a private temp dir, then
// handed to one backend:
//
//   - auto: Atrium's ~/.config/hypr/scripts/voice.py ("transcribe FILE --json",
//     local faster-whisper), else whisper-cli with the first ggml model in
//     ~/models or ~/.local/share/whisper, else the OpenAI-compatible URL when
//     one is set, else nothing.
//   - command: the configured argv, with {wav} replaced by the WAV path.
//   - openai: a multipart POST to {URL}/audio/transcriptions.
//   - off.
//
// Output is JSON {"text": ...} (an "error" field fails the call) or plain
// text, trimmed. The upload is first spooled to the private temp dir (bounded
// by Timeout, before any queueing, so a stalled upload holds nothing); then
// one transcription runs at a time, each bounded by Timeout.
// Nothing leaves the machine unless the user configures a non-local URL.
package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

const (
	// MaxBytes is the largest recording accepted (before conversion).
	MaxBytes = 25 << 20
	// Timeout bounds one transcription (conversion plus backend), and
	// separately the upload before it.
	Timeout = 120 * time.Second
	// lengthGrace is how far past MaxSeconds a recording may run and still
	// be accepted: a browser's stop timer fires late when the tab is hidden
	// or the phone locks, and losing the whole dictation for that is worse.
	lengthGrace = 1
	// wavBytesPerSecond is 16 kHz × 16-bit × mono.
	wavBytesPerSecond = 32000
	outputMax         = 1 << 20
	killGrace         = 2 * time.Second
)

// Backend names reported by Available.
const (
	BackendAtrium  = "atrium"
	BackendWhisper = "whisper-cli"
	BackendCommand = "command"
	BackendOpenAI  = "openai"
)

var (
	ErrUnavailable = errors.New("voice: no transcription backend")
	ErrTooLarge    = errors.New("voice: recording is larger than 25 MiB")
	ErrTooLong     = errors.New("voice: recording is longer than the limit")
	ErrNoAudio     = errors.New("voice: recording has no audio")
	ErrUnsupported = errors.New("voice: unsupported audio type")
)

// Option adjusts a Transcriber.
type Option func(*Transcriber)

// WithAPIKey supplies a bearer token for the openai backend (looked up on
// every request, so a vault change applies without a restart). An empty
// result sends no Authorization header, which suits local servers.
func WithAPIKey(key func() string) Option {
	return func(t *Transcriber) { t.apiKey = key }
}

// WithHTTPClient replaces the client used by the openai backend.
func WithHTTPClient(c *http.Client) Option {
	return func(t *Transcriber) { t.client = c }
}

// Transcriber converts and transcribes recordings. It is safe for concurrent
// use; calls are serialised.
type Transcriber struct {
	cfg    config.VoiceConfig
	log    *slog.Logger
	apiKey func() string
	client *http.Client
	slot   chan struct{} // capacity 1: the one transcription in flight
}

// New returns a Transcriber for cfg. Backends are resolved on each call, so
// installing whisper-cli or Atrium later needs no restart.
func New(cfg config.VoiceConfig, log *slog.Logger, opts ...Option) *Transcriber {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.MaxSeconds <= 0 {
		cfg.MaxSeconds = 300
	}
	if cfg.Backend == "" {
		cfg.Backend = "auto"
	}
	t := &Transcriber{cfg: cfg, log: log, client: &http.Client{}, slot: make(chan struct{}, 1)}
	for _, o := range opts {
		o(t)
	}
	return t
}

// MaxSeconds is the effective recording length limit.
func (t *Transcriber) MaxSeconds() int { return t.cfg.MaxSeconds }

// plan is a resolved backend: argv for command-style backends (with a {wav}
// placeholder), or the openai URL.
type plan struct {
	name string
	argv []string
	url  string
}

// Available reports whether a transcription can run now: true and the backend
// name, or false and the reason.
func (t *Transcriber) Available() (bool, string) {
	p, why := t.resolve()
	if p == nil {
		return false, why
	}
	return true, p.name
}

func (t *Transcriber) resolve() (*plan, string) {
	if !t.cfg.Enabled {
		return nil, "voice is disabled in config"
	}
	var p *plan
	switch t.cfg.Backend {
	case "off":
		return nil, "voice backend is off"
	case "command":
		if len(t.cfg.Command) == 0 {
			return nil, "voice.command is empty"
		}
		if _, err := exec.LookPath(t.cfg.Command[0]); err != nil {
			return nil, fmt.Sprintf("voice command %q not found", t.cfg.Command[0])
		}
		p = &plan{name: BackendCommand, argv: slices.Clone(t.cfg.Command)}
	case "openai":
		if t.cfg.URL == "" {
			return nil, "voice.url is empty"
		}
		p = &plan{name: BackendOpenAI, url: t.cfg.URL}
	case "auto":
		p = t.autoPlan()
		if p == nil {
			return nil, "no transcription backend found (Atrium voice.py, whisper-cli with a ggml model, or voice.url)"
		}
	default:
		return nil, fmt.Sprintf("unknown voice backend %q", t.cfg.Backend)
	}
	if _, err := exec.LookPath(t.cfg.FFmpeg); err != nil {
		return nil, fmt.Sprintf("ffmpeg not found (%s)", t.cfg.FFmpeg)
	}
	return p, ""
}

func (t *Transcriber) autoPlan() *plan {
	home, _ := os.UserHomeDir()
	if home != "" {
		script := filepath.Join(home, ".config", "hypr", "scripts", "voice.py")
		if fi, err := os.Stat(script); err == nil && fi.Mode().IsRegular() {
			if executable(fi) {
				return &plan{name: BackendAtrium, argv: []string{script, "transcribe", "{wav}", "--json"}}
			}
			if py, err := exec.LookPath("python3"); err == nil {
				return &plan{name: BackendAtrium, argv: []string{py, script, "transcribe", "{wav}", "--json"}}
			}
		}
	}
	if bin, err := exec.LookPath("whisper-cli"); err == nil {
		if model := findModel(home); model != "" {
			// whisper-cli assumes English unless told; "" in config means detect.
			lang := t.cfg.Language
			if lang == "" {
				lang = "auto"
			}
			return &plan{name: BackendWhisper, argv: []string{bin, "-m", model, "-f", "{wav}", "-nt", "-np", "-l", lang}}
		}
	}
	if t.cfg.URL != "" {
		return &plan{name: BackendOpenAI, url: t.cfg.URL}
	}
	return nil
}

func executable(fi os.FileInfo) bool {
	return runtime.GOOS == "windows" || fi.Mode().Perm()&0o111 != 0
}

// findModel returns the first ggml*.bin (by name) in ~/models, then in
// ~/.local/share/whisper.
func findModel(home string) string {
	if home == "" {
		return ""
	}
	for _, dir := range []string{filepath.Join(home, "models"), filepath.Join(home, ".local", "share", "whisper")} {
		entries, err := os.ReadDir(dir) // sorted by name
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "ggml") && strings.HasSuffix(n, ".bin") && !e.IsDir() {
				return filepath.Join(dir, n)
			}
		}
	}
	return ""
}

// Transcribe converts audio (of the given MIME type; "" lets ffmpeg probe it)
// and returns the transcript, trimmed.
func (t *Transcriber) Transcribe(ctx context.Context, audio io.Reader, mimeType string) (string, error) {
	p, why := t.resolve()
	if p == nil {
		return "", fmt.Errorf("%w: %s", ErrUnavailable, why)
	}
	kind, err := audioKind(mimeType)
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "cerberus-voice-")
	if err != nil {
		return "", fmt.Errorf("voice: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	_ = os.Chmod(dir, 0o700)

	// The upload is read to the end here, before the slot: ffmpeg then reads
	// a file, so nothing reads the request body after we return.
	input := filepath.Join(dir, "input."+kind)
	if err := spool(ctx, audio, input); err != nil {
		return "", err
	}

	select {
	case t.slot <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-t.slot }()

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	start := time.Now()
	wav := filepath.Join(dir, "speech.wav")
	if err := t.convert(ctx, input, wav); err != nil {
		return "", err
	}
	n, err := wavDataLen(wav)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrNoAudio
	}
	if n > int64(t.cfg.MaxSeconds+lengthGrace)*wavBytesPerSecond {
		return "", fmt.Errorf("%w (%d s)", ErrTooLong, t.cfg.MaxSeconds)
	}

	var text string
	if p.url != "" {
		text, err = t.postOpenAI(ctx, p.url, wav)
	} else {
		text, err = t.runCommand(ctx, p.argv, wav)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("voice: %s: %w", p.name, ctx.Err())
		}
		return "", err
	}
	t.log.Info("voice transcribed", "backend", p.name,
		"audio_s", float64(n)/wavBytesPerSecond, "took", time.Since(start).Round(time.Millisecond), "chars", len(text))
	return text, nil
}

// audioKind validates the MIME type: "mp4" for MP4-family containers, "audio"
// for everything else ffmpeg can probe. It names the spooled input file.
func audioKind(mimeType string) (string, error) {
	if mimeType == "" {
		return "audio", nil
	}
	mt, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrUnsupported, mimeType)
	}
	switch {
	case mt == "audio/mp4" || mt == "video/mp4" || mt == "audio/m4a" || mt == "audio/x-m4a" || mt == "audio/aac":
		return "mp4", nil
	case strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") || mt == "application/octet-stream" || mt == "application/ogg":
		return "audio", nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnsupported, mimeType)
}

// capped passes at most limit bytes through and remembers if there was more.
type capped struct {
	r     io.Reader
	left  int64
	over  bool
	total int64
}

func (c *capped) Read(b []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		if n, _ := io.ReadFull(c.r, one[:]); n > 0 {
			c.over = true
			return 0, ErrTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(b)) > c.left {
		b = b[:c.left]
	}
	n, err := c.r.Read(b)
	c.left -= int64(n)
	c.total += int64(n)
	return n, err
}

// ctxReader stops reading once ctx ends. A Read already blocked is ended by
// the HTTP handler's read deadline, which it sets to the same Timeout.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

// spool copies the upload (at most MaxBytes, within Timeout) to path, a new
// 0600 file in the private temp dir. MP4 keeps its index at the end, which
// only a seekable input reaches, and a file also lets ffmpeg run without a
// goroutine still reading the request body.
func spool(ctx context.Context, audio io.Reader, path string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("voice: spool: %w", err)
	}
	in := &capped{r: ctxReader{ctx, audio}, left: MaxBytes}
	_, err = io.Copy(f, in)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case in.over:
		return ErrTooLarge
	case err != nil && ctx.Err() != nil:
		return fmt.Errorf("voice: upload: %w", ctx.Err())
	case err != nil:
		return fmt.Errorf("voice: upload: %w", err)
	case in.total == 0:
		return ErrNoAudio
	}
	return nil
}

// convert runs ffmpeg from the spooled input into wav. Decoding stops just
// past the accepted length (-t), so a small, highly compressed upload cannot
// expand into gigabytes of PCM in a tmpfs /tmp before the length check.
func (t *Transcriber) convert(ctx context.Context, input, wav string) error {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", input, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le",
		"-t", strconv.Itoa(t.cfg.MaxSeconds + lengthGrace + 1), "-f", "wav", wav}
	var stderr tail
	if err := run(ctx, t.cfg.FFmpeg, args, nil, io.Discard, &stderr); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("voice: ffmpeg: %w", ctx.Err())
		}
		return fmt.Errorf("voice: ffmpeg could not convert the recording: %v%s", err, stderr.suffix())
	}
	return nil
}

// wavDataLen returns the number of bytes after the WAV "data" chunk header.
// It measures the file rather than trusting the size field, which a writer
// that could not seek leaves as 0 or 0xFFFFFFFF.
func wavDataLen(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("voice: converted audio missing: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return 0, errors.New("voice: converted audio is not a WAV")
	}
	off := int64(12)
	for off+8 <= fi.Size() {
		var ch [8]byte
		if _, err := f.ReadAt(ch[:], off); err != nil {
			break
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		if string(ch[0:4]) == "data" {
			return fi.Size() - off - 8, nil
		}
		off += 8 + size + size&1
	}
	return 0, errors.New("voice: converted audio has no data chunk")
}

// runCommand runs a command-style backend with {wav} substituted.
func (t *Transcriber) runCommand(ctx context.Context, argv []string, wav string) (string, error) {
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = strings.ReplaceAll(a, "{wav}", wav)
	}
	var stdout, stderr tail
	stdout.max = outputMax
	if err := run(ctx, args[0], args[1:], nil, &stdout, &stderr); err != nil {
		return "", fmt.Errorf("voice: %s failed: %v%s", filepath.Base(args[0]), err, stderr.suffix())
	}
	return parseOutput(stdout.Bytes())
}

// parseOutput accepts {"text": ...} (with "error" as a failure) or plain text.
func parseOutput(out []byte) (string, error) {
	s := strings.TrimSpace(string(out))
	if strings.HasPrefix(s, "{") {
		var v struct {
			Text  *string `json:"text"`
			Error any     `json:"error"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			if v.Error != nil && v.Error != "" && v.Error != false {
				if m, ok := v.Error.(map[string]any); ok && m["message"] != nil {
					return "", fmt.Errorf("voice: backend: %v", m["message"])
				}
				return "", fmt.Errorf("voice: backend: %v", v.Error)
			}
			if v.Text != nil {
				return strings.TrimSpace(*v.Text), nil
			}
		}
	}
	return s, nil
}

func (t *Transcriber) postOpenAI(ctx context.Context, base, wav string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if t.cfg.Model != "" {
		_ = mw.WriteField("model", t.cfg.Model)
	}
	if t.cfg.Language != "" {
		_ = mw.WriteField("language", t.cfg.Language)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="speech.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	f, err := os.Open(wav)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(part, f)
	f.Close()
	if err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/audio/transcriptions", &body)
	if err != nil {
		return "", fmt.Errorf("voice: url: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	if t.apiKey != nil {
		if k := t.apiKey(); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("voice: transcription server: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, outputMax))
	if err != nil {
		return "", fmt.Errorf("voice: transcription server: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return "", fmt.Errorf("voice: transcription server returned %s: %s", resp.Status, snippet)
	}
	return parseOutput(out)
}

// run starts name in its own process group and waits; on cancellation the
// whole group is terminated, so a backend's helpers cannot outlive it.
func run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	platform.NewGroup(cmd)
	done := make(chan struct{})
	defer close(done)
	cmd.Cancel = func() error {
		platform.Terminate(cmd.Process, killGrace, done)
		return nil
	}
	cmd.WaitDelay = killGrace + time.Second
	return cmd.Run()
}

// tail keeps the first max bytes written (default 4 KiB, for error messages)
// and discards the rest without failing the writer.
type tail struct {
	bytes.Buffer
	max int
}

func (w *tail) Write(b []byte) (int, error) {
	limit := w.max
	if limit == 0 {
		limit = 4 << 10
	}
	if room := limit - w.Len(); room > 0 {
		w.Buffer.Write(b[:min(len(b), room)])
	}
	return len(b), nil
}

func (w *tail) suffix() string {
	s := strings.TrimSpace(w.String())
	if s == "" {
		return ""
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 && len(s)-i > 1 {
		s = s[i+1:] // the last line is usually the reason
	}
	return ": " + s
}
