package api

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// flowFixture is a planner tree: root r spawned a (older) and b; a spawned
// a1. x and y point at each other (a loop) and z is someone else's tree.
func flowFixture() []session.Info {
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	return []session.Info{
		{ID: "b", Parent: "r", Created: t0.Add(2 * time.Minute), Updated: t0.Add(3 * time.Minute), Agent: "demo", State: "done"},
		{ID: "a1", Parent: "a", Created: t0.Add(4 * time.Minute), Updated: t0.Add(5 * time.Minute), State: "running"},
		{ID: "r", Created: t0, Updated: t0.Add(time.Minute), Task: "Plan the release\n  and   delegate", Name: "Planner",
			Agent: "planner", Model: "cloud/big", Workspace: "scratch", State: "waiting"},
		{ID: "a", Parent: "r", Created: t0.Add(time.Minute), Updated: t0.Add(2 * time.Minute), Title: "Child A", State: "running"},
		{ID: "x", Parent: "y", Created: t0},
		{ID: "y", Parent: "x", Created: t0},
		{ID: "z", Created: t0, Parent: "gone"},
	}
}

func treeIDs(list []session.Info) string {
	ids := make([]string, len(list))
	for i, in := range list {
		ids[i] = in.ID
	}
	return strings.Join(ids, ",")
}

func TestFlowTree(t *testing.T) {
	all := flowFixture()
	for _, from := range []string{"r", "a", "a1", "b"} {
		if got := treeIDs(flowTree(all, from)); got != "r,a,a1,b" {
			t.Errorf("from %s: %s", from, got)
		}
	}
	if got := treeIDs(flowTree(all, "x")); got != "y,x" && got != "x,y" {
		t.Errorf("loop: %s", got)
	}
	if got := treeIDs(flowTree(all, "z")); got != "z" {
		t.Errorf("missing parent: %s", got)
	}
	if flowTree(all, "nope") != nil {
		t.Error("unknown id has a tree")
	}
}

func findNode(g FlowGraph, id string) (FlowNode, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return FlowNode{}, false
}

func findEdge(g FlowGraph, from, to, kind string) (FlowEdge, bool) {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Kind == kind {
			return e, true
		}
	}
	return FlowEdge{}, false
}

func TestBuildFlowSessions(t *testing.T) {
	tree := flowTree(flowFixture(), "a1")
	last := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	in := flowInput{
		sessions: tree,
		usage: []usage.SessionModel{
			{Session: "r", Model: "qwen", Client: usage.ClientDelegate, Provider: "ollama", Requests: 2, PromptTokens: 3000, CompletionTokens: 400},
			{Session: "r", Model: "cloud/big", Client: "", Provider: "openrouter", Requests: 5, PromptTokens: 9000, CompletionTokens: 3000, Last: last},
			{Session: "a", Model: "qwen", Client: usage.ClientDelegate, Provider: "ollama", Requests: 1, PromptTokens: 10, CompletionTokens: 5},
			{Session: "a", Model: "gemma", Client: usage.ClientDelegate, Provider: "ollama", Requests: 1},
			{Session: "other", Model: "qwen", Client: usage.ClientDelegate, Requests: 9},
		},
		briefs: []finetune.Brief{
			{ID: "ex-1", Kind: finetune.KindDelegated, Model: "qwen", Session: "r"},
			{ID: "ex-2", Kind: finetune.KindDelegated, Model: "qwen", Session: "r"},
			{ID: "ex-3", Kind: finetune.KindDelegated, Model: "qwen", Session: "a"},
			{ID: "ex-4", Kind: finetune.KindDelegated, Model: "tiny", Session: "b"}, // usage aged out
			{ID: "ex-5", Kind: finetune.KindCloud, Model: "cloud/big", Session: "r"},
		},
		rates: map[string]finetune.Rating{
			"ex-1": {ID: "ex-1", Good: true},
			"ex-2": {ID: "ex-2", Good: true, Better: "fixed"},
			"ex-3": {ID: "ex-3", Good: false},
			"ex-5": {ID: "ex-5", Good: true},
		},
		models: []engine.ModelInfo{
			{ID: "qwen", Backend: "ollama", Local: true, Loaded: true},
			{ID: "gemma", Backend: "llama.cpp", Local: true},
			{ID: "cloud/big", Backend: "openai"},
		},
	}
	g := buildFlow(in)

	var ids []string
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	if got := strings.Join(ids, ","); got != "you,s:r,s:a,s:a1,s:b,m:qwen,m:cloud/big,m:gemma,m:tiny" {
		t.Fatalf("nodes %s", got)
	}
	you, _ := findNode(g, "you")
	if you.Kind != flowYou || you.Label != "You" || you.Sub != "Plan the release and delegate" {
		t.Errorf("you %+v", you)
	}
	r, _ := findNode(g, "s:r")
	if r.Kind != flowSession || r.Label != "Planner" || r.Sub != "planner · cloud/big · scratch" || r.State != "waiting" || r.Href != "#/s/r" {
		t.Errorf("root %+v", r)
	}
	if a, _ := findNode(g, "s:a"); a.Label != "Child A" || a.Sub != "" {
		t.Errorf("a %+v", a)
	}
	if a1, _ := findNode(g, "s:a1"); a1.Label != "a1" {
		t.Errorf("a1 falls back to its id: %+v", a1)
	}
	for id, want := range map[string]string{"m:qwen": "loaded", "m:gemma": "unloaded", "m:cloud/big": "cloud", "m:tiny": ""} {
		n, _ := findNode(g, id)
		if n.Kind != flowModel || n.State != want || n.Href != "#/models" {
			t.Errorf("%s: %+v", id, n)
		}
	}
	if n, _ := findNode(g, "m:gemma"); n.Sub != "llama.cpp" {
		t.Errorf("gemma sub %q", n.Sub)
	}

	if e, ok := findEdge(g, "you", "s:r", flowTask); !ok || e.Count != 1 {
		t.Errorf("task %+v", e)
	}
	for _, c := range []string{"s:a", "s:b"} {
		if e, ok := findEdge(g, "s:r", c, flowSpawn); !ok || e.Label != "spawned" {
			t.Errorf("spawn r→%s %+v", c, e)
		}
	}
	if _, ok := findEdge(g, "s:a", "s:a1", flowSpawn); !ok {
		t.Error("spawn a→a1 missing")
	}
	e, ok := findEdge(g, "s:r", "m:qwen", flowAsk)
	if !ok || e.Count != 2 || e.Tokens != 3400 || e.Good != 1 || e.Corrected != 1 || e.Bad != 0 || e.Label != "2 jobs · 3.4k tokens" {
		t.Errorf("ask r→qwen %+v", e)
	}
	if e, _ := findEdge(g, "s:a", "m:qwen", flowAsk); e.Count != 1 || e.Bad != 1 || e.Label != "1 job · 15 tokens" {
		t.Errorf("ask a→qwen %+v", e)
	}
	if e, _ := findEdge(g, "s:b", "m:tiny", flowAsk); e.Count != 1 || e.Tokens != 0 || e.Label != "1 job" {
		t.Errorf("examples only %+v", e)
	}
	if e, _ := findEdge(g, "s:r", "m:cloud/big", flowAnswer); e.Count != 5 || e.Good != 1 || e.Label != "5 requests · 12k tokens" {
		t.Errorf("answer %+v", e)
	}
	for _, e := range g.Edges {
		if strings.Contains(e.From+e.To, "other") {
			t.Errorf("a session outside the tree leaked: %+v", e)
		}
	}
	// task, three spawns, then model edges of r (ask first), a, b
	kinds := []string{}
	for _, e := range g.Edges {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "task,spawn,spawn,spawn,ask,answer,ask,ask,ask" {
		t.Errorf("edge order %s", got)
	}
	if !g.Updated.Equal(last) {
		t.Errorf("updated %v", g.Updated)
	}
}

func TestBuildFlowChat(t *testing.T) {
	now := float64(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC).Unix())
	c := &chat.Chat{ID: "c1", Title: "", Model: "local/qwen", Mode: "ask", Updated: now}
	_ = json.Unmarshal([]byte(`[{"role":"user","content":""},{"role":"user","content":"Why is   the sky blue?"},
		{"role":"assistant","content":"Rayleigh."},{"role":"assistant","content":""},{"role":"assistant","content":"More."}]`), &c.Messages)
	g := buildFlow(flowInput{chat: &chat.View{Chat: c, Status: chat.Status{Backend: "native", Busy: true}}})
	if len(g.Nodes) != 3 || len(g.Edges) != 2 {
		t.Fatalf("graph %+v", g)
	}
	you, _ := findNode(g, "you")
	ch, _ := findNode(g, "c:c1")
	if you.Sub != "Why is the sky blue?" || ch.Kind != flowChat || ch.Label != "Chat" || ch.Sub != "local/qwen · native · ask" ||
		ch.State != "busy" || ch.Href != "#/chat/c1" {
		t.Errorf("you %+v chat %+v", you, ch)
	}
	if e, ok := findEdge(g, "c:c1", "m:local/qwen", flowAnswer); !ok || e.Count != 2 || e.Label != "2 replies" {
		t.Errorf("replies edge %+v", e)
	}
	if !g.Updated.Equal(time.Unix(int64(now), 0)) {
		t.Errorf("updated %v", g.Updated)
	}
	// with usage by chat id, the records answer instead
	g = buildFlow(flowInput{chat: &chat.View{Chat: c},
		usage: []usage.SessionModel{{Session: "c1", Model: "local/qwen", Client: "cerberus-chat", Requests: 4, PromptTokens: 999}}})
	if e, _ := findEdge(g, "c:c1", "m:local/qwen", flowAnswer); e.Count != 4 || e.Label != "4 requests · 999 tokens" {
		t.Errorf("usage edge %+v", e)
	}
	if n, _ := findNode(g, "c:c1"); n.State != "idle" {
		t.Errorf("idle state %q", n.State)
	}
	if g := buildFlow(flowInput{}); len(g.Nodes) != 0 || len(g.Edges) != 0 || g.Nodes == nil || g.Edges == nil {
		t.Errorf("empty graph %+v", g)
	}
}

func TestFlowText(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1k", 3449: "3.4k", 12_345: "12k", 999_499: "999k", 1_240_000: "1.2M", 12_600_000: "13M"} {
		if got := flowTokens(n); got != want {
			t.Errorf("flowTokens(%d) = %q, want %q", n, got, want)
		}
	}
	if flowCount(1, "reply") != "1 reply" || flowCount(3, "reply") != "3 replies" || flowCount(0, "job") != "0 jobs" {
		t.Error("flowCount")
	}
	if got := flowClip("héllo  wörld\nagain", 8); got != "héllo w…" {
		t.Errorf("flowClip %q", got)
	}
	if flowJoin("", "a", " ", "b") != "a · b" {
		t.Error("flowJoin")
	}
}

// flowSessions answers Get from the listed infos (the shared fake only
// knows s1).
type flowSessions struct{ *fakeSessions }

func (f flowSessions) Get(id string) (session.Info, bool) {
	for _, in := range f.infos {
		if in.ID == id {
			return in, true
		}
	}
	return session.Info{}, false
}

func TestSessionFlowRoute(t *testing.T) {
	e := newEnv(t, nil)
	fs := e.sessions
	fs.infos = flowFixture()
	e.srv.sessions = flowSessions{fs}
	us, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer us.Close()
	us.Add(usage.Record{Time: time.Now(), SessionID: "a", Model: "openrouter/meta/llama", Client: usage.ClientDelegate,
		Provider: "openrouter", PromptTokens: 100, CompletionTokens: 20, Status: 200})
	e.srv.usage = us
	ft, err := finetune.New(filepath.Join(t.TempDir(), "ft"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ft.Add(finetune.Example{Kind: finetune.KindDelegated, Model: "openrouter/meta/llama", Session: "a",
		Messages: []finetune.Message{{Role: "user", Content: "q"}}, Answer: "x"})
	_ = ft.Rate(finetune.Rating{ID: id, Good: true})
	e.srv.finetune = ft
	e.h = e.srv.handler()

	w := e.do("GET", "/api/sessions/b/flow", "", withBearer(testToken))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var g FlowGraph
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 6 || g.Nodes[0].ID != "you" {
		t.Fatalf("nodes %+v", g.Nodes)
	}
	edge, ok := findEdge(g, "s:a", "m:openrouter/meta/llama", flowAsk)
	if !ok || edge.Count != 1 || edge.Tokens != 120 || edge.Good != 1 {
		t.Errorf("ask %+v", edge)
	}
	if m, _ := findNode(g, "m:openrouter/meta/llama"); m.Sub != "openai" || m.State != "cloud" {
		t.Errorf("model %+v", m)
	}
	// the raw JSON carries every contract field, even zero ones
	var raw struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
		Upd   string           `json:"updated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"id", "kind", "label", "sub", "state", "href"} {
		if _, ok := raw.Nodes[0][k]; !ok {
			t.Errorf("node field %q missing", k)
		}
	}
	for _, k := range []string{"from", "to", "kind", "label", "count", "tokens", "good", "bad", "corrected"} {
		if _, ok := raw.Edges[0][k]; !ok {
			t.Errorf("edge field %q missing", k)
		}
	}
	if _, err := time.Parse(time.RFC3339, raw.Upd); err != nil {
		t.Errorf("updated %q: %v", raw.Upd, err)
	}

	if w := e.do("GET", "/api/sessions/nope/flow", "", withBearer(testToken)); w.Code != 404 {
		t.Errorf("unknown session: %d", w.Code)
	}
	if w := e.do("GET", "/api/sessions/b/flow", "", withBearer(e.srv.cfg.SessionAgentToken("b")), withHeader(sessionHeader, "b")); !refused(w.Code, "") && w.Code != 404 {
		t.Errorf("agent token: %d", w.Code)
	}
	e.srv.sessions = nil
	e.h = e.srv.handler()
	if w := e.do("GET", "/api/sessions/b/flow", "", withBearer(testToken)); w.Code != 503 {
		t.Errorf("no sessions: %d", w.Code)
	}
}

func TestChatFlowRoute(t *testing.T) {
	e := newChatEnv(t, "http://127.0.0.1:1/v1")
	c := e.create(t, "ask")
	ft, err := finetune.New(filepath.Join(t.TempDir(), "ft"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ft.Add(finetune.Example{Kind: finetune.KindCloud, Model: "api/model", Session: c.ID,
		Messages: []finetune.Message{{Role: "user", Content: "q"}}, Answer: "x"})
	_ = ft.Rate(finetune.Rating{ID: id, Good: false, Better: "y"})
	e.srv.finetune = ft

	var g FlowGraph
	if code := e.call(t, "GET", "/api/chats/"+c.ID+"/flow", "", &g); code != 200 {
		t.Fatalf("flow: %d", code)
	}
	if len(g.Nodes) != 3 || g.Nodes[1].ID != "c:"+c.ID || g.Nodes[1].Kind != flowChat {
		t.Fatalf("nodes %+v", g.Nodes)
	}
	if edge, ok := findEdge(g, "c:"+c.ID, "m:api/model", flowAnswer); !ok || edge.Count != 1 || edge.Corrected != 1 {
		t.Errorf("answer %+v", edge)
	}
	if code := e.call(t, "GET", "/api/chats/"+strings.Repeat("0", len(c.ID))+"/flow", "", nil); code != 404 {
		t.Errorf("unknown chat: %d", code)
	}
}
