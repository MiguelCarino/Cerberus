package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/session"
)

// richSessions adds what the flow host reads after a session ends: the
// diff, the last reply, the transcript and the vault's masking.
type richSessions struct {
	*hostSessions
	diff, reply string
	transcript  []byte
}

func (r *richSessions) Diff(id string) (string, error)       { return r.diff, nil }
func (r *richSessions) LastReply(id string) (string, error)  { return r.reply, nil }
func (r *richSessions) Transcript(id string) ([]byte, error) { return r.transcript, nil }
func (r *richSessions) MaskSecrets(strs []string) []string {
	for i, s := range strs {
		strs[i] = strings.ReplaceAll(s, "hunter2", "[secret]")
	}
	return strs
}

func TestFlowHostSessionResult(t *testing.T) {
	rs := &richSessions{hostSessions: newHostSessions()}
	cfg := config.Defaults()
	host := NewFlowHost(FlowHostDeps{Config: func() *config.Config { return cfg }, Sessions: rs})
	sr := host.Options(t.TempDir()).Sessions
	ctx := context.Background()

	// scratch: every file (not .git), the checks tail, the last reply masked
	ws := t.TempDir()
	for rel, body := range map[string]string{"index.html": "<p>hi</p>", "css/site.css": "p{}", ".git/HEAD": "x"} {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rs.reply = "Done. The password is hunter2."
	rs.set(session.Info{ID: "s1", State: session.StatePassed, Workspace: session.WorkspaceScratch, Worktree: ws,
		ChecksOutput: strings.Repeat("x", 6000) + "\nok all", Activity: "done"}, true)
	r, err := sr.Wait(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Workspace != ws || r.Final != "Done. The password is [secret]." || r.Summary != "done" {
		t.Fatalf("result %+v", r)
	}
	if len(r.Files) != 2 || r.Files[0].Path != "css/site.css" || r.Files[1].Path != "index.html" || r.Files[1].Bytes != 9 {
		t.Fatalf("files %+v", r.Files)
	}
	if len(r.Checks) > flowChecksTail+10 || !strings.HasSuffix(r.Checks, "ok all") || !strings.HasPrefix(r.Checks, "…") {
		t.Fatalf("checks %d bytes: %q…", len(r.Checks), r.Checks[:20])
	}

	// no reply: the transcript's tail, masked
	rs.reply, rs.transcript = "", []byte(strings.Repeat("line\n", 5000)+"token hunter2 at the end\n")
	r, _ = sr.Wait(ctx, "s1")
	if !strings.HasSuffix(r.Final, "token [secret] at the end") || len(r.Final) > flowFinalMax+10 {
		t.Fatalf("final from transcript %d bytes: …%q", len(r.Final), r.Final[max(0, len(r.Final)-40):])
	}

	// folder: only files changed since the run started
	old := filepath.Join(ws, "index.html")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	rs.set(session.Info{ID: "s2", State: session.StateFailed, Workspace: session.WorkspaceFolder, Dir: ws, Worktree: ws,
		RunStarted: time.Now().Add(-time.Minute)}, true)
	r, _ = sr.Wait(ctx, "s2")
	if len(r.Files) != 1 || r.Files[0].Path != "css/site.css" {
		t.Fatalf("folder files %+v", r.Files)
	}

	// repo: the diff summary and the changed paths
	rs.diff = "diff --git a/index.html b/index.html\n--- a/index.html\n+++ b/index.html\n@@ -1 +1,2 @@\n-old\n+new\n+more\n" +
		"diff --git a/gone.txt b/gone.txt\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n"
	rs.set(session.Info{ID: "s3", State: session.StatePassed, Workspace: session.WorkspaceRepo, Worktree: ws}, true)
	r, _ = sr.Wait(ctx, "s3")
	want := "index.html | +2 -1\ngone.txt | +0 -1\n2 files changed, 2 insertions(+), 2 deletions(-)"
	if r.DiffStat != want || len(r.Files) != 2 || r.Files[0].Bytes != 9 || r.Files[1].Bytes != 0 {
		t.Fatalf("repo result %q %+v", r.DiffStat, r.Files)
	}

	// accepted: the workspace is gone, nothing more is read
	rs.set(session.Info{ID: "s4", State: session.StateAccepted, Workspace: session.WorkspaceRepo, Worktree: ws}, true)
	if r, _ = sr.Wait(ctx, "s4"); r.Workspace != "" || r.DiffStat != "" || r.Final != "" {
		t.Fatalf("accepted %+v", r)
	}
}

// Workspace "step" reaches the session manager as SameAs (workspace
// "same") or as Seeds on a scratch session.
func TestFlowHostStepWorkspace(t *testing.T) {
	hs := newHostSessions()
	cfg := config.Defaults()
	host := NewFlowHost(FlowHostDeps{Config: func() *config.Config { return cfg }, Sessions: hs})
	sr := host.Options(t.TempDir()).Sessions
	ctx := context.Background()
	if _, err := sr.Start(ctx, flows.SessionRequest{Task: "x", Workspace: flows.WorkspaceStep, SameAs: "s-a"}); err != nil {
		t.Fatal(err)
	}
	if got := hs.created[0]; got.Workspace != session.WorkspaceSame || got.SameAs != "s-a" || got.Parent != "" {
		t.Fatalf("same-as spec %+v", got)
	}
	seeds := []flows.SessionSeed{{Session: "s-b", Into: "parts/1-home"}, {Session: "s-c", Into: "parts/2-part"}}
	if _, err := sr.Start(ctx, flows.SessionRequest{Task: "x", Workspace: flows.WorkspaceStep, Seeds: seeds}); err != nil {
		t.Fatal(err)
	}
	if got := hs.created[1]; got.Workspace != session.WorkspaceScratch || len(got.Seeds) != 2 || got.Seeds[1] != (session.Seed{Session: "s-c", Into: "parts/2-part"}) {
		t.Fatalf("seeded spec %+v", got)
	}
	if _, err := sr.Start(ctx, flows.SessionRequest{Task: "x", Workspace: flows.WorkspaceStep}); err == nil {
		t.Fatal("a step workspace without a source was accepted")
	}
}
