package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/records"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/trajectory"
)

// trajSessions is the fake session manager with a fixed Info for s1 and a
// setup snapshot.
type trajSessions struct {
	*fakeSessions
	info  session.Info
	setup *session.SetupView
}

func (f *trajSessions) Get(id string) (session.Info, bool) {
	if id != f.info.ID {
		return session.Info{}, false
	}
	return f.info, true
}

func (f *trajSessions) Setup(id string) (session.SetupView, error) {
	if f.setup == nil || id != f.info.ID {
		return session.SetupView{}, session.ErrNoSetup
	}
	return *f.setup, nil
}

func trajEnv(t *testing.T, created time.Time, text string) (*safetyEnv, *records.Store) {
	t.Helper()
	env := newSafetyEnv(t, nil)
	fs := &trajSessions{fakeSessions: env.sessions,
		info: session.Info{ID: "s1", Agent: "opencode", Task: "Fix it", State: session.StatePassed, Created: created},
		setup: &session.SetupView{Hash: "0123456789ab", Recorded: "0123456789ab", Verified: true,
			Setup: session.Setup{Agent: session.SetupAgent{Name: "opencode", Version: "opencode 1.0"}, Workspace: "scratch"}}}
	env.srv.sessions = fs
	var st *records.Store
	if text != "" {
		var err error
		st, err = records.Open(records.Config{Enabled: true, Text: text, Dir: filepath.Join(t.TempDir(), "records")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
	}
	env.srv.records = st
	env.h = env.srv.handler()
	return env, st
}

func TestSessionTrajectoryRoute(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	env, st := trajEnv(t, now.Add(-time.Minute), records.TextUser)
	st.Add(records.Line{Time: now.Add(-50 * time.Second), API: "chat", Model: "qwen", Session: "s1", Messages: 1,
		PromptTokens: 10, CompletionTokens: 3, Calls: []records.Call{{ID: "c1", Name: "read"}},
		Text: &records.Text{Messages: "user: Fix it", Reply: "Reading"}})
	st.Add(records.Line{Time: now.Add(-40 * time.Second), API: "chat", Model: "qwen", Session: "s1", Messages: 3, Results: 1,
		PromptTokens: 20, CompletionTokens: 2, Text: &records.Text{Messages: "user: Fix it\nassistant: Reading", Reply: "Done"}})
	st.Add(records.Line{Time: now.Add(-45 * time.Second), API: "chat", Model: "other", Session: "s2", Messages: 1})

	w := env.do("GET", "/api/sessions/s1/trajectory", "", withBearer(testToken))
	if w.Code != http.StatusOK {
		t.Fatalf("atif: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "cerberus-s1.atif.json") {
		t.Fatalf("disposition %q", w.Header().Get("Content-Disposition"))
	}
	var tr trajectory.Trajectory
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.SchemaVersion != trajectory.SchemaVersion || tr.SessionID != "s1" || len(tr.Steps) != 3 ||
		tr.Agent.Version != "opencode 1.0" || tr.Agent.Extra["setup_hash"] != "0123456789ab" {
		t.Fatalf("trajectory %s", w.Body)
	}
	if o := tr.Steps[1].Observation; o == nil || o.Results[0].SourceCallID != "c1" {
		t.Fatalf("observation %+v", tr.Steps[1])
	}
	// The read of text is audited.
	found := false
	for _, rec := range env.records(t) {
		if rec.Route == "GET /api/sessions/{id}/trajectory" && strings.Contains(rec.Detail, "2 request record(s) with text") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit record: %+v", env.records(t))
	}

	w = env.do("GET", "/api/sessions/s1/trajectory?format=jsonl", "", withBearer(testToken))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("jsonl: %d %s", w.Code, w.Body)
	}
	got := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(got) != 2 || !strings.Contains(got[0], `"Reading"`) || strings.Contains(w.Body.String(), `"s2"`) {
		t.Fatalf("jsonl lines %q", got)
	}
	var l records.Line
	if json.Unmarshal([]byte(got[1]), &l) != nil || l.Session != "s1" || l.Text.Reply != "Done" {
		t.Fatalf("jsonl line %q", got[1])
	}

	if w := env.do("GET", "/api/sessions/s1/trajectory?format=xml", "", withBearer(testToken)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad format: %d", w.Code)
	}
	if w := env.do("GET", "/api/sessions/nope/trajectory", "", withBearer(testToken)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", w.Code)
	}
}

func conflictReason(t *testing.T, env *safetyEnv, target string) string {
	t.Helper()
	w := env.do("GET", target, "", withBearer(testToken))
	if w.Code != http.StatusConflict {
		t.Fatalf("%s: %d %s, want 409", target, w.Code, w.Body)
	}
	var e struct{ Error, Reason string }
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error == "" {
		t.Fatalf("no message: %s", w.Body)
	}
	return e.Reason
}

func TestSessionTrajectoryConflicts(t *testing.T) {
	now := time.Now().UTC()
	// Records off.
	env, _ := trajEnv(t, now, "")
	for _, f := range []string{"atif", "jsonl"} {
		if r := conflictReason(t, env, "/api/sessions/s1/trajectory?format="+f); r != "records_off" {
			t.Fatalf("%s off: %q", f, r)
		}
	}
	// The session started before the oldest kept day, and nothing of it is left.
	env, st := trajEnv(t, now.AddDate(0, 0, -30), records.TextNone)
	st.Add(records.Line{Time: now, Session: "s2", Model: "m"})
	for _, f := range []string{"atif", "jsonl"} {
		if r := conflictReason(t, env, "/api/sessions/s1/trajectory?format="+f); r != "swept" {
			t.Fatalf("%s swept: %q", f, r)
		}
	}
	// A recent session with no records at all.
	env, _ = trajEnv(t, now, records.TextNone)
	if r := conflictReason(t, env, "/api/sessions/s1/trajectory"); r != "none" {
		t.Fatalf("none: %q", r)
	}
	// Partly swept: what is left is exported, and says so; text none is not audited.
	env, st = trajEnv(t, now.AddDate(0, 0, -30), records.TextNone)
	st.Add(records.Line{Time: now, Session: "s1", Model: "m", Messages: 1})
	w := env.do("GET", "/api/sessions/s1/trajectory", "", withBearer(testToken))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "first model calls may be missing") {
		t.Fatalf("partial: %d %s", w.Code, w.Body)
	}
	for _, rec := range env.records(t) {
		if rec.Route == "GET /api/sessions/{id}/trajectory" {
			t.Fatalf("an export without text was audited: %+v", rec)
		}
	}
}
