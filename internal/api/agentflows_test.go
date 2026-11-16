package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/users"
)

func TestAgentFlowsForMCPClients(t *testing.T) {
	e := newFlowEnv(t)
	enabled := strings.Replace(askTwice, `"name":"Ask twice"`, `"name":"Ask twice","enabled":true`, 1)
	var made FlowView
	e.call(t, "POST", "/api/flows", enabled, http.StatusCreated, &made)
	e.call(t, "POST", "/api/flows", strings.Replace(askTwice, "Ask twice", "Off flow", 1), http.StatusCreated, nil)
	slow := `{"name":"Slow","enabled":true,"trigger":{"kind":"manual"},
	  "steps":[{"id":"a","kind":"prompt","model":"slow","template":"{input}"}]}`
	var slowFlow FlowView
	e.call(t, "POST", "/api/flows", slow, http.StatusCreated, &slowFlow)

	tokA, devA, err := e.devices.PairScoped("claude-code", devices.ScopeMCP, "")
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := e.devices.PairScoped("other", devices.ScopeMCP, "")
	if err != nil {
		t.Fatal(err)
	}

	// A session's agent cannot start flows.
	sessTok := e.srv.cfg.SessionAgentToken("s1")
	if w := e.do("GET", "/api/agent/flows", "", withBearer(sessTok), withHeader(sessionHeader, "s1")); w.Code != http.StatusForbidden {
		t.Fatalf("session caller: %d %s", w.Code, w.Body)
	}
	// The owner's credentials never reach the agent scope.
	if w := e.do("GET", "/api/agent/flows", "", withBearer(testToken)); w.Code == http.StatusOK {
		t.Fatal("the admin token listed agent flows")
	}

	// The list has the enabled flows only.
	w := e.do("GET", "/api/agent/flows", "", withBearer(tokA))
	var list []agent.FlowSummary
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if len(list) != 2 || list[0].ID == "" || !strings.Contains(agent.FormatFlows(list), "Ask twice") ||
		strings.Contains(w.Body.String(), "Off flow") {
		t.Fatalf("list %s", w.Body)
	}

	// Run it and wait for the result.
	w = e.do("POST", "/api/agent/flows/"+made.Flow.ID+"/run", `{"input":"why?"}`, withBearer(tokA))
	var st agent.FlowRunStatus
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &st) != nil || !flows.ValidRunID(st.ID) {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	w = e.do("GET", "/api/agent/flows/runs/"+st.ID+"?wait=20", "", withBearer(tokA))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	if !st.Done || st.State != flows.StatePassed || st.OutStep != "out" || !strings.Contains(st.Output, "why?") || st.Tokens == 0 {
		t.Fatalf("status %+v", st)
	}
	run, _ := e.eng.GetRun(st.ID)
	if run.Run.Actor != "client:claude-code#"+devA.ID {
		t.Fatalf("actor %q", run.Run.Actor)
	}
	if txt := agent.FormatFlowRun(st); !strings.Contains(txt, "is passed") || !strings.Contains(txt, "Output of step out") {
		t.Fatalf("tool text %q", txt)
	}

	// Another client does not see it; unknown and disabled flows are 404.
	if w := e.do("GET", "/api/agent/flows/runs/"+st.ID, "", withBearer(tokB)); w.Code != http.StatusNotFound {
		t.Fatalf("other client: %d", w.Code)
	}
	for _, id := range []string{"fl-nope", "fl-off-flow", "BAD"} {
		if w := e.do("POST", "/api/agent/flows/"+id+"/run", `{}`, withBearer(tokA)); w.Code != http.StatusNotFound {
			t.Fatalf("flow %s: %d %s", id, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/api/agent/flows/runs/"+st.ID+"?wait=x", "", withBearer(tokA)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad wait: %d", w.Code)
	}

	// At most 2 runs going per client.
	var runs []string
	for range 2 {
		w := e.do("POST", "/api/agent/flows/"+slowFlow.Flow.ID+"/run", `{"input":"x"}`, withBearer(tokA))
		if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &st) != nil {
			t.Fatalf("slow run: %d %s", w.Code, w.Body)
		}
		runs = append(runs, st.ID)
	}
	if w := e.do("POST", "/api/agent/flows/"+made.Flow.ID+"/run", `{}`, withBearer(tokA)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("third run: %d %s", w.Code, w.Body)
	}
	// Another client has its own allowance.
	w = e.do("POST", "/api/agent/flows/"+made.Flow.ID+"/run", `{}`, withBearer(tokB))
	if w.Code != http.StatusAccepted {
		t.Fatalf("other client's run: %d %s", w.Code, w.Body)
	}
	// A short wait on a running run answers it as it is.
	w = e.do("GET", "/api/agent/flows/runs/"+runs[0]+"?wait=0.05", "", withBearer(tokA))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Done {
		t.Fatalf("running status: %d %s", w.Code, w.Body)
	}
	for _, id := range runs {
		e.call(t, "POST", "/api/flows/runs/"+id+"/cancel", "", http.StatusOK, nil)
		e.wait(t, id)
	}
	if w := e.do("POST", "/api/agent/flows/"+made.Flow.ID+"/run", `{}`, withBearer(tokA)); w.Code != http.StatusAccepted {
		t.Fatalf("after the cancels: %d %s", w.Code, w.Body)
	}
}

// With users, a client sees and runs only its own user's flows.
func TestAgentFlowsOwnerScope(t *testing.T) {
	e := newFlowEnv(t)
	t.Cleanup(users.SetCostForTest(1000))
	st, err := users.Open(e.srv.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.users = st
	if _, err := st.Add("root", "", users.RoleAdmin, "a long password"); err != nil {
		t.Fatal(err)
	}
	ana, err := st.Add("ana", "", users.RoleUser, "a long password")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.Add("bob", "", users.RoleUser, "a long password")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, owner string) flows.Flow {
		f, _, err := e.eng.Create("owner:test", flows.Flow{Name: name, Enabled: true, Owner: owner,
			Steps: []flows.Step{{ID: "a", Kind: flows.KindPrompt, Model: "m-local", Template: "{input}"}}})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	mine, theirs := mk("Ana's", ana.ID), mk("Bob's", bob.ID)
	tok, _, err := e.devices.PairScoped("ana-agent", devices.ScopeMCP, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/api/agent/flows", "", withBearer(tok))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), mine.ID) || strings.Contains(w.Body.String(), theirs.ID) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/agent/flows/"+theirs.ID+"/run", `{}`, withBearer(tok)); w.Code != http.StatusNotFound {
		t.Fatalf("another owner's flow: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/api/agent/flows/"+mine.ID+"/run", `{"input":"hi"}`, withBearer(tok))
	var rs agent.FlowRunStatus
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &rs) != nil {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	if r := e.wait(t, rs.ID); r.Owner != ana.ID || r.State != flows.StatePassed {
		t.Fatalf("run %+v", r)
	}
}
