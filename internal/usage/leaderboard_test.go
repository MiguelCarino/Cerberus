package usage

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// leaderFixture: now is 2026-11-15 12:00 in UTC-6.
func leaderFixture(t *testing.T, keep int) (Sources, time.Time) {
	t.Helper()
	zone := time.FixedZone("CST", -6*3600)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, zone)
	s, err := Open(filepath.Join(t.TempDir(), "usage.jsonl"), keep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.now = func() time.Time { return now }
	at := func(days int, h, m int) time.Time {
		y, mo, d := now.AddDate(0, 0, -days).Date()
		return time.Date(y, mo, d, h, m, 0, 0, zone)
	}
	const kWhDC = 2.88e6 // 1 kWh at the wall through a PSU at 80 %
	for _, r := range []Record{
		{Time: at(20, 9, 0), SessionID: "s3", Model: "qwen", PromptTokens: 1, CompletionTokens: 1, DurationMs: 500},
		{Time: at(3, 10, 0), SessionID: "s1", Model: "qwen", PromptTokens: 200, CompletionTokens: 100, EnergyJ: kWhDC, DurationMs: 4000},
		{Time: at(0, 10, 0), SessionID: "s1", Model: "qwen", PromptTokens: 100, CompletionTokens: 50, EnergyJ: kWhDC, DurationMs: 2000},
		{Time: at(0, 11, 0), SessionID: "s2", Model: "gpt-x", Provider: "openai", PromptTokens: 1000, CompletionTokens: 500, CostUSD: 0.5, DurationMs: 3000},
		{Time: at(0, 11, 30), Client: "zed", Model: "gpt-x", Provider: "openai", PromptTokens: 10, CompletionTokens: 10, CostUSD: 0.01, DurationMs: 1000},
	} {
		s.Add(r)
	}
	sessions := []SessionInfo{
		{ID: "s1", Label: "Fix login", Agent: "claude", Repo: "web", Start: at(3, 9, 0), End: at(0, 10, 30), WallJ: 4 * 3.6e6},
		{ID: "s2", Label: "Docs", Agent: "codex", Start: at(0, 11, 0)}, // running
		{ID: "s4", Label: "Old", Agent: "claude", Repo: "web", Start: at(40, 9, 0), End: at(39, 9, 0), WallJ: 3.6e6},
	}
	return Sources{Usage: s, Sessions: sessions, WallJ: func(j float64) float64 { return j / 0.8 },
		PricePerKWh: 2.5, Now: now}, now
}

type wantRow struct {
	key   string
	value float64
}

func TestLeaderboardTable(t *testing.T) {
	s1Life := (73*time.Hour + 30*time.Minute).Seconds()
	cases := []struct {
		period, by, metric string
		limit              int
		want               []wantRow
		total              float64
	}{
		{"today", "session", "tokens", 0, []wantRow{{"s2", 1500}, {"s1", 150}, {"", 20}}, 1670},
		{"today", "session", "tokens", 1, []wantRow{{"s2", 1500}}, 1670},
		{"today", "agent", "cost", 0, []wantRow{{"codex", 0.5}, {"client:zed", 0.01}}, 0.51},
		{"today", "model", "energy", 0, []wantRow{{"qwen", 1}}, 1},
		{"today", "model", "time", 0, []wantRow{{"gpt-x", 4}, {"qwen", 2}}, 6},
		// s1 lived 10.5 h of 73.5 h today: its prorated 4/7 kWh is below its
		// requests' 1 kWh, so the requests win.
		{"today", "session", "energy", 0, []wantRow{{"s1", 1}}, 1},
		{"today", "repo", "time", 0, []wantRow{{"web", 10.5 * 3600}, {"-", 3600}, {"", 1}}, 10.5*3600 + 3600 + 1},
		{"7d", "session", "energy", 0, []wantRow{{"s1", 4}}, 4},
		{"7d", "session", "time", 0, []wantRow{{"s1", s1Life}, {"s2", 3600}, {"", 1}}, s1Life + 3601},
		{"7d", "model", "tokens", 0, []wantRow{{"gpt-x", 1520}, {"qwen", 450}}, 1970},
		{"30d", "model", "tokens", 0, []wantRow{{"gpt-x", 1520}, {"qwen", 452}}, 1972},
		{"30d", "agent", "tokens", 0, []wantRow{{"codex", 1500}, {"claude", 450}, {"client:zed", 20}, {"?", 2}}, 1972},
		{"month", "model", "tokens", 0, []wantRow{{"gpt-x", 1520}, {"qwen", 450}}, 1970},
	}
	for _, keep := range []int{1000, 2} { // exact ring, then the quarter-hour totals
		src, _ := leaderFixture(t, keep)
		for _, c := range cases {
			src.Limit = c.limit
			b, err := Leaderboard(src, c.period, c.by, c.metric)
			if err != nil {
				t.Fatalf("keep %d %s/%s/%s: %v", keep, c.period, c.by, c.metric, err)
			}
			name := c.period + "/" + c.by + "/" + c.metric
			if len(b.Rows) != len(c.want) {
				t.Errorf("keep %d %s: rows = %+v", keep, name, b.Rows)
				continue
			}
			for i, w := range c.want {
				if b.Rows[i].Key != w.key || math.Abs(b.Rows[i].Value-w.value) > 1e-9 {
					t.Errorf("keep %d %s row %d = %q %v, want %q %v", keep, name, i, b.Rows[i].Key, b.Rows[i].Value, w.key, w.value)
				}
			}
			if math.Abs(b.Total.Value-c.total) > 1e-9 {
				t.Errorf("keep %d %s total = %v, want %v", keep, name, b.Total.Value, c.total)
			}
			if b.CostCurrency != "USD" || b.Currency != "MXN" {
				t.Errorf("currencies %q %q", b.CostCurrency, b.Currency)
			}
		}
	}
}

func TestLeaderboardRowDetails(t *testing.T) {
	src, now := leaderFixture(t, 0)
	b, err := Leaderboard(src, "today", "session", "tokens")
	if err != nil {
		t.Fatal(err)
	}
	s1 := b.Rows[1]
	if s1.Label != "Fix login" || s1.Agent != "claude" || s1.Repo != "web" || s1.Requests != 1 ||
		s1.PromptTokens != 100 || s1.CompletionTokens != 50 || math.Abs(s1.EnergyCost-2.5) > 1e-9 || s1.Sessions != 1 {
		t.Fatalf("s1 = %+v", s1)
	}
	if d := b.Rows[2]; d.Label != LabelDirect || d.CostUSD != 0.01 {
		t.Fatalf("direct = %+v", d)
	}
	if !b.Until.Equal(now) || b.Since.Hour() != 0 || b.Since.Day() != 15 {
		t.Fatalf("window %v .. %v", b.Since, b.Until)
	}
	b30, _ := Leaderboard(src, "30d", "session", "tokens")
	for _, r := range b30.Rows {
		if r.Key == "s3" && r.Label != "s3" {
			t.Fatalf("pruned session label = %q", r.Label)
		}
		if r.Key == "s4" {
			t.Fatal("session outside the period listed")
		}
	}
	bm, _ := Leaderboard(src, "month", "model", "tokens")
	if !bm.Since.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, now.Location())) {
		t.Fatalf("month since %v", bm.Since)
	}
}

func TestLeaderboardErrors(t *testing.T) {
	src, _ := leaderFixture(t, 0)
	for _, c := range []struct {
		p, by, m string
		err      error
	}{
		{"year", "model", "cost", ErrPeriod},
		{"today", "client", "cost", ErrBy},
		{"today", "model", "joy", ErrMetric},
	} {
		if _, err := Leaderboard(src, c.p, c.by, c.m); !errors.Is(err, c.err) {
			t.Errorf("%v: err = %v", c, err)
		}
	}
	// Nothing recorded: an empty board, not an error.
	b, err := Leaderboard(Sources{}, "today", "repo", "energy")
	if err != nil || len(b.Rows) != 0 || b.Rows == nil {
		t.Fatalf("empty = %+v %v", b, err)
	}
}

// TestLeaderboardChatIDs: requests a chat labels with its chat id rank like
// direct traffic (by client), never as a session row.
func TestLeaderboardChatIDs(t *testing.T) {
	src, now := leaderFixture(t, 1000)
	src.Usage.Add(Record{Time: now.Add(-time.Minute), SessionID: "chat-20261115-115900-abcdef", Client: "cerberus-chat",
		Model: "qwen", PromptTokens: 7, CompletionTokens: 3})
	b, err := Leaderboard(src, "today", "session", "tokens")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range b.Rows {
		if r.Key == "chat-20261115-115900-abcdef" {
			t.Fatalf("chat ranked as a session: %+v", b.Rows)
		}
		if r.Key == "" && r.Value != 30 {
			t.Fatalf("direct row %v, want 30 (zed 20 + chat 10)", r.Value)
		}
	}
	b, err = Leaderboard(src, "today", "agent", "tokens")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range b.Rows {
		found = found || (r.Key == "client:cerberus-chat" && r.Value == 10)
	}
	if !found {
		t.Fatalf("no client:cerberus-chat row: %+v", b.Rows)
	}
}
