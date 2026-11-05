package api

// Flow: the read-only graph of who handed what to whom, for a session's
// whole tree or for one chat (owner scope).
//
//	GET /api/sessions/{id}/flow → FlowGraph for the session's tree: its root
//	                              ancestor (Parent links) and every descendant
//	GET /api/chats/{id}/flow    → FlowGraph for one chat
//
// Both answer 404 for an id the caller cannot see (unknown session or
// chat, or an agent credential), 503 when the session or chat service is
// missing, and the chat route 403 while the LLM chat is turned off. Nothing
// is stored for the graph: it is built on each request from the session
// manager (Parent links, states), the usage records by session id (client
// "cerberus-delegate" for delegated jobs, any other client for the
// session's or chat's own requests) and the fine-tuning store's verdicts.
//
// The JSON contract (every field is always present; strings may be ""):
//
//	{
//	  "nodes": [{
//	    "id":    "you" | "s:<session id>" | "c:<chat id>" | "m:<model id>",
//	    "kind":  "you" | "session" | "chat" | "model",
//	    "label": "You" | session name (else title, else id) | chat title (else "Chat") | model id,
//	    "sub":   you: the root session's task or the chat's first message, one line, at most 120 characters;
//	             session: "agent · model · workspace" (empty parts left out);
//	             chat: "model · backend · mode" (empty parts left out);
//	             model: its backend ("ollama", "llama.cpp", ...) or cloud provider,
//	    "state": session: its state ("running", "waiting", "done", ...);
//	             chat: its run state ("thinking", "writing", "tool", "waiting", "done",
//	                   "stopped", "error"), else "busy" or "idle";
//	             model: "loaded" | "unloaded" (local models), "cloud" (cloud models),
//	                    "" (the engine no longer lists it);
//	             you: "",
//	    "href":  "#/s/<id>" (session) | "#/chat/<id>" (chat) | "#/models" (model) | "" (you)
//	  }],
//	  "edges": [{
//	    "from":  node id, "to": node id,
//	    "kind":  "task"   you → the root session or the chat (count 1),
//	             "spawn"  parent session → child session (count 1),
//	             "ask"    session or chat → model: delegated jobs (cerberus_ask), one edge per pair,
//	             "answer" session or chat → model: the model that answered the session's own
//	                      agent or the chat (its own requests through the gateway); it points
//	                      the same way as ask so the graph stays acyclic,
//	    "label": a plain default caption: "task", "spawned", "2 jobs · 3.4k tokens",
//	             "5 requests · 12k tokens", "3 replies" (the UI may draw its own from the numbers),
//	    "count": jobs (ask), requests (answer; replies for a chat with no usage rows under
//	             its id, such as one from before chats sent X-Cerberus-Session), 1 for task
//	             and spawn,
//	    "tokens": prompt + completion tokens of those requests (0 when unknown; an addition to
//	              the SCOPE list),
//	    "good", "bad", "corrected": verdicts from the fine-tuning store on this pair's examples
//	             (delegated examples on ask edges, cloud examples on answer edges); the newest
//	             verdict per example counts; one with a better answer is "corrected" whatever
//	             it said about good; 0 when nothing was kept or rated
//	  }],
//	  "updated": RFC 3339 time of the newest thing in the graph (a session change, a request,
//	             an example or a verdict)
//	}
//
// Order: nodes are "you", then sessions depth first from the root (children
// oldest first; a chat graph has its chat here), then models in order of
// first use by that walk; edges are task, spawn (in node order), then ask
// and answer per session in node order, models by name. Numbers are
// totals over what the usage store still holds (62 days).

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// FlowGraph is the reply of the flow routes (see the contract above).
type FlowGraph struct {
	Nodes   []FlowNode `json:"nodes"`
	Edges   []FlowEdge `json:"edges"`
	Updated time.Time  `json:"updated"`
}

// FlowNode is one box or circle of the graph.
type FlowNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Sub   string `json:"sub"`
	State string `json:"state"`
	Href  string `json:"href"`
}

// FlowEdge is one arrow, aggregated per (from, to, kind).
type FlowEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Count     int    `json:"count"`
	Tokens    int    `json:"tokens"`
	Good      int    `json:"good"`
	Bad       int    `json:"bad"`
	Corrected int    `json:"corrected"`
}

// Node and edge kinds.
const (
	flowYou     = "you"
	flowSession = "session"
	flowChat    = "chat"
	flowModel   = "model"

	flowTask   = "task"
	flowSpawn  = "spawn"
	flowAsk    = "ask"
	flowAnswer = "answer"
)

const flowSubMax = 120 // runes in the "you" node's sub

// flowUsage is the part of *usage.Store the graph reads.
type flowUsage interface {
	BySession(ids []string) []usage.SessionModel
}

func (s *server) flowRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/sessions/{id}/flow", s.handleSessionFlow)
	mux.HandleFunc("GET /api/chats/{id}/flow", s.handleChatFlow)
}

func (s *server) handleSessionFlow(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		unavailable(w, "sessions")
		return
	}
	id := r.PathValue("id")
	info, ok := s.sessions.Get(id)
	if !ok || !principalOf(r).owner() {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	tree := flowTree(s.sessions.List(), id)
	if len(tree) == 0 {
		tree = []session.Info{info}
	}
	in := flowInput{sessions: tree}
	ids := make([]string, len(tree))
	for i, t := range tree {
		ids[i] = t.ID
	}
	s.flowRecords(r.Context(), &in, ids)
	writeJSON(w, http.StatusOK, buildFlow(in))
}

func (s *server) handleChatFlow(w http.ResponseWriter, r *http.Request) {
	id, ok := s.chatID(w, r)
	if !ok {
		return
	}
	v, err := s.chats.Get(id)
	if err != nil {
		chatError(w, err)
		return
	}
	in := flowInput{chat: v}
	s.flowRecords(r.Context(), &in, []string{id})
	writeJSON(w, http.StatusOK, buildFlow(in))
}

// flowRecords fills in the usage rows, examples, verdicts and models for
// the given session (or chat) ids.
func (s *server) flowRecords(ctx context.Context, in *flowInput, ids []string) {
	if u, ok := s.usage.(flowUsage); ok && u != nil {
		in.usage = u.BySession(ids)
	}
	if s.finetune != nil {
		briefs, rates, err := s.finetune.Briefs(ids)
		if err != nil {
			s.log.Warn("flow: fine-tuning examples not read", "err", err)
		} else {
			in.briefs, in.rates = briefs, rates
		}
	}
	if s.models != nil && (len(in.usage) > 0 || len(in.briefs) > 0 || in.chat != nil) {
		in.models = s.models.Models(ctx)
	}
}

// flowTree is the tree id belongs to: its root ancestor (following Parent
// links while the parent is listed) and every descendant, depth first with
// children oldest first. It is nil when id is not listed.
func flowTree(all []session.Info, id string) []session.Info {
	byID := make(map[string]session.Info, len(all))
	kids := map[string][]session.Info{}
	for _, in := range all {
		byID[in.ID] = in
	}
	if _, ok := byID[id]; !ok {
		return nil
	}
	for _, in := range all {
		if in.Parent != "" && in.Parent != in.ID {
			if _, ok := byID[in.Parent]; ok {
				kids[in.Parent] = append(kids[in.Parent], in)
			}
		}
	}
	for _, list := range kids {
		sort.Slice(list, func(i, j int) bool {
			if !list[i].Created.Equal(list[j].Created) {
				return list[i].Created.Before(list[j].Created)
			}
			return list[i].ID < list[j].ID
		})
	}
	root := id
	for up := map[string]bool{root: true}; ; {
		p := byID[root].Parent
		if _, ok := byID[p]; !ok || up[p] {
			break // a person's session, a parent no longer listed, or a loop
		}
		up[p] = true
		root = p
	}
	var out []session.Info
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		out = append(out, byID[n])
		for _, k := range kids[n] {
			walk(k.ID)
		}
	}
	walk(root)
	return out
}

// flowInput is everything buildFlow draws from: a session tree (root
// first) or a chat, plus the records of those ids.
type flowInput struct {
	sessions []session.Info
	chat     *chat.View
	usage    []usage.SessionModel
	briefs   []finetune.Brief
	rates    map[string]finetune.Rating
	models   []engine.ModelInfo
}

// buildFlow turns the input into the graph (pure: no I/O).
func buildFlow(in flowInput) FlowGraph {
	g := FlowGraph{Nodes: []FlowNode{}, Edges: []FlowEdge{}}
	var updated time.Time
	seen := func(t time.Time) {
		if t.After(updated) {
			updated = t
		}
	}
	owner := map[string]string{} // session or chat id → node id
	var order []string           // those ids in node order

	switch {
	case in.chat != nil && in.chat.Chat != nil:
		c := in.chat.Chat
		nid := "c:" + c.ID
		g.Nodes = append(g.Nodes,
			FlowNode{ID: flowYou, Kind: flowYou, Label: "You", Sub: flowClip(chatFirstMessage(c), flowSubMax)},
			FlowNode{ID: nid, Kind: flowChat, Label: flowOr(c.Title, "Chat"),
				Sub:   flowJoin(flowOr(c.Model, in.chat.Status.Model), flowOr(c.Backend, in.chat.Status.Backend), flowOr(c.Mode, in.chat.Status.Mode)),
				State: chatFlowState(in.chat.Status), Href: "#/chat/" + c.ID})
		g.Edges = append(g.Edges, FlowEdge{From: flowYou, To: nid, Kind: flowTask, Label: flowTask, Count: 1})
		owner[c.ID] = nid
		order = append(order, c.ID)
		seen(unixTime(c.Updated))
	case len(in.sessions) > 0:
		root := in.sessions[0]
		g.Nodes = append(g.Nodes, FlowNode{ID: flowYou, Kind: flowYou, Label: "You", Sub: flowClip(root.Task, flowSubMax)})
		for _, t := range in.sessions {
			nid := "s:" + t.ID
			g.Nodes = append(g.Nodes, FlowNode{ID: nid, Kind: flowSession, Label: flowOr(t.Name, flowOr(t.Title, t.ID)),
				Sub: flowJoin(t.Agent, t.Model, t.Workspace), State: t.State, Href: "#/s/" + t.ID})
			owner[t.ID] = nid
			order = append(order, t.ID)
			seen(t.Updated)
		}
		g.Edges = append(g.Edges, FlowEdge{From: flowYou, To: "s:" + root.ID, Kind: flowTask, Label: flowTask, Count: 1})
		for _, t := range in.sessions[1:] {
			if p, ok := owner[t.Parent]; ok {
				g.Edges = append(g.Edges, FlowEdge{From: p, To: "s:" + t.ID, Kind: flowSpawn, Label: "spawned", Count: 1})
			}
		}
	default:
		return g
	}

	// Model edges, aggregated per (owner, model, kind).
	type ekey struct{ owner, model, kind string }
	edges := map[ekey]*FlowEdge{}
	examples := map[ekey]int{}
	provider := map[string]string{}
	edge := func(k ekey) *FlowEdge {
		e := edges[k]
		if e == nil {
			e = &FlowEdge{From: owner[k.owner], To: "m:" + k.model, Kind: k.kind}
			edges[k] = e
		}
		return e
	}
	for _, u := range in.usage {
		if _, ok := owner[u.Session]; !ok || u.Model == "" {
			continue
		}
		kind := flowAnswer
		if u.Client == usage.ClientDelegate {
			kind = flowAsk
		}
		e := edge(ekey{u.Session, u.Model, kind})
		e.Count += u.Requests
		e.Tokens += u.PromptTokens + u.CompletionTokens
		if provider[u.Model] == "" {
			provider[u.Model] = u.Provider
		}
		seen(u.Last)
	}
	for _, b := range in.briefs {
		if _, ok := owner[b.Session]; !ok || b.Model == "" {
			continue
		}
		kind := flowAnswer
		if b.Kind == finetune.KindDelegated {
			kind = flowAsk
		}
		k := ekey{b.Session, b.Model, kind}
		e := edge(k)
		examples[k]++
		seen(b.Time)
		r, ok := in.rates[b.ID]
		if !ok {
			continue
		}
		seen(r.Time)
		switch {
		case r.Better != "":
			e.Corrected++
		case r.Good:
			e.Good++
		default:
			e.Bad++
		}
	}
	for k, n := range examples {
		e := edges[k]
		e.Count = max(e.Count, n) // usage may have aged out, or the store had none
	}
	replies := false
	if in.chat != nil && len(edges) == 0 {
		// No usage rows for the chat (replies sent before chat requests
		// carried the chat id, or aged out): show the chat's model with its
		// replies.
		c := in.chat.Chat
		if m := flowOr(c.Model, in.chat.Status.Model); m != "" {
			if n := chatReplies(c); n > 0 {
				e := edge(ekey{c.ID, m, flowAnswer})
				e.Count = n
				replies = true
			}
		}
	}

	// Model nodes in order of first use; edges per owner in node order.
	info := map[string]engine.ModelInfo{}
	for _, m := range in.models {
		info[m.ID] = m
	}
	keys := make([]ekey, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if pos[a.owner] != pos[b.owner] {
			return pos[a.owner] < pos[b.owner]
		}
		if a.kind != b.kind {
			return a.kind == flowAsk // ask before answer
		}
		return a.model < b.model
	})
	added := map[string]bool{}
	for _, k := range keys {
		e := edges[k]
		e.Label = flowEdgeLabel(e, replies)
		g.Edges = append(g.Edges, *e)
		if added[k.model] {
			continue
		}
		added[k.model] = true
		n := FlowNode{ID: "m:" + k.model, Kind: flowModel, Label: k.model, Sub: provider[k.model], Href: "#/models"}
		if m, ok := info[k.model]; ok {
			n.Sub = flowOr(m.Backend, n.Sub)
			switch {
			case !m.Local:
				n.State = "cloud"
			case m.Loaded:
				n.State = "loaded"
			default:
				n.State = "unloaded"
			}
		}
		g.Nodes = append(g.Nodes, n)
	}
	if updated.IsZero() {
		updated = time.Now()
	}
	g.Updated = updated.UTC()
	return g
}

// flowEdgeLabel is the default caption of a model edge.
func flowEdgeLabel(e *FlowEdge, replies bool) string {
	unit := "request"
	switch {
	case e.Kind == flowAsk:
		unit = "job"
	case replies:
		unit = "reply"
	}
	parts := []string{flowCount(e.Count, unit)}
	if e.Tokens > 0 {
		parts = append(parts, flowTokens(e.Tokens)+" tokens")
	}
	return strings.Join(parts, " · ")
}

// flowCount is "1 job", "2 jobs", "3 replies".
func flowCount(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	if strings.HasSuffix(unit, "y") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(unit, "y"))
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// flowTokens is 950, 3.4k, 12k, 1.2M.
func flowTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	case n < 10_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	}
	return fmt.Sprintf("%dM", (n+500_000)/1_000_000)
}

// flowClip is s on one line (runs of white space become one space), cut to
// n runes with an ellipsis.
func flowClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

func flowOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// flowJoin joins the non-empty parts with " · ".
func flowJoin(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" }), " · ")
}

// chatFirstMessage is the text of the chat's first user message.
func chatFirstMessage(c *chat.Chat) string {
	for _, m := range c.Messages {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

// chatReplies counts the assistant messages with words in them.
func chatReplies(c *chat.Chat) int {
	n := 0
	for _, m := range c.Messages {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			n++
		}
	}
	return n
}

func chatFlowState(st chat.Status) string {
	switch {
	case st.State != "":
		return st.State
	case st.Busy:
		return "busy"
	}
	return "idle"
}

// unixTime is Unix seconds (as the chat store keeps them) as a time.
func unixTime(sec float64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(sec*1e9))
}
