package flows

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed trigger schedule. Every form is reduced to the
// same model: a set of minutes of the day (local wall clock), plus the
// days of the month, months and days of the week it may run on.
//
// Accepted forms (case does not matter):
//
//	every 6h | every 30m | every 90m      every N minutes or hours, counted
//	                                      from local midnight each day
//	                                      (00:00, 06:00, 12:00, 18:00)
//	daily 08:00                           every day at 08:00
//	weekdays 08:00 | weekends 10:00       Monday to Friday | Saturday and Sunday
//	mon,thu 19:30 | mon-fri 07:15         the named days ("sun".."sat", lists, ranges)
//	0 8 * * 1-5                           a 5-field cron line: minute hour
//	                                      day-of-month month day-of-week, with
//	                                      numbers, "*", lists, ranges and "/n"
//	                                      steps (month and day names allowed,
//	                                      0 or 7 = Sunday)
//
// Like cron, when both day-of-month and day-of-week are restricted a day
// matches when either does.
//
// Local time and daylight saving: times are wall-clock times in the
// location Next is given. A time that does not exist on a spring-forward
// day (02:30 when clocks jump from 02:00 to 03:00) runs once at the
// instant Go's time.Date gives for it (03:30); a time that happens twice
// on a fall-back day runs once.
type Schedule struct {
	src     string
	minutes []int // minutes of the day, ascending, 0..1439
	dom     uint64
	month   uint64
	dow     uint64
	domStar bool
	dowStar bool
	kind    string // "every", "daily", "weekdays", "weekends", "days", "cron"
	every   int    // minutes, for "every"
	at      int    // minute of day, for the friendly day forms
}

const (
	allDom   = (uint64(1)<<32 - 1) &^ 1 // bits 1..31
	allMonth = (uint64(1)<<13 - 1) &^ 1 // bits 1..12
	allDow   = uint64(1)<<7 - 1         // bits 0..6
	weekdays = allDow &^ (1 | 1<<6)
	weekends = 1 | 1<<6
)

var (
	dayNames   = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	monthNames = []string{"", "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	dayWords   = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
)

// ParseSchedule parses a trigger schedule (see Schedule for the forms).
func ParseSchedule(src string) (*Schedule, error) {
	s := strings.Join(strings.Fields(strings.ToLower(src)), " ")
	if s == "" {
		return nil, errors.New("the schedule is empty")
	}
	sc := &Schedule{src: s, dom: allDom, month: allMonth, dow: allDow, domStar: true, dowStar: true}
	f := strings.Fields(s)
	switch {
	case f[0] == "every":
		if len(f) != 2 {
			return nil, errors.New(`write "every 6h" or "every 30m"`)
		}
		n, err := parseEvery(f[1])
		if err != nil {
			return nil, err
		}
		sc.kind, sc.every = "every", n
		for m := 0; m < 1440; m += n {
			sc.minutes = append(sc.minutes, m)
		}
		return sc, nil
	case len(f) == 2 && strings.Contains(f[1], ":"):
		at, err := parseClock(f[1])
		if err != nil {
			return nil, err
		}
		sc.at, sc.minutes = at, []int{at}
		switch f[0] {
		case "daily":
			sc.kind = "daily"
		case "weekdays":
			sc.kind, sc.dow, sc.dowStar = "weekdays", weekdays, false
		case "weekends":
			sc.kind, sc.dow, sc.dowStar = "weekends", weekends, false
		default:
			set, err := parseField(f[0], 0, 7, dayNames)
			if err != nil {
				return nil, fmt.Errorf("days %q: %v", f[0], err)
			}
			sc.kind, sc.dow, sc.dowStar = "days", foldSunday(set), false
		}
		return sc, nil
	case len(f) == 5:
		return parseCron(sc, f)
	}
	return nil, fmt.Errorf("%q is not a schedule: use every 6h, daily 08:00, weekdays 08:00, mon,thu 19:30 or a 5-field cron line", src)
}

func parseEvery(v string) (int, error) {
	if len(v) < 2 {
		return 0, fmt.Errorf("%q: write a number and h or m (6h, 30m)", v)
	}
	unit := v[len(v)-1]
	n, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q: write a number and h or m (6h, 30m)", v)
	}
	switch unit {
	case 'h':
		n *= 60
	case 'm':
	default:
		return 0, fmt.Errorf("%q: the unit is h (hours) or m (minutes)", v)
	}
	if n > 1440 {
		return 0, fmt.Errorf("%q: at most 24h; use daily or a cron line for longer", v)
	}
	return n, nil
}

func parseClock(v string) (int, error) {
	hh, mm, ok := strings.Cut(v, ":")
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if !ok || err1 != nil || err2 != nil || len(mm) != 2 || len(hh) < 1 || len(hh) > 2 || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM, 00:00 to 23:59)", v)
	}
	return h*60 + m, nil
}

func parseCron(sc *Schedule, f []string) (*Schedule, error) {
	mins, err := parseField(f[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("minute %q: %v", f[0], err)
	}
	hours, err := parseField(f[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("hour %q: %v", f[1], err)
	}
	if sc.dom, err = parseField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day of month %q: %v", f[2], err)
	}
	if sc.month, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month %q: %v", f[3], err)
	}
	dow, err := parseField(f[4], 0, 7, dayNames)
	if err != nil {
		return nil, fmt.Errorf("day of week %q: %v", f[4], err)
	}
	sc.dow = foldSunday(dow)
	sc.domStar = strings.HasPrefix(f[2], "*")
	sc.dowStar = strings.HasPrefix(f[4], "*")
	sc.kind = "cron"
	for h := 0; h < 24; h++ {
		if hours&(1<<h) == 0 {
			continue
		}
		for m := 0; m < 60; m++ {
			if mins&(1<<m) != 0 {
				sc.minutes = append(sc.minutes, h*60+m)
			}
		}
	}
	if _, ok := sc.next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); !ok {
		return nil, errors.New("this cron line never runs")
	}
	return sc, nil
}

// foldSunday maps day 7 onto 0.
func foldSunday(set uint64) uint64 {
	if set&(1<<7) != 0 {
		set = set&^(1<<7) | 1
	}
	return set
}

// parseField parses one cron field (or a list of day names) into a bit
// set over lo..hi.
func parseField(v string, lo, hi int, names []string) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(v, ",") {
		if part == "" {
			return 0, errors.New("empty list item")
		}
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q", stepS)
			}
			step = n
		}
		var a, b int
		switch {
		case rng == "*":
			a, b = lo, hi
			if names != nil && hi == 7 {
				b = 6 // "*" days: Sunday once
			}
		case strings.Contains(rng, "-"):
			as, bs, _ := strings.Cut(rng, "-")
			var err error
			if a, err = fieldValue(as, lo, hi, names); err != nil {
				return 0, err
			}
			if b, err = fieldValue(bs, lo, hi, names); err != nil {
				return 0, err
			}
			if names != nil && hi == 7 && b == 0 && a > 0 {
				b = 7 // "fri-sun": Sunday as 7
			}
			if b < a {
				return 0, fmt.Errorf("range %q runs backwards", rng)
			}
		default:
			var err error
			if a, err = fieldValue(rng, lo, hi, names); err != nil {
				return 0, err
			}
			b = a
			if hasStep {
				b = hi
			}
		}
		for i := a; i <= b; i += step {
			set |= 1 << i
		}
	}
	return set, nil
}

func fieldValue(v string, lo, hi int, names []string) (int, error) {
	for i, n := range names {
		if n != "" && v == n {
			return i, nil
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", v)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%d is out of range %d-%d", n, lo, hi)
	}
	return n, nil
}

// String is the schedule as normalised (lower case, single spaces).
func (s *Schedule) String() string { return s.src }

// Next is the first run time strictly after t, in loc (time.Local when
// nil). It is the zero time when there is none in the next five years.
func (s *Schedule) Next(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	n, _ := s.next(t, loc)
	return n
}

func (s *Schedule) dayMatches(d time.Time) bool {
	dom := s.dom&(1<<d.Day()) != 0
	dow := s.dow&(1<<int(d.Weekday())) != 0
	if s.month&(1<<int(d.Month())) == 0 {
		return false
	}
	if !s.domStar && !s.dowStar {
		return dom || dow
	}
	return dom && dow
}

func (s *Schedule) next(t time.Time, loc *time.Location) (time.Time, bool) {
	lt := t.In(loc)
	y, mo, d := lt.Date()
	for i := 0; i < 5*366+2; i++ {
		day := time.Date(y, mo, d+i, 12, 0, 0, 0, loc) // noon: never in a DST gap
		if !s.dayMatches(day) {
			continue
		}
		dy, dm, dd := day.Date()
		var best time.Time
		for _, m := range s.minutes {
			c := time.Date(dy, dm, dd, m/60, m%60, 0, 0, loc)
			gap := false
			if got := wallMinute(c, dy, dm, dd); got != m {
				// m does not exist today (spring forward). Go may normalise
				// the wall time either way: when it went back (got < m),
				// move forward by the difference, to the instant just past
				// the jump; when it went forward, c is already past it.
				gap = true
				if got < m {
					c = c.Add(time.Duration(m-got) * time.Minute)
				}
			}
			if !c.After(t) {
				continue
			}
			if best.IsZero() || c.Before(best) {
				best = c
			}
			if !gap {
				break // later minutes are later instants
			}
		}
		if !best.IsZero() {
			return best, true
		}
	}
	return time.Time{}, false
}

// wallMinute is c's wall-clock minute counted from the start of the day
// y-m-d (negative on the day before, 1440 and up on the day after), so a
// normalisation across midnight compares the right way.
func wallMinute(c time.Time, y int, m time.Month, d int) int {
	got := c.Hour()*60 + c.Minute()
	cy, cm, cd := c.Date()
	switch day, want := time.Date(cy, cm, cd, 0, 0, 0, 0, time.UTC), time.Date(y, m, d, 0, 0, 0, 0, time.UTC); {
	case day.After(want):
		got += 1440
	case day.Before(want):
		got -= 1440
	}
	return got
}

// Describe is the schedule in words: "Every 6 hours", "Daily at 08:00",
// "Weekdays at 08:00", "Mon, Thu at 19:30", "Cron 0 8 * * 1-5".
func (s *Schedule) Describe() string {
	clock := func(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }
	switch s.kind {
	case "every":
		switch {
		case s.every%60 == 0 && s.every == 60:
			return "Every hour"
		case s.every%60 == 0:
			return fmt.Sprintf("Every %d hours", s.every/60)
		case s.every == 1:
			return "Every minute"
		default:
			return fmt.Sprintf("Every %d minutes", s.every)
		}
	case "daily":
		return "Daily at " + clock(s.at)
	case "weekdays":
		return "Weekdays at " + clock(s.at)
	case "weekends":
		return "Weekends at " + clock(s.at)
	case "days":
		var names []string
		for i := 1; i <= 7; i++ { // Monday first
			if s.dow&(1<<(i%7)) != 0 {
				names = append(names, dayWords[i%7])
			}
		}
		return strings.Join(names, ", ") + " at " + clock(s.at)
	}
	return "Cron " + s.src
}

// Missed counts the run times in (last, now], at most limit.
func (s *Schedule) Missed(last, now time.Time, loc *time.Location, limit int) int {
	n := 0
	for t := s.Next(last, loc); !t.IsZero() && !t.After(now) && n < limit; t = s.Next(t, loc) {
		n++
	}
	return n
}

// DescribeTrigger is a trigger in words: "Manual", or the schedule's
// Describe (the raw schedule when it does not parse).
func DescribeTrigger(t Trigger) string {
	if t.Kind != TriggerSchedule {
		return "Manual"
	}
	sc, err := ParseSchedule(t.Schedule)
	if err != nil {
		return t.Schedule
	}
	return sc.Describe()
}
