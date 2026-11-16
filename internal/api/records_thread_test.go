package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/records"
)

// A record keeps only its new turn; ?thread=1 answers the conversation
// stitched from the earlier lines, and the read is audited once.
func TestRecordThreadRoute(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	env, st := trajEnv(t, now.Add(-time.Minute), records.TextUser)
	st.Add(records.Line{Time: now.Add(-50 * time.Second), API: "chat", Session: "s1", Client: "opencode", Messages: 1,
		Text: &records.Text{Messages: "user: Fix it", Reply: "Reading"}})
	st.Add(records.Line{Time: now.Add(-40 * time.Second), API: "chat", Session: "s1", Client: "opencode", Messages: 3, Prefix: 2,
		Text: &records.Text{Messages: "user: go on", Reply: "Done"}})
	lines, _ := st.List(records.Filter{})
	if len(lines) != 2 {
		t.Fatalf("lines %+v", lines)
	}
	url := "/api/records/" + lines[0].Ref

	w := env.do("GET", url, "", withBearer(testToken))
	var one records.Line
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &one) != nil || one.Prefix != 2 || one.Text.Messages != "user: go on" {
		t.Fatalf("line %d %s", w.Code, w.Body)
	}
	w = env.do("GET", url+"?thread=1", "", withBearer(testToken))
	var th records.Stitched
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &th) != nil {
		t.Fatalf("thread %d %s", w.Code, w.Body)
	}
	if th.Earlier != 1 || th.Missing || th.Text.Messages != "user: Fix it\nassistant: Reading\nuser: go on" {
		t.Fatalf("stitched %s", w.Body)
	}
	found := 0
	for _, rec := range env.records(t) {
		if strings.Contains(rec.Detail, "and its earlier turns") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("%d audit records for the stitched read", found)
	}
}
