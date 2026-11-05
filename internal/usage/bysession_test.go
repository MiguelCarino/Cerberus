package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBySession(t *testing.T) {
	for _, keep := range []int{100, 2} { // 2: the ring overflows and the totals answer
		s, err := Open(filepath.Join(t.TempDir(), "usage.jsonl"), keep)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		add := func(session, model, client string, p, c int) {
			s.Add(Record{Time: now, SessionID: session, Model: model, Client: client, Provider: "ollama",
				PromptTokens: p, CompletionTokens: c, Status: 200})
		}
		add("s1", "qwen", ClientDelegate, 100, 10)
		add("s1", "qwen", ClientDelegate, 200, 20)
		add("s1", "cloud/big", "cerberus-agent", 50, 5)
		add("s2", "qwen", ClientDelegate, 1, 1)
		add("", "qwen", "cerberus-chat", 7, 7)
		got := s.BySession([]string{"s1", "nope", ""})
		if len(got) != 2 {
			t.Fatalf("keep %d: %+v", keep, got)
		}
		if g := got[0]; g.Model != "cloud/big" || g.Requests != 1 || g.PromptTokens != 50 || g.Client != "cerberus-agent" {
			t.Errorf("keep %d: first %+v", keep, g)
		}
		if g := got[1]; g.Model != "qwen" || g.Requests != 2 || g.PromptTokens != 300 || g.CompletionTokens != 30 ||
			g.Client != ClientDelegate || g.Provider != "ollama" || g.Last.Before(now) {
			t.Errorf("keep %d: second %+v", keep, g)
		}
		if after := time.Now(); got[1].Last.After(after) {
			t.Errorf("keep %d: Last %v is in the future (now %v)", keep, got[1].Last, after)
		}
		if s.BySession(nil) != nil || len(s.BySession([]string{""})) != 0 {
			t.Errorf("keep %d: empty ids match something", keep)
		}
		s.Close()
	}
}
