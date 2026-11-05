package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/chat"
	"carino.systems/cerberus/internal/files"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// ownedSessions is a session service with owners: List and Get over a
// map, Create records the spec, Subscribe fans out to every subscriber.
type ownedSessions struct {
	*fakeSessions
	mu    sync.Mutex
	infos map[string]session.Info
	specs []session.TaskSpec
	subs  map[chan session.Event]bool
}

func newOwnedSessions() *ownedSessions {
	return &ownedSessions{fakeSessions: newFakeSessions(), infos: map[string]session.Info{}, subs: map[chan session.Event]bool{}}
}

func (o *ownedSessions) add(id, owner string) {
	o.mu.Lock()
	o.infos[id] = session.Info{ID: id, Owner: owner, State: session.StateRunning, Workspace: session.WorkspaceScratch}
	o.mu.Unlock()
}

func (o *ownedSessions) List() []session.Info {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []session.Info
	for _, in := range o.infos {
		out = append(out, in)
	}
	return out
}

func (o *ownedSessions) Get(id string) (session.Info, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	in, ok := o.infos[id]
	return in, ok
}

func (o *ownedSessions) Create(ctx context.Context, t session.TaskSpec) ([]session.Info, error) {
	o.mu.Lock()
	o.specs = append(o.specs, t)
	o.mu.Unlock()
	return []session.Info{{ID: "new", Owner: t.Owner, Confined: t.Confined}}, nil
}

func (o *ownedSessions) Stop(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	in, ok := o.infos[id]
	if !ok {
		return session.ErrNotFound
	}
	in.State = session.StateStopped
	o.infos[id] = in
	return nil
}

func (o *ownedSessions) Subscribe() (<-chan session.Event, func()) {
	ch := make(chan session.Event, 16)
	o.mu.Lock()
	o.subs[ch] = true
	o.mu.Unlock()
	return ch, func() {
		o.mu.Lock()
		delete(o.subs, ch)
		o.mu.Unlock()
	}
}

func (o *ownedSessions) emit(ev session.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for ch := range o.subs {
		ch <- ev
	}
}

func (o *ownedSessions) subscribers() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.subs)
}

// usersEnv is a safety env with accounts: ana (admin), bob and cy (user),
// vic (viewer), mo (monitor) and cat (chat), each with a login cookie.
type usersEnv struct {
	*safetyEnv
	st   *users.Store
	ss   *ownedSessions
	u    map[string]users.User
	tok  map[string]string
	ib   *inbox.Store
	used *usage.Store
}

func newUsersEnv(t *testing.T) *usersEnv {
	t.Helper()
	t.Cleanup(users.SetCostForTest(1000))
	env := newSafetyEnv(t, nil)
	st, err := users.Open(env.srv.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	env.srv.users = st
	ss := newOwnedSessions()
	env.srv.sessions, env.srv.control = ss, nil
	ib, err := inbox.Open(filepath.Join(env.srv.dataDir, "inbox.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ib.Close() })
	env.srv.inbox = ib
	used, err := usage.Open(filepath.Join(env.srv.dataDir, "usage.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { used.Close() })
	env.srv.usage, env.srv.spend = used, used
	e := &usersEnv{safetyEnv: env, st: st, ss: ss, u: map[string]users.User{}, tok: map[string]string{}, ib: ib, used: used}
	for _, x := range []struct{ name, role string }{{"ana", "admin"}, {"bob", "user"}, {"cy", "user"},
		{"vic", "viewer"}, {"mo", "monitor"}, {"cat", "chat"}} {
		u, err := st.Add(x.name, "", x.role, "a long password")
		if err != nil {
			t.Fatal(err)
		}
		e.u[x.name] = u
		tok, _, err := st.Logins.Create(u.ID, "192.0.2.10", "test")
		if err != nil {
			t.Fatal(err)
		}
		e.tok[x.name] = tok
	}
	return e
}

// as is a request option set for a signed-in user (cookie + CSRF header).
func (e *usersEnv) as(name string) []reqOpt {
	return []reqOpt{withCookie(e.tok[name]), withHeader(csrfHeader, "1")}
}

func (e *usersEnv) call(name, method, target, body string, extra ...reqOpt) *httptest.ResponseRecorder {
	opts := append(e.as(name), extra...)
	if body != "" {
		opts = append(opts, withHeader("Content-Type", "application/json"))
	}
	return e.do(method, target, body, opts...)
}

func loginReq(body string, extra ...reqOpt) []reqOpt {
	return append([]reqOpt{withHeader(csrfHeader, "1"), withHeader("Content-Type", "application/json")}, extra...)
}

func TestLoginFlow(t *testing.T) {
	e := newUsersEnv(t)
	// /api/auth is public.
	w := e.do("GET", "/api/auth", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"has_users":true`) {
		t.Fatalf("auth: %d %s", w.Code, w.Body)
	}
	// Success: cookie set, Me returned.
	w = e.do("POST", "/api/login", `{"name":"Bob","password":"a long password"}`, loginReq("")...)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var me Me
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if me.Kind != "login" || me.Role != "user" || me.User == nil || me.User.Name != "bob" || me.Home != "#/chat" {
		t.Fatalf("me %+v", me)
	}
	ck := w.Result().Cookies()
	if len(ck) != 1 || !strings.HasPrefix(ck[0].Value, "cls-") || !ck[0].HttpOnly || ck[0].SameSite != http.SameSiteLaxMode || ck[0].MaxAge <= 0 {
		t.Fatalf("cookie %+v", ck)
	}
	cookie := ck[0].Value
	if w := e.do("GET", "/api/auth", "", withCookie(cookie)); !strings.Contains(w.Body.String(), `"signed_in":true`) {
		t.Fatalf("auth with a sign-in: %s", w.Body)
	}
	if w := e.do("GET", "/api/auth", "", withCookie(testToken)); !strings.Contains(w.Body.String(), `"signed_in":false`) {
		t.Fatalf("auth with the token cookie: %s", w.Body)
	}
	if w := e.do("GET", "/api/me", "", withCookie(cookie)); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"bob"`) {
		t.Fatalf("me with cookie: %d %s", w.Code, w.Body)
	}
	// Wrong password, unknown name and disabled user: the same body.
	bodies := map[string]string{}
	for what, body := range map[string]string{
		"wrong":   `{"name":"bob","password":"wrong password"}`,
		"unknown": `{"name":"nobody","password":"a long password"}`,
	} {
		w := e.do("POST", "/api/login", body, loginReq("", func(r *http.Request) { r.RemoteAddr = "192.0.2.50:1" })...)
		if w.Code != 401 {
			t.Fatalf("%s: %d", what, w.Code)
		}
		bodies[what] = w.Body.String()
	}
	if _, err := e.st.SetDisabled(e.u["cy"].ID, true); err != nil {
		t.Fatal(err)
	}
	w = e.do("POST", "/api/login", `{"name":"cy","password":"a long password"}`, loginReq("", func(r *http.Request) { r.RemoteAddr = "192.0.2.51:1" })...)
	bodies["disabled"] = w.Body.String()
	if bodies["wrong"] != bodies["unknown"] || bodies["wrong"] != bodies["disabled"] ||
		!strings.Contains(bodies["wrong"], `"code":"login"`) || !strings.Contains(bodies["wrong"], "Wrong user name or password.") {
		t.Fatalf("bodies differ: %v", bodies)
	}
	// The disabled user's old cookie stops working.
	if w := e.do("GET", "/api/me", "", withCookie(e.tok["cy"])); w.Code != 401 {
		t.Fatalf("disabled cookie: %d", w.Code)
	}
	// CSRF guard: no header, a form body, a cross-site page.
	for name, opts := range map[string][]reqOpt{
		"no header":  {withHeader("Content-Type", "application/json")},
		"form":       {withHeader(csrfHeader, "1"), withHeader("Content-Type", "application/x-www-form-urlencoded")},
		"cross-site": loginReq("", withHeader("Sec-Fetch-Site", "cross-site")),
	} {
		if w := e.do("POST", "/api/login", `{"name":"bob","password":"a long password"}`, opts...); w.Code != 403 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	// Token sign-in.
	w = e.do("POST", "/api/login", `{"token":"`+testToken+`"}`, loginReq("")...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"admin-token"`) {
		t.Fatalf("token login: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/login", `{"token":"cdv-nope"}`, loginReq("", func(r *http.Request) { r.RemoteAddr = "192.0.2.52:1" })...); w.Code != 401 {
		t.Fatalf("bad token: %d", w.Code)
	}
	// Logout ends the sign-in.
	w = e.do("POST", "/api/logout", "", withCookie(cookie), withHeader(csrfHeader, "1"))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "/api/me", "", withCookie(cookie)); w.Code != 401 {
		t.Fatalf("after logout: %d", w.Code)
	}
	// Audit: auth.login, auth.login_failed (with the name), never a password.
	var routes []string
	for _, r := range e.records(t) {
		routes = append(routes, r.Route)
		if strings.Contains(r.Detail, "a long password") || strings.Contains(r.Detail, "wrong password") {
			t.Fatalf("a password in the audit log: %+v", r)
		}
	}
	all := strings.Join(routes, " ")
	for _, want := range []string{"auth.login", "auth.login_failed", "POST /api/logout"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %s record: %s", want, all)
		}
	}
}

func TestLoginLockoutAndLimiter(t *testing.T) {
	e := newUsersEnv(t)
	// Five wrong passwords for one name from five addresses lock the name
	// (the per-address limiter is not what stops them).
	for i := range 5 {
		addr := func(r *http.Request) { r.RemoteAddr = "198.51.100." + string(rune('1'+i)) + ":1" }
		w := e.do("POST", "/api/login", `{"name":"bob","password":"wrong password `+string(rune('a'+i))+`"}`, loginReq("", addr)...)
		if w.Code != 401 {
			t.Fatalf("try %d: %d %s", i+1, w.Code, w.Body)
		}
	}
	w := e.do("POST", "/api/login", `{"name":"bob","password":"a long password"}`, loginReq("", func(r *http.Request) { r.RemoteAddr = "198.51.100.9:1" })...)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"locked"`) || w.Header().Get("Retry-After") == "" ||
		!strings.Contains(w.Body.String(), `"retry_after":900`) || !strings.Contains(w.Body.String(), "15 minutes") {
		t.Fatalf("locked: %d %v %s", w.Code, w.Header(), w.Body)
	}
	// Unknown names lock the same way.
	for i := range 5 {
		e.do("POST", "/api/login", `{"name":"ghost","password":"try `+string(rune('a'+i))+` password"}`, loginReq("", func(r *http.Request) { r.RemoteAddr = "203.0.113." + string(rune('1'+i)) + ":1" })...)
	}
	if w := e.do("POST", "/api/login", `{"name":"ghost","password":"x long password"}`, loginReq("", func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1" })...); w.Code != 429 {
		t.Fatalf("unknown name not locked: %d", w.Code)
	}
	// An admin reset clears the lockout.
	if w := e.call("ana", "POST", "/api/users/"+e.u["bob"].ID+"/password", `{"password":"bobs new password"}`); w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/login", `{"name":"bob","password":"bobs new password"}`, loginReq("", func(r *http.Request) { r.RemoteAddr = "198.51.100.10:1" })...); w.Code != 200 {
		t.Fatalf("after reset: %d %s", w.Code, w.Body)
	}
	// The per-address limiter: 5 bad sign-ins from one address, then 429.
	codes := []int{}
	for i := range 7 {
		w := e.do("POST", "/api/login", `{"name":"name`+string(rune('a'+i))+`","password":"some password"}`, loginReq("", func(r *http.Request) { r.RemoteAddr = "192.0.2.200:1" })...)
		codes = append(codes, w.Code)
	}
	if codes[len(codes)-1] != 429 {
		t.Fatalf("per-address limiter: %v", codes)
	}
}

func TestMustChangeAndPasswordChange(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.u["bob"]
	// A device of bob's and a second browser.
	devTok, _, err := e.devices.PairScoped("bob-phone", "", bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := e.st.Logins.Create(bob.ID, "", "")
	// Change own password: wrong current is 403 login.
	if w := e.call("bob", "POST", "/api/me/password", `{"current":"nope nope nope","new":"a newer password"}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"login"`) {
		t.Fatalf("wrong current: %d %s", w.Code, w.Body)
	}
	if w := e.call("bob", "POST", "/api/me/password", `{"current":"a long password","new":"bob"}`); w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"password"`) {
		t.Fatalf("rule: %d %s", w.Code, w.Body)
	}
	w := e.call("bob", "POST", "/api/me/password", `{"current":"a long password","new":"a newer password"}`)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Set-Cookie"), "cls-") {
		t.Fatalf("change: %d %s %v", w.Code, w.Body, w.Header())
	}
	waitFor(t, "other sign-ins ended", func() bool {
		return e.do("GET", "/api/me", "", withCookie(other)).Code == 401 &&
			e.do("GET", "/api/me", "", withBearer(devTok)).Code == 401
	})
	// The admin token has no password.
	if w := e.do("POST", "/api/me/password", `{"new":"a newer password"}`, withBearer(testToken)); w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"no_user"`) {
		t.Fatalf("admin token: %d %s", w.Code, w.Body)
	}
	// An admin reset sets must_change: everything but the allow-list is 403.
	if w := e.call("ana", "POST", "/api/users/"+e.u["cy"].ID+"/password", `{}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"password":`) {
		t.Fatalf("generate: %d %s", w.Code, w.Body)
	}
	tok, _, _ := e.st.Logins.Create(e.u["cy"].ID, "", "")
	if w := e.do("GET", "/api/sessions", "", withCookie(tok)); w.Code != 403 || !strings.Contains(w.Body.String(), "must_change") {
		t.Fatalf("must change: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/me", "", withCookie(tok)); w.Code != 200 || !strings.Contains(w.Body.String(), `"must_change":true`) {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	// The forced change needs no current password.
	if w := e.do("POST", "/api/me/password", `{"new":"cys own password"}`, withCookie(tok), withHeader(csrfHeader, "1")); w.Code != 200 {
		t.Fatalf("forced change: %d %s", w.Code, w.Body)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sse opens a stream on a real server and returns a line reader.
func sse(t *testing.T, srv *httptest.Server, path, cookie string) (*bufio.Reader, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	req.Host = "example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream %s: %d %s", path, resp.StatusCode, b)
	}
	return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
}

// events reads "event: X / data: Y" pairs until d passes.
func readEvents(r *bufio.Reader, d time.Duration) []string {
	out := make(chan string, 64)
	go func() {
		ev := ""
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(out)
				return
			}
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && ev != "":
				out <- ev + " " + strings.TrimPrefix(line, "data: ")
				ev = ""
			}
		}
	}()
	var got []string
	timer := time.After(d)
	for {
		select {
		case s, ok := <-out:
			if !ok {
				return got
			}
			got = append(got, s)
		case <-timer:
			return got
		}
	}
}

func TestRevocationCutsLiveStream(t *testing.T) {
	e := newUsersEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	for _, change := range []struct {
		name string
		body string
	}{{"disable", `{"disabled":true}`}, {"role", `{"role":"viewer"}`}} {
		bob := e.u["bob"]
		tok, _, _ := e.st.Logins.Create(bob.ID, "", "")
		r, closeFn := sse(t, srv, "/api/events", tok)
		before := e.ss.subscribers()
		_ = before
		done := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(r)
			done <- err
		}()
		waitFor(t, "subscribed", func() bool { return e.ss.subscribers() > 0 })
		w := e.call("ana", "PATCH", "/api/users/"+bob.ID, change.body)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"cut":`) {
			t.Fatalf("%s: %d %s", change.name, w.Code, w.Body)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the live stream was not cut", change.name)
		}
		closeFn()
		// Put bob back.
		_, _ = e.st.SetDisabled(bob.ID, false)
		_, _ = e.st.SetRole(bob.ID, users.RoleUser)
	}
}

func TestOwnershipScoping(t *testing.T) {
	e := newUsersEnv(t)
	bob, cy := e.u["bob"], e.u["cy"]
	e.ss.add("sb", bob.ID)
	e.ss.add("sc", cy.ID)
	e.ss.add("old", "") // from before users: the first admin's
	// Lists.
	list := func(who string) []string {
		w := e.call(who, "GET", "/api/sessions", "")
		var infos []session.Info
		_ = json.Unmarshal(w.Body.Bytes(), &infos)
		var ids []string
		for _, in := range infos {
			ids = append(ids, in.ID+":"+in.OwnerName)
		}
		return ids
	}
	if got := strings.Join(list("bob"), ","); got != "sb:bob" {
		t.Fatalf("bob sees %v", got)
	}
	if n := len(list("vic")); n != 3 {
		t.Fatalf("viewer sees %d", n)
	}
	if got := list("ana"); len(got) != 3 || !strings.Contains(strings.Join(got, ","), "old:ana") {
		t.Fatalf("admin sees %v (legacy owner should be ana)", got)
	}
	// Get, stream, diff, actions: 404 for another user's session.
	for _, req := range [][2]string{{"GET", "/api/sessions/sc"}, {"GET", "/api/sessions/sc/diff"},
		{"POST", "/api/sessions/sc/stop"}, {"DELETE", "/api/sessions/sc"}, {"PATCH", "/api/sessions/sc"},
		{"GET", "/api/sessions/sc/transcript"}} {
		if w := e.call("bob", req[0], req[1], "{}"); w.Code != 404 || !strings.Contains(w.Body.String(), `"not found"`) {
			t.Errorf("bob %s %s: %d %s", req[0], req[1], w.Code, w.Body)
		}
	}
	// Viewer reads every session and is refused every write.
	if w := e.call("vic", "GET", "/api/sessions/sc", ""); w.Code != 200 {
		t.Fatalf("viewer get: %d", w.Code)
	}
	for _, req := range [][2]string{{"POST", "/api/sessions/sc/stop"}, {"DELETE", "/api/sessions/sc"}, {"POST", "/api/sessions"}} {
		if w := e.call("vic", req[0], req[1], "{}"); w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"role"`) {
			t.Errorf("viewer %s %s: %d %s", req[0], req[1], w.Code, w.Body)
		}
	}
	// Admin acts on any session.
	if w := e.call("ana", "POST", "/api/sessions/sc/stop", "{}"); w.Code != 200 {
		t.Fatalf("admin stop: %d %s", w.Code, w.Body)
	}
	// Inbox: own items, system items to admins only.
	e.ib.Add(inbox.Item{Title: "for bob", Owner: bob.ID})
	e.ib.Add(inbox.Item{Title: "for cy", Owner: cy.ID})
	sys := e.ib.Add(inbox.Item{Title: "sysnote"})
	body := e.call("bob", "GET", "/api/inbox", "").Body.String()
	if !strings.Contains(body, "for bob") || strings.Contains(body, "for cy") || strings.Contains(body, "sysnote") || !strings.Contains(body, `"unread":1`) {
		t.Fatalf("bob inbox: %s", body)
	}
	if body := e.call("ana", "GET", "/api/inbox", "").Body.String(); !strings.Contains(body, "sysnote") || strings.Contains(body, "for bob") {
		t.Fatalf("admin inbox: %s", body)
	}
	if w := e.call("bob", "DELETE", "/api/inbox/"+sys.ID, ""); w.Code != 404 {
		t.Fatalf("bob deletes a system item: %d", w.Code)
	}
	if w := e.call("bob", "POST", "/api/inbox/read", `{"all":true}`); !strings.Contains(w.Body.String(), `"changed":1`) {
		t.Fatalf("read all: %s", w.Body)
	}
	if e.ib.UnreadWhere(func(it inbox.Item) bool { return it.Owner == cy.ID }) != 1 {
		t.Fatal("bob's read-all touched cy's items")
	}
	// Devices: own for users, all for admins; others' are not found.
	_, cyDev, _ := e.devices.PairScoped("cy-phone", "", cy.ID)
	if body := e.call("bob", "GET", "/api/devices", "").Body.String(); strings.Contains(body, "cy-phone") {
		t.Fatalf("bob sees cy's device: %s", body)
	}
	if body := e.call("ana", "GET", "/api/devices", "").Body.String(); !strings.Contains(body, `"user_name":"cy"`) {
		t.Fatalf("admin devices: %s", body)
	}
	if w := e.call("bob", "DELETE", "/api/devices/"+cyDev.ID, ""); w.Code != 404 {
		t.Fatalf("bob revokes cy's device: %d", w.Code)
	}
	// A user pairs for themselves only; a device token cannot pair.
	if w := e.call("bob", "POST", "/api/devices", `{"name":"tab","user":"`+cy.ID+`"}`); w.Code != 403 {
		t.Fatalf("pair for another: %d", w.Code)
	}
	if w := e.call("bob", "POST", "/api/devices", `{"name":"tab"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"`+bob.ID) {
		t.Fatalf("pair self: %d %s", w.Code, w.Body)
	}
	if w := e.call("vic", "POST", "/api/devices", `{"name":"m","scope":"mcp"}`); w.Code != 403 {
		t.Fatalf("viewer pairs an MCP client: %d", w.Code)
	}
	// Uploads: a staging folder of another user is not found.
	e.srv.files = newFilesStore(t, e.srv)
	stg, err := e.srv.files.NewStaging()
	if err != nil {
		t.Fatal(err)
	}
	_ = e.srv.files.SetStagingOwner(stg, cy.ID)
	if w := e.call("bob", "GET", "/api/uploads/"+stg, ""); w.Code != 404 {
		t.Fatalf("other's upload: %d", w.Code)
	}
	if w := e.call("bob", "POST", "/api/sessions", `{"Task":"x","Staging":"`+stg+`"}`); w.Code != 400 {
		t.Fatalf("task with other's staging: %d %s", w.Code, w.Body)
	}
}

func TestEventsFiltering(t *testing.T) {
	e := newUsersEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	bob, cy := e.u["bob"], e.u["cy"]
	streams := map[string]*bufio.Reader{}
	for _, who := range []string{"bob", "cy", "vic", "mo", "ana"} {
		r, closeFn := sse(t, srv, "/api/events", e.tok[who])
		defer closeFn()
		streams[who] = r
	}
	waitFor(t, "5 subscribers", func() bool { return e.ss.subscribers() == 5 })
	e.ss.emit(session.Event{Type: "state", Session: session.Info{ID: "sb", Owner: bob.ID}})
	e.ss.emit(session.Event{Type: "state", Session: session.Info{ID: "sc", Owner: cy.ID}})
	e.ib.Add(inbox.Item{Title: "bob's note", Owner: bob.ID})
	e.ib.Add(inbox.Item{Title: "system note"})
	got := map[string]string{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for who, r := range streams {
		wg.Go(func() {
			evs := strings.Join(readEvents(r, 700*time.Millisecond), "\n")
			mu.Lock()
			got[who] = evs
			mu.Unlock()
		})
	}
	wg.Wait()
	has := func(who, s string) bool { return strings.Contains(got[who], s) }
	switch {
	case !has("bob", `"ID":"sb"`) || has("bob", `"ID":"sc"`):
		t.Errorf("bob: %s", got["bob"])
	case !has("cy", `"ID":"sc"`) || has("cy", `"ID":"sb"`):
		t.Errorf("cy: %s", got["cy"])
	case !has("vic", `"ID":"sb"`) || !has("vic", `"ID":"sc"`):
		t.Errorf("viewer: %s", got["vic"])
	case has("mo", "session "):
		t.Errorf("monitor got session events: %s", got["mo"])
	case !has("bob", "bob's note") || has("bob", "system note") || !has("bob", `"Unread":1`):
		t.Errorf("bob inbox: %s", got["bob"])
	case has("cy", "bob's note") || has("cy", "system note"):
		t.Errorf("cy inbox: %s", got["cy"])
	case !has("ana", "system note") || has("ana", "bob's note"):
		t.Errorf("admin inbox: %s", got["ana"])
	}
}

func TestConfinementOfUserSessions(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.u["bob"]
	repo := t.TempDir()
	e.srv.cfg.Repos = nil
	defer func(old func(context.Context) error) { sandboxCheck = old }(sandboxCheck)
	sandboxCheck = func(context.Context) error { return errors.New("bwrap: not found") }
	if w := e.call("bob", "POST", "/api/sessions", `{"Task":"hi"}`); w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"sandbox"`) {
		t.Fatalf("no bwrap: %d %s", w.Code, w.Body)
	}
	sandboxCheck = func(context.Context) error { return nil }
	if w := e.call("bob", "POST", "/api/sessions", `{"Task":"hi","Workspace":"folder","Dir":"/tmp"}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"workspace"`) {
		t.Fatalf("folder: %d %s", w.Code, w.Body)
	}
	if w := e.call("bob", "POST", "/api/sessions", `{"Task":"hi","Dir":"`+repo+`"}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"workspace"`) {
		t.Fatalf("unshared repo: %d %s", w.Code, w.Body)
	}
	if w := e.call("bob", "POST", "/api/sessions", `{"Task":"hi","Agent":"claude"}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"agent"`) {
		t.Fatalf("agent: %d %s", w.Code, w.Body)
	}
	if _, err := e.st.SetRepos(bob.ID, []users.RepoShare{{Path: repo}}); err != nil {
		t.Fatal(err)
	}
	if w := e.call("bob", "POST", "/api/sessions", `{"Task":"hi","Dir":"`+repo+`"}`); w.Code != 201 {
		t.Fatalf("shared repo: %d %s", w.Code, w.Body)
	}
	spec := e.ss.specs[len(e.ss.specs)-1]
	if !spec.Confined || spec.Owner != bob.ID || spec.Workspace != session.WorkspaceRepo || spec.Dir != repo {
		t.Fatalf("spec %+v", spec)
	}
	// Admins are not confined.
	if w := e.call("ana", "POST", "/api/sessions", `{"Task":"hi","Workspace":"folder","Dir":"/tmp"}`); w.Code != 201 {
		t.Fatalf("admin folder: %d %s", w.Code, w.Body)
	}
	if spec := e.ss.specs[len(e.ss.specs)-1]; spec.Confined || spec.Owner != e.u["ana"].ID {
		t.Fatalf("admin spec %+v", spec)
	}
	// Accept needs RepoShare.Accept.
	e.ss.mu.Lock()
	e.ss.infos["sr"] = session.Info{ID: "sr", Owner: bob.ID, Repo: repo, Confined: true, State: session.StatePassed}
	e.ss.mu.Unlock()
	if w := e.call("bob", "POST", "/api/sessions/sr/accept", `{}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"accept"`) {
		t.Fatalf("accept: %d %s", w.Code, w.Body)
	}
	// Save only under the user's own folder.
	if w := e.call("bob", "POST", "/api/sessions/sr/save", `{"dir":"/tmp/out"}`); w.Code != 403 {
		t.Fatalf("save outside: %d %s", w.Code, w.Body)
	}
	// Inside it, the save goes through the confined SaveInto (os.Root), never
	// the symlink-following SaveTo.
	own := filepath.Join(users.FilesDir(e.st.Dir(), bob.ID), "out")
	e.call("bob", "POST", "/api/sessions/sr/save", `{"dir":"`+own+`"}`)
	if got := e.ss.lastCall(); !strings.HasPrefix(got, "saveinto:"+users.FilesDir(e.st.Dir(), bob.ID)+":"+own+":") {
		t.Fatalf("own-folder save went to %q", got)
	}
}

func TestBudgets(t *testing.T) {
	e := newUsersEnv(t)
	bob := e.u["bob"]
	if _, err := e.st.SetBudget(bob.ID, users.Budget{MonthlyUSD: 1}); err != nil {
		t.Fatal(err)
	}
	defer func(old func(context.Context) error) { sandboxCheck = old }(sandboxCheck)
	sandboxCheck = func(context.Context) error { return nil }
	if w := e.call("bob", "POST", "/api/sessions", `{"Task":"hi"}`); w.Code != 201 {
		t.Fatalf("under budget: %d %s", w.Code, w.Body)
	}
	e.used.Add(usage.Record{Time: time.Now(), Model: "x", CostUSD: 1.5, PromptTokens: 10, User: bob.ID})
	w := e.call("bob", "POST", "/api/sessions", `{"Task":"hi"}`)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"budget"`) || !strings.Contains(w.Body.String(), "US$1.50 of US$1.00") {
		t.Fatalf("over budget: %d %s", w.Code, w.Body)
	}
	// /v1: the OpenAI-style error.
	w = e.do("POST", "/v1/chat/completions", `{}`, withCookie(e.tok["bob"]), withHeader(csrfHeader, "1"))
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"type":"budget_exceeded"`) || !strings.Contains(w.Body.String(), `"code":"user_budget"`) {
		t.Fatalf("/v1: %d %s", w.Code, w.Body)
	}
	// Quick answers too.
	if w := e.call("bob", "POST", "/api/quick", `{"text":"hi"}`); w.Code != 429 && w.Code != 503 {
		t.Fatalf("quick: %d %s", w.Code, w.Body)
	}
	// Others are not affected; Me shows the spend.
	if w := e.call("cy", "POST", "/v1/chat/completions", `{}`); w.Code == 429 {
		t.Fatalf("cy refused: %s", w.Body)
	}
	if body := e.call("bob", "GET", "/api/me", "").Body.String(); !strings.Contains(body, `"spent_usd":1.5`) {
		t.Fatalf("me: %s", body)
	}
	// 80 % notice once.
	if _, err := e.st.SetBudget(e.u["cy"].ID, users.Budget{MonthlyTokens: 100}); err != nil {
		t.Fatal(err)
	}
	e.used.Add(usage.Record{Time: time.Now(), Model: "x", PromptTokens: 85, User: e.u["cy"].ID})
	e.call("cy", "POST", "/api/sessions", `{"Task":"hi"}`)
	e.call("cy", "POST", "/api/sessions", `{"Task":"hi"}`)
	n := len(e.ib.ListWhere(func(it inbox.Item) bool { return it.Owner == e.u["cy"].ID && it.Kind == "budget" }, false, 0))
	if n != 1 {
		t.Fatalf("%d budget notices", n)
	}
}

func TestGatewayAttribution(t *testing.T) {
	e := newUsersEnv(t)
	var seen []string
	var mu sync.Mutex
	e.srv.gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(userHeader))
		mu.Unlock()
		w.WriteHeader(200)
	})
	e.ss.add("sb", e.u["bob"].ID)
	key, _ := e.st.NewAPIKey(e.u["cy"].ID)
	e.do("POST", "/v1/chat/completions", "{}", withBearer(key))                                                                               // personal key: cy
	e.do("POST", "/v1/chat/completions", "{}", withBearer(e.srv.cfg.SessionAgentToken("sb")))                                                 // session token: bob
	e.do("POST", "/v1/chat/completions", "{}", withBearer(testToken), withHeader(sessionHeader, "sb"))                                        // admin + session: bob
	e.do("POST", "/v1/chat/completions", "{}", withBearer(testToken))                                                                         // admin token: ana
	e.do("POST", "/v1/chat/completions", "{}", withBearer(testToken), withHeader(userHeader, e.u["cy"].ID))                                   // forged header: ignored
	e.do("POST", "/v1/chat/completions", "{}", withCookie(e.tok["bob"]), withHeader(csrfHeader, "1"), withHeader("X-Cerberus-Owner", "nope")) // owner header ignored for users
	want := []string{e.u["cy"].ID, e.u["bob"].ID, e.u["bob"].ID, e.u["ana"].ID, e.u["ana"].ID, e.u["bob"].ID}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("attribution %v, want %v", seen, want)
	}
	// Chat, viewer and monitor roles are refused /v1.
	for _, who := range []string{"cat", "vic", "mo"} {
		if w := e.call(who, "POST", "/v1/chat/completions", "{}"); w.Code != 403 {
			t.Errorf("%s on /v1: %d", who, w.Code)
		}
	}
}

func TestNotFoundPages(t *testing.T) {
	e := newUsersEnv(t)
	for name, opts := range map[string][]reqOpt{"signed out": nil, "signed in": e.as("bob")} {
		w := e.do("GET", "/nope/deeper", "", opts...)
		if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "Not found") {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("Content-Type"))
		}
	}
	if w := e.do("HEAD", "/nope", ""); w.Code != 404 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d %d", w.Code, w.Body.Len())
	}
	if w := e.do("POST", "/nope", "{}"); w.Code != 404 || !strings.Contains(w.Body.String(), `"error":"not found"`) {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/nope", ""); w.Code != 401 {
		t.Errorf("/api signed out: %d", w.Code)
	}
	if w := e.do("GET", "/api/nope", "", e.as("bob")...); w.Code != 404 || !strings.Contains(w.Body.String(), "unknown endpoint") {
		t.Errorf("/api signed in: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/", ""); w.Code != 302 || w.Header().Get("Location") != "/login" {
		t.Errorf("/ signed out: %d", w.Code)
	}
	// The built-in page answers when the build has no 404.html.
	e.srv.notFound = nil
	if w := e.do("GET", "/x", ""); w.Code != 404 || !strings.Contains(w.Body.String(), "Nothing here") {
		t.Errorf("built-in: %d %s", w.Code, w.Body)
	}
}

func TestStatusAndUsageByRole(t *testing.T) {
	e := newUsersEnv(t)
	e.ss.add("sb", e.u["bob"].ID)
	e.ss.add("sc", e.u["cy"].ID)
	count := func(who string) (int, int) {
		var st Status
		_ = json.Unmarshal(e.call(who, "GET", "/api/status", "").Body.Bytes(), &st)
		n := 0
		for _, c := range st.Sessions {
			n += c
		}
		return n, len(st.Agents)
	}
	if n, a := count("ana"); n != 2 || a == 0 {
		t.Errorf("admin: %d sessions %d agents", n, a)
	}
	if n, _ := count("bob"); n != 1 {
		t.Errorf("user: %d sessions", n)
	}
	if n, a := count("vic"); n != 2 || a != 0 {
		t.Errorf("viewer: %d sessions %d agents", n, a)
	}
	if n, a := count("mo"); n != 0 || a != 0 {
		t.Errorf("monitor: %d sessions %d agents", n, a)
	}
	e.used.Add(usage.Record{Time: time.Now(), Model: "m", CostUSD: 0.5, User: e.u["bob"].ID})
	e.used.Add(usage.Record{Time: time.Now(), Model: "m", CostUSD: 0.25, User: e.u["cy"].ID})
	body := e.call("bob", "GET", "/api/usage", "").Body.String()
	if strings.Contains(body, e.u["cy"].ID) || strings.Contains(body, `"budgets"`) || !strings.Contains(body, `"month_usd":0.5`) {
		t.Errorf("user usage: %s", body)
	}
	if body := e.call("ana", "GET", "/api/usage", "").Body.String(); !strings.Contains(body, `"users":[`) || !strings.Contains(body, `"name":"cy","spent_usd":0.25`) {
		t.Errorf("admin usage: %s", body)
	}
	// Doctor: counts only for viewers and monitors.
	e.srv.doctorEnv = func(env doctorEnvT) doctorEnvT { return env }
	if body := e.call("mo", "GET", "/api/doctor", "").Body.String(); !strings.Contains(body, `"checks":[]`) || !strings.Contains(body, `"counts":{`) {
		t.Errorf("monitor doctor: %s", body)
	}
}

func TestAdminUsersAPI(t *testing.T) {
	e := newUsersEnv(t)
	// Add with a generated password.
	w := e.call("ana", "POST", "/api/users", `{"name":"dee","role":"user"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"password":`) || !strings.Contains(w.Body.String(), `"must_change":true`) {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if w := e.call("ana", "POST", "/api/users", `{"name":"DEE","role":"user","password":"a long password"}`); w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"name"`) {
		t.Fatalf("taken: %d %s", w.Code, w.Body)
	}
	if w := e.call("ana", "POST", "/api/users", `{"name":"eve","role":"user","password":"short"}`); w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"password"`) {
		t.Fatalf("password rule: %d %s", w.Code, w.Body)
	}
	// List with counts.
	e.ss.add("sb", e.u["bob"].ID)
	var list struct {
		Users []UserPublic `json:"users"`
		Roles []string     `json:"roles"`
	}
	_ = json.Unmarshal(e.call("ana", "GET", "/api/users", "").Body.Bytes(), &list)
	if len(list.Users) != 7 || len(list.Roles) != 5 {
		t.Fatalf("list %+v", list)
	}
	for _, u := range list.Users {
		if u.Name == "bob" && u.Sessions != 1 {
			t.Fatalf("bob %+v", u)
		}
		if u.Name == "ana" && !u.LegacyOwner {
			t.Fatal("ana is the legacy owner")
		}
	}
	// Self and last-admin guards.
	ana := e.u["ana"]
	if w := e.call("ana", "PATCH", "/api/users/"+ana.ID, `{"role":"user"}`); w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"self"`) {
		t.Fatalf("self demote: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/users/"+ana.ID, `{"disabled":true}`, withBearer(testToken)); w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"last_admin"`) {
		t.Fatalf("last admin: %d %s", w.Code, w.Body)
	}
	if w := e.call("ana", "PATCH", "/api/users/"+e.u["bob"].ID, `{"repos":[{"path":"/not/a/repo"}]}`); w.Code != 400 {
		t.Fatalf("unknown repo: %d", w.Code)
	}
	if w := e.call("ana", "PATCH", "/api/users/"+e.u["bob"].ID, `{"agents":["nope"]}`); w.Code != 400 {
		t.Fatalf("unknown agent: %d", w.Code)
	}
	if w := e.call("bob", "GET", "/api/users", ""); w.Code != 403 {
		t.Fatalf("user lists users: %d", w.Code)
	}
	// Delete: bob's data moves to the deleting admin.
	w = e.call("ana", "DELETE", "/api/users/"+e.u["bob"].ID, ``)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sessions":1`) || !strings.Contains(w.Body.String(), `"reassigned_to":"`+ana.ID) {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if body := e.call("ana", "GET", "/api/sessions/sb", "").Body.String(); !strings.Contains(body, `"OwnerName":"ana"`) {
		t.Fatalf("moved session: %s", body)
	}
	if w := e.do("GET", "/api/me", "", withCookie(e.tok["bob"])); w.Code != 401 {
		t.Fatalf("deleted user's cookie: %d", w.Code)
	}
	if w := e.call("ana", "DELETE", "/api/users/"+ana.ID, ``); w.Code != 409 {
		t.Fatalf("delete self: %d", w.Code)
	}
	// Audit records carry the acting user.
	var found bool
	for _, r := range e.records(t) {
		if r.Route == "user.remove" {
			found = r.User == "ana" && r.Actor == "user:ana"
		}
	}
	if !found {
		t.Fatal("no user.remove record by ana")
	}
	// ?user= filter.
	body := e.call("ana", "GET", "/api/audit?user=ana", "").Body.String()
	if !strings.Contains(body, "user.remove") || strings.Contains(body, `"user":"bob"`) {
		t.Fatalf("audit filter: %s", body)
	}
}

func TestFirstAdminBootstrap(t *testing.T) {
	t.Cleanup(users.SetCostForTest(1000))
	env := newSafetyEnv(t, nil)
	st, _ := users.Open(env.srv.dataDir)
	env.srv.users = st
	// No users: the admin token is bootstrap.
	w := env.do("GET", "/api/me", "", withBearer(testToken))
	if !strings.Contains(w.Body.String(), `"bootstrap":true`) || !strings.Contains(w.Body.String(), `"user":null`) {
		t.Fatalf("me: %s", w.Body)
	}
	if w := env.do("POST", "/api/users", `{"name":"bob","role":"user","password":"a long password"}`, withBearer(testToken)); w.Code != 400 {
		t.Fatalf("first as user: %d %s", w.Code, w.Body)
	}
	if w := env.do("POST", "/api/users", `{"name":"ana","role":"admin","password":"a long password"}`, withBearer(testToken)); w.Code != 201 {
		t.Fatalf("first admin: %d %s", w.Code, w.Body)
	}
	if w := env.do("GET", "/api/me", "", withBearer(testToken)); !strings.Contains(w.Body.String(), `"bootstrap":false`) {
		t.Fatalf("me after: %s", w.Body)
	}
}

func TestChatScoping(t *testing.T) {
	e := newUsersEnv(t)
	cm := newChatManager(t, e.srv)
	e.srv.chats = cm
	e.srv.cfg.Chat.Enabled = true
	mk := func(who string) string {
		w := e.call(who, "POST", "/api/chats", `{"cwd":"/","model":"m"}`)
		if w.Code != 201 {
			t.Fatalf("%s create: %d %s", who, w.Code, w.Body)
		}
		var c struct {
			ID    string `json:"id"`
			Owner string `json:"owner"`
			Cwd   string `json:"cwd"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &c)
		if c.Owner != e.u[who].ID {
			t.Fatalf("%s chat owner %q", who, c.Owner)
		}
		if who != "ana" && !strings.Contains(c.Cwd, e.u[who].ID) {
			t.Fatalf("%s chat cwd %q not their folder", who, c.Cwd)
		}
		return c.ID
	}
	bobChat, catChat := mk("bob"), mk("cat")
	if body := e.call("bob", "GET", "/api/chats", "").Body.String(); strings.Contains(body, catChat) || !strings.Contains(body, bobChat) {
		t.Fatalf("bob's list: %s", body)
	}
	if body := e.call("ana", "GET", "/api/chats?all=1", "").Body.String(); !strings.Contains(body, catChat) || !strings.Contains(body, `"owner_name":"cat"`) {
		t.Fatalf("admin all: %s", body)
	}
	if w := e.call("bob", "GET", "/api/chats/"+catChat, ""); w.Code != 404 {
		t.Fatalf("bob reads cat's chat: %d", w.Code)
	}
	if w := e.call("ana", "GET", "/api/chats/"+catChat, ""); w.Code != 200 {
		t.Fatalf("admin reads: %d", w.Code)
	}
	if w := e.call("ana", "POST", "/api/chats/"+catChat+"/messages", `{"text":"hi"}`); w.Code != 404 {
		t.Fatalf("admin posts into cat's chat: %d %s", w.Code, w.Body)
	}
	if w := e.call("cat", "POST", "/api/chats", `{"model":"claude:sonnet"}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"backend"`) {
		t.Fatalf("claude backend for chat role: %d %s", w.Code, w.Body)
	}
	if w := e.call("bob", "GET", "/api/chats/atrium", ""); w.Code != 403 {
		t.Fatalf("atrium import: %d", w.Code)
	}
}

type doctorEnvT = doctor.Env

func newFilesStore(t *testing.T, s *server) *files.Store {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	return files.New(s.cfg, nil, nil, slog.New(slog.DiscardHandler))
}

// newChatManager is a chat manager with per-role access (no gateway: runs
// fail, which these tests do not reach).
func newChatManager(t *testing.T, s *server) *chat.Manager {
	t.Helper()
	root := t.TempDir()
	m, err := chat.New(chat.Options{
		DataDir: filepath.Join(root, "data"), Config: s.conf, Home: root,
		Gateway: func() (string, string) { return "http://127.0.0.1:1/v1", "tok" },
		Claude:  func() string { return "/bin/true" },
		Access:  ChatAccess(s.users, s.conf), IsAdmin: ChatIsAdmin(s.users),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}
