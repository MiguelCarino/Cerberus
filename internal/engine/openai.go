package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
)

// discoveryTTL is how long a provider's GET /models answer is reused.
const discoveryTTL = 10 * time.Minute

// openAI is one cloud or remote provider with an OpenAI-compatible API.
// Its models are never "loaded"; Load and Unload do nothing.
type openAI struct {
	cfg  *config.Config
	p    config.ProviderConfig
	log  *slog.Logger
	http *http.Client // injectable for tests

	secrets func(name string) string // vault look-up; nil = none

	mu      sync.Mutex
	models  []string
	fetched time.Time
}

func newOpenAI(cfg *config.Config, p config.ProviderConfig, log *slog.Logger) *openAI {
	p.BaseURL = trimSlash(p.BaseURL)
	return &openAI{cfg: cfg, p: p, log: log, http: defaultHTTPClient}
}

func (b *openAI) Name() string { return BackendOpenAI }

func (b *openAI) Available() bool { return b.p.Enabled && b.p.Name != "" && b.p.BaseURL != "" }

// key comes from KeyEnv, else the vault (KeySecret, default Name), else
// secrets.json[provider].
func (b *openAI) key() string {
	if b.p.KeyEnv != "" {
		if k := os.Getenv(b.p.KeyEnv); k != "" {
			return k
		}
	}
	if b.secrets != nil {
		name := b.p.KeySecret
		if name == "" {
			name = b.p.Name
		}
		if k := b.secrets(name); k != "" {
			return k
		}
	}
	return b.cfg.Secret(b.p.Name)
}

// headers merges configured headers with the bearer token (an explicit
// Authorization header in config wins).
func (b *openAI) headers() map[string]string {
	h := make(map[string]string, len(b.p.Headers)+1)
	for k, v := range b.p.Headers {
		h[k] = v
	}
	for k := range h {
		if strings.EqualFold(k, "Authorization") {
			return h
		}
	}
	if k := b.key(); k != "" {
		h["Authorization"] = "Bearer " + k
	}
	return h
}

// List returns configured models, or discovered ones (cached 10 min). A
// failed discovery keeps the previous answer.
func (b *openAI) List(ctx context.Context) ([]ModelInfo, error) {
	names := b.p.Models
	if len(names) == 0 {
		var err error
		if names, err = b.discover(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]ModelInfo, 0, len(names))
	for _, n := range names {
		out = append(out, ModelInfo{ID: b.p.Name + "/" + n, Backend: BackendOpenAI, Name: n})
	}
	return out, nil
}

func (b *openAI) discover(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.fetched.IsZero() && time.Since(b.fetched) < discoveryTTL {
		return b.models, nil
	}
	names, err := b.fetchModels(ctx)
	if err != nil {
		if b.models != nil {
			b.log.Debug("provider discovery failed; keeping cached list", "provider", b.p.Name, "err", err)
			return b.models, nil
		}
		return nil, err
	}
	b.models, b.fetched = names, time.Now()
	return names, nil
}

func (b *openAI) fetchModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.p.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range b.headers() {
		req.Header.Set(k, v)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s models: %w", b.p.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s models: %s", b.p.Name, resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s models: %w", b.p.Name, err)
	}
	names := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID != "" {
			names = append(names, d.ID)
		}
	}
	return names, nil
}

func (b *openAI) Load(ctx context.Context, m ModelInfo) (Endpoint, error) { return b.Endpoint(m), nil }

func (b *openAI) Unload(ctx context.Context, m ModelInfo) error { return nil }

func (b *openAI) Endpoint(m ModelInfo) Endpoint {
	model := m.Name
	if model == "" {
		model = strings.TrimPrefix(m.ID, b.p.Name+"/")
	}
	return Endpoint{BaseURL: b.p.BaseURL, Model: model, Headers: b.headers(), Local: false, Backend: BackendOpenAI}
}
