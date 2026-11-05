package finetune

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCollectRateExport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ft")
	s, err := New(dir, 4, func() []string { return []string{"sk-SECRETVALUE123"} })
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }
	prompt := []Message{{Role: "user", Content: "Summarise this log. key=sk-SECRETVALUE123"}}
	good, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Messages: prompt, Answer: "It failed at step 3."})
	fixed, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Messages: prompt, Answer: "All fine."})
	bad, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Messages: prompt, Answer: "Nonsense."})
	unrated, _ := s.Add(Example{Kind: KindDelegated, Model: "qwen", Messages: prompt, Answer: "Maybe."})
	cloud, _ := s.Add(Example{Kind: KindCloud, Model: "p/big", Messages: prompt, Answer: "Step 3 timed out."})
	for _, id := range []string{good, fixed, bad, unrated, cloud} {
		if !strings.HasPrefix(id, "ex-") {
			t.Fatalf("id %q", id)
		}
	}
	must(t, s.Rate(Rating{ID: good, Good: true}))
	must(t, s.Rate(Rating{ID: fixed, Good: false, Better: "Step 3 timed out after 30 s."}))
	must(t, s.Rate(Rating{ID: bad, Good: true}))
	must(t, s.Rate(Rating{ID: bad, Good: false})) // the newest verdict wins
	if err := s.Rate(Rating{ID: "nope"}); err == nil {
		t.Error("rating an unknown id was accepted")
	}
	if _, err := s.Add(Example{Kind: KindCloud, Messages: prompt, Answer: strings.Repeat("x", 5000)}); err != ErrTooBig {
		t.Errorf("big example: %v", err)
	}

	st, err := s.Stats()
	must(t, err)
	if st.Examples != 5 || st.ByKind[KindDelegated] != 4 || st.Rated != 3 || st.Good != 1 || st.Corrected != 1 || st.Usable != 3 || st.UsableUnrated != 4 {
		t.Errorf("stats %+v", st)
	}

	var buf bytes.Buffer
	n, err := s.Export(&buf, ExportOptions{})
	must(t, err)
	if n != 3 {
		t.Fatalf("exported %d rows, want 3 (good, corrected, cloud):\n%s", n, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"It failed at step 3.", "Step 3 timed out after 30 s.", "Step 3 timed out."} {
		if !strings.Contains(out, want) {
			t.Errorf("export lacks %q", want)
		}
	}
	for _, not := range []string{"All fine.", "Nonsense.", "Maybe.", "sk-SECRETVALUE123"} {
		if strings.Contains(out, not) {
			t.Errorf("export has %q", not)
		}
	}
	var row struct{ Messages []Message }
	must(t, json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &row))
	if len(row.Messages) != 2 || row.Messages[1].Role != "assistant" || !strings.Contains(row.Messages[0].Content, "[secret]") {
		t.Errorf("row %+v", row)
	}

	buf.Reset()
	n, _ = s.Export(&buf, ExportOptions{Unrated: true, Kinds: []string{KindDelegated}})
	if n != 3 { // good, corrected, unrated; never the one rated bad
		t.Errorf("unrated export: %d rows", n)
	}

	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(dir)
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v", fi.Mode().Perm())
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, f := range files {
			fi, _ := os.Stat(f)
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v", f, fi.Mode().Perm())
			}
		}
	}
}

func TestExpand(t *testing.T) {
	got := Expand([]string{"train", "--data={dataset}", "{base_model}", "-o", "{out}"}, "/d.jsonl", "qwen", "/out")
	if strings.Join(got, " ") != "train --data=/d.jsonl qwen -o /out" {
		t.Errorf("%v", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
