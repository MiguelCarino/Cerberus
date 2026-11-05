package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// ---------- fakes ----------

type fakeRuns struct{ dir string } // <dir>/<session>/<attempt>.cast

func (f fakeRuns) Runs(id string) ([]session.RunCast, error) {
	ents, err := os.ReadDir(filepath.Join(f.dir, id))
	if err != nil {
		return nil, session.ErrNotFound
	}
	var out []session.RunCast
	for _, e := range ents {
		if n, ok := session.ParseRunName(e.Name()); ok {
			st, _ := e.Info()
			out = append(out, session.RunCast{Attempt: n, Size: st.Size(), Modified: st.ModTime()})
		}
	}
	return out, nil
}

func (f fakeRuns) OpenRun(id string, attempt int) (*os.File, error) {
	if _, err := os.Stat(filepath.Join(f.dir, id)); err != nil {
		return nil, session.ErrNotFound
	}
	return os.Open(filepath.Join(f.dir, id, fmt.Sprintf("%d.cast", attempt)))
}

type fakeDownloads struct {
	mu    sync.Mutex
	items []downloads.Item
	err   error
	reqs  []downloads.Request
}

func (f *fakeDownloads) Add(req downloads.Request) (downloads.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return downloads.Item{}, f.err
	}
	f.reqs = append(f.reqs, req)
	it := downloads.Item{ID: fmt.Sprintf("dl-%d", len(f.reqs)), Engine: req.Engine, Ref: req.Ref, Name: req.Ref,
		State: downloads.StateQueued, ETASec: -1}
	f.items = append([]downloads.Item{it}, f.items...)
	return it, nil
}

func (f *fakeDownloads) Cancel(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		if it.ID == id {
			return nil
		}
	}
	return downloads.ErrNotFound
}

func (f *fakeDownloads) List() []downloads.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]downloads.Item(nil), f.items...)
}

func newInbox(t *testing.T) *inbox.Store {
	t.Helper()
	ib, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ib.Close() })
	return ib
}

func decodeAs[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return v
}

var (
	adminTok = withBearer(testToken)
	csrfH    = withHeader(csrfHeader, "1")
)

// ---------- inbox ----------

func TestInboxEndpoints(t *testing.T) {
	env := newSafetyEnv(t, nil)
	ib := newInbox(t)
	env.srv.inbox = ib
	a := ib.Add(inbox.Item{Kind: "session", Title: "first", Link: "#/s/s1"})
	b := ib.Add(inbox.Item{Kind: "download", Level: "warn", Title: "second"})
	c := ib.Add(inbox.Item{Kind: "chat", Title: "third"})

	type list struct {
		Items  []inbox.Item `json:"items"`
		Unread int          `json:"unread"`
	}
	w := env.do("GET", "/api/inbox", "", adminTok)
	l := decodeAs[list](t, w)
	if w.Code != 200 || len(l.Items) != 3 || l.Items[0].ID != c.ID || l.Unread != 3 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if l := decodeAs[list](t, env.do("GET", "/api/inbox?limit=1", "", adminTok)); len(l.Items) != 1 || l.Items[0].ID != c.ID {
		t.Fatalf("limit: %+v", l)
	}
	for _, bad := range []string{"limit=0", "limit=x"} {
		if w := env.do("GET", "/api/inbox?"+bad, "", adminTok); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	if w := env.do("GET", "/api/inbox", ""); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}

	// Mark one read; the cookie needs the CSRF header.
	body := `{"ids":["` + a.ID + `","nope"]}`
	if w := env.do("POST", "/api/inbox/read", body, withCookie(testToken)); w.Code != 403 {
		t.Fatalf("cookie without csrf: %d", w.Code)
	}
	w = env.do("POST", "/api/inbox/read", body, withCookie(testToken), csrfH)
	if r := decodeAs[map[string]int](t, w); w.Code != 200 || r["changed"] != 1 || r["unread"] != 2 {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	if l := decodeAs[list](t, env.do("GET", "/api/inbox?unread=1", "", adminTok)); len(l.Items) != 2 || l.Items[1].ID != b.ID {
		t.Fatalf("unread only: %+v", l)
	}
	if w := env.do("POST", "/api/inbox/read", `{}`, adminTok); w.Code != 400 {
		t.Fatalf("empty read: %d", w.Code)
	}
	if w := env.do("POST", "/api/inbox/read", `{"ids":`, adminTok); w.Code != 400 {
		t.Fatalf("bad json: %d", w.Code)
	}
	w = env.do("POST", "/api/inbox/read", `{"all":true}`, adminTok)
	if r := decodeAs[map[string]int](t, w); r["changed"] != 2 || r["unread"] != 0 {
		t.Fatalf("read all: %s", w.Body)
	}

	// Delete.
	if w := env.do("DELETE", "/api/inbox/"+b.ID, "", adminTok); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := env.do("DELETE", "/api/inbox/"+b.ID, "", adminTok); w.Code != 404 {
		t.Fatalf("delete again: %d", w.Code)
	}
	if got := ib.List(false, 0); len(got) != 2 {
		t.Fatalf("after delete: %+v", got)
	}

	// Every write is audited with its route.
	routes := map[string]int{}
	for _, r := range env.records(t) {
		routes[r.Route]++
	}
	if routes["POST /api/inbox/read"] < 3 || routes["DELETE /api/inbox/{id}"] != 2 {
		t.Fatalf("audit: %v", routes)
	}
}

func TestInboxUnavailable(t *testing.T) {
	env := newEnv(t, nil)
	for _, c := range [][2]string{{"GET", "/api/inbox"}, {"POST", "/api/inbox/read"}, {"DELETE", "/api/inbox/x"}} {
		if w := env.do(c[0], c[1], `{"all":true}`, adminTok); w.Code != 503 {
			t.Fatalf("%v: %d", c, w.Code)
		}
	}
}

// nextNamed skips pings and returns the next frame with a name.
func nextNamed(t *testing.T, sse sseReader) (string, string) {
	t.Helper()
	for {
		ev, data := sse.next(t)
		if ev != "" {
			return ev, data
		}
	}
}

func TestEventsCarryInboxAndDownloads(t *testing.T) {
	env := newEnv(t, nil)
	ib := newInbox(t)
	env.srv.inbox = ib
	env.srv.dlFeed = NewDownloadFeed()
	env.h = env.srv.handler()
	sse, cancel := openSSE(t, env, "/api/events")
	defer cancel()

	// The stream subscribes before it sends its headers: nothing is missed.
	it := ib.Add(inbox.Item{Kind: "session", Title: "hello"})
	ev, data := nextNamed(t, sse)
	var ie inbox.Event
	if ev != "inbox" || json.Unmarshal([]byte(data), &ie) != nil || ie.Type != "added" || ie.Item == nil ||
		ie.Item.ID != it.ID || ie.Unread != 1 {
		t.Fatalf("inbox frame %q %s", ev, data)
	}
	env.srv.dlFeed.Publish(downloads.Item{ID: "dl-1", State: downloads.StateRunning, Bytes: 10, Total: 100})
	ev, data = nextNamed(t, sse)
	var di downloads.Item
	if ev != "download" || json.Unmarshal([]byte(data), &di) != nil || di.ID != "dl-1" || di.Total != 100 {
		t.Fatalf("download frame %q %s", ev, data)
	}
	if _, err := ib.MarkRead(nil, true); err != nil {
		t.Fatal(err)
	}
	if ev, data = nextNamed(t, sse); ev != "inbox" || !strings.Contains(data, `"Type":"read"`) {
		t.Fatalf("read frame %q %s", ev, data)
	}
	env.sessions.events <- session.Event{Type: "state", Session: session.Info{ID: "s1"}}
	if ev, _ = nextNamed(t, sse); ev != "session" {
		t.Fatalf("session frame %q", ev)
	}
}

// seqInbox hands out prepared subscription channels in order.
type seqInbox struct {
	inboxService
	mu   sync.Mutex
	subs []chan inbox.Event
	n    int
}

func (f *seqInbox) Subscribe() (<-chan inbox.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.subs[min(f.n, len(f.subs)-1)]
	f.n++
	return ch, func() {}
}

func (f *seqInbox) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func TestEventsResyncWhenBehind(t *testing.T) {
	env := newEnv(t, nil)
	// 1st subscription: dropped for falling behind (its buffer, then closed).
	// 2nd: live. 3rd: closed empty, as when the inbox shuts down.
	dropped, live, shut := make(chan inbox.Event, 1), make(chan inbox.Event, 1), make(chan inbox.Event)
	dropped <- inbox.Event{Type: "added", Unread: 1}
	close(dropped)
	close(shut)
	fi := &seqInbox{subs: []chan inbox.Event{dropped, live, shut}}
	env.srv.inbox = fi
	env.h = env.srv.handler()
	sse, cancel := openSSE(t, env, "/api/events")
	defer cancel()

	if ev, _ := nextNamed(t, sse); ev != "inbox" {
		t.Fatalf("buffered frame %q", ev)
	}
	if ev, data := nextNamed(t, sse); ev != "resync" || data != `{"what":"inbox"}` {
		t.Fatalf("resync %q %s", ev, data)
	}
	live <- inbox.Event{Type: "deleted", IDs: []string{"x"}}
	if ev, data := nextNamed(t, sse); ev != "inbox" || !strings.Contains(data, `"deleted"`) {
		t.Fatalf("after resync %q %s", ev, data)
	}
	close(live) // dropped again: resubscribes and gets the closed, empty one
	if ev, _ := nextNamed(t, sse); ev != "resync" {
		t.Fatalf("second resync %q", ev)
	}
	// The empty closed channel stops inbox listening, not the stream.
	env.sessions.events <- session.Event{Type: "state", Session: session.Info{ID: "s1"}}
	if ev, _ := nextNamed(t, sse); ev != "session" {
		t.Fatalf("session frame %q", ev)
	}
	if n := fi.count(); n != 3 {
		t.Fatalf("subscriptions %d", n)
	}
}

func TestDownloadFeedDropsSlowSubscriber(t *testing.T) {
	f := NewDownloadFeed()
	ch, cancel := f.Subscribe()
	defer cancel()
	for range feedBuffer + 1 {
		f.Publish(downloads.Item{ID: "x"})
	}
	n := 0
	for range ch {
		n++
	}
	if n != feedBuffer {
		t.Fatalf("got %d before close", n)
	}
	cancel() // after a drop: no double close
	var nilFeed *DownloadFeed
	nilFeed.Publish(downloads.Item{}) // a nil feed is a no-op
}

func TestEventsUnavailableWithoutSources(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.sessions = nil
	if w := env.do("GET", "/api/events", "", adminTok); w.Code != 503 {
		t.Fatalf("events: %d", w.Code)
	}
}

// ---------- recordings ----------

func castFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range []string{"s1", "s2"} {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cast := `{"version":2,"width":80,"height":24,"timestamp":1,"title":"s1"}` + "\n" + `[0.5,"o","hi"]` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s1", "1.cast"), []byte(cast), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s2", "1.cast"), []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunEndpoints(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.runs = fakeRuns{dir: castFixture(t)}

	w := env.do("GET", "/api/sessions/s1/runs", "", adminTok)
	runs := decodeAs[[]session.RunCast](t, w)
	if w.Code != 200 || len(runs) != 1 || runs[0].Attempt != 1 || runs[0].Size == 0 {
		t.Fatalf("runs: %d %s", w.Code, w.Body)
	}
	w = env.do("GET", "/api/sessions/s1/runs/1.cast", "", adminTok)
	if w.Code != 200 || w.Header().Get("Content-Type") != castContentType || !strings.HasPrefix(w.Body.String(), `{"version":2`) ||
		w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Disposition"), `s1-1.cast`) {
		t.Fatalf("cast: %d %v %q", w.Code, w.Header(), w.Body)
	}
	// Range works, for a player that reads the header first.
	w = env.do("GET", "/api/sessions/s1/runs/1.cast", "", adminTok, withHeader("Range", "bytes=0-9"))
	if w.Code != 206 || w.Body.String() != `{"version"` {
		t.Fatalf("range: %d %q", w.Code, w.Body)
	}
	for target, want := range map[string]int{
		"/api/sessions/s1/runs/2.cast":   404,
		"/api/sessions/s1/runs/0.cast":   404,
		"/api/sessions/s1/runs/1.txt":    404,
		"/api/sessions/s1/runs/..%2Fx":   404,
		"/api/sessions/none/runs":        404,
		"/api/sessions/none/runs/1.cast": 404,
	} {
		if w := env.do("GET", target, "", adminTok); w.Code != want {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body)
		}
	}
	if w := env.do("GET", "/api/sessions/s1/runs", ""); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
	env.srv.runs = nil
	if w := env.do("GET", "/api/sessions/s1/runs", "", adminTok); w.Code != 503 {
		t.Fatalf("no recordings: %d", w.Code)
	}
}

func TestAgentRunsOwnSessionOnly(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.runs = fakeRuns{dir: castFixture(t)}
	agent := []reqOpt{withBearer(env.srv.cfg.SessionAgentToken("s1")), withHeader(sessionHeader, "s1")}

	w := env.do("GET", "/api/agent/runs", "", agent...)
	if runs := decodeAs[[]session.RunCast](t, w); w.Code != 200 || len(runs) != 1 {
		t.Fatalf("agent runs: %d %s", w.Code, w.Body)
	}
	if w := env.do("GET", "/api/agent/runs/1.cast", "", agent...); w.Code != 200 || !strings.Contains(w.Body.String(), `"hi"`) {
		t.Fatalf("agent cast: %d %q", w.Code, w.Body)
	}
	// Another session's recordings are out of reach: the person routes
	// refuse agent tokens, and the agent scope pins the caller.
	if w := env.do("GET", "/api/sessions/s2/runs/1.cast", "", agent...); w.Code != 401 {
		t.Fatalf("agent on person route: %d", w.Code)
	}
	if w := env.do("GET", "/api/agent/runs/1.cast", "", withBearer(env.srv.cfg.SessionAgentToken("s1")), withHeader(sessionHeader, "s2")); w.Code != 403 {
		t.Fatalf("agent naming another session: %d", w.Code)
	}
	if w := env.do("GET", "/api/agent/runs", "", adminTok); w.Code != 403 {
		t.Fatalf("adminTok in agent scope: %d", w.Code)
	}
}

// ---------- downloads ----------

func TestDownloadEndpoints(t *testing.T) {
	env := newSafetyEnv(t, nil)
	fd := &fakeDownloads{}
	env.srv.downloads = fd

	w := env.do("GET", "/api/downloads", "", adminTok)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty list: %d %s", w.Code, w.Body)
	}
	body := `{"engine":"ollama","ref":"qwen3:8b"}`
	if w := env.do("POST", "/api/downloads", body, withCookie(testToken)); w.Code != 403 {
		t.Fatalf("cookie without csrf: %d", w.Code)
	}
	w = env.do("POST", "/api/downloads", body, withCookie(testToken), csrfH)
	it := decodeAs[downloads.Item](t, w)
	if w.Code != 202 || it.ID != "dl-1" || it.Engine != "ollama" || fd.reqs[0].Ref != "qwen3:8b" {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if w := env.do("POST", "/api/downloads", `{"ref":"  "}`, adminTok); w.Code != 400 {
		t.Fatalf("empty ref: %d", w.Code)
	}
	for err, want := range map[error]int{
		fmt.Errorf("%w: x", downloads.ErrBadRef):      400,
		fmt.Errorf("%w: x", downloads.ErrUnavailable): 409,
		downloads.ErrClosed:                           503,
	} {
		fd.err = err
		if w := env.do("POST", "/api/downloads", body, adminTok); w.Code != want {
			t.Fatalf("%v: %d", err, w.Code)
		}
	}
	fd.err = nil
	if l := decodeAs[[]downloads.Item](t, env.do("GET", "/api/downloads", "", adminTok)); len(l) != 1 {
		t.Fatalf("list: %+v", l)
	}
	if w := env.do("DELETE", "/api/downloads/dl-1", "", adminTok); w.Code != 200 {
		t.Fatalf("cancel: %d", w.Code)
	}
	if w := env.do("DELETE", "/api/downloads/nope", "", adminTok); w.Code != 404 {
		t.Fatalf("cancel unknown: %d", w.Code)
	}
	if w := env.do("GET", "/api/downloads", ""); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
	var sawAdd bool
	for _, r := range env.records(t) {
		if r.Route == "POST /api/downloads" && r.Status == 202 {
			sawAdd = r.Detail == "ollama qwen3:8b" && len(r.Targets) == 1 && r.Targets[0] == "dl-1"
		}
	}
	if !sawAdd {
		t.Fatalf("audit: %+v", env.records(t))
	}
	env.srv.downloads = nil
	if w := env.do("GET", "/api/downloads", "", adminTok); w.Code != 503 {
		t.Fatalf("unavailable: %d", w.Code)
	}
}

// TestDownloadThroughEventStream runs the real manager against a fake
// Ollama: POST queues it, and the event stream reports it to the end.
func TestDownloadThroughEventStream(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, `{"status":"pulling manifest"}`)
		fmt.Fprintln(w, `{"status":"downloading","digest":"sha256:a","total":100,"completed":50}`)
		fmt.Fprintln(w, `{"status":"downloading","digest":"sha256:a","total":100,"completed":100}`)
		fmt.Fprintln(w, `{"status":"success"}`)
	}))
	defer ollama.Close()
	env := newEnv(t, func(c *config.Config) {
		c.Engines.Ollama.Enabled = true
		c.Engines.Ollama.URL = ollama.URL
		c.Engines.LMStudio.Enabled = false
	})
	feed := NewDownloadFeed()
	var done sync.WaitGroup
	done.Add(1)
	m := downloads.New(downloads.Options{Config: env.srv.conf, OnChange: feed.Publish,
		OnDone: func(downloads.Item) { done.Done() }, LMSBinary: func() string { return "" }})
	defer m.Close()
	env.srv.downloads, env.srv.dlFeed = m, feed
	env.h = env.srv.handler()
	sse, cancel := openSSE(t, env, "/api/events")
	defer cancel()

	w := env.do("POST", "/api/downloads", `{"ref":"ollama:tiny:1b"}`, adminTok)
	if w.Code != 202 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	id := decodeAs[downloads.Item](t, w).ID
	for {
		ev, data := nextNamed(t, sse)
		if ev != "download" {
			continue
		}
		var it downloads.Item
		if err := json.Unmarshal([]byte(data), &it); err != nil || it.ID != id {
			t.Fatalf("frame %s", data)
		}
		if it.Final() {
			if it.State != downloads.StateDone || it.Total != 100 {
				t.Fatalf("final %+v", it)
			}
			break
		}
	}
	done.Wait()
}

// ---------- leaderboard ----------

func TestLeaderboardEndpoint(t *testing.T) {
	env := newEnv(t, nil)
	u, err := usage.Open(filepath.Join(t.TempDir(), "usage.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	now := time.Now()
	u.Add(usage.Record{Time: now.Add(-time.Minute), Model: "big", SessionID: "s1", PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.5})
	u.Add(usage.Record{Time: now.Add(-time.Minute), Model: "small", SessionID: "s2", PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.1})
	u.Add(usage.Record{Time: now.Add(-time.Minute), Model: "small", PromptTokens: 1, CompletionTokens: 1, CostUSD: 0.01})
	infos := []session.Info{
		{ID: "s1", Name: "fix bug", Agent: "claude", Repo: "alpha", State: session.StateRunning, Created: now.Add(-time.Hour)},
		{ID: "s2", Title: "docs", Agent: "codex", Dir: "/x/beta", Workspace: "repo", State: session.StatePassed,
			Created: now.Add(-2 * time.Hour), Updated: now.Add(-time.Hour)},
	}
	env.srv.board = newBoard(u, func() []session.Info { return infos }, nil)

	w := env.do("GET", "/api/leaderboard?period=today&by=session&metric=cost", "", adminTok)
	b := decodeAs[usage.Board](t, w)
	if w.Code != 200 || len(b.Rows) != 3 || b.Rows[0].Key != "s1" || b.Rows[0].Label != "fix bug" || b.Rows[1].Label != "docs" ||
		b.Rows[2].Label != usage.LabelDirect || b.CostCurrency != "USD" {
		t.Fatalf("by session: %d %s", w.Code, w.Body)
	}
	b = decodeAs[usage.Board](t, env.do("GET", "/api/leaderboard?period=30d&by=repo&metric=tokens", "", adminTok))
	if len(b.Rows) < 2 || b.Rows[0].Key != "alpha" || b.Rows[0].Tokens != 150 || b.Rows[1].Key != "beta" {
		t.Fatalf("by repo: %+v", b.Rows)
	}
	b = decodeAs[usage.Board](t, env.do("GET", "/api/leaderboard?by=model&limit=1", "", adminTok))
	if b.Period != "7d" || b.Metric != "cost" || len(b.Rows) != 1 || b.Rows[0].Key != "big" {
		t.Fatalf("defaults + limit: %+v", b)
	}
	for _, q := range []string{"period=year", "by=user", "metric=joy", "limit=0", "limit=-3"} {
		if w := env.do("GET", "/api/leaderboard?"+q, "", adminTok); w.Code != 400 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	if w := env.do("GET", "/api/leaderboard", ""); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
	env.srv.board = nil
	if w := env.do("GET", "/api/leaderboard", "", adminTok); w.Code != 503 {
		t.Fatalf("no usage: %d", w.Code)
	}
}

func TestBoardSessions(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	got := boardSessions([]session.Info{
		{ID: "a", Title: "t", State: session.StateRunning, Created: now.Add(-time.Hour), Updated: now.Add(-time.Minute), EnergyJ: 7},
		{ID: "b", Name: "n", Title: "t", State: session.StateStopped, Attempt: 1, Created: now.Add(-time.Hour), Updated: now.Add(-time.Minute)},
		{ID: "c", Workspace: session.WorkspaceScratch, Dir: "/tmp/x"},
	}, now)
	if got[0].Label != "t" || !got[0].End.IsZero() || got[0].WallJ != 7 {
		t.Fatalf("running: %+v", got[0])
	}
	if got[1].Label != "n" || !got[1].End.Equal(now.Add(-time.Minute)) {
		t.Fatalf("stopped: %+v", got[1])
	}
	if got[2].Repo != "" || !got[2].Start.Equal(now) {
		t.Fatalf("scratch: %+v", got[2])
	}
}

// TestBoardSessionsRunWindow: the life is the run window, not Created to
// Updated: a session accepted two days after its 10-minute run, or
// restarted later, is charged 10 minutes; queue time is not counted.
func TestBoardSessionsRunWindow(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	mon := now.Add(-48 * time.Hour)
	got := boardSessions([]session.Info{
		{ID: "acc", State: session.StateAccepted, Attempt: 1, Created: mon.Add(-time.Hour), Updated: now.Add(-time.Minute),
			RunStarted: mon, RunEnded: mon.Add(10 * time.Minute)},
		{ID: "q", State: session.StateQueued, Created: now.Add(-time.Hour), Updated: now.Add(-time.Hour)},
		{ID: "never", State: session.StateStopped, Created: mon, Updated: now},
		{ID: "retry", State: session.StateRunning, Attempt: 2, Created: mon, Updated: now,
			RunStarted: mon.Add(time.Hour), RunEnded: mon.Add(2 * time.Hour)},
	}, now)
	if !got[0].Start.Equal(mon) || got[0].End.Sub(got[0].Start) != 10*time.Minute {
		t.Fatalf("accepted later: %+v", got[0])
	}
	if !got[1].Start.Equal(now) || !got[1].End.IsZero() {
		t.Fatalf("queued, never ran: %+v", got[1])
	}
	if !got[2].Start.Equal(mon) || !got[2].End.Equal(mon) {
		t.Fatalf("stopped before running: %+v", got[2])
	}
	if !got[3].Start.Equal(mon.Add(time.Hour)) || !got[3].End.IsZero() {
		t.Fatalf("running again: %+v", got[3])
	}
	b, err := usage.Leaderboard(usage.Sources{Sessions: got[:1], Now: now}, "30d", "session", "time")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rows) != 1 || b.Rows[0].WallSec != 600 {
		t.Fatalf("wall time: %+v", b.Rows)
	}
}

// ---------- PWA ----------

func pwaEnv(t *testing.T, files fstest.MapFS) *testEnv {
	t.Helper()
	env := newEnv(t, nil)
	if files != nil {
		files["dist/index.html"] = &fstest.MapFile{Data: []byte("<!doctype html>")}
		env.srv.loadUI(files)
	}
	env.h = env.srv.handler()
	return env
}

func TestPWAFiles(t *testing.T) {
	env := pwaEnv(t, fstest.MapFS{
		"dist/manifest.webmanifest": {Data: []byte(`{"name":"Cerberus"}`)},
		"dist/sw.js":                {Data: []byte(`self.addEventListener("fetch", () => {})`)},
	})
	// Public: a browser fetches the manifest without credentials.
	w := env.do("GET", "/manifest.webmanifest", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" || w.Body.String() != `{"name":"Cerberus"}` ||
		w.Header().Get("Service-Worker-Allowed") != "" {
		t.Fatalf("manifest: %d %v %q", w.Code, w.Header(), w.Body)
	}
	w = env.do("GET", "/sw.js", "")
	h := w.Header()
	if w.Code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/javascript") || h.Get("Cache-Control") != "no-cache" ||
		h.Get("Service-Worker-Allowed") != "/" || h.Get("ETag") == "" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("sw: %d %v", w.Code, h)
	}
	if w := env.do("GET", "/sw.js", "", withHeader("If-None-Match", h.Get("ETag"))); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("revalidate: %d", w.Code)
	}
	if w := env.do("HEAD", "/sw.js", ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("head: %d %d", w.Code, w.Body.Len())
	}
	// Nothing else at the root is served: the public 404 page (Phase 4a).
	if w := env.do("GET", "/sw.js.map", ""); w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("other root file: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestPWAFilesMissing(t *testing.T) {
	env := pwaEnv(t, fstest.MapFS{})
	for _, p := range []string{"/manifest.webmanifest", "/sw.js"} {
		if w := env.do("GET", p, ""); w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
}

// TestPWAFilesEmbedded checks the real web/dist: when the build ships the
// files, they are served with their types.
func TestPWAFilesEmbedded(t *testing.T) {
	env := pwaEnv(t, nil)
	for name, ctype := range pwaFiles {
		if _, ok := env.srv.pwa[name]; !ok {
			t.Logf("%s not in web/dist yet", name)
			continue
		}
		if w := env.do("GET", "/"+name, ""); w.Code != 200 || w.Header().Get("Content-Type") != ctype {
			t.Fatalf("%s: %d %q", name, w.Code, w.Header().Get("Content-Type"))
		}
	}
}
