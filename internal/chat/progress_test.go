package chat

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
)

// TestProgressEventsSpaced: prompt reading reaches the chat stream at most
// twice a second, and the last one (reading done) always (web-api-ops-8).
func TestProgressEventsSpaced(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	m, err := New(Options{DataDir: filepath.Join(t.TempDir(), "d"), Config: func() *config.Config { return cfg },
		Home: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	l := &live{chat: &Chat{ID: "chat-x"}, subs: map[*sub]struct{}{}}
	count := func() int {
		n := 0
		for _, e := range l.events {
			if e.Type == agent.EvProgress {
				n++
			}
		}
		return n
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emitAgentLocked(l, agent.Event{Type: agent.EvProgress, Progress: 1000, Total: 48000})
	now = now.Add(100 * time.Millisecond)
	m.emitAgentLocked(l, agent.Event{Type: agent.EvProgress, Progress: 2000, Total: 48000})
	if count() != 1 {
		t.Fatalf("events within 500 ms: %d", count())
	}
	now = now.Add(500 * time.Millisecond)
	m.emitAgentLocked(l, agent.Event{Type: agent.EvProgress, Progress: 30000, Total: 48000})
	now = now.Add(10 * time.Millisecond)
	m.emitAgentLocked(l, agent.Event{Type: agent.EvProgress, Progress: 48000, Total: 48000})
	if count() != 3 {
		t.Fatalf("events: %d, want 3 (the last always)", count())
	}
	if raw := string(l.events[len(l.events)-1].Raw); raw == "" || !strings.Contains(raw, `"processed":48000`) || !strings.Contains(raw, `"type":"progress"`) {
		t.Fatalf("wire %s", raw)
	}
}
