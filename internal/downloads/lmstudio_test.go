//go:build !windows

package downloads

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeLMS writes an lms script that logs its argv and runs body.
func fakeLMS(t *testing.T, body string) (bin, argsLog string) {
	t.Helper()
	dir := t.TempDir()
	argsLog = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "lms")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsLog + "'\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsLog
}

func TestLMSGet(t *testing.T) {
	bin, argsLog := fakeLMS(t, `printf 'Searching...\n'
printf '\033[32mDownloading\033[0m  10.00%% | 0.50 GB / 5.00 GB\r'
printf 'Downloading  60.00%% | 3.00 GB / 5.00 GB\r'
printf 'Downloading 100.00%% | 5.00 GB / 5.00 GB\n'
printf 'Download complete\n'
exit 0
`)
	e := newEnv(t, func(o *Options) { o.LMSBinary = func() string { return bin } })
	it, err := e.m.Add(Request{Ref: "qwen/qwen3-8b"}) // no engine: lms exists, so LM Studio
	if err != nil {
		t.Fatal(err)
	}
	if it.Engine != EngineLMStudio {
		t.Fatalf("engine %q", it.Engine)
	}
	it = e.final(it.ID)
	if it.State != StateDone || it.Total != 5e9 || it.Percent != 100 {
		t.Fatalf("final %+v", it)
	}
	args, _ := os.ReadFile(argsLog)
	if string(args) != "get\nqwen/qwen3-8b\n--yes\n" {
		t.Fatalf("argv %q", args)
	}
}

func TestLMSFailureAndRefusals(t *testing.T) {
	bin, _ := fakeLMS(t, "echo 'Error: No models found for query' >&2\nexit 3\n")
	e := newEnv(t, func(o *Options) { o.LMSBinary = func() string { return bin } })
	it, err := e.m.Add(Request{Engine: EngineLMStudio, Ref: "no-such-model"})
	if err != nil {
		t.Fatal(err)
	}
	if it = e.final(it.ID); it.State != StateFailed || !strings.Contains(it.Error, "No models found") {
		t.Fatalf("final %+v", it)
	}
	for _, bad := range []string{"", "--help", "a\nb", strings.Repeat("x", 301)} {
		if _, err := e.m.Add(Request{Engine: EngineLMStudio, Ref: bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLMSCancel(t *testing.T) {
	bin, _ := fakeLMS(t, "printf 'Downloading 5.00%% | 0.10 GB / 2.00 GB\\n'\nexec /bin/sleep 30\n")
	e := newEnv(t, func(o *Options) { o.LMSBinary = func() string { return bin } })
	it, err := e.m.Add(Request{Engine: EngineLMStudio, Ref: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	e.wait(it.ID, func(it Item) bool { return it.Bytes > 0 })
	e.m.Cancel(it.ID)
	if it = e.final(it.ID); it.State != StateCanceled {
		t.Fatalf("final %+v", it)
	}
}

func TestParseLMSLine(t *testing.T) {
	p := parseLMSLine("\x1b[2K⠋ 45.3% [=====>    ] 1.5 GiB / 3 GiB | 20 MB/s")
	if !p.hasSize || p.bytes != 1.5*(1<<30) || p.total != 3<<30 || !p.hasPct || p.pct != 45.3 {
		t.Fatalf("%+v", p)
	}
	if p := parseLMSLine("Resolving model"); p.hasSize || p.hasPct {
		t.Fatalf("%+v", p)
	}
}

// An owner/name LM Studio's catalogue does not know is tried as a Hugging
// Face repo; when that fails too, GGUF builds are suggested.
func TestLMSHuggingFaceFallback(t *testing.T) {
	notFound := `case "$2" in https://*) ;; *) echo 'Error: Failed to resolve artifact "a/b": The artifact does not exist or you do not have permission to read it' >&2; exit 1;; esac
`
	// The repo URL works.
	bin, argsLog := fakeLMS(t, notFound+"printf 'Download complete\\n'\nexit 0\n")
	e := newEnv(t, func(o *Options) { o.LMSBinary = func() string { return bin } })
	it, err := e.m.Add(Request{Engine: EngineLMStudio, Ref: "deepseek-ai/DeepSeek-V4.1-Flash"})
	if err != nil {
		t.Fatal(err)
	}
	if it = e.final(it.ID); it.State != StateDone {
		t.Fatalf("final %+v", it)
	}
	if args, _ := os.ReadFile(argsLog); string(args) != "get\nhttps://huggingface.co/deepseek-ai/DeepSeek-V4.1-Flash\n--yes\n" {
		t.Fatalf("argv %q", args)
	}

	// The repo URL fails as well (safetensors only): suggestions.
	bin2, _ := fakeLMS(t, notFound+"echo 'Error: No compatible download options' >&2\nexit 2\n")
	e2 := newEnv(t, func(o *Options) { o.LMSBinary = func() string { return bin2 } })
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"id":"unsloth/DeepSeek-V4.1-Flash-GGUF"},{"id":"bad id/x"},{"id":"bartowski/DeepSeek-V4.1-Flash-GGUF"}]`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	e2.m.hfTransport = rewrite{target: u, rt: srv.Client().Transport}
	it, err = e2.m.Add(Request{Engine: EngineLMStudio, Ref: "deepseek-ai/DeepSeek-V4.1-Flash"})
	if err != nil {
		t.Fatal(err)
	}
	it = e2.final(it.ID)
	want := []string{"lms https://huggingface.co/unsloth/DeepSeek-V4.1-Flash-GGUF", "lms https://huggingface.co/bartowski/DeepSeek-V4.1-Flash-GGUF"}
	if it.State != StateFailed || !slices.Equal(it.Suggest, want) || !strings.Contains(it.Error, "No compatible download options") || !strings.Contains(it.Error, "unsloth/") {
		t.Fatalf("final %+v", it)
	}
	if !strings.Contains(query, "filter=gguf") || !strings.Contains(query, "search=DeepSeek-V4.1-Flash") {
		t.Fatalf("query %q", query)
	}

	// A plain name (not owner/name) is not retried.
	if it, err = e2.m.Add(Request{Engine: EngineLMStudio, Ref: "gemma"}); err != nil {
		t.Fatal(err)
	}
	if it = e2.final(it.ID); it.State != StateFailed || len(it.Suggest) != 0 || !strings.Contains(it.Error, "Failed to resolve") {
		t.Fatalf("plain name %+v", it)
	}
}
