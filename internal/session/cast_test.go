package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeClock advances by step on every call.
func fakeClock(step time.Duration) func() time.Time {
	t := time.Unix(1_800_000_000, 0)
	return func() time.Time {
		cur := t
		t = t.Add(step)
		return cur
	}
}

// readCast parses a cast file into its header and events.
func readCast(t *testing.T, path string) (castHeader, [][]any) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var hdr castHeader
	var evs [][]any
	for i := 0; sc.Scan(); i++ {
		if i == 0 {
			if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
				t.Fatalf("header %q: %v", sc.Text(), err)
			}
			continue
		}
		var ev []any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("event %q: %v", sc.Text(), err)
		}
		evs = append(evs, ev)
	}
	return hdr, evs
}

func TestCastFormat(t *testing.T) {
	dir := t.TempDir()
	c, err := openCast(dir, 2, 100, 30, "fix the build", 1<<20, fakeClock(250*time.Millisecond))
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	c.write([]byte("hello \x1b[31mred\x1b[0m\r\n"))
	c.write([]byte("caf\xc3")) // é split across chunks
	c.write([]byte("\xa9 done"))
	c.close()
	path := filepath.Join(dir, "runs", "2.cast")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	hdr, evs := readCast(t, path)
	if hdr.Version != 2 || hdr.Width != 100 || hdr.Height != 30 || hdr.Timestamp != 1_800_000_000 || hdr.Title != "fix the build" {
		t.Fatalf("header %+v", hdr)
	}
	if len(evs) != 3 {
		t.Fatalf("events %v", evs)
	}
	if evs[0][1] != "o" || evs[0][2] != "hello \x1b[31mred\x1b[0m\r\n" || evs[0][0].(float64) != 0.25 {
		t.Fatalf("event 0 %v", evs[0])
	}
	// The split character is held back whole, never mangled to U+FFFD.
	if evs[1][2] != "caf" || evs[2][2] != "é done" || evs[2][0].(float64) <= evs[1][0].(float64) {
		t.Fatalf("events 1-2 %v", evs[1:])
	}
}

func TestCastCapStopsWithMarker(t *testing.T) {
	dir := t.TempDir()
	const capBytes = 4096
	c, err := openCast(dir, 1, 80, 24, "", capBytes, fakeClock(time.Millisecond))
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	chunk := []byte(strings.Repeat("x", 100))
	for range 200 {
		c.write(chunk)
	}
	if !c.stopped {
		t.Fatal("not stopped at the cap")
	}
	c.write([]byte("after the cap"))
	c.close()
	path := filepath.Join(dir, "runs", "1.cast")
	st, _ := os.Stat(path)
	if st.Size() > capBytes {
		t.Fatalf("size %d over cap %d", st.Size(), capBytes)
	}
	_, evs := readCast(t, path)
	last, notice := evs[len(evs)-1], evs[len(evs)-2]
	if last[1] != "m" || last[2] != castMarker || notice[1] != "o" || notice[2] != castNotice {
		t.Fatalf("tail %v %v", notice, last)
	}
	for _, ev := range evs {
		if strings.Contains(ev[2].(string), "after the cap") {
			t.Fatal("recorded past the cap")
		}
	}
}

func TestCastOffAndBadAttempt(t *testing.T) {
	dir := t.TempDir()
	if c, err := openCast(dir, 1, 80, 24, "", 0, nil); c != nil || err != nil {
		t.Fatal("cap 0 must record nothing", c, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); !os.IsNotExist(err) {
		t.Fatal("runs folder created with recording off")
	}
	if _, err := openCast(dir, 0, 80, 24, "", 1<<20, nil); err == nil {
		t.Fatal("attempt 0 accepted")
	}
}

func TestIncompleteTail(t *testing.T) {
	for in, want := range map[string]int{
		"abc": 0, "a\xc3": 1, "a\xc3\xa9": 0, "\xe2\x82": 2, "\xe2\x82\xac": 0,
		"\xf0\x9f\x98": 3, "\xf0\x9f\x98\x80": 0, "\x80\x80\x80": 0, "": 0,
	} {
		if got := incompleteTail([]byte(in)); got != want {
			t.Errorf("incompleteTail(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseRunName(t *testing.T) {
	if n, ok := ParseRunName("3.cast"); !ok || n != 3 {
		t.Fatal(n, ok)
	}
	for _, bad := range []string{"0.cast", "../1.cast", "1.cast.bak", "x.cast", "-1.cast", "1"} {
		if _, ok := ParseRunName(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
