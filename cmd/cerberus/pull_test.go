package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/notify"
)

// pullDaemon is the real API with the real download manager, a fake
// Ollama and an inbox fed through a (silent) notifier, wired like serve.
type pullDaemon struct {
	cfgPath string
	inbox   *inbox.Store
	mgr     *downloads.Manager
	// release lets the fake Ollama finish a pull ("" = finish at once).
	release chan struct{}
}

func newPullDaemon(t *testing.T, ollamaBody string, hold bool) *pullDaemon {
	t.Helper()
	d := &pullDaemon{}
	if hold {
		d.release = make(chan struct{})
	}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"pulling manifest"}`)
		w.(http.Flusher).Flush()
		if d.release != nil {
			select {
			case <-d.release:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, ollamaBody)
	}))
	t.Cleanup(ollama.Close)

	cfg := config.Defaults()
	cfg.Server.Token = "tok"
	cfg.Engines.Ollama.Enabled, cfg.Engines.Ollama.URL = true, ollama.URL
	cfg.Engines.LMStudio.Enabled = false
	cfg.Notify.Disabled = true // the inbox still gets every message
	ib, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ib.Close() })
	n := notify.New(cfg.Notify, slog.New(slog.DiscardHandler))
	n.SetInbox(ib)
	feed := api.NewDownloadFeed()
	d.mgr = newDownloads(newLiveConfig(cfg), n, feed, slog.New(slog.DiscardHandler))
	t.Cleanup(d.mgr.Close)
	srv := httptest.NewServer(api.New(api.Deps{Cfg: cfg, Log: slog.New(slog.DiscardHandler), Version: "test",
		Inbox: ib, Downloads: d.mgr, DownloadFeed: feed}))
	t.Cleanup(srv.Close)
	d.inbox = ib
	d.cfgPath = writeConfig(t, strings.TrimPrefix(srv.URL, "http://"))
	return d
}

// waitInbox waits for the inbox to hold n items.
func (d *pullDaemon) waitInbox(t *testing.T, n int) []inbox.Item {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if items := d.inbox.List(false, 0); len(items) >= n || time.Now().After(deadline) {
			return items
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPullFollowsToDone(t *testing.T) {
	d := newPullDaemon(t, `{"status":"downloading","digest":"sha256:a","total":2048,"completed":1024}
{"status":"downloading","digest":"sha256:a","total":2048,"completed":2048}
{"status":"success"}
`, false)
	var out, errb bytes.Buffer
	if code := run([]string{"pull", "-config", d.cfgPath, "ollama:tiny:1b"}, nil, &out, &errb); code != 0 {
		t.Fatalf("pull: %d %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "tiny:1b (ollama) ") || !strings.Contains(out.String(), "done: tiny:1b (not loaded)") {
		t.Fatalf("output:\n%s", out.String())
	}
	items := d.waitInbox(t, 1)
	if len(items) != 1 || items[0].Kind != "download" || items[0].Title != "Model downloaded" || items[0].Link != "#/models" ||
		items[0].Level != inbox.LevelInfo {
		t.Fatalf("inbox: %+v", items)
	}
}

func TestPullReportsFailure(t *testing.T) {
	d := newPullDaemon(t, `{"error":"pull model manifest: file does not exist"}`+"\n", false)
	var out, errb bytes.Buffer
	if code := run([]string{"pull", "-config", d.cfgPath, "--engine", "ollama", "nope:1b"}, nil, &out, &errb); code != 1 {
		t.Fatalf("pull: %d %s", code, out.String())
	}
	if !strings.Contains(errb.String(), "download failed: Ollama: pull model manifest") {
		t.Fatalf("stderr: %s", errb.String())
	}
	if items := d.waitInbox(t, 1); len(items) != 1 || items[0].Level != inbox.LevelWarn {
		t.Fatalf("inbox: %+v", items)
	}
}

func TestPullDetachAndCancel(t *testing.T) {
	d := newPullDaemon(t, `{"status":"success"}`+"\n", true)
	var out, errb bytes.Buffer
	if code := run([]string{"pull", "-config", d.cfgPath, "--detach", "ollama:slow:1b"}, nil, &out, &errb); code != 0 {
		t.Fatalf("detach: %d %s", code, errb.String())
	}
	id, _, _ := strings.Cut(out.String(), ":")
	if !strings.HasPrefix(id, "dl-") {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	if code := run([]string{"pull", "-config", d.cfgPath, "--cancel", id}, nil, &out, &errb); code != 0 {
		t.Fatalf("cancel: %d %s", code, errb.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		it, err := d.mgr.Get(id)
		if err == nil && it.State == downloads.StateCanceled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not canceled: %+v %v", it, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := run([]string{"pull", "-config", d.cfgPath, "--cancel", "dl-none"}, nil, &out, &errb); code != 1 {
		t.Fatalf("cancel unknown: %d", code)
	}
	close(d.release)
}

func TestPullUsageAndRefusals(t *testing.T) {
	d := newPullDaemon(t, "", false)
	var out, errb bytes.Buffer
	if code := run([]string{"pull", "-config", d.cfgPath}, nil, &out, &errb); code != 2 {
		t.Fatalf("no ref: %d", code)
	}
	errb.Reset()
	// LM Studio is disabled: the daemon refuses with 409 and pull says why.
	if code := run([]string{"pull", "-config", d.cfgPath, "lms:qwen3-8b"}, nil, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "lms command was not found") {
		t.Fatalf("refused: %d %s", code, errb.String())
	}
}

func TestReadSSE(t *testing.T) {
	in := ": ping\n\nevent: download\ndata: {\"id\":\"a\"}\n\nevent: x\ndata: one\ndata: two\n\nevent: stop\ndata: 1\n\nevent: after\ndata: 2\n\n"
	var got []string
	readSSE(strings.NewReader(in), func(ev, data string) bool {
		got = append(got, ev+"="+data)
		return ev != "stop"
	})
	if strings.Join(got, "|") != `download={"id":"a"}|x=one`+"\n"+`two|stop=1` {
		t.Fatalf("got %q", got)
	}
}

func TestProgressLine(t *testing.T) {
	it := downloads.Item{State: "running", Percent: 50, Bytes: 1 << 30, Total: 2 << 30, Rate: 10 << 20, ETASec: 102, Status: "downloading"}
	if got := progressLine(it); got != "running 50.0% 1.0 GiB / 2.0 GiB 10.0 MiB/s ETA 1m42s downloading" {
		t.Fatalf("got %q", got)
	}
	if got := progressLine(downloads.Item{State: "queued", ETASec: -1}); got != "queued" {
		t.Fatalf("queued: %q", got)
	}
	if humanBytes(512) != "512 B" || humanBytes(1536) != "1.5 KiB" {
		t.Fatal(humanBytes(512), humanBytes(1536))
	}
	var buf bytes.Buffer
	p := &pullPrinter{w: &buf}
	for _, pct := range []float64{1, 2, 11, 12, 25} {
		p.show(downloads.Item{State: "running", Percent: pct, ETASec: -1})
	}
	if n := strings.Count(buf.String(), "\n"); n != 3 { // 1 %, 11 %, 25 %
		t.Fatalf("lines %d:\n%s", n, buf.String())
	}
}

func TestDownloadMessage(t *testing.T) {
	m := downloadMessage(downloads.Item{ID: "dl-1", Engine: downloads.EngineOllama, Name: "qwen", State: downloads.StateDone})
	if m.Kind != "download" || m.Level != "info" || m.Link != "#/models" || m.SessionID != "download:dl-1" ||
		!strings.Contains(m.Body, "not loaded") {
		t.Fatalf("done: %+v", m)
	}
	kind, _, link := notify.Classify(m)
	if kind != "download" || link != "#/models" {
		t.Fatalf("classified as %s %s", kind, link)
	}
	m = downloadMessage(downloads.Item{ID: "dl-2", Name: "x", State: downloads.StateFailed, Error: "boom"})
	if m.Level != "warn" || !strings.Contains(m.Body, "boom") {
		t.Fatalf("failed: %+v", m)
	}
}

func TestPullSHA256OnlyForGGUF(t *testing.T) {
	d := newPullDaemon(t, "", false)
	var out, errb bytes.Buffer
	if code := run([]string{"pull", "-config", d.cfgPath, "--sha256", strings.Repeat("a", 64), "ollama:qwen3:8b"}, nil, &out, &errb); code != 2 ||
		!strings.Contains(errb.String(), "only checked") {
		t.Fatalf("ollama with sha256: %d %s", code, errb.String())
	}
}
