// Package engine supervises the model backends Cerberus routes to: llama.cpp
// processes it spawns itself, LM Studio and Ollama daemons it drives, and
// cloud providers with OpenAI-compatible APIs.
//
// The Manager merges every backend's models into one list, resolves model IDs,
// loads models on demand (making room in VRAM first), counts requests in
// flight and unloads idle local models after their TTL. It never runs an
// inference engine itself; it only starts, stops and points at them.
package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Backend names as reported by Backend.Name and ModelInfo.Backend.
const (
	BackendLlamaCpp = "llamacpp"
	BackendLMStudio = "lmstudio"
	BackendOllama   = "ollama"
	BackendOpenAI   = "openai"
)

var (
	// ErrUnknownModel is returned when an ID resolves to no model (the
	// gateway maps it to 404).
	ErrUnknownModel = errors.New("unknown model")
	// ErrNoRoom is returned when a local model will not fit in free VRAM even
	// after unloading idle models, and over-commit is not allowed.
	ErrNoRoom = errors.New("not enough free VRAM")
)

// ModelInfo describes one model as the UI and the gateway see it.
type ModelInfo struct {
	ID       string
	Backend  string
	Name     string // the name the backend expects (Endpoint.Model)
	Local    bool
	Loaded   bool
	SizeMiB  int
	Ctx      int
	LastUsed time.Time
	InFlight int
	// MaxParallel is how many requests Cerberus sends this model at once
	// (its queue limit; 0 = no limit, as for cloud models). Waiting is how
	// many requests are queued for a slot right now.
	MaxParallel int
	Waiting     int
	// LongestWaitS is how long the oldest queued request has waited (G5).
	// BusyForS (the oldest request in flight) and Stalls (since start) are
	// filled by the API from the gateway.
	LongestWaitS float64
	BusyForS     float64
	Stalls       int
	Vision       bool
	// Path is a llama.cpp model's .gguf file ("" for other engines). It is
	// not part of any JSON reply.
	Path string `json:"-"`
	// plan is set by the manager on the copy it hands to Backend.Load.
	plan *loadPlan

	// Energy and cost through the gateway (filled by the API from usage records).
	EnergyJ24h, CostMXN24h     float64
	EnergyJMonth, CostMXNMonth float64
}

// Endpoint is where to send OpenAI-compatible requests for a model.
type Endpoint struct {
	BaseURL string // ends in /v1
	Model   string // id the backend expects
	Headers map[string]string
	Local   bool
	Backend string
	// Gen identifies the load of the model this endpoint was handed for
	// (Manager.MarkCrashedGen ignores a crash reported against an older
	// load). 0 for cloud models and models Cerberus did not load.
	Gen uint64 `json:"-"`
}

// Backend is one source of models.
type Backend interface {
	Name() string // "llamacpp" | "lmstudio" | "ollama" | "openai"
	Available() bool
	List(ctx context.Context) ([]ModelInfo, error)
	Load(ctx context.Context, m ModelInfo) (Endpoint, error) // idempotent
	Unload(ctx context.Context, m ModelInfo) error
	Endpoint(m ModelInfo) Endpoint
}

// spawner is implemented by backends that own child processes; Shutdown
// stops them. Externally managed backends do not implement it.
type spawner interface {
	StopAll(ctx context.Context)
}

// modelName is the backend-native name of m, falling back to its ID.
func modelName(m ModelInfo) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// defaultHTTPClient has no global timeout: callers bound requests with ctx.
var defaultHTTPClient = &http.Client{}

// getOK reports whether GET url answers 2xx within ctx.
func getOK(ctx context.Context, c *http.Client, url string, headers map[string]string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode/100 == 2
}

// trimSlash removes trailing slashes so paths can be appended safely.
func trimSlash(s string) string { return strings.TrimRight(s, "/") }

// mib converts bytes to MiB, rounding up so a non-empty file is never 0.
func mib(b int64) int {
	if b <= 0 {
		return 0
	}
	return int((b + (1<<20 - 1)) >> 20)
}
