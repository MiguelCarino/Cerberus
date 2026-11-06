package api

import (
	"context"
	"errors"
	"strings"

	"carino.systems/cerberus/internal/evals"
	"carino.systems/cerberus/internal/flows"
)

// Judge runs a flow's judge step (flows.JudgeRunner, evals E2) with the
// eval engine's rules: evals.Judge over the step's session, its calls
// through the gateway as client cerberus-eval with the flow run id as the
// usage session and the flow's owner charged, feedback resuming the
// session (budgeted and recorded in its feed like an eval run's). Each
// call's usage is handed to req.Charge, so the flow's limits apply.
func (h *EvalHost) Judge(ctx context.Context, req flows.JudgeRequest) (flows.JudgeResult, error) {
	if h.d.Sessions == nil {
		return flows.JudgeResult{}, errors.New("agent sessions are not available")
	}
	if h.d.Gateway == nil {
		return flows.JudgeResult{}, errors.New("the judge model cannot be called here (no gateway)")
	}
	s := h.settings()
	if !s.Enabled {
		return flows.JudgeResult{}, evals.ErrDisabled
	}
	model := ""
	for _, m := range []string{req.Model, s.JudgeModel, s.DefaultModel} {
		if m = strings.TrimSpace(m); m != "" && m != "default" { // the editor's "Default model": the judge fallbacks
			model = m
			break
		}
	}
	task := ""
	if in, ok := h.d.Sessions.Get(req.Session); ok {
		task = in.Task
	}
	job := evals.Job{Session: req.Session, Task: task, Gates: req.Gates, Criteria: req.Criteria, Persona: req.Persona,
		JudgeModel: model, Run: req.Run, Owner: req.Owner, Setup: req.Step, Feedback: true}
	if req.Charge != nil {
		job.Charge = func(u evals.Usage) error {
			prompt, completion := u.Prompt, u.Completion
			if prompt+completion != u.Tokens { // only the total is known
				prompt, completion = u.Tokens, 0
			}
			return req.Charge(flows.ModelReply{Model: model, PromptTokens: prompt, CompletionTokens: completion,
				CostUSD: u.CostUSD, EnergyWh: u.EnergyWh})
		}
	}
	rep, err := evals.Judge(ctx, evals.JudgeDeps{Sessions: evalSessions{h}, Models: evalModels{h}}, s, job)
	if err != nil {
		return flows.JudgeResult{}, err
	}
	return judgeResult(rep), nil
}

// judgeResult is a judge step's output from a report: passed when every
// gate passed; failed lists the criteria that failed or could not be
// judged (not the skipped ones).
func judgeResult(rep evals.Report) flows.JudgeResult {
	out := flows.JudgeResult{GatesPassed: rep.GatesPassed, GatesTotal: rep.GatesTotal, Failed: []string{}, Report: rep}
	out.Passed = rep.State == evals.ReportJudged && rep.GatesTotal > 0 && rep.GatesPassed == rep.GatesTotal
	for _, r := range rep.Results {
		if r.State == evals.StateFail || r.State == evals.StateError {
			out.Failed = append(out.Failed, r.Criterion)
		}
	}
	return out
}
