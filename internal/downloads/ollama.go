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
	"regexp"
	"strings"
)

// Ollama: POST /api/pull streams one JSON object per line.

var ollamaName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,199}$`)

func validOllamaName(s string) error {
	if !ollamaName.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") {
		return fmt.Errorf("%w: an Ollama model name looks like llama3.2 or qwen2.5-coder:7b", ErrBadRef)
	}
	return nil
}

type pullLine struct {
	Status    string `json:"status"`
	Digest    string `json:"digest"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Error     string `json:"error"`
}

func (m *Manager) runOllama(ctx context.Context, j *job) error {
	base := strings.TrimRight(m.opt.Config().Engines.Ollama.URL, "/")
	body, _ := json.Marshal(map[string]any{"model": j.req.Ref, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.opt.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("Ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var e pullLine
		if json.Unmarshal(msg, &e) == nil && e.Error != "" {
			return fmt.Errorf("Ollama: %s", e.Error)
		}
		return fmt.Errorf("Ollama: HTTP %d", resp.StatusCode)
	}
	type layer struct{ total, done int64 }
	layers := map[string]*layer{}
	var order []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ln pullLine
		if err := json.Unmarshal(sc.Bytes(), &ln); err != nil {
			continue
		}
		if ln.Error != "" {
			return fmt.Errorf("Ollama: %s", ln.Error)
		}
		if ln.Status == "success" {
			return nil
		}
		if ln.Status != "" {
			m.update(j, false, func(it *Item) { it.Status = ln.Status })
		}
		if ln.Digest != "" && ln.Total > 0 {
			l := layers[ln.Digest]
			if l == nil {
				l = &layer{}
				layers[ln.Digest] = l
				order = append(order, ln.Digest)
			}
			l.total, l.done = ln.Total, min(ln.Completed, ln.Total)
			var total, done int64
			for _, d := range order {
				total += layers[d].total
				done += layers[d].done
			}
			m.progress(j, done, total)
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Ollama: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("Ollama: the pull ended without success")
}
