package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
)

// ollamaProbeTTL is how long a reachability check is trusted, so a missing
// daemon costs one cheap request per interval rather than one per listing.
const ollamaProbeTTL = 30 * time.Second

// ollama drives an Ollama daemon over its HTTP API.
type ollama struct {
	cfg     *config.Config
	live    atomic.Pointer[config.Config] // set by reload
	url     string
	timeout time.Duration
	log     *slog.Logger
	http    *http.Client // injectable for tests

	mu        sync.Mutex
	up        bool
	probed    time.Time
	loggedOff bool // "unreachable" already logged since the last success
}

func newOllama(cfg *config.Config, log *slog.Logger) *ollama {
	url := cfg.Engines.Ollama.URL
	if url == "" {
		url = "http://127.0.0.1:11434"
	}
	timeout := time.Duration(cfg.Engines.LoadTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	return &ollama{cfg: cfg, url: trimSlash(url), timeout: timeout, log: log, http: defaultHTTPClient}
}

func (b *ollama) Name() string { return BackendOllama }

// Available probes the daemon (cached); unreachable is logged once at debug.
func (b *ollama) Available() bool {
	if !b.cfg.Engines.Ollama.Enabled {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.probed.IsZero() && time.Since(b.probed) < ollamaProbeTTL {
		return b.up
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b.up = getOK(ctx, b.http, b.url+"/api/version", nil)
	b.probed = time.Now()
	if b.up {
		b.loggedOff = false
	} else if !b.loggedOff {
		b.loggedOff = true
		b.log.Debug("ollama unreachable", "url", b.url)
	}
	return b.up
}

// markDown forgets a positive probe after a failed request.
func (b *ollama) markDown() {
	b.mu.Lock()
	b.probed = time.Time{}
	b.mu.Unlock()
}

type ollamaModel struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Size     int64  `json:"size"`
	SizeVRAM int64  `json:"size_vram"`
	Context  int    `json:"context_length"`
	Details  struct {
		Family   string   `json:"family"`
		Families []string `json:"families"`
	} `json:"details"`
}

func (m ollamaModel) name() string {
	if m.Name != "" {
		return m.Name
	}
	return m.Model
}

// vision guesses from the model families (projector families mean images).
func (m ollamaModel) vision() bool {
	fams := append([]string{m.Details.Family}, m.Details.Families...)
	return slices.ContainsFunc(fams, func(f string) bool {
		f = strings.ToLower(f)
		return f == "clip" || f == "mllama" || strings.Contains(f, "vision")
	})
}

func (b *ollama) getModels(ctx context.Context, path string) ([]ollamaModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		b.markDown()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama %s: %s", path, resp.Status)
	}
	var body struct {
		Models []ollamaModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("ollama %s: %w", path, err)
	}
	return body.Models, nil
}

// List merges pulled models (/api/tags) with loaded ones (/api/ps).
func (b *ollama) List(ctx context.Context) ([]ModelInfo, error) {
	tags, err := b.getModels(ctx, "/api/tags")
	if err != nil {
		return nil, err
	}
	ps, err := b.getModels(ctx, "/api/ps")
	if err != nil {
		b.log.Debug("ollama ps failed", "err", err)
	}
	loaded := make(map[string]ollamaModel, len(ps))
	for _, m := range ps {
		loaded[m.name()] = m
	}
	out := make([]ModelInfo, 0, len(tags))
	for _, m := range tags {
		n := m.name()
		if n == "" {
			continue
		}
		info := ModelInfo{ID: n, Backend: BackendOllama, Name: n, Local: true,
			SizeMiB: mib(m.Size), Vision: m.vision()}
		if p, ok := loaded[n]; ok {
			info.Loaded = true
			info.Ctx = p.Context
			if p.SizeVRAM > 0 {
				info.SizeMiB = mib(p.SizeVRAM)
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// Load asks Ollama to load m and keep it for the idle TTL.
func (b *ollama) Load(ctx context.Context, m ModelInfo) (Endpoint, error) {
	ttl := b.conf().IdleTTL(modelConfig(b.conf(), m.ID))
	body := map[string]any{"model": modelName(m), "keep_alive": fmt.Sprintf("%ds", int(ttl/time.Second)), "stream": false}
	if m.Ctx > 0 {
		body["options"] = map[string]any{"num_ctx": m.Ctx}
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	if err := b.generate(ctx, body); err != nil {
		return Endpoint{}, fmt.Errorf("ollama load %s: %w", m.ID, err)
	}
	b.log.Info("model loaded", "backend", BackendOllama, "model", m.ID)
	return b.Endpoint(m), nil
}

// Unload sets keep_alive to 0, which frees the model immediately.
func (b *ollama) Unload(ctx context.Context, m ModelInfo) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.generate(ctx, map[string]any{"model": modelName(m), "keep_alive": 0, "stream": false}); err != nil {
		return fmt.Errorf("ollama unload %s: %w", m.ID, err)
	}
	b.log.Info("model unloaded", "backend", BackendOllama, "model", m.ID)
	return nil
}

func (b *ollama) generate(ctx context.Context, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url+"/api/generate", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		b.markDown()
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Slots is models[].parallel, else 1: Ollama's OLLAMA_NUM_PARALLEL lives in
// its own environment, which Cerberus cannot see.
func (b *ollama) Slots(m ModelInfo) int {
	if mc := modelConfig(b.conf(), m.ID); mc != nil && mc.Parallel > 0 {
		return mc.Parallel
	}
	return 1
}

func (b *ollama) Endpoint(m ModelInfo) Endpoint {
	return Endpoint{BaseURL: b.url + "/v1", Model: modelName(m), Local: true, Backend: BackendOllama}
}
