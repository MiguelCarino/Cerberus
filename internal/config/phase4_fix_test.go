package config

import (
	"errors"
	"strings"
	"testing"
)

// TestListenBindSafety: a public listen IP without allow_public is an error
// (the daemon would refuse to start on it after a restart); with
// allow_public, and for a wildcard, it is a warning.
func TestListenBindSafety(t *testing.T) {
	find := func(probs []Problem, path string) *Problem {
		for i := range probs {
			if probs[i].Path == path {
				return &probs[i]
			}
		}
		return nil
	}
	c := Defaults()
	if _, err := c.SetAll(map[string]any{"server.listen": "8.8.8.8:7340"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("public listen accepted: %v", err)
	}
	if _, err := c.SetAll(map[string]any{"server.extra_listen": []any{"203.0.113.5:7340"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("public extra listen accepted: %v", err)
	}
	for _, ok := range []string{"127.0.0.1:7381", "192.168.1.5:7340", "100.64.0.1:7340", "[fd00::1]:7340", "myhost.lan:7340"} {
		if _, err := Defaults().SetAll(map[string]any{"server.extra_listen": []any{ok}}); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	probs, err := Defaults().SetAll(map[string]any{"server.extra_listen": []any{"0.0.0.0:7340"}})
	if err != nil {
		t.Fatalf("wildcard refused: %v", err)
	}
	if p := find(probs, "server.extra_listen[0]"); p == nil || p.Level != LevelWarn || !strings.Contains(p.Message, "every interface") {
		t.Errorf("wildcard warning = %+v", p)
	}
	c = Defaults()
	c.Server.AllowPublic = true
	probs, err = c.SetAll(map[string]any{"server.listen": "8.8.8.8:7340"})
	if err != nil {
		t.Fatalf("public with allow_public refused: %v", err)
	}
	if p := find(probs, "server.listen"); p == nil || p.Level != LevelWarn {
		t.Errorf("allowed public warning = %+v", p)
	}
}

// TestSetAllExistingErrorMoves: deleting or reordering list items in front
// of an item that was already invalid is not refused (its error only moved),
// while a second broken item still is.
func TestSetAllExistingErrorMoves(t *testing.T) {
	c := Defaults()
	c.Agents = []AgentProfile{{Name: "x1", Command: []string{"x"}}, {Name: "bad"}}
	c.DefaultAgent = "x1"
	before := c.Validate()
	if len(before) == 0 {
		t.Fatal("fixture has no error")
	}
	bad := map[string]any{"name": "bad"}
	if _, err := c.Clone().SetAll(map[string]any{"agents": []any{bad}, "default_agent": "bad"}); err != nil {
		t.Fatalf("delete in front of a broken item refused: %v", err)
	}
	if _, err := c.Clone().SetAll(map[string]any{"agents": []any{bad, map[string]any{"name": "x1", "command": []any{"x"}}}}); err != nil {
		t.Fatalf("reorder refused: %v", err)
	}
	probs, err := c.Clone().SetAll(map[string]any{"agents": []any{bad, map[string]any{"name": "bad2"}}, "default_agent": "bad"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a second broken agent accepted: %v", err)
	}
	if len(probs) != 1 || !strings.HasSuffix(probs[0].Path, "].command") {
		t.Errorf("new problems = %+v, want one agents[i].command", probs)
	}
}
