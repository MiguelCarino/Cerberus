package flows

import (
	"testing"
	"time"
	_ "time/tzdata" // DST tests need America/New_York everywhere
)

func mustSched(t *testing.T, s string) *Schedule {
	t.Helper()
	sc, err := ParseSchedule(s)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", s, err)
	}
	return sc
}

func TestScheduleNextFriendlyAndCron(t *testing.T) {
	loc := time.UTC
	// 2026-11-02 is a Monday.
	from := time.Date(2026, 11, 2, 9, 10, 0, 0, loc)
	cases := []struct {
		sched string
		want  time.Time
	}{
		{"every 6h", time.Date(2026, 11, 2, 12, 0, 0, 0, loc)},
		{"every 30m", time.Date(2026, 11, 2, 9, 30, 0, 0, loc)},
		{"every 90m", time.Date(2026, 11, 2, 10, 30, 0, 0, loc)},
		{"every 24h", time.Date(2026, 11, 3, 0, 0, 0, 0, loc)},
		{"daily 08:00", time.Date(2026, 11, 3, 8, 0, 0, 0, loc)},
		{"daily 9:15", time.Date(2026, 11, 2, 9, 15, 0, 0, loc)},
		{"weekdays 08:00", time.Date(2026, 11, 3, 8, 0, 0, 0, loc)},
		{"weekends 10:00", time.Date(2026, 11, 7, 10, 0, 0, 0, loc)},
		{"mon,thu 19:30", time.Date(2026, 11, 2, 19, 30, 0, 0, loc)},
		{"Thu 07:00", time.Date(2026, 11, 5, 7, 0, 0, 0, loc)},
		{"sat-sun 06:00", time.Date(2026, 11, 7, 6, 0, 0, 0, loc)},
		{"0 8 * * 1-5", time.Date(2026, 11, 3, 8, 0, 0, 0, loc)},
		{"*/15 * * * *", time.Date(2026, 11, 2, 9, 15, 0, 0, loc)},
		{"5,35 9-10 * * *", time.Date(2026, 11, 2, 9, 35, 0, 0, loc)},
		{"0 0 1 * *", time.Date(2026, 12, 1, 0, 0, 0, 0, loc)},
		{"0 12 * jan *", time.Date(2027, 1, 1, 12, 0, 0, 0, loc)},
		{"0 12 * * 7", time.Date(2026, 11, 8, 12, 0, 0, 0, loc)},      // 7 = Sunday
		{"0 12 * * sun", time.Date(2026, 11, 8, 12, 0, 0, 0, loc)},    // names
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, loc)},       // leap day
		{"0 6 13 * 5", time.Date(2026, 11, 6, 6, 0, 0, 0, loc)},       // dom OR dow: Friday 6th
		{"30 2-23/7 * * *", time.Date(2026, 11, 2, 9, 30, 0, 0, loc)}, // 2,9,16,23
		{"0 */8 * * *", time.Date(2026, 11, 2, 16, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		got := mustSched(t, c.sched).Next(from, loc)
		if !got.Equal(c.want) {
			t.Errorf("%q: Next = %v, want %v", c.sched, got, c.want)
		}
	}
}

func TestScheduleStrictlyAfter(t *testing.T) {
	sc := mustSched(t, "daily 08:00")
	at := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	if got := sc.Next(at, time.UTC); !got.Equal(at.AddDate(0, 0, 1)) {
		t.Fatalf("Next at the due time = %v", got)
	}
}

func TestScheduleErrors(t *testing.T) {
	for _, s := range []string{
		"", "every", "every 0h", "every 25h", "every 5s", "every h", "daily", "daily 24:00", "daily 8:5",
		"funday 08:00", "0 8 * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"0 0 31 2 *", "5-1 * * * *", "*/0 * * * *", "1,,2 * * * *", "hello",
	} {
		if _, err := ParseSchedule(s); err == nil {
			t.Errorf("ParseSchedule(%q) succeeded", s)
		}
	}
}

func TestScheduleDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// Spring forward 2026-03-08: 02:00 → 03:00 local.
	sc := mustSched(t, "daily 02:30")
	got := sc.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny), ny)
	if got.Year() != 2026 || got.Day() != 8 || got.Hour() != 3 || got.Minute() != 30 {
		t.Errorf("gap: Next = %v, want 2026-03-08 03:30 EDT", got)
	}
	// It runs once that day, then 02:30 the next.
	again := sc.Next(got, ny)
	if again.Day() != 9 || again.Hour() != 2 || again.Minute() != 30 {
		t.Errorf("after the gap: Next = %v", again)
	}
	// Daily 08:00 keeps local 08:00 across the change (the UTC hour moves).
	d8 := mustSched(t, "daily 08:00")
	a := d8.Next(time.Date(2026, 3, 7, 9, 0, 0, 0, ny), ny)
	if a.Hour() != 8 || a.UTC().Hour() != 12 {
		t.Errorf("after spring forward: %v (UTC %v)", a, a.UTC())
	}
	// Every hour on the spring-forward day: 01:00, then 03:00 (02:00 does not exist).
	h := mustSched(t, "every 1h")
	n1 := h.Next(time.Date(2026, 3, 8, 1, 0, 0, 0, ny), ny)
	if n1.Sub(time.Date(2026, 3, 8, 1, 0, 0, 0, ny)) != time.Hour || n1.Hour() != 3 {
		t.Errorf("hourly over the gap: %v", n1)
	}
	// Fall back 2026-11-01: 01:30 happens twice; it runs once.
	fb := mustSched(t, "daily 01:30")
	f1 := fb.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, ny), ny)
	f2 := fb.Next(f1, ny)
	if f1.Day() != 1 || f2.Day() != 2 {
		t.Errorf("fall back: %v then %v", f1, f2)
	}
	// Every 30m on the fall-back day never repeats or goes backwards.
	e := mustSched(t, "every 30m")
	prev := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	for i := 0; i < 60; i++ {
		n := e.Next(prev, ny)
		if !n.After(prev) {
			t.Fatalf("not after: %v → %v", prev, n)
		}
		prev = n
	}
}

func TestScheduleDescribeAndMissed(t *testing.T) {
	for s, want := range map[string]string{
		"every 6h":       "Every 6 hours",
		"every 1h":       "Every hour",
		"every 30m":      "Every 30 minutes",
		"daily 08:00":    "Daily at 08:00",
		"Weekdays 8:00":  "Weekdays at 08:00",
		"weekends 10:00": "Weekends at 10:00",
		"thu,mon 19:30":  "Mon, Thu at 19:30",
		"sun 09:00":      "Sun at 09:00",
		"0 8 * * 1-5":    "Cron 0 8 * * 1-5",
	} {
		if got := mustSched(t, s).Describe(); got != want {
			t.Errorf("%q: Describe = %q, want %q", s, got, want)
		}
	}
	if got := DescribeTrigger(Trigger{Kind: TriggerManual}); got != "Manual" {
		t.Errorf("manual: %q", got)
	}
	sc := mustSched(t, "every 1h")
	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n := sc.Missed(last, last.Add(5*time.Hour+time.Minute), time.UTC, 1000); n != 5 {
		t.Errorf("Missed = %d, want 5", n)
	}
	if n := sc.Missed(last, last.Add(50*time.Hour), time.UTC, 3); n != 3 {
		t.Errorf("Missed capped = %d", n)
	}
}

// In zones where Go normalises a nonexistent wall time forward (Lord Howe
// Island's 30-minute shift), the gap time still runs that day, just after
// the jump, instead of skipping to the next day.
func TestScheduleDSTForwardNormalisation(t *testing.T) {
	lh, err := time.LoadLocation("Australia/Lord_Howe")
	if err != nil {
		t.Skip(err)
	}
	// Spring forward 2026-10-04: 02:00 → 02:30 local.
	sc := mustSched(t, "daily 02:15")
	got := sc.Next(time.Date(2026, 10, 3, 12, 0, 0, 0, lh), lh)
	jump := time.Date(2026, 10, 4, 2, 30, 0, 0, lh)
	if got.Before(jump) || got.Sub(jump) > 30*time.Minute || got.Day() != 4 {
		t.Errorf("gap: Next = %v, want 2026-10-04 just after 02:30", got)
	}
	if again := sc.Next(got, lh); again.Day() != 5 || again.Hour() != 2 || again.Minute() != 15 {
		t.Errorf("after the gap: Next = %v", again)
	}
	// Whatever way Go normalises, every gap time lands just past the jump.
	for _, at := range []string{"daily 02:00", "daily 02:01", "daily 02:29"} {
		n := mustSched(t, at).Next(time.Date(2026, 10, 3, 12, 0, 0, 0, lh), lh)
		if n.Before(jump) || n.Sub(jump) > 30*time.Minute {
			t.Errorf("%s: Next = %v", at, n)
		}
	}
}
