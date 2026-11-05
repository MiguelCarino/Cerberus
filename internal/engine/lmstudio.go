package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/config"
)

// lmsCmdTimeout bounds quick lms calls (ls, ps, unload, server start).
const lmsCmdTimeout = 30 * time.Second

// lmStudio drives LM Studio through its `lms` CLI and OpenAI-compatible server.
type lmStudio struct {
	cfg     *config.Config
	live    atomic.Pointer[config.Config] // set by reload
	lc      config.LMStudioConfig
	timeout time.Duration
	log     *slog.Logger

	// Injectable for tests.
	binary func() string // "" when lms is not installed
	run    func(ctx context.Context, bin string, args ...string) ([]byte, error)
	http   *http.Client
	roots  func() []string // models folders (nil: from LM Studio's settings and defaults)
	hub    func() string   // catalogue manifests folder (nil: ~/.lmstudio/hub/models)
}

func newLMStudio(cfg *config.Config, log *slog.Logger) *lmStudio {
	lc := cfg.Engines.LMStudio
	if lc.Binary == "" {
		lc.Binary = "lms"
	}
	if lc.BaseURL == "" {
		lc.BaseURL = "http://127.0.0.1:1234/v1"
	}
	if lc.Ctx <= 0 {
		lc.Ctx = 32768
	}
	timeout := time.Duration(cfg.Engines.LoadTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	return &lmStudio{cfg: cfg, lc: lc, timeout: timeout, log: log,
		binary: func() string { return findLMS(lc.Binary) },
		run:    runCommand,
		http:   defaultHTTPClient,
	}
}

// findLMS resolves the lms binary, falling back to ~/.lmstudio/bin/lms.
func findLMS(name string) string {
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

// runCommand runs bin and returns stdout; stderr is folded into the error.
func runCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
}

func (b *lmStudio) Name() string { return BackendLMStudio }

func (b *lmStudio) Available() bool { return b.lc.Enabled && b.binary() != "" }

// lmsModel is one entry of `lms ls --json` / `lms ps --json`. Every field is
// optional: the CLI's JSON shape has changed between releases.
type lmsModel struct {
	Type             string `json:"type"`
	ModelKey         string `json:"modelKey"`
	Path             string `json:"path"`
	DisplayName      string `json:"displayName"`
	SizeBytes        int64  `json:"sizeBytes"`
	MaxContextLength int    `json:"maxContextLength"`
	Vision           *bool  `json:"vision"`
	Identifier       string `json:"identifier"`    // ps only
	ContextLength    int    `json:"contextLength"` // ps only
	LastUsedTime     int64  `json:"lastUsedTime"`  // ps only, unix ms
}

func (m lmsModel) key() string {
	if m.ModelKey != "" {
		return m.ModelKey
	}
	return m.Path
}

// parseLMS decodes lms JSON output, tolerating leading noise and an object
// wrapper ({"models":[...]}).
func parseLMS(raw []byte) ([]lmsModel, error) {
	if i := bytes.IndexAny(raw, "[{"); i > 0 {
		raw = raw[i:]
	}
	raw = bytes.TrimSpace(raw)
	var list []lmsModel
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrap struct {
		Models []lmsModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("lms: unexpected JSON: %w", err)
	}
	return wrap.Models, nil
}

func (b *lmStudio) lms(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	bin := b.binary()
	if bin == "" {
		return nil, errors.New("lms not found")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return b.run(ctx, bin, args...)
}

// List merges downloaded models (ls) with loaded ones (ps).
func (b *lmStudio) List(ctx context.Context) ([]ModelInfo, error) {
	raw, err := b.lms(ctx, lmsCmdTimeout, "ls", "--json")
	if err != nil {
		return nil, fmt.Errorf("lms ls: %w", err)
	}
	all, err := parseLMS(raw)
	if err != nil {
		return nil, err
	}
	loaded := b.loaded(ctx)
	out := make([]ModelInfo, 0, len(all))
	for _, m := range all {
		if m.key() == "" {
			continue
		}
		out = append(out, b.info(m, loaded))
	}
	return out, nil
}

// loaded maps model key (and identifier) to its ps entry; errors mean none.
func (b *lmStudio) loaded(ctx context.Context) map[string]lmsModel {
	out := map[string]lmsModel{}
	raw, err := b.lms(ctx, lmsCmdTimeout, "ps", "--json")
	if err != nil {
		b.log.Debug("lms ps failed", "err", err)
		return out
	}
	ps, err := parseLMS(raw)
	if err != nil {
		b.log.Debug("lms ps parse", "err", err)
		return out
	}
	for _, m := range ps {
		if k := m.key(); k != "" {
			out[k] = m
		}
		if m.Identifier != "" {
			out[m.Identifier] = m
		}
	}
	return out
}

func (b *lmStudio) info(m lmsModel, loaded map[string]lmsModel) ModelInfo {
	key := m.key()
	ctx := b.lc.Ctx
	if m.MaxContextLength > 0 {
		ctx = min(ctx, m.MaxContextLength)
	}
	info := ModelInfo{ID: key, Backend: BackendLMStudio, Name: key, Local: true,
		SizeMiB: mib(m.SizeBytes), Ctx: ctx, Vision: m.Vision != nil && *m.Vision}
	if p, ok := loaded[key]; ok {
		info.Loaded = true
		if p.ContextLength > 0 {
			info.Ctx = p.ContextLength
		}
		if p.LastUsedTime > 0 {
			info.LastUsed = time.UnixMilli(p.LastUsedTime)
		}
		if p.Identifier != "" {
			info.Name = p.Identifier
		}
	}
	return info
}

// Load starts the LM Studio server if needed and loads m with its context
// length and a TTL, so LM Studio frees it even if Cerberus goes away.
func (b *lmStudio) Load(ctx context.Context, m ModelInfo) (Endpoint, error) {
	if err := b.ensureServer(ctx); err != nil {
		return Endpoint{}, err
	}
	if p, ok := b.loaded(ctx)[m.ID]; ok {
		if p.Identifier != "" {
			m.Name = p.Identifier
		}
		return b.Endpoint(m), nil
	}
	ttl := int(b.conf().IdleTTL(modelConfig(b.conf(), m.ID)) / time.Second)
	args := lmsLoadArgs(m, ttl, b.conf().Engines.LMStudio.GPU)
	if _, err := b.lms(ctx, b.timeout, args...); err != nil {
		return Endpoint{}, fmt.Errorf("lms load %s: %w", m.ID, err)
	}
	m.Name = m.ID
	b.log.Info("model loaded", "backend", BackendLMStudio, "model", m.ID, "args", strings.Join(args[2:], " "))
	return b.Endpoint(m), nil
}

// ensureServer starts the LM Studio server when its API doesn't answer.
func (b *lmStudio) ensureServer(ctx context.Context) error {
	if b.serverUp(ctx) {
		return nil
	}
	if _, err := b.lms(ctx, lmsCmdTimeout, "server", "start"); err != nil {
		return fmt.Errorf("lms server start: %w", err)
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	delay := 100 * time.Millisecond
	for !b.serverUp(wait) {
		select {
		case <-wait.Done():
			return fmt.Errorf("lms server did not come up at %s", b.lc.BaseURL)
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Second)
	}
	return nil
}

func (b *lmStudio) serverUp(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return getOK(ctx, b.http, trimSlash(b.lc.BaseURL)+"/models", nil)
}

func (b *lmStudio) Unload(ctx context.Context, m ModelInfo) error {
	name := modelName(m)
	if _, err := b.lms(ctx, lmsCmdTimeout, "unload", name); err != nil {
		return fmt.Errorf("lms unload %s: %w", name, err)
	}
	b.log.Info("model unloaded", "backend", BackendLMStudio, "model", m.ID)
	return nil
}

// Slots is the parallel LM Studio serves m with: models[].parallel, else
// LM Studio's default of 4. The manager's queue limit is derived from it and
// passed back as --parallel on load.
func (b *lmStudio) Slots(m ModelInfo) int {
	if mc := modelConfig(b.conf(), m.ID); mc != nil && mc.Parallel > 0 {
		return mc.Parallel
	}
	return lmsDefaultParallel
}

func (b *lmStudio) Endpoint(m ModelInfo) Endpoint {
	return Endpoint{BaseURL: trimSlash(b.lc.BaseURL), Model: modelName(m), Local: true, Backend: BackendLMStudio}
}

// modelConfig finds the ModelConfig override for id (nil if none).
func modelConfig(cfg *config.Config, id string) *config.ModelConfig {
	for i := range cfg.Models {
		if cfg.Models[i].ID == id {
			return &cfg.Models[i]
		}
	}
	return nil
}
