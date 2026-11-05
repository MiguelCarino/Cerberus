package api

import (
	"encoding/json"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/finetune"
)

// ftItem is the Example JSON the UI reads.
type ftItem struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Answer   string `json:"answer"`
	Messages []struct {
		Role, Content string
	} `json:"messages"`
	Rating *struct {
		Good   bool   `json:"good"`
		Better string `json:"better"`
		Note   string `json:"note"`
		By     string `json:"by"`
	} `json:"rating"`
	Excluded  bool `json:"excluded"`
	Truncated bool `json:"truncated"`
}

type ftPage struct {
	Examples []ftItem `json:"examples"`
	Total    int      `json:"total"`
	Next     string   `json:"next"`
}

func decodeT[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// seedFinetune adds n delegated examples (oldest first) and returns their ids.
func seedFinetune(t *testing.T, ft *finetune.Store, n int, answer string) []string {
	t.Helper()
	var ids []string
	for i := range n {
		id, err := ft.Add(finetune.Example{Kind: finetune.KindDelegated, Model: "qwen", Session: "s1",
			Messages: []finetune.Message{{Role: "user", Content: "question " + string(rune('a'+i))}}, Answer: answer})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestFinetuneExamplesAPI(t *testing.T) {
	e := newDelegEnv(t, nil)
	owner := withBearer(testToken)
	ids := seedFinetune(t, e.ft, 25, strings.Repeat("long answer ", 600)) // > 4 KiB
	cloud, _ := e.ft.Add(finetune.Example{Kind: finetune.KindCloud, Model: "p/big",
		Messages: []finetune.Message{{Role: "user", Content: "find the needle"}}, Answer: "teacher"})
	if err := e.ft.Rate(finetune.Rating{ID: ids[0], Good: true, By: finetune.ByAgent}); err != nil {
		t.Fatal(err)
	}

	// Paging: 20, then the rest; ids from the same second sort by id.
	w := e.do("GET", "/api/finetune/examples", "", owner)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	p1 := decodeT[ftPage](t, w.Body.Bytes())
	if len(p1.Examples) != 20 || p1.Total != 26 || p1.Next == "" {
		t.Fatalf("page 1: %d listed, total %d, next %q", len(p1.Examples), p1.Total, p1.Next)
	}
	long := p1.Examples[len(p1.Examples)-1]
	if !long.Truncated || len(long.Answer) > finetune.ListClip {
		t.Errorf("list answer not clipped: %d bytes, truncated %v", len(long.Answer), long.Truncated)
	}
	w = e.do("GET", "/api/finetune/examples?before="+p1.Next, "", owner)
	p2 := decodeT[ftPage](t, w.Body.Bytes())
	if len(p2.Examples) != 6 || p2.Next != "" {
		t.Fatalf("page 2: %d listed, next %q", len(p2.Examples), p2.Next)
	}
	seen := map[string]bool{}
	for _, it := range append(p1.Examples, p2.Examples...) {
		seen[it.ID] = true
	}
	if len(seen) != 26 {
		t.Errorf("paging lost or repeated examples: %d distinct", len(seen))
	}

	// Filters.
	for q, want := range map[string]int{
		"?kind=cloud": 1, "?rated=good": 1, "?rated=unrated": 25, "?q=NEEDLE": 1, "?kind=delegated&rated=unrated&limit=5": 24,
	} {
		w := e.do("GET", "/api/finetune/examples"+q, "", owner)
		if pg := decodeT[ftPage](t, w.Body.Bytes()); w.Code != 200 || pg.Total != want {
			t.Errorf("%s: %d total %d, want %d", q, w.Code, pg.Total, want)
		}
	}
	for _, q := range []string{"?kind=x", "?rated=x", "?limit=0", "?limit=500", "?limit=x", "?before=ex-nope"} {
		if w := e.do("GET", "/api/finetune/examples"+q, "", owner); w.Code != 400 {
			t.Errorf("%s: %d, want 400", q, w.Code)
		}
	}

	// One example, whole.
	w = e.do("GET", "/api/finetune/examples/"+ids[3], "", owner)
	if it := decodeT[ftItem](t, w.Body.Bytes()); w.Code != 200 || it.Truncated || len(it.Answer) <= finetune.ListClip {
		t.Errorf("get: %d truncated=%v %d bytes", w.Code, it.Truncated, len(it.Answer))
	}
	if w := e.do("GET", "/api/finetune/examples/ex-nope", "", owner); w.Code != 404 {
		t.Errorf("get unknown: %d", w.Code)
	}

	// The owner's verdict replaces the agent's.
	w = e.do("POST", "/api/finetune/examples/"+ids[0]+"/rate", `{"good":false,"better":"The right answer.","note":" too long "}`, owner)
	it := decodeT[ftItem](t, w.Body.Bytes())
	if w.Code != 200 || it.Rating == nil || it.Rating.By != "owner" || it.Rating.Better != "The right answer." || it.Rating.Note != "too long" {
		t.Fatalf("rate: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/finetune/examples?rated=corrected", "", owner); decodeT[ftPage](t, w.Body.Bytes()).Total != 1 {
		t.Errorf("corrected filter after the owner's rating: %s", w.Body)
	}
	if w := e.do("POST", "/api/finetune/examples/"+ids[1]+"/rate", `{"good":true}`, owner); w.Code != 200 {
		t.Errorf("rate good: %d", w.Code)
	}
	if w := e.do("POST", "/api/finetune/examples/ex-nope/rate", `{"good":true}`, owner); w.Code != 404 {
		t.Errorf("rate unknown: %d", w.Code)
	}
	if w := e.do("POST", "/api/finetune/examples/"+ids[1]+"/rate", `{"good":`, owner); w.Code != 400 {
		t.Errorf("rate bad body: %d", w.Code)
	}

	// Exclusion: the corrected one leaves the export, keeps its rating.
	w = e.do("GET", "/api/finetune/export", "", owner)
	if w.Header().Get("X-Cerberus-Rows") != "3" || !strings.Contains(w.Body.String(), "The right answer.") {
		t.Fatalf("export before exclusion: rows %s\n%s", w.Header().Get("X-Cerberus-Rows"), w.Body)
	}
	w = e.do("POST", "/api/finetune/examples/"+ids[0]+"/exclude", `{"exclude":true}`, owner)
	if it := decodeT[ftItem](t, w.Body.Bytes()); w.Code != 200 || !it.Excluded || it.Rating == nil || it.Rating.Better == "" {
		t.Fatalf("exclude: %d %s", w.Code, w.Body)
	}
	w = e.do("GET", "/api/finetune/export", "", owner)
	if w.Header().Get("X-Cerberus-Rows") != "2" || strings.Contains(w.Body.String(), "The right answer.") {
		t.Errorf("export after exclusion: rows %s", w.Header().Get("X-Cerberus-Rows"))
	}
	w = e.do("GET", "/api/finetune", "", owner)
	if !strings.Contains(w.Body.String(), `"excluded":1`) || !strings.Contains(w.Body.String(), `"usable":2`) {
		t.Errorf("stats: %s", w.Body)
	}
	if w := e.do("POST", "/api/finetune/examples/"+ids[0]+"/exclude", `{}`, owner); w.Code != 400 {
		t.Errorf("exclude without a value: %d", w.Code)
	}
	if w := e.do("POST", "/api/finetune/examples/ex-nope/exclude", `{"exclude":true}`, owner); w.Code != 404 {
		t.Errorf("exclude unknown: %d", w.Code)
	}
	if w := e.do("POST", "/api/finetune/examples/"+ids[0]+"/exclude", `{"exclude":false}`, owner); decodeT[ftItem](t, w.Body.Bytes()).Excluded {
		t.Error("re-include did not take")
	}

	// Agents cannot reach any of it.
	for _, rt := range []struct{ m, p string }{{"GET", "/api/finetune/examples"}, {"POST", "/api/finetune/examples/" + cloud + "/rate"},
		{"POST", "/api/finetune/train"}, {"GET", "/api/finetune/runs"}} {
		if w := e.do(rt.m, rt.p, `{}`, e.agent...); w.Code != 401 && w.Code != 403 {
			t.Errorf("agent %s %s: %d", rt.m, rt.p, w.Code)
		}
	}

	// No runs yet; train with the fake (non-manager) sessions: 409 without
	// a trainer, 503 with one but no session manager to run it.
	if w := e.do("GET", "/api/finetune/runs", "", owner); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"runs":[]}` {
		t.Errorf("runs: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/finetune/train", `{}`, owner); w.Code != 409 || !strings.Contains(w.Body.String(), "train_command") {
		t.Errorf("train without a trainer: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/finetune/train", `{"base":"bad{cwd}"}`, owner); w.Code != 400 {
		t.Errorf("train with a bad base: %d %s", w.Code, w.Body)
	}
}

func TestFinetuneTrainNeedsSessions(t *testing.T) {
	e := newDelegEnv(t, func(c *config.Config) { c.Finetune.TrainCommand = []string{"true"} })
	seedFinetune(t, e.ft, 1, "a")
	if w := e.do("POST", "/api/finetune/train", `{}`, withBearer(testToken)); w.Code != 503 {
		t.Errorf("train without a session manager: %d %s", w.Code, w.Body)
	}
	e.srv.finetune = nil
	for _, rt := range []struct{ m, p string }{{"GET", "/api/finetune/examples"}, {"GET", "/api/finetune/examples/ex-1"},
		{"POST", "/api/finetune/examples/ex-1/rate"}, {"POST", "/api/finetune/train"}, {"GET", "/api/finetune/runs"}} {
		if w := e.do(rt.m, rt.p, `{}`, withBearer(testToken)); w.Code != 503 {
			t.Errorf("%s %s without a store: %d", rt.m, rt.p, w.Code)
		}
	}
}
