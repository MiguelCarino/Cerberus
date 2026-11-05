package main

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/permits"
	"carino.systems/cerberus/internal/users"
)

// The LLM chat's wiring (day phase 5): the manager reads the live config
// (liveConfig, a Reloader), runs native chats through this daemon's own
// gateway over loopback with the current admin token, and learns context
// lengths from the engine without ever waiting on a backend listing.

// liveConfig holds the config the last PATCH /api/config applied.
type liveConfig struct{ p atomic.Pointer[config.Config] }

func newLiveConfig(cfg *config.Config) *liveConfig {
	l := &liveConfig{}
	l.p.Store(cfg)
	return l
}

// Reload implements api.Reloader.
func (l *liveConfig) Reload(cfg *config.Config) { l.p.Store(cfg) }

// Load is the live config.
func (l *liveConfig) Load() *config.Config { return l.p.Load() }

// modelLister is what the chat needs of the engine.
type modelLister interface {
	Models(ctx context.Context) []engine.ModelInfo
}

// modelResolver is the part of the engine manager that maps an alias to a model.
type modelResolver interface {
	Resolve(ctx context.Context, id string) (engine.ModelInfo, error)
}

// newChats makes the chat manager. boot is the startup config: its
// Server.Listen is the address bound now (a change needs a restart), and
// token rotation updates its admin token in place.
func newChats(boot *config.Config, live *liveConfig, eng modelLister, n *notify.Notifier, data string, accounts *users.Store, saved *permits.Store, log *slog.Logger) (*chat.Manager, error) {
	base := gatewayBase(boot.Server.Listen)
	ctxLen := newContextCache(eng, 30*time.Second)
	o := chat.Options{
		DataDir: data,
		Config:  live.Load,
		Gateway: func() (string, string) { return base, boot.TokenSnapshot() },
		Log:     log,
		Permits: saved,
	}
	if accounts != nil {
		o.Access = api.ChatAccess(accounts, live.Load)
		o.IsAdmin = api.ChatIsAdmin(accounts)
	}
	if n != nil {
		o.Notifier = n
	}
	if eng != nil {
		o.ContextLength = func(model string) int {
			if model == "default" || model == "" {
				model = live.Load().Engines.DefaultModel
			}
			return ctxLen.get(model)
		}
	}
	if rs, ok := eng.(modelResolver); ok && eng != nil {
		o.ResolveModel = func(ctx context.Context, id string) (string, error) {
			m, err := rs.Resolve(ctx, id)
			return m.ID, err
		}
	}
	return chat.New(o)
}

// gatewayBase is the loopback URL of this daemon's own /v1 for a listen
// address: a wildcard or empty host means loopback.
func gatewayBase(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "127.0.0.1", "7340"
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/v1"
}

// contextCache maps model ids to context lengths from the engine's listing.
// get never blocks on a listing: a stale cache answers at once and starts
// one refresh in the background (the chat asks while holding its lock).
type contextCache struct {
	eng modelLister
	ttl time.Duration

	mu         sync.Mutex
	ctx        map[string]int
	at         time.Time
	refreshing bool
}

func newContextCache(eng modelLister, ttl time.Duration) *contextCache {
	return &contextCache{eng: eng, ttl: ttl, ctx: map[string]int{}}
}

func (c *contextCache) get(model string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.refreshing && time.Since(c.at) > c.ttl {
		c.refreshing = true
		go c.refresh()
	}
	return c.ctx[model]
}

func (c *contextCache) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := map[string]int{}
	for _, info := range c.eng.Models(ctx) {
		if info.Ctx > 0 {
			m[info.ID] = info.Ctx
			if info.Name != "" {
				if _, ok := m[info.Name]; !ok {
					m[info.Name] = info.Ctx
				}
			}
		}
	}
	c.mu.Lock()
	c.ctx, c.at, c.refreshing = m, time.Now(), false
	c.mu.Unlock()
}
