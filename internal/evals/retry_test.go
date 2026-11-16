package evals

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// scriptModels answers judge calls from a list of results.
type scriptModels struct {
	mu    sync.Mutex
	errs  []error // one per call while it lasts; nil = a pass verdict
	calls []ModelRequest
}

func (s *scriptModels) Complete(ctx context.Context, req ModelRequest) (ModelReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		if err != nil {
			return ModelReply{PromptTokens: 7}, err
		}
	}
	return ModelReply{Text: `{"verdict":"pass","rationale":"ok","evidence":[]}`, PromptTokens: 10, CompletionTokens: 5}, nil
}

func shortJudgeWait(t *testing.T, d time.Duration) {
	old := judgeRetryWait
	judgeRetryWait = d
	t.Cleanup(func() { judgeRetryWait = old })
}

func TestJudgeOneRetriesTransientOnce(t *testing.T) {
	shortJudgeWait(t, time.Millisecond)
	m := &scriptModels{errs: []error{&ModelError{Status: 503, Message: "the local engine failed and is restarting"}}}
	v, rep, err := judgeOne(context.Background(), m, ModelRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || v.Verdict != StatePass || len(m.calls) != 2 {
		t.Fatalf("verdict %+v err %v calls %d", v, err, len(m.calls))
	}
	if rep.PromptTokens != 17 { // the failed call's usage counts too
		t.Errorf("usage %+v", rep)
	}
}

func TestJudgeOneRetriesOnlyOnce(t *testing.T) {
	shortJudgeWait(t, time.Millisecond)
	crash := &ModelError{Status: 502, Message: "upstream: connection reset"}
	m := &scriptModels{errs: []error{crash, crash}}
	_, _, err := judgeOne(context.Background(), m, ModelRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, error(crash)) || len(m.calls) != 2 {
		t.Fatalf("err %v calls %d", err, len(m.calls))
	}
}

func TestJudgeOneNeverRetriesLimitsOrPlainErrors(t *testing.T) {
	shortJudgeWait(t, time.Millisecond)
	for _, e := range []error{
		&ModelError{Status: 429, Message: "rate limited"},
		&ModelError{Status: 400, Message: "bad request"},
		&ModelError{Status: 503, Message: "not enough free VRAM for this model"},
		errors.New("monthly budget reached"),
		context.Canceled,
	} {
		m := &scriptModels{errs: []error{e}}
		if _, _, err := judgeOne(context.Background(), m, ModelRequest{}); err == nil || len(m.calls) != 1 {
			t.Errorf("%v: err %v calls %d", e, err, len(m.calls))
		}
	}
}

func TestJudgeOneRetryWaitEndsWithCancel(t *testing.T) {
	shortJudgeWait(t, time.Hour)
	m := &scriptModels{errs: []error{&ModelError{Status: 503, Message: "terminated"}}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := judgeOne(ctx, m, ModelRequest{})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}

func TestJudgeCallsCarryTheVerdictSchema(t *testing.T) {
	var s struct {
		Type     string         `json:"type"`
		Required []string       `json:"required"`
		Props    map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(VerdictSchema, &s); err != nil || s.Type != "object" || len(s.Required) != 3 || s.Props["verdict"] == nil {
		t.Fatalf("schema %s: %v", VerdictSchema, err)
	}
	m := &scriptModels{}
	j := &judging{models: m, job: Job{JudgeModel: "judge", Task: "t"}}
	res, _ := j.judgeCriterion(context.Background(), evidenceInput{}, Criterion{ID: "c1", Prompt: "p"}, "g", 0)
	if res.State != StatePass || len(m.calls) != 1 || string(m.calls[0].Schema) != string(VerdictSchema) {
		t.Fatalf("result %+v calls %+v", res, m.calls)
	}
}
