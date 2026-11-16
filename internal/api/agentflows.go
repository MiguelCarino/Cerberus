package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/flows"
)

// Flows for outside agents (the MCP tools cerberus_flows,
// cerberus_run_flow and cerberus_flow_run). These agent-scope routes
// answer MCP clients only: a session's agent gets 403 (a flow can start
// sessions, and a session starting flows would loop). A client sees the
// enabled flows of its own user (the flow's owner is the client's user;
// admins too see only their own here), starts runs that act for that
// owner (flows.max_runs, the run limits and the owner's budget apply),
// and reads only the runs it started itself. Its runs are recorded with
// actor "client:<name>#<device id>", the same identity its sessions
// carry, so a re-paired client of the same name does not inherit them.
//
//	GET  /api/agent/flows                   → [agent.FlowSummary]
//	POST /api/agent/flows/{id}/run {input}  → 202 agent.FlowRunStatus (429 when the
//	                                          client has 2 runs going, or over budget)
//	GET  /api/agent/flows/runs/{run}?wait=S → agent.FlowRunStatus (waits up to 300 s
//	                                          for a final or waiting state, on the
//	                                          engine's change signal; no polling)

const (
	clientFlowRuns    = 2 // runs going at once per MCP client
	clientFlowWaitMax = agent.FlowWaitMaxS * time.Second
	clientFlowOutKiB  = 16
)

// clientFlowStart makes the "at most 2 runs" check and the start one step.
var clientFlowStart sync.Mutex

func (s *server) agentFlowRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/agent/flows", s.handleAgentFlows)
	mux.HandleFunc("POST /api/agent/flows/{id}/run", s.handleAgentFlowRun)
	mux.HandleFunc("GET /api/agent/flows/runs/{run}", s.handleAgentFlowStatus)
}

// flowClient answers the client's run actor, or writes the refusal.
func (s *server) flowClient(w http.ResponseWriter, r *http.Request) (string, bool) {
	client := clientID(r)
	if client == "" {
		writeError(w, http.StatusForbidden, "flows are for MCP clients (cerberus mcp --client); a session cannot start flows")
		return "", false
	}
	if !s.flowReady(w) {
		return "", false
	}
	return "client:" + strings.TrimPrefix(client, "mcp:"), true
}

func (s *server) handleAgentFlows(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.flowClient(w, r); !ok {
		return
	}
	pr := principalOf(r)
	out := []agent.FlowSummary{}
	for _, f := range s.flows.List() {
		if !f.Enabled || !s.sameOwner(pr, f.Owner) || (s.hasUsers() && movedOwner(s.users, f.Owner)) {
			continue
		}
		out = append(out, agent.FlowSummary{ID: f.ID, Name: f.Name, Description: f.Description,
			InputHint: f.InputHint, Trigger: f.TriggerText})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleAgentFlowRun(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.flowClient(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var b struct {
		Input string `json:"input"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	f, err := s.flows.Get(id)
	if !flows.ValidID(id) || err != nil || !f.Enabled || !s.sameOwner(principalOf(r), f.Owner) ||
		(s.hasUsers() && movedOwner(s.users, f.Owner)) {
		writeError(w, http.StatusNotFound, "no such flow "+strconv.Quote(id)+" (cerberus_flows lists yours)")
		return
	}
	if s.budgetRefused(w, s.ownerOf(f.Owner)) {
		return
	}
	clientFlowStart.Lock()
	defer clientFlowStart.Unlock()
	going := 0
	for _, run := range s.flows.Runs("", 0) {
		if run.Actor == actor && flows.Going(run.State) {
			going++
		}
	}
	if going >= clientFlowRuns {
		writeError(w, http.StatusTooManyRequests,
			"you have "+strconv.Itoa(going)+" flow runs going; wait for one to end (at most "+strconv.Itoa(clientFlowRuns)+")")
		return
	}
	run, err := s.flows.RunFlow(actor, id, b.Input)
	if err != nil {
		flowError(w, err)
		return
	}
	noteAudit(r, "run", id, run.ID)
	writeJSON(w, http.StatusAccepted, s.flowStatus(flows.RunDetail{Run: run}))
}

func (s *server) handleAgentFlowStatus(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.flowClient(w, r)
	if !ok {
		return
	}
	id := r.PathValue("run")
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "wait must be seconds (0 to 300)")
			return
		}
		wait = min(time.Duration(n*float64(time.Second)), clientFlowWaitMax)
	}
	pr := principalOf(r)
	d, err := s.flows.GetRun(id)
	if !flows.ValidRunID(id) || err != nil || d.Run.Actor != actor || !s.sameOwner(pr, d.Run.Owner) {
		writeError(w, http.StatusNotFound, "no such run "+strconv.Quote(id)+" (you see the runs you started)")
		return
	}
	if wait > 0 && flows.Going(d.Run.State) && d.Run.State != flows.StateWaiting {
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		_, _ = s.flows.Wait(ctx, id) // the engine's change signal; a timeout answers the state as it is
		cancel()
		if r.Context().Err() != nil {
			return
		}
		if d, err = s.flows.GetRun(id); err != nil {
			flowError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.flowStatus(d))
}

// flowStatus is what cerberus_flow_run shows of a run: its state, and the
// output of its last step that has one (an output step's text, else the
// last passed step's output; at most 16 KiB) and the files output steps
// wrote.
func (s *server) flowStatus(d flows.RunDetail) agent.FlowRunStatus {
	r := d.Run
	st := agent.FlowRunStatus{ID: r.ID, Flow: r.Flow, FlowName: r.FlowName, State: r.State, Step: r.Step,
		Reason: r.Reason, Done: flows.Final(r.State), Waiting: r.State == flows.StateWaiting,
		Files: []agent.FlowRunFile{}, Tokens: r.Tokens, CostUSD: r.CostUSD}
	for i := len(d.Steps) - 1; i >= 0; i-- {
		rec := d.Steps[i]
		if rec.State != flows.StepPassed || rec.Kind == flows.KindIf || rec.Kind == flows.KindApprove {
			continue
		}
		text := flows.OutputText(rec)
		if rec.Kind == flows.KindOutput {
			text = rec.Input // the text it sent or wrote
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		st.OutStep = rec.ID
		if len(text) > clientFlowOutKiB<<10 {
			text = text[:clientFlowOutKiB<<10]
			for len(text) > 0 && !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			st.Cut = true
		}
		st.Output = text
		break
	}
	for _, rec := range d.Steps {
		if rec.File != "" {
			f := agent.FlowRunFile{Path: rec.File, Step: rec.ID}
			if m, ok := rec.Output.(map[string]any); ok {
				if n, ok := m["bytes"].(float64); ok {
					f.Bytes = int(n)
				}
			}
			st.Files = append(st.Files, f)
		}
	}
	return st
}
