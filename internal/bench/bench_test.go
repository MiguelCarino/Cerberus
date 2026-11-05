package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The prompt set is append-only: a changed prompt gets a new name. Add a
// new entry's hash here; never change an existing one.
var pinned = map[string]string{
	"code_a":      "475cc2df6eda561e",
	"code_b":      "2b945b1f1e785435",
	"edit_a":      "de19bae3429e2d00",
	"edit_b":      "75cee8baf8137b0d",
	"proof_a":     "9f6815f299aa0dc6",
	"proof_b":     "a4fd9d213e45edb5",
	"procedure_a": "49349382cb671f0f",
	"procedure_b": "e13ff68c8483a4e0",
	"chat_a":      "4f44811df2cc56fa",
	"chat_b":      "6553e55db31d8745",
	"prose_a":     "7c2b4a886f149de4",
	"prose_b":     "f789bc8ad27e127e",
}

const pinnedPrefill = "db431d426274ec99"

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

func TestPromptSetIsAppendOnly(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range set.Prompts {
		if seen[p.Name] {
			t.Errorf("%s appears twice", p.Name)
		}
		seen[p.Name] = true
		got := hash(p.Name + "\x00" + p.Kind + "\x00" + strconv.Itoa(p.MaxTokens) + "\x00" + p.Prompt)
		want, ok := pinned[p.Name]
		if !ok {
			t.Errorf("new prompt %s: pin its hash %q in bench_test.go", p.Name, got)
			continue
		}
		if got != want {
			t.Errorf("prompt %s changed (hash %s, pinned %s): add a new name instead of editing", p.Name, got, want)
		}
	}
	for name := range pinned {
		if !seen[name] {
			t.Errorf("prompt %s was removed", name)
		}
	}
	f := set.Prefill
	if got := hash(f.Question + "\x00" + strconv.Itoa(f.MaxTokens) + "\x00" + strings.Join(f.Filler, "\n")); got != pinnedPrefill {
		t.Errorf("prefill text changed (hash %s)", got)
	}
	for _, k := range Kinds {
		n := 0
		for _, p := range set.Prompts {
			if p.Kind == k {
				n++
			}
		}
		if n < 2 {
			t.Errorf("kind %s has %d prompts", k, n)
		}
	}
}

func TestJobs(t *testing.T) {
	set, _ := Load()
	if n := len(set.jobs(Options{})); n != 12 {
		t.Errorf("all kinds: %d jobs", n)
	}
	if n := len(set.jobs(Options{Kinds: []string{"code", "chat"}})); n != 4 {
		t.Errorf("two kinds: %d jobs", n)
	}
	j := set.jobs(Options{Kinds: []string{"none"}, Prefill: true, Ctx: 40000})
	if len(j) != 3 || j[0].kind != "prefill_8k" || j[2].kind != "prefill_19k" {
		t.Errorf("prefill jobs %v", kinds(j))
	}
	if j := set.jobs(Options{Kinds: []string{"none"}, Prefill: true, Ctx: 16384}); len(j) != 2 {
		t.Errorf("a 16k context runs %v", kinds(j)) // 8k and ctx/2 = 8k; 32k does not fit
	}
	if a, b := set.filler(1000), set.filler(1000); a != b || len(a) < 4000 {
		t.Error("filler is not stable")
	}
}

func kinds(js []job) []string {
	var out []string
	for _, j := range js {
		out = append(out, j.kind)
	}
	return out
}

// fakeDaemon serves the gateway (a canned stream) and /api/usage, /api/status.
func fakeDaemon(t *testing.T, busy bool) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	var tags []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		switch r.URL.Path {
		case "/api/status":
			n := 0
			if busy {
				n = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": map[string]int{"running": n}})
		case "/api/usage":
			mu.Lock()
			var recent []map[string]any
			for _, tg := range tags {
				recent = append(recent, map[string]any{"SessionID": tg, "EnergyJ": 2.0})
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"recent": recent})
		case "/v1/chat/completions":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			bodies = append(bodies, b)
			tags = append(tags, r.Header.Get("X-Cerberus-Session"))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			time.Sleep(20 * time.Millisecond) // the prompt read
			for i := 0; i < 5; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d \"}}]}\n\n", i)
				w.(http.Flusher).Flush()
				time.Sleep(15 * time.Millisecond)
			}
			io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestExecuteAgainstFakeDaemon(t *testing.T) {
	srv, bodies := fakeDaemon(t, false)
	c := Client{Base: srv.URL, Token: "tok"}
	set, _ := Load()
	r := &Run{ID: NewID(time.Now(), "qwen"), Options: Options{Model: "qwen", Repeat: 2, Kinds: []string{"code", "chat"}}}
	var steps []string
	if err := Execute(context.Background(), c, set, r.Options, r, func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1+2*4 {
		t.Errorf("%d requests (1 warm-up + 2 x 4)", len(*bodies))
	}
	b := (*bodies)[1]
	if b["temperature"] != float64(0) || b["reasoning_effort"] != "none" || b["model"] != "qwen" {
		t.Errorf("request %v", b)
	}
	if len(r.Kinds) != 2 || r.Kinds[0].Kind != "code" || len(r.Kinds[0].Samples) != 4 {
		t.Fatalf("kinds %+v", r.Kinds)
	}
	m := r.Kinds[0].Median
	if m.TTFTMs < 15 || m.DecodeTPS <= 0 || m.PrefillTPS <= 0 || m.EnergyJ != 2 || m.TokPerJ != 2.5 {
		t.Errorf("median %+v", m)
	}
	if len(steps) == 0 || !strings.Contains(steps[0], "warming up") {
		t.Errorf("progress %v", steps)
	}
}

func TestBusy(t *testing.T) {
	srv, _ := fakeDaemon(t, true)
	if busy, err := (Client{Base: srv.URL, Token: "tok"}).Busy(context.Background()); err != nil || !busy {
		t.Errorf("busy %v %v", busy, err)
	}
}

func TestSaveListGetCompare(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	a := &Run{ID: NewID(at, "qwen/3.8"), Time: at, State: "done", Options: Options{Model: "qwen"},
		Kinds: []KindResult{{Kind: "code", Median: Stats{DecodeTPS: 100, TTFTMs: 200}}, {Kind: "prefill_8k", Median: Stats{PrefillTPS: 2000}}}}
	b := &Run{ID: NewID(at.Add(time.Hour), "qwen"), Time: at.Add(time.Hour), State: "done", Options: Options{Model: "qwen"},
		Config: ModelConfig{Draft: "/m/draft.gguf"},
		Kinds:  []KindResult{{Kind: "code", Median: Stats{DecodeTPS: 130, TTFTMs: 190}}, {Kind: "prefill_8k", Median: Stats{PrefillTPS: 2010}}}}
	for _, r := range []*Run{a, b} {
		if err := Save(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(a.ID, "20261105-100000-qwen_3.8") {
		t.Errorf("id %s", a.ID)
	}
	runs, err := List(dir)
	if err != nil || len(runs) != 2 || runs[0].ID != b.ID {
		t.Fatalf("list %v %v", runs, err)
	}
	got, err := Get(dir, a.ID)
	if err != nil || got.Kinds[0].Median.DecodeTPS != 100 {
		t.Fatalf("get %v %v", got, err)
	}
	if _, err := Get(dir, "../../etc/passwd"); err == nil {
		t.Error("path escape accepted")
	}
	rows := Compare(a, b)
	if len(rows) != 2 || rows[0].DecodePct < 29.9 || rows[0].DecodePct > 30.1 {
		t.Fatalf("rows %+v", rows)
	}
	s := Summary(a, b, rows)
	for _, want := range []string{"decode: code +30%", "prefill: prefill_8k flat", "draft model draft.gguf"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q lacks %q", s, want)
		}
	}
}

func TestSummarizeSkipsErrors(t *testing.T) {
	kr := &KindResult{Samples: []Sample{{DecodeTPS: 10}, {DecodeTPS: 30}, {Error: "boom"}, {DecodeTPS: 20}}}
	summarize(kr)
	if kr.Median.DecodeTPS != 20 || kr.Min.DecodeTPS != 10 || kr.Max.DecodeTPS != 30 || kr.Errors != 1 {
		t.Errorf("%+v", kr)
	}
}
