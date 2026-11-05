package api

import (
	"encoding/json"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/permits"
)

// TestPermitsAPI: own rules only (bob never sees or removes cy's), manual
// adds go through Validate (its text in the 400), roles outside own work
// get 403, and the policy lists come along read-only.
func TestPermitsAPI(t *testing.T) {
	e := newUsersEnv(t)
	st, err := permits.Open(e.srv.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	st.SetResolver(e.st.Owner)
	e.srv.permits = st
	e.srv.cfg.Agent.Policy.Deny = []string{"shutdown"}

	w := e.call("bob", "POST", "/api/permits", `{"tool":"Bash","content":"go test:*"}`)
	if w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var added struct {
		Rule  permits.Saved `json:"rule"`
		Added bool          `json:"added"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if !added.Added || added.Rule.Source != permits.SourceSettings || added.Rule.Owner != e.u["bob"].ID {
		t.Fatalf("added %+v", added)
	}
	if w := e.call("bob", "POST", "/api/permits", `{"tool":"Bash","content":"rm:*"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "prefix") {
		t.Fatalf("risky prefix: %d %s", w.Code, w.Body)
	}
	if w := e.call("bob", "POST", "/api/permits", `{"tool":"Write","content":""}`); w.Code != 400 {
		t.Fatalf("tool-wide Write: %d %s", w.Code, w.Body)
	}
	if w := e.call("cy", "POST", "/api/permits", `{"tool":"Read","content":"dir:~/notes"}`); w.Code != 200 {
		t.Fatalf("cy add: %d %s", w.Code, w.Body)
	}

	var list struct {
		Rules  []permits.Saved     `json:"rules"`
		Policy map[string][]string `json:"policy"`
		Link   string              `json:"policy_link"`
	}
	w = e.call("bob", "GET", "/api/permits", "")
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Rules) != 1 || list.Rules[0].Content != "go test:*" || len(list.Policy["deny"]) != 1 || list.Link != "" {
		t.Fatalf("bob's list: %d %s", w.Code, w.Body)
	}
	w = e.call("ana", "GET", "/api/permits", "")
	list.Rules = nil
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Rules) != 0 || list.Link == "" {
		t.Fatalf("ana's list: %s", w.Body)
	}
	cyRule := st.List(e.u["cy"].ID)[0]
	if w := e.call("bob", "DELETE", "/api/permits/"+cyRule.ID, ""); w.Code != 404 {
		t.Fatalf("bob removed cy's rule: %d", w.Code)
	}
	if w := e.call("cy", "DELETE", "/api/permits/"+cyRule.ID, ""); w.Code != 200 {
		t.Fatalf("cy remove: %d %s", w.Code, w.Body)
	}
	for _, who := range []string{"vic", "mo", "cat"} {
		if w := e.call(who, "GET", "/api/permits", ""); w.Code != 403 {
			t.Errorf("%s GET: %d", who, w.Code)
		}
		if w := e.call(who, "POST", "/api/permits", `{"tool":"Bash","content":"ls"}`); w.Code != 403 {
			t.Errorf("%s POST: %d", who, w.Code)
		}
	}
}
