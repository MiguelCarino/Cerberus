package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
)

func TestGatewayBase(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7340":  "http://127.0.0.1:7340/v1",
		":7381":           "http://127.0.0.1:7381/v1",
		"0.0.0.0:7397":    "http://127.0.0.1:7397/v1",
		"[::]:7340":       "http://[::1]:7340/v1",
		"100.64.0.2:7340": "http://100.64.0.2:7340/v1",
		"not an address":  "http://127.0.0.1:7340/v1",
		"desk.lan:7340":   "http://desk.lan:7340/v1",
		"[fd00::5]:7340":  "http://[fd00::5]:7340/v1",
		"localhost:7340":  "http://localhost:7340/v1",
	} {
		if got := gatewayBase(in); got != want {
			t.Errorf("gatewayBase(%q) = %q, want %q", in, got, want)
		}
	}
}

type countingModels struct {
	calls   atomic.Int32
	release chan struct{}
}

func (c *countingModels) Models(ctx context.Context) []engine.ModelInfo {
	c.calls.Add(1)
	<-c.release
	return []engine.ModelInfo{{ID: "llamacpp/qwen", Name: "qwen", Ctx: 32768}, {ID: "api/x"}}
}

func TestContextCacheNeverBlocks(t *testing.T) {
	m := &countingModels{release: make(chan struct{})}
	c := newContextCache(m, time.Hour)
	// The first lookup starts one refresh and answers at once (unknown = 0),
	// even though the listing hangs.
	if got := c.get("llamacpp/qwen"); got != 0 {
		t.Fatalf("before the listing: %d", got)
	}
	_ = c.get("llamacpp/qwen")
	close(m.release)
	deadline := time.Now().Add(5 * time.Second)
	for c.get("llamacpp/qwen") != 32768 {
		if time.Now().After(deadline) {
			t.Fatal("the cache never filled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.get("qwen") != 32768 || c.get("api/x") != 0 {
		t.Errorf("by name %d, no ctx %d", c.get("qwen"), c.get("api/x"))
	}
	if n := m.calls.Load(); n != 1 {
		t.Errorf("%d listings, want 1", n)
	}
}

func TestNewChatsLiveConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	boot := config.Defaults()
	boot.Server.Listen = "127.0.0.1:7381"
	live := newLiveConfig(boot)
	m, err := newChats(boot, live, nil, nil, t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(m.List()) != 0 {
		t.Fatal("a fresh data dir has chats")
	}
	if _, err := m.Create(chatCreate(t)); err != nil {
		t.Fatalf("create while enabled: %v", err)
	}
	off := boot.Clone()
	off.Chat.Enabled = false
	live.Reload(off)
	if _, err := m.Create(chatCreate(t)); err == nil {
		t.Fatal("create after chat.enabled=false went through")
	}
}

func chatCreate(t *testing.T) chat.CreateOptions {
	return chat.CreateOptions{Cwd: t.TempDir(), Model: "api/model"}
}
