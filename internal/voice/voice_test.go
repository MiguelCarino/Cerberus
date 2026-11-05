package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// fmtChunk is a PCM "fmt " chunk body, preceded by its size.
type fmtChunk struct {
	Size                 uint32
	Format, Channels     uint16
	Rate, ByteRate       uint32
	BlockAlign, BitDepth uint16
}

// genWav returns a 16 kHz mono 16-bit WAV of the given length (a quiet ramp).
func genWav(seconds float64) []byte {
	n := int(seconds * 16000)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+2*n))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, fmtChunk{16, 1, 1, 16000, 32000, 2, 16})
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(2*n))
	for i := range n {
		_ = binary.Write(&b, binary.LittleEndian, int16((i%200)*20-2000))
	}
	return b.Bytes()
}

// sandbox gives the test a private HOME, TMPDIR and a PATH holding only bin
// (plus the dirs of sh/cat for the fake scripts), and returns bin.
func sandbox(t *testing.T) (home, bin, tmp string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake backends are shell scripts")
	}
	root := t.TempDir()
	home, bin, tmp = filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "tmp")
	for _, d := range []string{home, bin, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", tmp)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	return home, bin, tmp
}

func script(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeFFmpeg copies the -i input to the last argument (input WAVs pass
// through) and records its arguments in ffmpeg.args next to itself.
func fakeFFmpeg(t *testing.T, bin string) string {
	return script(t, filepath.Join(bin, "ffmpeg"), `printf '%s\n' "$*" > "$(dirname "$0")/ffmpeg.args"
while [ $# -gt 0 ]; do
	[ "$1" = -i ] && in=$2
	out=$1
	shift
done
cat "$in" > "$out"
`)
}

func cfg() config.VoiceConfig {
	return config.VoiceConfig{Enabled: true, Backend: "auto", FFmpeg: "ffmpeg", MaxSeconds: 300}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("temp dir not cleaned: %d entries left in %s", len(entries), dir)
	}
}

func TestCommandBackend(t *testing.T) {
	_, bin, tmp := sandbox(t)
	fakeFFmpeg(t, bin)
	log := filepath.Join(t.TempDir(), "args")
	cmd := script(t, filepath.Join(bin, "stt"), `echo "$@" > `+log+`
wav=$2
head -c 4 "$wav" | grep -q RIFF || { echo "not a wav" >&2; exit 3; }
echo '  hello from the fake  '
`)
	c := cfg()
	c.Backend, c.Command = "command", []string{cmd, "--in", "{wav}", "--lang=x"}
	tr := New(c, nil)
	if ok, name := tr.Available(); !ok || name != BackendCommand {
		t.Fatalf("Available = %v %q", ok, name)
	}
	text, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.5)), "audio/wav")
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello from the fake" {
		t.Fatalf("text = %q", text)
	}
	args, _ := os.ReadFile(log)
	f := strings.Fields(string(args))
	if len(f) != 3 || f[0] != "--in" || !strings.HasSuffix(f[1], ".wav") || f[2] != "--lang=x" {
		t.Fatalf("argv = %q", args)
	}
	assertEmpty(t, tmp)

	// JSON output, and a JSON error.
	script(t, cmd, `echo '{"text":" json text ","language":"en"}'`)
	if text, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.2)), ""); err != nil || text != "json text" {
		t.Fatalf("json: %q %v", text, err)
	}
	script(t, cmd, `echo '{"error":"the voice daemon is not running"}'`)
	if _, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.2)), ""); err == nil || !strings.Contains(err.Error(), "daemon is not running") {
		t.Fatalf("json error: %v", err)
	}
	script(t, cmd, `echo boom >&2; exit 2`)
	if _, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.2)), ""); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("exit error: %v", err)
	}
	assertEmpty(t, tmp)
}

func TestOpenAIBackend(t *testing.T) {
	_, bin, tmp := sandbox(t)
	fakeFFmpeg(t, bin)
	wav := genWav(0.3)
	var got struct {
		path, auth, model, language, filename, ctype string
		file                                         []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "multipart/form-data" {
			http.Error(w, "want multipart", 400)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "model":
				got.model = string(b)
			case "language":
				got.language = string(b)
			case "file":
				got.filename, got.ctype, got.file = p.FileName(), p.Header.Get("Content-Type"), b
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"  transcribed  "}`)
	}))
	defer srv.Close()

	c := cfg()
	c.Backend, c.URL, c.Model, c.Language = "openai", srv.URL+"/v1/", "whisper-1", "es"
	tr := New(c, nil, WithAPIKey(func() string { return "sk-test" }))
	text, err := tr.Transcribe(context.Background(), bytes.NewReader(wav), "audio/webm;codecs=opus")
	if err != nil {
		t.Fatal(err)
	}
	if text != "transcribed" {
		t.Fatalf("text = %q", text)
	}
	if got.path != "/v1/audio/transcriptions" || got.auth != "Bearer sk-test" || got.model != "whisper-1" ||
		got.language != "es" || got.filename != "speech.wav" || got.ctype != "audio/wav" || !bytes.Equal(got.file, wav) {
		t.Fatalf("request: path=%q auth=%q model=%q lang=%q file=%q/%q len=%d",
			got.path, got.auth, got.model, got.language, got.filename, got.ctype, len(got.file))
	}
	assertEmpty(t, tmp)

	// No key, no language: neither is sent; a server error surfaces.
	c.Language = ""
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected Authorization header")
		}
		_ = r.ParseMultipartForm(1 << 20)
		if _, ok := r.MultipartForm.Value["language"]; ok {
			t.Error("language sent although unset")
		}
		http.Error(w, `{"error":{"message":"model not loaded"}}`, http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	c.URL = srv2.URL + "/v1"
	_, err = New(c, nil).Transcribe(context.Background(), bytes.NewReader(wav), "audio/wav")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("want 503 error, got %v", err)
	}
}

func TestAutoSelection(t *testing.T) {
	home, bin, _ := sandbox(t)
	tr := New(cfg(), nil)

	// Nothing installed (and no ffmpeg either).
	if ok, why := tr.Available(); ok || !strings.Contains(why, "no transcription backend") {
		t.Fatalf("empty: %v %q", ok, why)
	}

	// whisper-cli alone is not enough without a model.
	script(t, filepath.Join(bin, "whisper-cli"), `echo " from whisper"`)
	if ok, _ := tr.Available(); ok {
		t.Fatal("whisper-cli without a model should not be available")
	}
	// A URL is the last resort.
	c := cfg()
	c.URL = "http://127.0.0.1:1/v1"
	fakeFFmpeg(t, bin)
	if ok, name := New(c, nil).Available(); !ok || name != BackendOpenAI {
		t.Fatalf("url fallback: %v %q", ok, name)
	}

	// A model in ~/.local/share/whisper; ~/models wins when both exist.
	share := filepath.Join(home, ".local", "share", "whisper")
	_ = os.MkdirAll(share, 0o755)
	_ = os.WriteFile(filepath.Join(share, "ggml-small.bin"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(share, "readme.txt"), nil, 0o644)
	if ok, name := tr.Available(); !ok || name != BackendWhisper {
		t.Fatalf("whisper: %v %q", ok, name)
	}
	models := filepath.Join(home, "models")
	_ = os.MkdirAll(models, 0o755)
	_ = os.WriteFile(filepath.Join(models, "ggml-base.en.bin"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(models, "llama.gguf"), nil, 0o644)
	if got := findModel(home); got != filepath.Join(models, "ggml-base.en.bin") {
		t.Fatalf("findModel = %q", got)
	}
	p, _ := tr.resolve()
	if want := []string{filepath.Join(bin, "whisper-cli"), "-m", filepath.Join(models, "ggml-base.en.bin"), "-f", "{wav}", "-nt", "-np", "-l", "auto"}; strings.Join(p.argv, " ") != strings.Join(want, " ") {
		t.Fatalf("whisper argv = %q", p.argv)
	}
	if text, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.2)), "audio/wav"); err != nil || text != "from whisper" {
		t.Fatalf("whisper transcribe: %q %v", text, err)
	}

	// Atrium's voice.py comes first; called as "transcribe FILE --json".
	log := filepath.Join(t.TempDir(), "args")
	script(t, filepath.Join(home, ".config", "hypr", "scripts", "voice.py"), `echo "$@" > `+log+`
test -s "$2" || exit 4
echo '{"text": "from atrium", "language": "en", "audio_s": 0.2, "secs": 0.1, "device": "cpu", "model": "small"}'
`)
	if ok, name := tr.Available(); !ok || name != BackendAtrium {
		t.Fatalf("atrium: %v %q", ok, name)
	}
	text, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.2)), "audio/wav")
	if err != nil || text != "from atrium" {
		t.Fatalf("atrium transcribe: %q %v", text, err)
	}
	args, _ := os.ReadFile(log)
	if f := strings.Fields(string(args)); len(f) != 3 || f[0] != "transcribe" || !strings.HasSuffix(f[1], ".wav") || f[2] != "--json" {
		t.Fatalf("atrium argv = %q", args)
	}

	// off and disabled win over everything.
	c = cfg()
	c.Backend = "off"
	if ok, _ := New(c, nil).Available(); ok {
		t.Fatal("off should be unavailable")
	}
	c = cfg()
	c.Enabled = false
	if ok, _ := New(c, nil).Available(); ok {
		t.Fatal("disabled should be unavailable")
	}
	if _, err := New(c, nil).Transcribe(context.Background(), strings.NewReader("x"), ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled Transcribe: %v", err)
	}
}

func TestLimits(t *testing.T) {
	_, bin, tmp := sandbox(t)
	fakeFFmpeg(t, bin)
	stt := script(t, filepath.Join(bin, "stt"), `echo ok`)
	c := cfg()
	c.Backend, c.Command, c.MaxSeconds = "command", []string{stt, "{wav}"}, 1
	tr := New(c, nil)

	if text, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(1)), "audio/wav"); err != nil || text != "ok" {
		t.Fatalf("1 s at a 1 s limit: %q %v", text, err)
	}
	// A stop timer that fired late (hidden tab) is tolerated…
	if text, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(1.8)), "audio/wav"); err != nil || text != "ok" {
		t.Fatalf("1.8 s at a 1 s limit (grace): %q %v", text, err)
	}
	// …and decoding is bounded just past the accepted length.
	if args, _ := os.ReadFile(filepath.Join(bin, "ffmpeg.args")); !strings.Contains(string(args), " -t 3 ") {
		t.Fatalf("ffmpeg must stop decoding past the limit: %s", args)
	}
	if _, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(2.5)), "audio/wav"); !errors.Is(err, ErrTooLong) {
		t.Fatalf("too long: %v", err)
	}
	big := io.MultiReader(bytes.NewReader(genWav(0)), io.LimitReader(zero{}, MaxBytes))
	if _, err := tr.Transcribe(context.Background(), big, "audio/wav"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	big = io.MultiReader(bytes.NewReader(genWav(0)), io.LimitReader(zero{}, MaxBytes))
	if _, err := tr.Transcribe(context.Background(), big, "audio/mp4"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large mp4: %v", err)
	}
	if _, err := tr.Transcribe(context.Background(), bytes.NewReader(nil), "audio/wav"); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.1)), "text/html"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("mime: %v", err)
	}
	assertEmpty(t, tmp)
}

type zero struct{}

func (zero) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestOneAtATime(t *testing.T) {
	_, bin, _ := sandbox(t)
	fakeFFmpeg(t, bin)
	lock := filepath.Join(t.TempDir(), "lock")
	stt := script(t, filepath.Join(bin, "stt"), `mkdir `+lock+` 2>/dev/null || { echo overlap; exit 0; }
sleep 0.2
rmdir `+lock+`
echo ok
`)
	c := cfg()
	c.Backend, c.Command = "command", []string{stt}
	tr := New(c, nil)
	var wg sync.WaitGroup
	results := make([]string, 3)
	for i := range results {
		wg.Go(func() {
			text, err := tr.Transcribe(context.Background(), bytes.NewReader(genWav(0.1)), "")
			if err != nil {
				text = err.Error()
			}
			results[i] = text
		})
	}
	wg.Wait()
	for _, r := range results {
		if r != "ok" {
			t.Fatalf("results = %q", results)
		}
	}
}

func TestCancelKillsBackend(t *testing.T) {
	_, bin, tmp := sandbox(t)
	fakeFFmpeg(t, bin)
	stt := script(t, filepath.Join(bin, "stt"), "exec sleep 30\n")
	c := cfg()
	c.Backend, c.Command = "command", []string{stt}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(c, nil).Transcribe(ctx, bytes.NewReader(genWav(0.1)), "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("backend outlived the deadline: %v", d)
	}
	assertEmpty(t, tmp)
}

// TestFFmpegConversion runs the real ffmpeg on a generated 44.1 kHz stereo
// WAV and checks the 16 kHz mono result reaches the backend.
func TestFFmpegConversion(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	_, bin, tmp := sandbox(t)
	// Stereo 44.1 kHz, 0.5 s.
	n := 22050
	var in bytes.Buffer
	in.WriteString("RIFF")
	_ = binary.Write(&in, binary.LittleEndian, uint32(36+4*n))
	in.WriteString("WAVEfmt ")
	_ = binary.Write(&in, binary.LittleEndian, fmtChunk{16, 1, 2, 44100, 176400, 4, 16})
	in.WriteString("data")
	_ = binary.Write(&in, binary.LittleEndian, uint32(4*n))
	for i := range n {
		v := int16((i % 100) * 300)
		_ = binary.Write(&in, binary.LittleEndian, []int16{v, -v})
	}
	out := filepath.Join(t.TempDir(), "seen.wav")
	stt := script(t, filepath.Join(bin, "stt"), `cp "$1" `+out+`; echo converted`)
	c := cfg()
	c.Backend, c.Command, c.FFmpeg = "command", []string{stt, "{wav}"}, ff
	text, err := New(c, nil).Transcribe(context.Background(), &in, "audio/wav")
	if err != nil || text != "converted" {
		t.Fatalf("%q %v", text, err)
	}
	b, err := os.ReadFile(out)
	if err != nil || len(b) < 44 {
		t.Fatalf("converted wav: %v (%d bytes)", err, len(b))
	}
	channels := binary.LittleEndian.Uint16(b[22:24])
	rate := binary.LittleEndian.Uint32(b[24:28])
	if channels != 1 || rate != 16000 {
		t.Fatalf("converted to %d ch at %d Hz", channels, rate)
	}
	dl, _ := wavDataLen(out)
	if dl < 15000 || dl > 17000 { // 0.5 s × 32000 B/s
		t.Fatalf("data length %d", dl)
	}
	assertEmpty(t, tmp)
}

// TestFFmpegBrowserFormats feeds the real ffmpeg what browsers record:
// webm/opus through the pipe, and mp4/aac through the spooled file.
func TestFFmpegBrowserFormats(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	_, bin, tmp := sandbox(t)
	src := filepath.Join(t.TempDir(), "src.wav")
	if err := os.WriteFile(src, genWav(0.5), 0o600); err != nil {
		t.Fatal(err)
	}
	stt := script(t, filepath.Join(bin, "stt"), `echo ok`)
	c := cfg()
	c.Backend, c.Command, c.FFmpeg = "command", []string{stt, "{wav}"}, ff
	tr := New(c, nil)
	for _, f := range []struct{ ext, mime string }{{"webm", "audio/webm;codecs=opus"}, {"mp4", "audio/mp4"}} {
		t.Run(f.ext, func(t *testing.T) {
			enc := filepath.Join(t.TempDir(), "rec."+f.ext)
			if out, err := exec.Command(ff, "-nostdin", "-loglevel", "error", "-i", src, enc).CombinedOutput(); err != nil {
				t.Skipf("this ffmpeg cannot encode %s: %s", f.ext, out)
			}
			b, _ := os.ReadFile(enc)
			if text, err := tr.Transcribe(context.Background(), bytes.NewReader(b), f.mime); err != nil || text != "ok" {
				t.Fatalf("%q %v", text, err)
			}
		})
	}
	assertEmpty(t, tmp)
}

func TestParseOutput(t *testing.T) {
	for in, want := range map[string]string{
		"  plain  \n":             "plain",
		`{"text":"a"}`:            "a",
		`{"text":""}`:             "",
		`{not json`:               "{not json",
		`{"text":"b","error":""}`: "b",
	} {
		if got, err := parseOutput([]byte(in)); err != nil || got != want {
			t.Errorf("parseOutput(%q) = %q %v", in, got, err)
		}
	}
	if _, err := parseOutput([]byte(`{"error":{"message":"bad"}}`)); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("nested error: %v", err)
	}
}

// stalled yields some bytes, then blocks until released (an upload from a
// phone on a flaky link).
type stalled struct {
	head    []byte
	release chan struct{}
}

func (s *stalled) Read(b []byte) (int, error) {
	if len(s.head) > 0 {
		n := copy(b, s.head)
		s.head = s.head[n:]
		return n, nil
	}
	<-s.release
	return 0, io.ErrUnexpectedEOF
}

// A stalled upload is spooled before the transcription slot is taken, so
// another recording is transcribed meanwhile.
func TestStalledUploadDoesNotHoldSlot(t *testing.T) {
	_, bin, tmp := sandbox(t)
	fakeFFmpeg(t, bin)
	stt := script(t, filepath.Join(bin, "stt"), `echo ok`)
	c := cfg()
	c.Backend, c.Command = "command", []string{stt, "{wav}"}
	tr := New(c, nil)
	slow := &stalled{head: genWav(0.5)[:100], release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := tr.Transcribe(context.Background(), slow, "audio/mp4")
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if text, err := tr.Transcribe(ctx, bytes.NewReader(genWav(0.2)), "audio/webm"); err != nil || text != "ok" {
		t.Fatalf("second recording waited behind a stalled upload: %q %v", text, err)
	}
	close(slow.release)
	if err := <-done; err == nil {
		t.Fatal("a broken upload should fail")
	}
	assertEmpty(t, tmp)
}
