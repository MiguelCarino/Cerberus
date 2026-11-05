package downloads

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// LM Studio: `lms get <query> --yes`, progress parsed from its output.

var (
	ansiSeq   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	sizePair  = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*([KMGT]i?B|B)\s*/\s*(\d+(?:\.\d+)?)\s*([KMGT]i?B|B)\b`)
	pctFigure = regexp.MustCompile(`(\d{1,3}(?:\.\d+)?)\s*%`)
)

// findLMS resolves the lms binary: name on PATH, then ~/.lmstudio/bin/lms.
func findLMS(name string) string {
	if name == "" {
		name = "lms"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	exe := "lms"
	if filepath.Separator == '\\' {
		exe = "lms.exe"
	}
	p := filepath.Join(home, ".lmstudio", "bin", exe)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

// validLMSQuery refuses empty, overlong, flag-like or control-character
// queries (the query is one argv element, never a shell string).
func validLMSQuery(q string) error {
	if q == "" || len(q) > 300 || strings.HasPrefix(q, "-") {
		return fmt.Errorf("%w: an LM Studio query is a model name or a Hugging Face repo, not starting with -", ErrBadRef)
	}
	for _, r := range q {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control characters in the query", ErrBadRef)
		}
	}
	return nil
}

// lmsProgress is what one line of lms output says.
type lmsProgress struct {
	bytes, total int64
	pct          float64
	hasSize      bool
	hasPct       bool
}

// parseLMSLine reads "1.23 GB / 4.56 GB" and "45.3%" from one output line.
func parseLMSLine(line string) (p lmsProgress) {
	line = ansiSeq.ReplaceAllString(line, "")
	if mt := sizePair.FindStringSubmatch(line); mt != nil {
		p.bytes, p.total, p.hasSize = sizeBytes(mt[1], mt[2]), sizeBytes(mt[3], mt[4]), true
	}
	if mt := pctFigure.FindStringSubmatch(line); mt != nil {
		if v, err := strconv.ParseFloat(mt[1], 64); err == nil && v <= 100 {
			p.pct, p.hasPct = v, true
		}
	}
	return p
}

func sizeBytes(num, unit string) int64 {
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[unit]
	return int64(v * mult)
}

// splitCRLF splits on \n or \r (progress bars redraw with \r).
func splitCRLF(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// lmsRepo is an "owner/name" reference, which lms looks up in LM Studio's
// own catalogue only.
var lmsRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// lmsNotFound is how lms says its catalogue has no such model.
var lmsNotFound = regexp.MustCompile(`(?i)failed to resolve artifact|artifact does not exist`)

// runLMS runs lms get. An "owner/name" that LM Studio's catalogue does not
// know is tried once more as a Hugging Face repo (lms get <URL>); when that
// fails too, the item suggests GGUF builds of it found on Hugging Face.
func (m *Manager) runLMS(ctx context.Context, j *job) error {
	err := m.runLMSOnce(ctx, j, j.req.Ref)
	if err == nil || ctx.Err() != nil || !lmsRepo.MatchString(j.req.Ref) || !lmsNotFound.MatchString(err.Error()) {
		return err
	}
	m.update(j, true, func(it *Item) { it.Status = "Not in LM Studio's catalogue; trying it as a Hugging Face repo" })
	hfErr := m.runLMSOnce(ctx, j, "https://huggingface.co/"+j.req.Ref)
	if hfErr == nil || ctx.Err() != nil {
		return hfErr
	}
	sug := m.suggestGGUF(ctx, j.req.Ref)
	m.update(j, true, func(it *Item) { it.Suggest = sug })
	msg := fmt.Sprintf("%s is not in LM Studio's catalogue, and as a Hugging Face repo: %s", j.req.Ref, strings.TrimPrefix(hfErr.Error(), "lms get: "))
	if len(sug) > 0 {
		short := make([]string, len(sug))
		for i, s := range sug {
			short[i] = strings.TrimPrefix(s, "lms https://huggingface.co/")
		}
		msg += ". GGUF builds of it on Hugging Face: " + strings.Join(short, ", ")
	}
	return errors.New(msg)
}

// suggestGGUF asks Hugging Face for the most downloaded GGUF repos whose
// name contains the repo's name (at most 3, as "lms <URL>" references).
func (m *Manager) suggestGGUF(ctx context.Context, ref string) []string {
	_, name, _ := strings.Cut(ref, "/")
	q := url.Values{"search": {name}, "filter": {"gguf"}, "sort": {"downloads"}, "direction": {"-1"}, "limit": {"3"}}
	u := "https://huggingface.co/api/models?" + q.Encode()
	if err := m.checkURL(u, false); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	client := &http.Client{Transport: m.hfTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list []struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list) != nil {
		return nil
	}
	var out []string
	for _, r := range list {
		if lmsRepo.MatchString(r.ID) && len(out) < 3 {
			out = append(out, "lms https://huggingface.co/"+r.ID)
		}
	}
	return out
}

func (m *Manager) runLMSOnce(ctx context.Context, j *job, ref string) error {
	bin := m.opt.LMSBinary()
	if bin == "" {
		return fmt.Errorf("%w: LM Studio's lms command was not found", ErrUnavailable)
	}
	cmd := exec.CommandContext(ctx, bin, "get", ref, "--yes")
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "FORCE_COLOR=0")
	cmd.WaitDelay = 5 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		return fmt.Errorf("start lms: %w", err)
	}
	waitErr := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		waitErr <- err
	}()
	var last string
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 16<<10), 1<<20)
	sc.Split(splitCRLF)
	for sc.Scan() {
		line := strings.TrimSpace(ansiSeq.ReplaceAllString(sc.Text(), ""))
		if line == "" {
			continue
		}
		p := parseLMSLine(line)
		switch {
		case p.hasSize && p.total > 0:
			m.progress(j, p.bytes, p.total)
		case p.hasPct:
			m.percent(j, p.pct)
		default:
			last = line
			m.update(j, false, func(it *Item) { it.Status = clip(line, 200) })
		}
	}
	_, _ = io.Copy(io.Discard, pr) // a line past the scanner's limit: keep draining
	if err := <-waitErr; err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if last != "" {
			return fmt.Errorf("lms get: %s", clip(last, 300))
		}
		return fmt.Errorf("lms get: %w", err)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
