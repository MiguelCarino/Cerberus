package api

// Flows: saved recipes (a trigger plus steps that hand work to models and
// agent sessions), their runs and the built-in templates, over the flow
// engine (internal/flows). Owner scope only: an agent credential never
// reaches these routes. Every write is in the audit log twice over: the
// HTTP record (route, actor, status) and the engine's own event
// ("flow.create", "flow.run.start", "flow.run.approve", ...).
//
// The JSON contract for the UI. Times are RFC 3339; every listed field is
// always present (strings may be "", lists are [] not null) unless marked
// "omitted when empty".
//
//	Flow (GET/PUT body, inside every reply that carries a flow):
//	  {id: "fl-<slug>", name, description, enabled, input_hint, version, updated,
//	   trigger: {kind: "manual"|"schedule", schedule (omitted when empty):
//	             "every 6h" | "every 30m" | "daily 08:00" | "weekdays 08:00" |
//	             "mon,thu 19:30" | a 5-field cron line},
//	   limits: {max_steps, max_tokens, max_cost_usd, max_parallel, timeout_min}
//	           (each omitted when 0 = its default: 50, 200000, 1.00, 2, 60),
//	   steps: [Step]}
//	Step: {id, kind, name?, ...} with only its kind's fields (others omitted):
//	  prompt   model, system, template, json
//	  plan     model, template, max_items
//	  fan_out  from, routes: [{kind, model}], default_model, as ("prompt"|"session"),
//	           template, agent, workspace, repo, folder, max_parallel
//	  session  agent, workspace ("scratch"|"repo"|"folder"), repo, folder, task
//	  if       test ("contains"|"matches"|"equals"|"json"|"state"), on (step id),
//	           value, path (json), then, else (a later step id, "end" or "")
//	  approve  message
//	  output   to ("notify"|"inbox"|"file"), template, file
//	Problem: {step ("" = the flow), field ("" = the whole step), message,
//	          severity: "error" (blocks save and run) | "warning"}
//	FlowView: {flow: Flow, problems: [Problem] (checked now against the installed
//	          models and agents), trigger_text: "Weekdays at 08:00" | "Manual" | ...,
//	          versions: [int] (kept previous versions, newest first)}
//	Summary (list item): every Flow field at the top level, plus
//	          trigger_text, next (time or null: the next scheduled start; null when
//	          manual, disabled or flows are off), last_run (RunBrief or null),
//	          last_skip ({time, reason} or null: the last scheduled start that
//	          did not happen)
//	RunBrief: {id, state, trigger, created, ended, note}
//	Run:      {id, flow, flow_name, flow_version, trigger: "manual"|"schedule"|"test",
//	           test, actor, input, state, reason, note, step (current or last step id),
//	           pc, steps_run, created, started, ended (zero time until set: "0001-01-01T00:00:00Z"),
//	           updated, tokens, cost_usd, energy_wh, active_ms, sessions: [id],
//	           models: [id], approval (null or {step, message, inbox, asked,
//	           decided, ok, note, by}), snapshot: Flow (the flow as it ran;
//	           omitted in run lists)}
//	  states: queued | running | waiting (an approval) | passed | failed |
//	          rejected | limited | canceled
//	StepRecord: {n, id, kind, name, state, input (as rendered), output (any JSON:
//	          text, parsed JSON, plan items [{title, kind, task}], fan-out results,
//	          {session, state, summary}, true/false for if, {ok, note, by} for
//	          approve, {file, bytes} for a file), model, prompt_tokens,
//	          completion_tokens, calls, cost_usd, energy_wh, started, ended,
//	          duration_ms, error, session, jump, file, items: [ItemRecord]}
//	ItemRecord: {index, title, kind, model, state, input, output, prompt_tokens,
//	          completion_tokens, calls, cost_usd, energy_wh, session, error,
//	          started, ended}
//	Graph:    {nodes, edges, updated}, the Flow view's shape (see flow.go and
//	          internal/flows/doc.go): nodes "input", "st:<step>", "it:<step>:<n>",
//	          "end", "m:<model>"; edges next | then | else | item | ask
//	Template: {key, name, description, flow: Flow (no id, version 0)}
//	Error:    {error: "message"} plus, on 400 for an invalid flow, problems:
//	          [Problem]; on 409 for a stale save, flow: the stored Flow
//
// Routes:
//
//	GET    /api/flows                  → [Summary]
//	POST   /api/flows                  Flow (id optional: from the name) → 201 FlowView
//	GET    /api/flows/templates        → [Template]: Clarify → Answer, Draft → Critique →
//	                                     Revise, Plan → Delegate → Merge, Website
//	POST   /api/flows/validate         Flow (a draft; nothing saved) → {problems, graph}
//	                                     (graph: FlowGraph without states, for a preview)
//	POST   /api/flows/test             {flow, input} → 202 Run (marked test, never scheduled)
//	GET    /api/flows/runs?flow=&limit= → [Run] newest first (no snapshot; limit 1-500, default 50)
//	GET    /api/flows/runs/{run}       → {run: Run, steps: [StepRecord]}
//	POST   /api/flows/runs/{run}/cancel           → Run (409 when already final)
//	POST   /api/flows/runs/{run}/approve {ok, note} → Run (409 unless waiting)
//	GET    /api/flows/runs/{run}/flow  → Graph (live states)
//	GET    /api/flows/{id}             → FlowView
//	PUT    /api/flows/{id}             Flow with the version it was read at → FlowView
//	                                     (409 + {error, flow} when it changed elsewhere)
//	DELETE /api/flows/{id}             → {ok: true} (its runs stay)
//	POST   /api/flows/{id}/run {input} → 202 Run (the flow need not be enabled)
//
// Statuses: 400 invalid body or flow (with problems), 404 unknown flow or
// run (also a malformed id), 403 a run while flows.enabled is off, 409
// stale version or wrong run state, 503 the engine is not available (the
// templates still answer). Live changes go out on /api/events as "flowrun"
// events {run, flow, state, step}; a stream that fell behind gets a
// "resync" {what: "flows"}.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/flows"
)

// maxFlowBody bounds a flow in a request (the engine keeps flow files
// under 4 MiB; templates are at most 64 KiB each).
const maxFlowBody = 4 << 20

// Run list bounds.
const (
	flowRunsDefault = 50
	flowRunsMax     = 500
)

// FlowView is a flow with its problems, trigger in words and kept versions.
type FlowView struct {
	Flow        flows.Flow      `json:"flow"`
	Problems    []flows.Problem `json:"problems"`
	TriggerText string          `json:"trigger_text"`
	Versions    []int           `json:"versions"`
}

// flowRunItem is a run in a list: the snapshot is left out.
type flowRunItem struct {
	flows.Run
	Snapshot *flows.Flow `json:"snapshot,omitempty"`
}

func (s *server) flowsRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/flows", s.handleFlowList)
	mux.HandleFunc("POST /api/flows", s.handleFlowCreate)
	mux.HandleFunc("GET /api/flows/templates", s.handleFlowTemplates)
	mux.HandleFunc("POST /api/flows/validate", s.handleFlowValidate)
	mux.HandleFunc("POST /api/flows/test", s.handleFlowTest)
	mux.HandleFunc("GET /api/flows/runs", s.handleFlowRuns)
	mux.HandleFunc("GET /api/flows/runs/{run}", s.handleFlowRun)
	mux.HandleFunc("POST /api/flows/runs/{run}/cancel", s.handleFlowCancel)
	mux.HandleFunc("POST /api/flows/runs/{run}/approve", s.handleFlowApprove)
	mux.HandleFunc("GET /api/flows/runs/{run}/flow", s.handleFlowRunGraph)
	mux.HandleFunc("GET /api/flows/{id}", s.handleFlowGet)
	mux.HandleFunc("PUT /api/flows/{id}", s.handleFlowPut)
	mux.HandleFunc("DELETE /api/flows/{id}", s.handleFlowDelete)
	mux.HandleFunc("POST /api/flows/{id}/run", s.handleFlowStart)
}

// flowReady answers 503 when there is no engine.
func (s *server) flowReady(w http.ResponseWriter) bool {
	if s.flows == nil {
		unavailable(w, "flows")
		return false
	}
	return true
}

// flowActor is who the engine records for this request.
func flowActor(r *http.Request) string {
	if a := principalOf(r).Actor; a != "" {
		return a
	}
	return audit.Owner("admin")
}

// flowError maps an engine error to a reply.
func flowError(w http.ResponseWriter, err error) {
	var ve *flows.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Error(), "problems": nonNil(ve.Problems)})
	case errors.Is(err, flows.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such flow or run")
	case errors.Is(err, flows.ErrStale), errors.Is(err, flows.ErrState):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, flows.ErrDisabled):
		writeError(w, http.StatusForbidden, "flows are turned off (flows.enabled)")
	case errors.Is(err, flows.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// flowID reads {id}; a malformed one is a 404 like an unknown one.
func flowID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !flows.ValidID(id) {
		writeError(w, http.StatusNotFound, "no such flow")
		return "", false
	}
	return id, true
}

func runID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("run")
	if !flows.ValidRunID(id) {
		writeError(w, http.StatusNotFound, "no such run")
		return "", false
	}
	return id, true
}

func decodeFlow(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeBodyLimit(w, r, v, maxFlowBody); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return false
	}
	return true
}

func (s *server) flowView(f flows.Flow, ps []flows.Problem) FlowView {
	if ps == nil {
		ps = s.flows.Validate(f)
	}
	return FlowView{Flow: f, Problems: nonNil(ps), TriggerText: flows.DescribeTrigger(f.Trigger),
		Versions: nonNil(s.flows.Store().Versions(f.ID))}
}

func (s *server) handleFlowList(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	keep := s.ownFilter(principalOf(r), r.URL.Query().Get("all") == "1")
	out := []flows.Summary{}
	for _, f := range s.flows.List() {
		if keep != nil && !keep(f.Owner) {
			continue
		}
		s.decorateFlow(&f.Flow)
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleFlowCreate(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	var f flows.Flow
	if !decodeFlow(w, r, &f) {
		return
	}
	f.Owner = creator(principalOf(r))
	out, ps, err := s.flows.Create(flowActor(r), f)
	if err != nil {
		flowError(w, err)
		return
	}
	noteAudit(r, "create "+out.Name, out.ID)
	writeJSON(w, http.StatusCreated, s.flowView(out, ps))
}

func (s *server) handleFlowTemplates(w http.ResponseWriter, r *http.Request) {
	if s.flows != nil {
		writeJSON(w, http.StatusOK, s.flows.Templates())
		return
	}
	writeJSON(w, http.StatusOK, flows.Templates())
}

func (s *server) handleFlowValidate(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	var f flows.Flow
	if !decodeFlow(w, r, &f) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": nonNil(s.flows.Validate(f)), "graph": flows.FlowGraph(f)})
}

func (s *server) handleFlowTest(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	var b struct {
		Flow  *flows.Flow `json:"flow"`
		Input string      `json:"input"`
	}
	if !decodeFlow(w, r, &b) {
		return
	}
	if b.Flow == nil {
		writeError(w, http.StatusBadRequest, "flow is required")
		return
	}
	b.Flow.Owner = creator(principalOf(r))
	if s.budgetRefused(w, b.Flow.Owner) {
		return
	}
	run, err := s.flows.TestRun(flowActor(r), *b.Flow, b.Input)
	if err != nil {
		flowError(w, err)
		return
	}
	noteAudit(r, "test run of "+run.FlowName, run.Flow, run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

func (s *server) handleFlowRuns(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	q := r.URL.Query()
	flow := strings.TrimSpace(q.Get("flow"))
	if flow != "" && !flows.ValidID(flow) {
		writeError(w, http.StatusBadRequest, "invalid flow id")
		return
	}
	limit := flowRunsDefault
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, flowRunsMax)
	}
	keep := s.ownFilter(principalOf(r), q.Get("all") == "1")
	runs := s.flows.Runs(flow, 0)
	out := []flowRunItem{}
	for _, run := range runs {
		if keep != nil && !keep(run.Owner) {
			continue
		}
		s.decorateRun(&run)
		out = append(out, flowRunItem{Run: run})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleFlowRun(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := runID(w, r)
	if !ok {
		return
	}
	d, err := s.flows.GetRun(id)
	if err != nil {
		flowError(w, err)
		return
	}
	s.decorateRun(&d.Run)
	d.Steps = nonNil(d.Steps)
	for i := range d.Steps {
		d.Steps[i].Items = nonNil(d.Steps[i].Items)
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) handleFlowCancel(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := runID(w, r)
	if !ok {
		return
	}
	run, err := s.flows.Cancel(flowActor(r), id)
	if err != nil {
		flowError(w, err)
		return
	}
	noteAudit(r, "cancel", run.Flow, id)
	writeJSON(w, http.StatusOK, run)
}

func (s *server) handleFlowApprove(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := runID(w, r)
	if !ok {
		return
	}
	var b struct {
		OK   *bool  `json:"ok"`
		Note string `json:"note"`
	}
	if !decodeFlow(w, r, &b) {
		return
	}
	if b.OK == nil {
		writeError(w, http.StatusBadRequest, "ok is required (true approves, false rejects)")
		return
	}
	run, err := s.flows.Approve(flowActor(r), id, *b.OK, b.Note)
	if err != nil {
		flowError(w, err)
		return
	}
	verb := "approve"
	if !*b.OK {
		verb = "reject"
	}
	noteAudit(r, verb+" "+b.Note, run.Flow, id)
	writeJSON(w, http.StatusOK, run)
}

func (s *server) handleFlowRunGraph(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := runID(w, r)
	if !ok {
		return
	}
	g, err := s.flows.Graph(id)
	if err != nil {
		flowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *server) handleFlowGet(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	f, err := s.flows.Get(id)
	if err != nil {
		flowError(w, err)
		return
	}
	s.decorateFlow(&f)
	writeJSON(w, http.StatusOK, s.flowView(f, nil))
}

func (s *server) handleFlowPut(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	var f flows.Flow
	if !decodeFlow(w, r, &f) {
		return
	}
	switch {
	case f.ID == "":
		f.ID = id
	case f.ID != id:
		writeError(w, http.StatusBadRequest, "the flow id in the body does not match the URL")
		return
	}
	if f.Version <= 0 {
		writeError(w, http.StatusBadRequest, "version is required (the version the flow was read at)")
		return
	}
	out, ps, err := s.flows.Update(flowActor(r), f)
	if errors.Is(err, flows.ErrStale) {
		cur, gerr := s.flows.Get(id)
		if gerr == nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "flow": cur})
			return
		}
	}
	if err != nil {
		flowError(w, err)
		return
	}
	// An admin saving the flow of a removed user takes it over: it runs as
	// theirs from now on (until then it does not run at all).
	if pr := principalOf(r); pr.admin() && pr.User != "" && s.hasUsers() && movedOwner(s.users, out.Owner) {
		if adopted, aerr := s.flows.Adopt(flowActor(r), out.ID, pr.User); aerr == nil {
			out = adopted
		}
	}
	noteAudit(r, "update "+out.Name+" v"+strconv.Itoa(out.Version), id)
	writeJSON(w, http.StatusOK, s.flowView(out, ps))
}

func (s *server) handleFlowDelete(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	if err := s.flows.Delete(flowActor(r), id); err != nil {
		flowError(w, err)
		return
	}
	noteAudit(r, "delete", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleFlowStart(w http.ResponseWriter, r *http.Request) {
	if !s.flowReady(w) {
		return
	}
	id, ok := flowID(w, r)
	if !ok {
		return
	}
	var b struct {
		Input string `json:"input"`
	}
	if !decodeFlow(w, r, &b) {
		return
	}
	if f, err := s.flows.Get(id); err == nil && s.budgetRefused(w, s.ownerOf(f.Owner)) {
		return
	}
	run, err := s.flows.RunFlow(flowActor(r), id, b.Input)
	if err != nil {
		flowError(w, err)
		return
	}
	noteAudit(r, "run", id, run.ID)
	writeJSON(w, http.StatusAccepted, run)
}
