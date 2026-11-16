package evals

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Text mode: an answer (no session) is judged, the feedback goes to
// Revise, and the revised answer is judged on the next iteration.
func TestJudgeTextModeRevises(t *testing.T) {
	judge := &fakeJudge{failTimes: map[string]int{"short": 1}}
	var got []string
	job := Job{Text: "a long rambling answer with sk-SECRET123 in it", Task: "Write one line.", JudgeModel: "judge",
		Run: "fr-1", Owner: "u1", Feedback: true,
		Gates:    []Gate{{ID: "main", MaxIterations: 3}},
		Criteria: []Criterion{{ID: "short", Prompt: "It is one line"}},
		Revise: func(ctx context.Context, feedback string) (string, error) {
			got = append(got, feedback)
			return "one line", nil
		}}
	mask := func(s string) string { return strings.ReplaceAll(s, "sk-SECRET123", "[secret]") }
	rep, err := Judge(context.Background(), JudgeDeps{Sessions: maskOnly{mask}, Models: judge}, Settings{}, job)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GatesPassed != 1 || rep.Gates[0].Iterations != 2 || len(rep.Feedback) != 1 || !rep.Feedback[0].Sent {
		t.Fatalf("%+v", rep)
	}
	if len(got) != 1 || !strings.Contains(got[0], "short") {
		t.Fatalf("revise calls %q", got)
	}
	// The first judge call saw the masked answer as the only evidence; the
	// second saw the revised one.
	var judged []string
	for _, c := range judge.calls {
		if c.Messages[0].Content == JudgeSystemPrompt {
			judged = append(judged, c.Messages[1].Content)
		}
	}
	if len(judged) != 2 {
		t.Fatalf("%d judge calls", len(judged))
	}
	if !strings.Contains(judged[0], "== output (the answer to judge) ==") || strings.Contains(judged[0], "sk-SECRET123") ||
		!strings.Contains(judged[0], "[secret]") || strings.Contains(judged[0], "== files ==") {
		t.Fatalf("first evidence: %s", judged[0])
	}
	if !strings.Contains(judged[1], "one line") || !strings.Contains(judged[1], "Write one line.") {
		t.Fatalf("second evidence: %s", judged[1])
	}
	if rep.Session != "" || rep.SessionUsage.Tokens != 0 {
		t.Fatalf("session fields %+v", rep)
	}
}

// A Revise error ends the gate with the results it has; no Sessions is
// needed in text mode.
func TestJudgeTextModeReviseFails(t *testing.T) {
	judge := &fakeJudge{verdicts: map[string]string{"short": "fail"}}
	job := Job{Text: "x", JudgeModel: "judge", Feedback: true,
		Gates:    []Gate{{ID: "main", MaxIterations: 3}},
		Criteria: []Criterion{{ID: "short", Prompt: "It is one line"}},
		Revise: func(ctx context.Context, feedback string) (string, error) {
			return "", errors.New("the model is gone")
		}}
	rep, err := Judge(context.Background(), JudgeDeps{Models: judge}, Settings{}, job)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GatesPassed != 0 || len(rep.Feedback) != 1 || rep.Feedback[0].Sent ||
		!strings.Contains(rep.Feedback[0].Error, "the model is gone") || rep.Gates[0].Iterations != 1 {
		t.Fatalf("%+v", rep)
	}
	// Without Revise every gate is judged once.
	job.Revise = nil
	judge2 := &fakeJudge{verdicts: map[string]string{"short": "fail"}}
	rep, err = Judge(context.Background(), JudgeDeps{Models: judge2}, Settings{}, job)
	if err != nil || len(rep.Feedback) != 0 || judge2.callCount() != 1 {
		t.Fatalf("%+v %v (%d calls)", rep, err, judge2.callCount())
	}
}

// maskOnly is a Sessions that only masks (text mode reads nothing else).
type maskOnly struct{ mask func(string) string }

func (maskOnly) Start(context.Context, StartRequest) (string, error) { return "", errors.New("no") }
func (maskOnly) Wait(context.Context, string) (SessionInfo, error) {
	return SessionInfo{}, errors.New("text mode never waits")
}
func (maskOnly) Stop(string) error                 { return nil }
func (maskOnly) Info(string) (SessionInfo, bool)   { return SessionInfo{}, false }
func (maskOnly) Transcript(string) ([]byte, error) { return nil, errors.New("no") }
func (maskOnly) Diff(string) (string, error)       { return "", errors.New("no") }
func (m maskOnly) Mask(s string) string            { return m.mask(s) }
func (maskOnly) Usage(string) Usage                { return Usage{Tokens: 99} }
