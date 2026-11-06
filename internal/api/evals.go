package api

// Evals (docs/SCOPE.md "E1 Evals"): saved definitions (a task, a criteria
// graph in gates, the setups to try), their runs and reports, over the
// eval engine (internal/evals). People only (WORK): a user sees and runs
// their own evals, admins all. Every write is audited twice: the HTTP
// record and the engine's event ("eval.create", "eval.run.start", ...).
//
// The JSON contract for the UI (times RFC 3339; lists are [] not null):
//
//	Eval:     {id, name, version, updated, owner?, owner_name?, task, workspace:
//	           "scratch"|"repo", repo?, base?, gates: [{id, name?, max_iterations}],
//	           criteria: [{id, prompt, gate?, depends_on?, evidence?}],
//	           setups: [{label, agent, model?}], repeat, judge_model, persona}
//	EvalView: {eval, problems: [{field, message, severity}], judge_model (the
//	           model that would judge now), gates (effective gates)}
//	Summary:  every Eval field plus last_run (RunBrief or null)
//	RunBrief: {id, state, created, ended, sessions, judged}
//	Run:      {id, eval, eval_name, eval_version, owner, actor, state:
//	           running|judging|done|canceled|failed, reason, setup_filter, base,
//	           judge_model, judge_prompt, max_parallel, created, started, ended,
//	           updated, sessions: [{setup, repeat, agent, model, session, state:
//	           pending|running|judging|judged|error|canceled, error, live}],
//	           judge_usage: {tokens, cost_usd, energy_wh}, snapshot: Eval
//	           (omitted in lists)}
//	RunDetail: {run, reports: [Report], summary: [Rank]} (see internal/evals)
//
// Routes:
//
//	GET    /api/evals                 → [Summary]
//	POST   /api/evals                 Eval → 201 EvalView
//	POST   /api/evals/validate        Eval (nothing saved) → {problems, eval, gates, order}
//	GET    /api/evals/runs?eval=&limit= → [Run] newest first (no snapshot)
//	GET    /api/evals/runs/{run}      → RunDetail (with the sessions' live states)
//	POST   /api/evals/runs/{run}/cancel  → Run (409 unless going)
//	POST   /api/evals/runs/{run}/rejudge → 202 Run (409 while going)
//	GET    /api/evals/{id}            → EvalView
//	PUT    /api/evals/{id}            Eval with the version read → EvalView (409 + {error, eval})
//	DELETE /api/evals/{id}            → {ok: true} (runs stay)
//	POST   /api/evals/{id}/run {setup?} → 202 Run
//
// Statuses: 400 invalid (with problems), 403 evals.enabled off, 404 unknown
// (or malformed) id, 409 stale or wrong state, 503 no engine. Live changes
// go out on /api/events as "evalrun" events {run, eval, state, session};
// a stream that fell behind gets "resync" {what: "evals"}.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/evals"
)

const maxEvalBody = 1 << 20

// EvalView is an eval with its problems and the judge that would run now.
type EvalView struct {
	Eval       evals.Eval      `json:"eval"`
	Problems   []evals.Problem `json:"problems"`
	JudgeModel string          `json:"judge_model"`
	Gates      []evals.Gate    `json:"gates"`
}

// EvalRunBrief is a run in an eval list.
type EvalRunBrief struct {
	ID       string    `json:"id"`
	State    string    `json:"state"`
	Created  time.Time `json:"created"`
	Ended    time.Time `json:"ended"`
	Sessions int       `json:"sessions"`
	Judged   int       `json:"judged"`
}

// EvalSummary is an eval in the list.
type EvalSummary struct {
	evals.Eval
	LastRun *EvalRunBrief `json:"last_run"`
}

// evalRunItem is a run in a list: the snapshot is left out.
type evalRunItem struct {
	evals.Run
	Snapshot *evals.Eval `json:"snapshot,omitempty"`
}

func (s *server) evalsRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/evals", s.handleEvalList)
	mux.HandleFunc("POST /api/evals", s.handleEvalCreate)
	mux.HandleFunc("POST /api/evals/validate", s.handleEvalValidate)
	mux.HandleFunc("GET /api/evals/runs", s.handleEvalRuns)
	mux.HandleFunc("GET /api/evals/runs/{run}", s.handleEvalRun)
	mux.HandleFunc("POST /api/evals/runs/{run}/cancel", s.handleEvalCancel)
	mux.HandleFunc("POST /api/evals/runs/{run}/rejudge", s.handleEvalRejudge)
	mux.HandleFunc("GET /api/evals/{id}", s.handleEvalGet)
	mux.HandleFunc("PUT /api/evals/{id}", s.handleEvalPut)
	mux.HandleFunc("DELETE /api/evals/{id}", s.handleEvalDelete)
	mux.HandleFunc("POST /api/evals/{id}/run", s.handleEvalStart)
}

func (s *server) evalReady(w http.ResponseWriter) bool {
	if s.evals == nil {
		unavailable(w, "evals")
		return false
	}
	return true
}

func evalError(w http.ResponseWriter, err error) {
	var ve *evals.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Error(), "problems": nonNil(ve.Problems)})
	case errors.Is(err, evals.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such eval or run")
	case errors.Is(err, evals.ErrStale), errors.Is(err, evals.ErrState):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, evals.ErrDisabled):
		writeError(w, http.StatusForbidden, "evals are turned off (evals.enabled)")
	case errors.Is(err, evals.ErrNoJudge):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, evals.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		var ce *confineError
		if errors.As(err, &ce) {
			writeConfine(w, ce)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func evalID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !evals.ValidID(id) {
		writeError(w, http.StatusNotFound, "no such eval")
		return "", false
	}
	return id, true
}

func evalRunID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("run")
	if !evals.ValidRunID(id) {
		writeError(w, http.StatusNotFound, "no such run")
		return "", false
	}
	return id, true
}

func decodeEval(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeBodyLimit(w, r, v, maxEvalBody); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return false
	}
	return true
}

func (s *server) decorateEval(e *evals.Eval) {
	if s.hasUsers() {
		e.Owner = s.users.Owner(e.Owner)
		e.OwnerName = s.ownerName(e.Owner)
	}
}

func (s *server) decorateEvalRun(r *evals.Run) {
	if s.hasUsers() {
		r.Owner = s.users.Owner(r.Owner)
		r.OwnerName = s.ownerName(r.Owner)
	}
}

func (s *server) evalView(e evals.Eval, ps []evals.Problem) EvalView {
	if ps == nil {
		ps = s.evals.Validate(e)
	}
	s.decorateEval(&e)
	return EvalView{Eval: e, Problems: nonNil(ps), JudgeModel: s.evals.JudgeModelFor(e), Gates: evals.EffectiveGates(e)}
}

func evalBrief(r evals.Run) *EvalRunBrief {
	b := &EvalRunBrief{ID: r.ID, State: r.State, Created: r.Created, Ended: r.Ended, Sessions: len(r.Slots)}
	for _, sl := range r.Slots {
		if sl.State == evals.SlotJudged {
			b.Judged++
		}
	}
	return b
}

func (s *server) handleEvalList(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	keep := s.ownFilter(principalOf(r), r.URL.Query().Get("all") == "1")
	out := []EvalSummary{}
	for _, e := range s.evals.List() {
		if keep != nil && !keep(e.Owner) {
			continue
		}
		s.decorateEval(&e)
		item := EvalSummary{Eval: e}
		if last, ok := s.evals.LastRun(e.ID); ok {
			item.LastRun = evalBrief(last)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleEvalCreate(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	var e evals.Eval
	if !decodeEval(w, r, &e) {
		return
	}
	e.Owner = creator(principalOf(r))
	out, ps, err := s.evals.Create(flowActor(r), e)
	if err != nil {
		evalError(w, err)
		return
	}
	noteAudit(r, "create "+out.Name, out.ID)
	writeJSON(w, http.StatusCreated, s.evalView(out, ps))
}

func (s *server) handleEvalValidate(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	var e evals.Eval
	if !decodeEval(w, r, &e) {
		return
	}
	n := evals.Normalize(e)
	ps := s.evals.Validate(n)
	order := [][]string{}
	if !evals.HasErrors(ps) {
		for _, g := range evals.Order(n) {
			ids := []string{}
			for _, c := range g {
				ids = append(ids, c.ID)
			}
			order = append(order, ids)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": ps, "eval": n, "gates": evals.EffectiveGates(n), "order": order})
}

func (s *server) handleEvalRuns(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	q := r.URL.Query()
	ev := strings.TrimSpace(q.Get("eval"))
	if ev != "" && !evals.ValidID(ev) {
		writeError(w, http.StatusBadRequest, "invalid eval id")
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
	out := []evalRunItem{}
	for _, run := range s.evals.Runs(ev, 0) {
		if keep != nil && !keep(run.Owner) {
			continue
		}
		s.decorateEvalRun(&run)
		out = append(out, evalRunItem{Run: run})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleEvalRun(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	id, ok := evalRunID(w, r)
	if !ok {
		return
	}
	d, err := s.evals.GetRun(id)
	if err != nil {
		evalError(w, err)
		return
	}
	s.decorateEvalRun(&d.Run)
	s.decorateEval(&d.Run.Snapshot)
	writeJSON(w, http.StatusOK, d)
}

func (s *server) handleEvalCancel(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	id, ok := evalRunID(w, r)
	if !ok {
		return
	}
	run, err := s.evals.Cancel(flowActor(r), id)
	if err != nil {
		evalError(w, err)
		return
	}
	noteAudit(r, "cancel", run.Eval, id)
	s.decorateEvalRun(&run)
	writeJSON(w, http.StatusOK, evalRunItem{Run: run})
}

func (s *server) handleEvalRejudge(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	id, ok := evalRunID(w, r)
	if !ok {
		return
	}
	if o, ok := s.evals.RunOwner(id); ok && s.budgetRefused(w, s.ownerOf(o)) {
		return
	}
	run, err := s.evals.Rejudge(flowActor(r), id)
	if err != nil {
		evalError(w, err)
		return
	}
	noteAudit(r, "rejudge with "+run.JudgeModel, run.Eval, id)
	s.decorateEvalRun(&run)
	writeJSON(w, http.StatusAccepted, evalRunItem{Run: run})
}

func (s *server) handleEvalGet(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	id, ok := evalID(w, r)
	if !ok {
		return
	}
	e, err := s.evals.Get(id)
	if err != nil {
		evalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.evalView(e, nil))
}

func (s *server) handleEvalPut(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	id, ok := evalID(w, r)
	if !ok {
		return
	}
	var e evals.Eval
	if !decodeEval(w, r, &e) {
		return
	}
	switch {
	case e.ID == "":
		e.ID = id
	case e.ID != id:
		writeError(w, http.StatusBadRequest, "the eval id in the body does not match the URL")
		return
	}
	if e.Version <= 0 {
		writeError(w, http.StatusBadRequest, "version is required (the version the eval was read at)")
		return
	}
	out, ps, err := s.evals.Update(flowActor(r), e)
	if errors.Is(err, evals.ErrStale) {
		if cur, gerr := s.evals.Get(id); gerr == nil {
			s.decorateEval(&cur)
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "eval": cur})
			return
		}
	}
	if err != nil {
		evalError(w, err)
		return
	}
	noteAudit(r, "update "+out.Name+" v"+strconv.Itoa(out.Version), id)
	writeJSON(w, http.StatusOK, s.evalView(out, ps))
}

func (s *server) handleEvalDelete(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	id, ok := evalID(w, r)
	if !ok {
		return
	}
	if err := s.evals.Delete(flowActor(r), id); err != nil {
		evalError(w, err)
		return
	}
	noteAudit(r, "delete", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleEvalStart(w http.ResponseWriter, r *http.Request) {
	if !s.evalReady(w) {
		return
	}
	id, ok := evalID(w, r)
	if !ok {
		return
	}
	var b struct {
		Setup string `json:"setup"`
	}
	if !decodeEval(w, r, &b) {
		return
	}
	owner := creator(principalOf(r))
	if s.budgetRefused(w, s.ownerOf(owner)) {
		return
	}
	run, err := s.evals.Start(flowActor(r), owner, id, b.Setup)
	if err != nil {
		evalError(w, err)
		return
	}
	noteAudit(r, "run "+orDefault(b.Setup, "all setups"), id, run.ID)
	s.decorateEvalRun(&run)
	writeJSON(w, http.StatusAccepted, evalRunItem{Run: run})
}
