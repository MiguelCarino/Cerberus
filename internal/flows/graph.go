package flows

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Graph is a run's graph in the Flow view's shape ({nodes, edges,
// updated}, the same JSON as GET /api/sessions/{id}/flow), drawn by the
// same renderer. See doc.go for the node and edge kinds.
type Graph struct {
	Nodes   []Node    `json:"nodes"`
	Edges   []Edge    `json:"edges"`
	Updated time.Time `json:"updated"`
}

// Node is one box of the graph.
type Node struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Sub   string `json:"sub"`
	State string `json:"state"`
	Href  string `json:"href"`
	// Detail is more about the state, for the tooltip: why a "queued"
	// node waits for the engine, or that a model call was retried.
	Detail string `json:"detail,omitempty"`
}

// Edge is one arrow. Good, Bad and Corrected are always 0 here (they
// exist so the JSON matches the session graph).
type Edge struct {
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

// Node and edge kinds of a flow graph.
const (
	NodeInput = "input"
	NodeStep  = "step"
	NodeItem  = "item"
	NodeModel = "model"
	NodeEnd   = "end"

	EdgeNext = "next"
	EdgeThen = "then"
	EdgeElse = "else"
	EdgeItem = "item"
	EdgeAsk  = "ask"
)

const graphSubMax = 120

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func stepNodeID(id string) string { return "st:" + id }

// FlowGraph is a flow's static graph (every state ""), for previews.
func FlowGraph(f Flow) Graph {
	return buildGraph(f, nil, nil, nil, "")
}

// Graph is the graph of run id: one node per step with its state,
// fan-out items as child nodes (live while the step runs), and model
// nodes for the models used.
func (e *Engine) Graph(id string) (Graph, error) {
	d, err := e.GetRun(id)
	if err != nil {
		return Graph{}, err
	}
	e.mu.Lock()
	var liveItems map[string][]ItemRecord
	if lr := e.live[id]; lr != nil {
		liveItems = map[string][]ItemRecord{}
		for k, v := range lr.items {
			liveItems[k] = slices.Clone(v)
		}
	}
	e.mu.Unlock()
	r := d.Run
	return buildGraph(r.Snapshot, &r, d.Steps, liveItems, r.Input), nil
}

func buildGraph(f Flow, r *Run, recs []StepRecord, liveItems map[string][]ItemRecord, input string) Graph {
	g := Graph{Nodes: []Node{}, Edges: []Edge{}}
	byStep := map[string]StepRecord{}
	for _, rec := range recs {
		byStep[rec.ID] = rec
	}
	inState := ""
	if r != nil {
		g.Updated = r.Updated
		inState = StepPassed
	}
	g.Nodes = append(g.Nodes, Node{ID: NodeInput, Kind: NodeInput, Label: "Input", Sub: clipLine(input, graphSubMax), State: inState})

	type modelUse struct{ count, tokens int }
	var modelOrder []string
	modelEdges := map[[2]string]*modelUse{}
	var askOrder [][2]string
	ask := func(from, model string, calls, tokens int) {
		if model == "" {
			return
		}
		if !slices.Contains(modelOrder, model) {
			modelOrder = append(modelOrder, model)
		}
		k := [2]string{from, "m:" + model}
		u := modelEdges[k]
		if u == nil {
			u = &modelUse{}
			modelEdges[k] = u
			askOrder = append(askOrder, k)
		}
		u.count += calls
		u.tokens += tokens
	}

	var itemNodes []Node
	var itemEdges []Edge
	for i, s := range f.Steps {
		rec, done := byStep[s.ID]
		n := Node{ID: stepNodeID(s.ID), Kind: NodeStep, Label: s.Label(), Sub: stepSub(s)}
		var items []ItemRecord
		if s.Kind == KindFanOut {
			items = rec.Items
			if !done {
				items = liveItems[s.ID]
			}
		}
		if r != nil {
			n.State = stepGraphState(r, i, s, rec, done)
			if rec.Session != "" {
				n.Href = "#/s/" + rec.Session
			}
			if done && rec.Retries > 0 {
				n.Detail = retryDetail(rec.Retries, rec.RetryError)
			}
			// The current step waits for the engine: a prompt or plan
			// step while its call is held, a fan-out step while every
			// item it is running is held. It is not shown as live.
			if n.State == StepRunning && r.Waiting != "" {
				n.Detail = r.Waiting
				if s.Kind != KindFanOut || allRunningWait(items) {
					n.State = StepQueued
				}
			}
		}
		g.Nodes = append(g.Nodes, n)
		// models and items
		switch s.Kind {
		case KindPrompt, KindPlan:
			if done {
				ask(n.ID, rec.Model, max(rec.Calls, 1), rec.PromptTokens+rec.CompletionTokens)
			} else if r == nil {
				ask(n.ID, s.Model, 0, 0)
			}
		case KindFanOut:
			for _, it := range items {
				id := "it:" + s.ID + ":" + strconv.Itoa(it.Index)
				label := it.Title
				if label == "" {
					label = "Item " + strconv.Itoa(it.Index+1)
				}
				in := Node{ID: id, Kind: NodeItem, Label: clipLine(label, graphSubMax), Sub: it.Model, State: it.State}
				if it.Retries > 0 {
					in.Detail = retryDetail(it.Retries, it.RetryError)
				}
				if it.State == StepRunning && it.Waiting != "" {
					in.State, in.Detail = StepQueued, it.Waiting
				}
				if it.Session != "" {
					in.Href = "#/s/" + it.Session
				}
				itemNodes = append(itemNodes, in)
				itemEdges = append(itemEdges, Edge{From: n.ID, To: id, Kind: EdgeItem, Label: it.Kind, Count: 1})
				if s.fanAs() == KindPrompt && (it.State == StepPassed || it.State == StepFailed || it.Calls > 0) {
					ask(id, it.Model, max(it.Calls, 1), it.PromptTokens+it.CompletionTokens)
				}
			}
			if r == nil && s.fanAs() == KindPrompt {
				for _, rt := range s.Routes {
					ask(n.ID, rt.Model, 0, 0)
				}
				ask(n.ID, s.DefaultModel, 0, 0)
			}
		}
	}
	endState := ""
	if r != nil && Final(r.State) {
		endState = r.State
	}
	g.Nodes = append(g.Nodes, itemNodes...)
	g.Nodes = append(g.Nodes, Node{ID: NodeEnd, Kind: NodeEnd, Label: "End", State: endState})
	for _, m := range modelOrder {
		g.Nodes = append(g.Nodes, Node{ID: "m:" + m, Kind: NodeModel, Label: m, Href: "#/models"})
	}

	// control edges
	target := func(i int, to string) string {
		switch to {
		case "":
			if i+1 < len(f.Steps) {
				return stepNodeID(f.Steps[i+1].ID)
			}
			return NodeEnd
		case End:
			return NodeEnd
		}
		return stepNodeID(to)
	}
	if len(f.Steps) > 0 {
		g.Edges = append(g.Edges, Edge{From: NodeInput, To: stepNodeID(f.Steps[0].ID), Kind: EdgeNext, Label: "input", Count: 1})
	} else {
		g.Edges = append(g.Edges, Edge{From: NodeInput, To: NodeEnd, Kind: EdgeNext, Count: 1})
	}
	for i, s := range f.Steps {
		from := stepNodeID(s.ID)
		if s.Kind == KindIf {
			g.Edges = append(g.Edges,
				Edge{From: from, To: target(i, s.Then), Kind: EdgeThen, Label: "then", Count: 1},
				Edge{From: from, To: target(i, s.Else), Kind: EdgeElse, Label: "else", Count: 1})
			continue
		}
		g.Edges = append(g.Edges, Edge{From: from, To: target(i, ""), Kind: EdgeNext, Count: 1})
	}
	g.Edges = append(g.Edges, itemEdges...)
	for _, k := range askOrder {
		u := modelEdges[k]
		e := Edge{From: k[0], To: k[1], Kind: EdgeAsk, Count: u.count, Tokens: u.tokens}
		switch {
		case u.count == 0:
		case u.tokens > 0:
			e.Label = fmt.Sprintf("%s · %s tokens", plural(u.count, "call"), shortNum(u.tokens))
		default:
			e.Label = plural(u.count, "call")
		}
		g.Edges = append(g.Edges, e)
	}
	return g
}

func stepSub(s Step) string {
	parts := []string{s.Kind}
	switch s.Kind {
	case KindPrompt, KindPlan:
		parts = append(parts, s.Model)
	case KindFanOut:
		parts = append(parts, "from "+s.From, "as "+s.fanAs())
	case KindSession:
		parts = append(parts, s.Agent, s.workspace())
	case KindIf:
		parts = append(parts, s.On+" "+s.Test)
	case KindOutput:
		parts = append(parts, s.To)
	}
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return clipLine(strings.Join(keep, " · "), graphSubMax)
}

// stepGraphState is a step's state in a run's graph: its record's state
// when it ran; "running"/"waiting" for the current step of a going run;
// "skipped" for a step the run passed over or never reached before it
// ended; else "pending".
func stepGraphState(r *Run, i int, s Step, rec StepRecord, done bool) string {
	if done {
		return rec.State
	}
	if r.Step == s.ID && i == r.PC {
		switch r.State {
		case StateRunning:
			return StepRunning
		case StateWaiting:
			return StepWaiting
		}
	}
	if i < r.PC || Final(r.State) {
		return StepSkipped
	}
	return StepPending
}

// allRunningWait: at least one item runs and every running item waits
// for the engine.
func allRunningWait(items []ItemRecord) bool {
	n := 0
	for _, it := range items {
		if it.State != StepRunning {
			continue
		}
		if it.Waiting == "" {
			return false
		}
		n++
	}
	return n > 0
}

// retryDetail is a node's note for retried model calls.
func retryDetail(n int, first string) string {
	what := "retried once"
	if n > 1 {
		what = "retried " + strconv.Itoa(n) + " times"
	}
	if first != "" {
		what += " after: " + clipLine(first, graphSubMax)
	}
	return what
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func shortNum(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	case n >= 1000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), ".0") + "k"
	}
	return strconv.Itoa(n)
}
