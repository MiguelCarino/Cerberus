package api

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/evals"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// resumableEvalSessions adds Resume and the feed to fakeEvalSessions: a
// resumed session ends (passed) at once.
type resumableEvalSessions struct {
	*fakeEvalSessions
	rmu     sync.Mutex
	resumes []string
	feed    []string
}

func (r *resumableEvalSessions) Resume(ctx context.Context, id, text string) error {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.resumes = append(r.resumes, id+": "+text)
	return nil
}

func (r *resumableEvalSessions) Record(id, kind, text string) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.feed = append(r.feed, kind+" "+text)
}

func TestEvalHostJudgeStep(t *testing.T) {
	us, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { us.Close() })
	sess := &resumableEvalSessions{fakeEvalSessions: &fakeEvalSessions{root: t.TempDir(), infos: map[string]session.Info{}}}
	gw := &fakeJudgeGateway{use: us}
	cfg := config.Defaults()
	cfg.Evals.JudgeModel = "m-local"
	host := NewEvalHost(EvalHostDeps{Config: func() *config.Config { return cfg }, Gateway: gw, Usage: us, Sessions: sess,
		Models: func(context.Context) []engine.ModelInfo { return []engine.ModelInfo{{ID: "m-local", Local: true}} }})
	infos, _ := sess.Create(context.Background(), session.TaskSpec{Task: "build it", Agent: "cerberus", Workspace: session.WorkspaceScratch})
	sid := infos[0].ID

	var charged []flows.ModelReply
	var cmu sync.Mutex
	res, err := host.Judge(context.Background(), flows.JudgeRequest{Session: sid, Run: "fr-20261105-000000-abcd", Step: "check",
		Gates:    []evals.Gate{{ID: "main", MaxIterations: 2}},
		Criteria: []evals.Criterion{{ID: "ok", Prompt: "It is ok"}, {ID: "nope", Prompt: "It is perfect"}},
		Charge: func(r flows.ModelReply) error {
			cmu.Lock()
			charged = append(charged, r)
			cmu.Unlock()
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.GatesTotal != 1 || res.GatesPassed != 0 || strings.Join(res.Failed, ",") != "nope" {
		t.Fatalf("result %+v", res)
	}
	if res.Report.Gates[0].Iterations != 2 || len(res.Report.Feedback) != 1 || !res.Report.Feedback[0].Sent {
		t.Fatalf("report %+v", res.Report)
	}
	if len(sess.resumes) != 1 || !strings.HasPrefix(sess.resumes[0], sid+": ") {
		t.Fatalf("resumes %q", sess.resumes)
	}
	if len(sess.feed) != 1 || !strings.HasPrefix(sess.feed[0], session.FeedFollowUp+" "+evals.FeedbackPrefix) {
		t.Fatalf("feed %q", sess.feed)
	}
	// 2 criteria × 2 iterations + 1 feedback call, each charged to the flow.
	// The fake gateway answers 30 prompt + 10 completion tokens a call: the
	// split reaches the step's recorded usage, not a 40-token prompt total.
	if len(charged) != 5 || charged[0].PromptTokens != 30 || charged[0].CompletionTokens != 10 {
		t.Fatalf("charged %+v", charged)
	}
	gw.mu.Lock()
	for _, h := range gw.hdr {
		if h.Get("X-Cerberus-Client") != evals.ClientName || h.Get("X-Cerberus-Session") != "fr-20261105-000000-abcd" {
			t.Fatalf("headers %v", h)
		}
	}
	gw.mu.Unlock()

	// evals.enabled off refuses judge steps.
	cfg.Evals.Enabled = false
	if _, err := host.Judge(context.Background(), flows.JudgeRequest{Session: sid,
		Criteria: []evals.Criterion{{ID: "ok", Prompt: "x"}}}); err == nil {
		t.Fatal("judged with evals off")
	}
}

func TestJudgeResult(t *testing.T) {
	rep := evals.Report{State: evals.ReportJudged, GatesPassed: 2, GatesTotal: 2,
		Results: []evals.Result{{Criterion: "a", State: evals.StatePass}}}
	if r := judgeResult(rep); !r.Passed || len(r.Failed) != 0 {
		t.Fatalf("%+v", r)
	}
	rep.GatesPassed = 1
	rep.Results = append(rep.Results, evals.Result{Criterion: "b", State: evals.StateError},
		evals.Result{Criterion: "c", State: evals.StateSkipped})
	if r := judgeResult(rep); r.Passed || strings.Join(r.Failed, ",") != "b" {
		t.Fatalf("%+v", r)
	}
}

// The host's per-session usage map forgets sessions the manager no longer
// has, so it does not grow with every eval session a daemon ever ran.
func TestEvalHostUsageForgetsGoneSessions(t *testing.T) {
	sess := &fakeEvalSessions{root: t.TempDir(), infos: map[string]session.Info{}}
	host := NewEvalHost(EvalHostDeps{Config: func() *config.Config { return config.Defaults() }, Sessions: sess})
	es := evalSessions{host}
	a, err := es.Start(context.Background(), evals.StartRequest{Agent: "cerberus", Workspace: evals.WorkspaceScratch, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	sess.mu.Lock()
	delete(sess.infos, a) // removed
	sess.mu.Unlock()
	b, err := es.Start(context.Background(), evals.StartRequest{Agent: "cerberus", Workspace: evals.WorkspaceScratch, Task: "y"})
	if err != nil {
		t.Fatal(err)
	}
	host.useMu.Lock()
	_, hasA := host.use[a]
	_, hasB := host.use[b]
	n := len(host.use)
	host.useMu.Unlock()
	if hasA || !hasB || n != 1 {
		t.Fatalf("use map: a %v b %v len %d", hasA, hasB, n)
	}
}

// Judge evidence hides token shapes the vault never held: the session's
// own agent token (no longer pinned once the run ended) and keys in a
// workspace .env.
func TestEvalSessionsMaskTokenShapes(t *testing.T) {
	sess := &fakeEvalSessions{root: t.TempDir(), infos: map[string]session.Info{}}
	cfg := config.Defaults()
	cfg.Server.Token = "0123456789abcdef0123456789abcdef"
	host := NewEvalHost(EvalHostDeps{Config: func() *config.Config { return cfg }, Sessions: sess})
	tok := cfg.SessionAgentToken("20261105-120000-abcd")
	if tok == "" {
		t.Fatal("no agent token")
	}
	in := "OPENAI_API_KEY=" + tok + "\nOTHER=sk-abcdefghijklmnopqrstuvwxyz0123\nplain text"
	out := evalSessions{host}.Mask(in)
	if strings.Contains(out, tok) || strings.Contains(out, "sk-abcdefghijklmnop") || !strings.Contains(out, "plain text") {
		t.Fatalf("masked %q", out)
	}
}
