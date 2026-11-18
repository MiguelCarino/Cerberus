package api

import (
	"encoding/json"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/flows"
)

// TestFlowRunsPerFlow: ?per_flow=N keeps the newest N runs of each flow
// (newest first), counts every flow's runs in flow_runs, lifts the default
// limit and refuses a bad value; without it nothing changes.
func TestFlowRunsPerFlow(t *testing.T) {
	env := newFlowEnv(t)
	var a, b FlowView
	env.call(t, "POST", "/api/flows", askTwice, 201, &a)
	env.call(t, "POST", "/api/flows", strings.Replace(askTwice, `"Ask twice"`, `"Ask again"`, 1), 201, &b)
	if a.Flow.ID == b.Flow.ID {
		t.Fatalf("same id %s", a.Flow.ID)
	}
	start := func(id, input string) {
		var run flows.Run
		env.call(t, "POST", "/api/flows/"+id+"/run", `{"input":"`+input+`"}`, 202, &run)
		if done := env.wait(t, run.ID); done.State != flows.StatePassed {
			t.Fatalf("run %s: %s (%s)", run.ID, done.State, done.Reason)
		}
	}
	for i := range 3 {
		start(a.Flow.ID, "a"+string(rune('0'+i)))
	}
	for i := range 2 {
		start(b.Flow.ID, "b"+string(rune('0'+i)))
	}

	var got []flowRunItem
	env.call(t, "GET", "/api/flows/runs?per_flow=2", "", 200, &got)
	per := map[string]int{}
	for i, r := range got {
		per[r.Flow]++
		want := map[string]int{a.Flow.ID: 3, b.Flow.ID: 2}[r.Flow]
		if r.FlowRuns != want {
			t.Fatalf("run %s of %s: flow_runs %d, want %d", r.ID, r.Flow, r.FlowRuns, want)
		}
		if i > 0 && r.Created.After(got[i-1].Created) {
			t.Fatalf("not newest first: %+v", got)
		}
	}
	if len(got) != 4 || per[a.Flow.ID] != 2 || per[b.Flow.ID] != 2 {
		t.Fatalf("per flow %v (%d runs)", per, len(got))
	}
	// The kept runs of a flow are its newest.
	all := env.eng.Runs(a.Flow.ID, 0)
	keptA := []string{}
	for _, r := range got {
		if r.Flow == a.Flow.ID {
			keptA = append(keptA, r.ID)
		}
	}
	if keptA[0] != all[0].ID || keptA[1] != all[1].ID {
		t.Fatalf("kept %v of %v %v %v", keptA, all[0].ID, all[1].ID, all[2].ID)
	}

	// With one flow; and limit still bounds the whole list.
	env.call(t, "GET", "/api/flows/runs?per_flow=5&flow="+b.Flow.ID, "", 200, &got)
	if len(got) != 2 || got[0].FlowRuns != 2 || got[0].Flow != b.Flow.ID {
		t.Fatalf("one flow %+v", got)
	}
	env.call(t, "GET", "/api/flows/runs?per_flow=5&limit=3", "", 200, &got)
	if len(got) != 3 {
		t.Fatalf("limit 3: %d runs", len(got))
	}
	// Without per_flow: no flow_runs, the plain limit.
	w := env.do("GET", "/api/flows/runs?limit=4", "", withBearer(testToken))
	if w.Code != 200 || strings.Contains(w.Body.String(), `"flow_runs"`) {
		t.Fatalf("plain list: %d %s", w.Code, w.Body)
	}
	var plain []json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &plain) != nil || len(plain) != 4 {
		t.Fatalf("plain list %d", len(plain))
	}
	for _, bad := range []string{"0", "-1", "x"} {
		env.call(t, "GET", "/api/flows/runs?per_flow="+bad, "", 400, nil)
	}
}

// TestFlowRunsPerFlowScoped: per_flow keeps owner scoping: a user sees
// (and counts) only their own runs; an admin's all=1 sees everyone's.
func TestFlowRunsPerFlowScoped(t *testing.T) {
	e := newAskEnv(t)
	mk := func(owner, name string, runs int) string {
		f, probs, err := e.eng.Create("owner:test", flows.Flow{Name: name, Owner: e.user[owner].ID, Trigger: flows.Trigger{Kind: "manual"},
			Steps: []flows.Step{{ID: "build", Kind: flows.KindSession, Name: "Assemble", Agent: "cerberus",
				Workspace: flows.WorkspaceScratch, Task: "Build {input}"}}})
		if err != nil {
			t.Fatalf("create: %v %+v", err, probs)
		}
		for range runs {
			// A second run of a flow may stay queued: it is listed all the same.
			if _, err := e.eng.RunFlow("owner:test", f.ID, "x"); err != nil {
				t.Fatal(err)
			}
		}
		return f.ID
	}
	bobs := mk("bob", "Bob site", 2)
	cys := mk("cy", "Cy site", 1)

	list := func(name, target string) []flowRunItem {
		t.Helper()
		w := e.as(name, "GET", target, "")
		var got []flowRunItem
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("%s %s: %d %s", name, target, w.Code, w.Body)
		}
		return got
	}
	got := list("bob", "/api/flows/runs?per_flow=1")
	if len(got) != 1 || got[0].Flow != bobs || got[0].FlowRuns != 2 {
		t.Fatalf("bob %+v", got)
	}
	// all=1 means nothing to a plain user.
	got = list("cy", "/api/flows/runs?per_flow=1&all=1")
	if len(got) != 1 || got[0].Flow != cys || got[0].FlowRuns != 1 {
		t.Fatalf("cy %+v", got)
	}
	got = list("root", "/api/flows/runs?per_flow=1&all=1")
	byFlow := map[string]int{}
	for _, r := range got {
		byFlow[r.Flow] = r.FlowRuns
	}
	if len(got) != 2 || byFlow[bobs] != 2 || byFlow[cys] != 1 {
		t.Fatalf("root all %+v", got)
	}
}
