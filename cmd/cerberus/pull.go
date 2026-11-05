package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/downloads"
)

// pullCmd queues a model download on the daemon (POST /api/downloads) and
// follows its progress on the event stream until it ends. Interrupting
// only stops following: the download goes on in the daemon (cancel it in
// the web UI or with --cancel ID). The model is never loaded.
func pullCmd(args []string, stdout, stderr io.Writer) int {
	var engine, sum, cancelID string
	var detach bool
	cfg, pos, code := clientCmd("pull", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&engine, "engine", "", "lmstudio | ollama | llamacpp (default: from the reference)")
		fs.StringVar(&sum, "sha256", "", "expected SHA-256 of a GGUF file")
		fs.BoolVar(&detach, "detach", false, "queue the download and return at once")
		fs.StringVar(&cancelID, "cancel", "", "cancel the download with this ID instead")
	})
	if cfg == nil {
		return code
	}
	c := newClient(cfg)
	if cancelID != "" {
		if err := c.do(context.Background(), "DELETE", "/api/downloads/"+url.PathEscape(cancelID), nil, nil); err != nil {
			fmt.Fprintln(stderr, "cerberus:", err)
			return 1
		}
		fmt.Fprintln(stdout, "canceled", cancelID)
		return 0
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, `usage: cerberus pull [--engine E] [--sha256 HEX] [--detach] <ref>
  ref: an Ollama name (ollama:qwen3:8b), an LM Studio query (lms:qwen3-8b),
       or a Hugging Face https URL of a .gguf file (llama.cpp)`)
		return 2
	}
	req := downloads.ParseRef(pos[0])
	if engine != "" {
		req = downloads.Request{Engine: engine, Ref: strings.TrimSpace(pos[0])}
	}
	req.SHA256 = sum
	if sum != "" && req.Engine != downloads.EngineLlamaCpp {
		fmt.Fprintln(stderr, "cerberus: --sha256 is only checked for Hugging Face GGUF downloads (llama.cpp)")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// Follow first, so no change between the POST and the stream is lost.
	var events <-chan downloads.Item
	if !detach {
		ch, err := followDownloads(ctx, c)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus:", err)
			return 1
		}
		events = ch
	}
	var it downloads.Item
	if err := c.do(context.Background(), "POST", "/api/downloads", req, &it); err != nil {
		fmt.Fprintln(stderr, "cerberus:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: %s (%s) %s\n", it.ID, it.Name, it.Engine, it.State)
	if detach {
		return 0
	}
	f, isFile := stdout.(*os.File)
	p := &pullPrinter{w: stdout, tty: isFile && isTerminal(f)}
	for !it.Final() {
		select {
		case <-ctx.Done():
			p.end()
			fmt.Fprintf(stdout, "stopped following; %s goes on in the daemon (cerberus pull --cancel %s)\n", it.ID, it.ID)
			return 130
		case ev, ok := <-events:
			if !ok {
				p.end()
				fmt.Fprintln(stderr, "cerberus: lost the event stream; the download goes on in the daemon")
				return 1
			}
			if ev.ID != it.ID {
				continue
			}
			it = ev
			p.show(it)
		}
	}
	p.end()
	switch it.State {
	case downloads.StateDone:
		msg := "done: " + it.Name + " (not loaded)"
		if it.Path != "" {
			msg += " at " + it.Path
		}
		fmt.Fprintln(stdout, msg)
		return 0
	case downloads.StateCanceled:
		fmt.Fprintln(stdout, "canceled")
		return 1
	}
	fmt.Fprintln(stderr, "cerberus: download failed:", it.Error)
	return 1
}

// followDownloads opens GET /api/events and passes on every "download"
// event (and, on a "resync", the current list). The channel closes when
// the stream ends.
func followDownloads(ctx context.Context, c *client) (<-chan downloads.Item, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cerberus-Actor", audit.ActorCLI)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req) // no timeout: a long-lived stream
	if err != nil {
		return nil, &unreachableError{base: c.base, err: err}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return nil, fmt.Errorf("GET /api/events: %s", errorText(resp.Status, raw))
	}
	out := make(chan downloads.Item, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		readSSE(resp.Body, func(event, data string) bool {
			var items []downloads.Item
			switch event {
			case "download":
				var it downloads.Item
				if json.Unmarshal([]byte(data), &it) != nil {
					return true
				}
				items = []downloads.Item{it}
			case "resync":
				lctx, cancel := context.WithTimeout(ctx, clientTimeout)
				err := c.do(lctx, "GET", "/api/downloads", nil, &items)
				cancel()
				if err != nil {
					return true
				}
			default:
				return true
			}
			for _, it := range items {
				select {
				case out <- it:
				case <-ctx.Done():
					return false
				}
			}
			return true
		})
	}()
	return out, nil
}

// readSSE calls fn for every event in r until fn returns false or r ends.
// Multi-line data is joined with newlines; comments are skipped.
func readSSE(r io.Reader, fn func(event, data string) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var event string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 && !fn(event, strings.Join(data, "\n")) {
				return
			}
			event, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
}

// pullPrinter shows progress: one rewritten line on a terminal, else a
// line per state change and per 10 %.
type pullPrinter struct {
	w        io.Writer
	tty      bool
	open     bool // a \r line is showing
	state    string
	status   string
	lastStep int
}

func (p *pullPrinter) show(it downloads.Item) {
	line := progressLine(it)
	if p.tty {
		fmt.Fprintf(p.w, "\r\033[K%s", line)
		p.open = true
		return
	}
	step := int(it.Percent / 10)
	if it.State != p.state || it.Status != p.status || step > p.lastStep {
		fmt.Fprintln(p.w, line)
		p.state, p.status, p.lastStep = it.State, it.Status, step
	}
}

func (p *pullPrinter) end() {
	if p.open {
		fmt.Fprintln(p.w)
		p.open = false
	}
}

// progressLine is "running 42.0% 1.2 GiB / 2.9 GiB 35.0 MiB/s ETA 51s status".
func progressLine(it downloads.Item) string {
	parts := []string{it.State}
	if it.Percent > 0 || it.Total > 0 {
		parts = append(parts, fmt.Sprintf("%.1f%%", it.Percent))
	}
	if it.Total > 0 {
		parts = append(parts, humanBytes(it.Bytes)+" / "+humanBytes(it.Total))
	} else if it.Bytes > 0 {
		parts = append(parts, humanBytes(it.Bytes))
	}
	if it.Rate > 0 {
		parts = append(parts, humanBytes(int64(it.Rate))+"/s")
	}
	if it.ETASec >= 0 && !it.Final() && it.Rate > 0 {
		parts = append(parts, "ETA "+(time.Duration(it.ETASec)*time.Second).String())
	}
	if it.Status != "" {
		parts = append(parts, it.Status)
	}
	return strings.Join(parts, " ")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), -1
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGTP"[i])
}
