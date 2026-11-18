package api

// Approvals that block a flow run (SCOPE "Approvals in flows (2026-11-16)").
//
// A flow's session step (or a fan-out item run as a session, or a child
// either of them spawned) can stop at an approval: the built-in agent's
// tool prompt (WaitingReason "approval", answered with POST
// /api/sessions/{id}/answer) or a spawned child held until someone lets it
// start (WaitingReason "needs approval", POST /api/sessions/{id}/approve).
// The run waits on it, so the run page shows it and answers it, through
// those same session routes (same owner check, same audit). No new route:
//
//	GET /api/flows/runs/{run}       gains asks: [FlowAsk], oldest first
//	GET /api/flows/runs             each run gains asks: N (omitted when 0)
//	GET /api/flows/runs/{run}/flow  marks the step (or item) node of each
//	                                ask "waiting", with sub "approve: <tool>"
//	                                and the ask as its detail
//	GET /api/events                 sends a flowrun event {run, flow, state,
//	                                step} for the run when one of its
//	                                sessions starts or stops waiting
//
// The owner checks are the run's (a run another user owns is 404 before
// any of this is read) and the stream's flowVisible.

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/session"
)

// Kinds of FlowAsk.
const (
	flowAskTool  = "tool"  // an agent's approval prompt: answer y | a | n | save
	flowAskSpawn = "spawn" // a spawned child held until approved: approve true | false
)

// flowAskDepth bounds the walk from a waiting session up to the run's
// session (spawn depth is small; a loop in bad data must not hang).
const flowAskDepth = 16

// FlowAsk is one approval a flow run is held by.
type FlowAsk struct {
	Session string `json:"session"` // the waiting session
	Name    string `json:"name"`    // its name (else title, else id)
	Kind    string `json:"kind"`    // tool | spawn
	// Step is the run's step the session belongs to ("" when not known);
	// Item the fan-out item's index (as ItemRecord.index) when known.
	Step string `json:"step,omitempty"`
	Item *int   `json:"item,omitempty"`
	// Via is the run's own session when the waiting one is a child it
	// (or one of its children) spawned; "" when it is the run's session.
	Via string `json:"via,omitempty"`
	// Ask is what the agent wants ("run_command: npm test"; "" when the
	// agent does not say), Seq the prompt's number (Info.AskSeq, sent back
	// with the answer) and Always the rules "Every time" may save
	// (Info.WaitingAlways): the same fields the session list answers from.
	Ask    string                 `json:"ask"`
	Seq    int                    `json:"seq"`
	Always []session.AlwaysOption `json:"always,omitempty"`
	Since  time.Time              `json:"since"` // when the session last changed
}

// flowAskKind is what a session waits for: flowAskTool, flowAskSpawn or "".
func flowAskKind(in session.Info) string {
	switch {
	case in.State == session.StateWaiting && in.WaitingReason == "approval":
		return flowAskTool
	case in.State == session.StateQueued && in.WaitingReason == session.ReasonNeedsApproval:
		return flowAskSpawn
	}
	return ""
}

// flowAskSig identifies one wait of a session ("" when it does not wait),
// so a new prompt in the same session is a change too.
func flowAskSig(in session.Info) string {
	k := flowAskKind(in)
	if k == "" {
		return ""
	}
	return k + "/" + strconv.Itoa(in.AskSeq) + "/" + in.WaitingAsk
}

// askIndex is the waiting sessions of one moment, each with the chain of
// sessions above it (itself first).
type askIndex struct {
	waiting []session.Info
	chain   map[string][]string
	byID    map[string]session.Info
}

func newAskIndex(all []session.Info) *askIndex {
	x := &askIndex{chain: map[string][]string{}, byID: make(map[string]session.Info, len(all))}
	for _, in := range all {
		x.byID[in.ID] = in
	}
	for _, in := range all {
		if flowAskKind(in) == "" {
			continue
		}
		x.waiting = append(x.waiting, in)
		x.chain[in.ID] = sessionChain(in, func(id string) (session.Info, bool) { in, ok := x.byID[id]; return in, ok })
	}
	slices.SortStableFunc(x.waiting, func(a, b session.Info) int { return a.Updated.Compare(b.Updated) })
	return x
}

// sessionChain is in's id, then its parent's, and so on up (bounded).
func sessionChain(in session.Info, get func(string) (session.Info, bool)) []string {
	out := []string{in.ID}
	cur := in
	for range flowAskDepth {
		if cur.Parent == "" || slices.Contains(out, cur.Parent) {
			break
		}
		out = append(out, cur.Parent)
		p, ok := get(cur.Parent)
		if !ok {
			break
		}
		cur = p
	}
	return out
}

// runSessionSet is every session a run started (the run's list, and the
// step and item records for runs from before that list was complete).
func runSessionSet(d flows.RunDetail) map[string]bool {
	set := map[string]bool{}
	for _, id := range d.Run.Sessions {
		set[id] = true
	}
	for _, rec := range d.Steps {
		if rec.Session != "" {
			set[rec.Session] = true
		}
		for _, it := range rec.Items {
			if it.Session != "" {
				set[it.Session] = true
			}
		}
	}
	for _, it := range d.Live {
		if it.Session != "" {
			set[it.Session] = true
		}
	}
	return set
}

// owned returns the run's session in chain ("" when none is).
func owned(chain []string, set map[string]bool) string {
	for _, id := range chain {
		if set[id] {
			return id
		}
	}
	return ""
}

// count is how many waiting sessions belong to the run (its list only:
// run lists carry no step records).
func (x *askIndex) count(run flows.Run) int {
	if len(x.waiting) == 0 || len(run.Sessions) == 0 {
		return 0
	}
	set := runSessionSet(flows.RunDetail{Run: run})
	n := 0
	for _, in := range x.waiting {
		if owned(x.chain[in.ID], set) != "" {
			n++
		}
	}
	return n
}

// forRun lists the run's asks, oldest first.
func (x *askIndex) forRun(d flows.RunDetail) []FlowAsk {
	out := []FlowAsk{}
	if len(x.waiting) == 0 {
		return out
	}
	set := runSessionSet(d)
	for _, in := range x.waiting {
		top := owned(x.chain[in.ID], set)
		if top == "" {
			continue
		}
		a := FlowAsk{Session: in.ID, Name: flowOr(in.Name, flowOr(in.Title, in.ID)), Kind: flowAskKind(in),
			Ask: in.WaitingAsk, Seq: in.AskSeq, Always: in.WaitingAlways, Since: in.Updated}
		if top != in.ID {
			a.Via = top
		}
		a.Step, a.Item = askStep(d, top, x.byID[top])
		out = append(out, a)
	}
	return out
}

// askStep finds the step (and fan-out item) of the run's session top: the
// records that name it, newest first, else the live items, else the step
// the run is on now (sessions run inside the current step), whose live
// items are matched by the name the engine gives an item's session.
func askStep(d flows.RunDetail, top string, info session.Info) (string, *int) {
	for i := len(d.Steps) - 1; i >= 0; i-- {
		rec := d.Steps[i]
		for _, it := range rec.Items {
			if it.Session == top {
				return rec.ID, intPtr(it.Index)
			}
		}
		if rec.Session == top {
			return rec.ID, nil
		}
	}
	for _, it := range d.Live {
		if it.Session == top {
			return d.Run.Step, intPtr(it.Index)
		}
	}
	if flows.Final(d.Run.State) || d.Run.Step == "" {
		return "", nil
	}
	step := d.Run.Step
	var st *flows.Step
	for i := range d.Run.Snapshot.Steps {
		if d.Run.Snapshot.Steps[i].ID == step {
			st = &d.Run.Snapshot.Steps[i]
			break
		}
	}
	if st == nil || st.Kind != flows.KindFanOut || info.Name == "" {
		return step, nil
	}
	// The engine names an item's session "<flow> · <step> · <title>"
	// ("· <n>" without a title), cut to 80 characters by the host.
	var hit *int
	for _, it := range d.Live {
		title := it.Title
		if title == "" {
			title = strconv.Itoa(it.Index + 1)
		}
		if clipRunes(d.Run.Snapshot.Name+" · "+st.Label()+" · "+title, 80) == info.Name {
			if hit != nil {
				return step, nil // two items of that name: say the step only
			}
			hit = intPtr(it.Index)
		}
	}
	return step, hit
}

func intPtr(n int) *int { return &n }

// asksIndex reads the session list once (nil without sessions).
func (s *server) asksIndex() *askIndex {
	if s.sessions == nil {
		return nil
	}
	return newAskIndex(s.sessions.List())
}

// runAsks is the run's asks ([] without sessions).
func (s *server) runAsks(d flows.RunDetail) []FlowAsk {
	x := s.asksIndex()
	if x == nil {
		return []FlowAsk{}
	}
	return x.forRun(d)
}

// askLine is an ask in a few words, for the graph's tooltip.
func askLine(a FlowAsk) string {
	if a.Kind == flowAskSpawn {
		return "waiting for your approval: " + a.Name + " asks to start"
	}
	if a.Ask == "" {
		return "waiting for your approval (open the session to see what for)"
	}
	return "waiting for your approval: " + a.Ask
}

// askLabel is the node's short label: "approve: <tool>".
func askLabel(a FlowAsk) string {
	if a.Kind == flowAskSpawn {
		return "approve: a child session"
	}
	tool, _, ok := strings.Cut(a.Ask, ":")
	if !ok || strings.TrimSpace(tool) == "" {
		return "approve: a tool call"
	}
	return "approve: " + strings.TrimSpace(tool)
}

// markAsks sets the node of each ask's step (its item when known and
// drawn) to waiting, with the short label as its sub and the ask as its
// detail; the node opens the waiting session.
func markAsks(g *flows.Graph, asks []FlowAsk) {
	at := map[string]int{}
	for i, n := range g.Nodes {
		at[n.ID] = i
	}
	for _, a := range asks {
		if a.Step == "" {
			continue
		}
		i, ok := -1, false
		if a.Item != nil {
			i, ok = at["it:"+a.Step+":"+strconv.Itoa(*a.Item)]
		}
		if !ok {
			i, ok = at["st:"+a.Step]
		}
		if !ok {
			continue
		}
		n := &g.Nodes[i]
		if n.State == flows.StepWaiting && strings.HasPrefix(n.Sub, "approve") {
			// A second ask on the same node: say how many.
			n.Detail += "\n" + askLine(a)
			continue
		}
		n.State, n.Sub, n.Detail = flows.StepWaiting, askLabel(a), askLine(a)
		n.Href = "#/s/" + a.Session
	}
}

// flowAskWatch follows, for one event stream, which sessions wait for an
// approval, so that a change can go out as a flowrun event of the run
// they belong to. It keeps only the sessions waiting now.
type flowAskWatch struct {
	s    *server
	seen map[string]string // session id → flowAskSig
}

func (s *server) newFlowAskWatch() *flowAskWatch {
	return &flowAskWatch{s: s, seen: map[string]string{}}
}

// change returns the run event to send for a session event, if any: the
// session (or one under the run's session) started, changed or stopped
// waiting for an approval.
func (w *flowAskWatch) change(ev session.Event) (flows.Event, bool) {
	if w == nil || w.s.flows == nil {
		return flows.Event{}, false
	}
	in := ev.Session
	sig := ""
	if ev.Type != session.EventRemoved {
		sig = flowAskSig(in)
	}
	if sig == w.seen[in.ID] {
		return flows.Event{}, false
	}
	if sig == "" {
		delete(w.seen, in.ID)
	} else {
		w.seen[in.ID] = sig
	}
	run, ok := w.s.flowRunOfSession(in)
	if !ok {
		return flows.Event{}, false
	}
	return flows.Event{Run: run.ID, Flow: run.Flow, State: run.State, Step: run.Step, Owner: run.Owner}, true
}

// flowRunOfSession is the run that started in (or the session in was
// spawned under), newest first.
func (s *server) flowRunOfSession(in session.Info) (flows.Run, bool) {
	if s.flows == nil {
		return flows.Run{}, false
	}
	get := func(id string) (session.Info, bool) { return session.Info{}, false }
	if s.sessions != nil {
		get = s.sessions.Get
	}
	chain := sessionChain(in, get)
	for _, run := range s.flows.Runs("", 0) {
		for _, id := range chain {
			if slices.Contains(run.Sessions, id) {
				return run, true
			}
		}
	}
	return flows.Run{}, false
}

// sessionAskDetail is a session node's detail in the Flow view: what it
// waits for when that is an approval ("" otherwise).
func sessionAskDetail(in session.Info) string {
	switch flowAskKind(in) {
	case flowAskTool:
		return askLine(FlowAsk{Kind: flowAskTool, Ask: in.WaitingAsk})
	case flowAskSpawn:
		return "waiting for your approval to start"
	}
	return ""
}
