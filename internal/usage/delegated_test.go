package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDelegated(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	add := func(session, model, client string, p, c int, j float64) {
		s.Add(Record{Time: now, SessionID: session, Model: model, Client: client, Local: true,
			PromptTokens: p, CompletionTokens: c, EnergyJ: j, DurationMs: 2000, Status: 200})
	}
	add("s1", "qwen", ClientDelegate, 1000, 200, 50)
	add("s1", "qwen", ClientDelegate, 2000, 100, 30)
	add("s2", "gemma", ClientDelegate, 500, 50, 10)
	add("s1", "qwen", "cerberus-chat", 9999, 999, 999) // not delegated
	d := s.Delegated(now.Add(-time.Hour), "", "p/big", 3, 15)
	if d.Total.Requests != 3 || d.Total.PromptTokens != 3500 || d.Total.CompletionTokens != 350 || d.Total.EnergyJ != 90 || d.Total.Seconds != 6 {
		t.Fatalf("total %+v", d.Total)
	}
	// 3500 × 3/M + 350 × 15/M = 0.0105 + 0.00525
	if d.Total.SavedUSD < 0.01574 || d.Total.SavedUSD > 0.01576 {
		t.Errorf("saved %v", d.Total.SavedUSD)
	}
	if len(d.BySession) != 2 || d.BySession[0].Key != "s1" || d.BySession[0].Requests != 2 || len(d.ByModel) != 2 {
		t.Errorf("by session %+v by model %+v", d.BySession, d.ByModel)
	}
	one := s.Delegated(now.Add(-time.Hour), "s2", "", 0, 0)
	if one.Total.Requests != 1 || one.Total.SavedUSD != 0 || len(one.BySession) != 1 {
		t.Errorf("one session %+v", one)
	}
}
