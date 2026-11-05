package audit

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogRedactsClipsAndStripsQuery(t *testing.T) {
	dir := t.TempDir()
	secret := "sk-live-ABCDEF123456"
	l, err := Open(dir, Options{Secrets: func() []string { return []string{secret, "abc"} }})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	long := "follow-up: use key " + secret + " " + strings.Repeat("x", 200)
	if err := l.Log(Record{Actor: Owner("phone"), Remote: "192.168.1.5:5555", Method: "POST",
		Route: "/api/sessions/{id}/followup?token=leak", Targets: []string{"s1", "id-" + secret}, Status: 200, Detail: long}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	s := string(raw)
	if strings.Contains(s, secret) || strings.Contains(s, "ABCDEF") {
		t.Fatalf("secret leaked: %s", s)
	}
	if strings.Contains(s, "token=leak") {
		t.Errorf("query kept: %s", s)
	}
	recs, err := l.Query(time.Time{}, "", 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("query %v %v", recs, err)
	}
	r := recs[0]
	if n := len([]rune(r.Detail)); n != 80 || !strings.HasSuffix(r.Detail, "…") || !strings.Contains(r.Detail, mask) {
		t.Errorf("detail %q (%d runes)", r.Detail, n)
	}
	if r.Targets[1] != "id-"+mask || r.Actor != "owner:phone" || r.Route != "/api/sessions/{id}/followup" {
		t.Errorf("record %+v", r)
	}
	if strings.Contains(r.Detail, "abc"+mask) {
		t.Error("short values must not be masked")
	}
}

func TestRedactPartialAtClipEdge(t *testing.T) {
	// A secret straddling the 80-char edge is masked before the cut.
	secret := "SECRETSECRETSECRET"
	in := strings.Repeat("a", 70) + secret
	got := Clip(Redact(in, []string{secret}))
	if strings.Contains(got, "SECRET") {
		t.Errorf("partial secret leaked: %q", got)
	}
	if Redact("x\nkey-line-one\nkey-line-two", []string{"key-line-one\nkey-line-two"}) != "x\n"+mask {
		t.Error("multi-line value not masked whole")
	}
	if got := Redact("…key-line-two…", []string{"key-line-one\nkey-line-two"}); strings.Contains(got, "key-line-two") {
		t.Errorf("line of a multi-line value not masked: %q", got)
	}
	if got := Clip("a\x1b[31mb\nc"); got != "a [31mb c" {
		t.Errorf("clip control chars: %q", got)
	}
}

func TestFileModeAndRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir, Options{MaxBytes: 600, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", st.Mode().Perm())
		}
	}
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for i := range 40 {
		if err := l.Log(Record{Time: base.Add(time.Duration(i) * time.Second), Actor: ActorCLI, Route: "session.stop",
			Targets: []string{"s" + string(rune('a'+i%26))}, Detail: strings.Repeat("d", 60)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{p, p + ".1", p + ".2"} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if st.Size() > 600 {
			t.Errorf("%s is %d bytes, over the cap", f, st.Size())
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", f, st.Mode().Perm())
		}
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Error("kept more than Keep rotated files")
	}
	// Query reads across files, newest first, and honours since and limit.
	recs, err := l.Query(time.Time{}, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 6 || !recs[0].Time.Equal(base.Add(39*time.Second)) {
		t.Fatalf("got %d records, first %v", len(recs), recs[0].Time)
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Time.After(recs[i-1].Time) {
			t.Fatal("not newest first")
		}
	}
	got, _ := l.Query(base.Add(37*time.Second), "", 0)
	if len(got) != 3 {
		t.Errorf("since: %d records, want 3", len(got))
	}
	got, _ = l.Query(time.Time{}, "", 2)
	if len(got) != 2 {
		t.Errorf("limit: %d", len(got))
	}
}

func TestQueryActorFilter(t *testing.T) {
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, a := range []string{Agent("s1"), Agent("s2"), Owner(""), ActorCLI, ""} {
		if err := l.Event(a, "x", ""); err != nil {
			t.Fatal(err)
		}
	}
	count := func(f string) int { r, _ := l.Query(time.Time{}, f, 0); return len(r) }
	if count("agent") != 2 || count("agent:s1") != 1 || count("owner") != 1 || count("owner:admin") != 1 ||
		count("system") != 1 || count("cli") != 1 || count("") != 5 || count("agent:s") != 0 {
		t.Error("actor filter mismatch")
	}
}

func TestNilLoggerAndConcurrency(t *testing.T) {
	var nl *Logger
	if nl.Log(Record{}) != nil || nl.Close() != nil {
		t.Error("nil logger must be a no-op")
	}
	if r, err := nl.Query(time.Time{}, "", 0); r != nil || err != nil {
		t.Error("nil query")
	}
	dir := t.TempDir()
	l, err := Open(dir, Options{MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = l.Event(Agent(string(rune('a'+i))), "e", strings.Repeat("z", 30))
			}
		}()
	}
	wg.Wait()
	_ = l.Close()
	// Every line of every file is a whole record.
	matches, _ := filepath.Glob(filepath.Join(dir, FileName+"*"))
	for _, m := range matches {
		f, _ := os.Open(m)
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if !strings.HasPrefix(sc.Text(), "{") || !strings.HasSuffix(sc.Text(), "}") {
				t.Errorf("torn line in %s: %q", m, sc.Text())
			}
		}
		f.Close()
	}
}
