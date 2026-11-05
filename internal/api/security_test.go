package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

// safetyEnv is newEnv with an audit log, a device store and a config file
// in temp folders (rotation saves the config, so it must have a path).
type safetyEnv struct {
	*testEnv
	audit   *audit.Logger
	devices *devices.Store
	cfgPath string
}

func newSafetyEnv(t *testing.T, mut func(*config.Config)) *safetyEnv {
	t.Helper()
	dir := t.TempDir()
	al, err := audit.Open(filepath.Join(dir, "data"), audit.Options{Secrets: func() []string { return []string{"hunter2-secret"} }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	ds, err := devices.Open(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config", "config.json")
	env := newEnv(t, func(c *config.Config) {
		c.SetPath(cfgPath)
		if mut != nil {
			mut(c)
		}
	})
	env.srv.audit, env.srv.devices, env.srv.dataDir = al, ds, filepath.Join(dir, "data")
	env.h = env.srv.handler()
	return &safetyEnv{testEnv: env, audit: al, devices: ds, cfgPath: cfgPath}
}

func (e *safetyEnv) records(t *testing.T) []audit.Record {
	t.Helper()
	recs, err := e.audit.Query(time.Time{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func (e *safetyEnv) pair(t *testing.T, name string) (string, devices.Public) {
	t.Helper()
	w := e.do("POST", "/api/devices", `{"name":"`+name+`"}`, withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	var res PairResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.URL, "http://example.com/?token=cdv-") {
		t.Fatalf("pair url %q", res.URL)
	}
	if rows := strings.Split(strings.TrimRight(res.QR, "\n"), "\n"); len(rows) < 21 || len(rows[0]) != len(rows) {
		t.Fatalf("pair qr: %d rows", len(rows))
	}
	return res.Token, res.Device
}

// ---------- Host header ----------

func TestHostHeaderCheck(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.Server.Listen = "127.0.0.1:7340"
		c.Server.ExtraListen = []string{"100.101.102.103:7340"}
		c.Server.PublicURL = "https://desk.tailnet.ts.net:7340"
		c.Server.AllowedHosts = []string{"cerberus.lan", "*.home.arpa", "https://alias.example:9000"}
	})
	wild := newEnv(t, func(c *config.Config) {
		c.Server.Listen = "0.0.0.0:7340"
		c.Server.AllowedHosts = nil
	})
	cases := []struct {
		env  *testEnv
		host string
		want bool
	}{
		{env, "127.0.0.1:7340", true},
		{env, "[::1]:7340", true},
		{env, "localhost", true},
		{env, "app.localhost:7340", true},
		{env, "100.101.102.103:7340", true},
		{env, "desk.tailnet.ts.net:7340", true},
		{env, "DESK.tailnet.ts.net", true},
		{env, "cerberus.lan:7340", true},
		{env, "box.home.arpa", true},
		{env, "alias.example", true},
		{env, "home.arpa", false}, // the wildcard needs a label in front
		{env, "evil.example", false},
		{env, "evil.example:7340", false},
		{env, "192.168.1.20:7340", false}, // not a listen address
		{env, "", false},
		{wild, "192.168.1.20:7340", true}, // wildcard bind: any IP literal (no rebinding with IPs)
		{wild, "[fd00::1]:7340", true},
		{wild, "evil.example", false},
	}
	for _, tc := range cases {
		for _, target := range []string{"/healthz", "/api/status", "/v1/models"} {
			w := tc.env.do("GET", target, "", withBearer(testToken), withHost(tc.host))
			if got := w.Code != http.StatusForbidden; got != tc.want {
				t.Errorf("Host %q %s: status %d, want allowed=%v", tc.host, target, w.Code, tc.want)
			}
		}
	}
}

// ---------- audit middleware ----------

func TestAuditRecordsStateChanges(t *testing.T) {
	env := newSafetyEnv(t, nil)
	env.do("GET", "/api/status", "", withBearer(testToken))
	env.do("POST", "/api/sessions/s1/stop?token=leak&x=1", "", withBearer(testToken))
	env.do("POST", "/api/sessions/s1/followup", `{"text":"use key hunter2-secret then `+strings.Repeat("x", 200)+`"}`,
		withBearer(testToken), withHeader(actorHeader, "cli"))
	env.do("POST", "/api/sessions/s1/input", `{"data":"p4ssw0rd\r"}`, withBearer(testToken))
	env.do("POST", "/api/sessions/zzz/stop", "", withBearer(testToken))
	env.do("POST", "/api/usage/watch", `{"on":true}`, withBearer(testToken))
	env.do("POST", "/v1/chat/completions", `{}`, withBearer(testToken))
	recs := env.records(t)
	if len(recs) != 4 {
		t.Fatalf("want 4 records (no GET, no watch, no /v1), got %d: %+v", len(recs), recs)
	}
	// Newest first.
	missing, input, follow, stop := recs[0], recs[1], recs[2], recs[3]
	if stop.Actor != "owner:admin" || stop.Method != "POST" || stop.Route != "POST /api/sessions/{id}/{action}" ||
		strings.Join(stop.Targets, ",") != "s1,stop" || stop.Status != 200 || stop.Remote != "192.0.2.10" {
		t.Fatalf("stop record %+v", stop)
	}
	raw, _ := os.ReadFile(env.audit.Path())
	for _, leak := range []string{"leak", "hunter2-secret", "p4ssw0rd", "x=1"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("audit file contains %q:\n%s", leak, raw)
		}
	}
	if follow.Actor != "cli" || !strings.HasPrefix(follow.Detail, "followup: use key ••••••") ||
		len([]rune(follow.Detail)) != 80 {
		t.Fatalf("followup record %+v", follow)
	}
	// No control service in this env: input counts as hidden (never logged).
	if input.Detail != "input (hidden, 9 bytes)" {
		t.Fatalf("input record %+v", input)
	}
	if missing.Status != 404 {
		t.Fatalf("missing session record %+v", missing)
	}
}

// The X-Cerberus-Actor header names the CLI only for the admin token.
func TestCLIActorNeedsAdminToken(t *testing.T) {
	env := newSafetyEnv(t, nil)
	tok, _ := env.pair(t, "phone")
	env.do("POST", "/api/sessions/s1/stop", "", withBearer(tok), withHeader(actorHeader, "cli"))
	recs := env.records(t)
	if recs[0].Actor != "owner:phone" {
		t.Fatalf("device claimed the cli actor: %+v", recs[0])
	}
	if recs[1].Route != "POST /api/devices" || recs[1].Actor != "owner:admin" || recs[1].Detail != "paired phone" {
		t.Fatalf("pair record %+v", recs[1])
	}
}

func TestAuditAgentActor(t *testing.T) {
	env := newSafetyEnv(t, nil)
	tok := env.srv.cfg.SessionAgentToken("s1")
	env.do("POST", "/api/agent/sessions/c1/stop", "", withBearer(tok), withHeader(sessionHeader, "s1"))
	recs := env.records(t)
	if len(recs) != 1 || recs[0].Actor != "agent:s1" || recs[0].Route != "POST /api/agent/sessions/{id}/stop" ||
		recs[0].Targets[0] != "c1" {
		t.Fatalf("records %+v", recs)
	}
}

func TestAuditEndpoint(t *testing.T) {
	env := newSafetyEnv(t, nil)
	env.do("POST", "/api/sessions/s1/stop", "", withBearer(testToken), withHeader(actorHeader, "cli"))
	env.do("POST", "/api/sessions/s1/stop", "", withBearer(testToken))
	old := time.Now().Add(-48 * time.Hour)
	_ = env.audit.Log(audit.Record{Time: old, Actor: "cli", Route: "token.rotate"})
	get := func(q string) AuditView {
		t.Helper()
		w := env.do("GET", "/api/audit"+q, "", withBearer(testToken))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
		var v AuditView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	if v := get(""); len(v.Records) != 3 {
		t.Fatalf("all: %+v", v)
	}
	if v := get("?actor=cli"); len(v.Records) != 2 {
		t.Fatalf("cli: %+v", v)
	}
	if v := get("?actor=cli&since=24h"); len(v.Records) != 1 {
		t.Fatalf("cli 24h: %+v", v)
	}
	if v := get("?limit=1"); len(v.Records) != 1 || v.Records[0].Route != "token.rotate" { // newest appended
		t.Fatalf("limit: %+v", v)
	}
	if w := env.do("GET", "/api/audit?since=yesterday-ish", "", withBearer(testToken)); w.Code != 400 {
		t.Fatalf("bad since: %d", w.Code)
	}
	if w := newEnv(t, nil).do("GET", "/api/audit", "", withBearer(testToken)); w.Code != 503 {
		t.Fatalf("no audit log: %d", w.Code)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                     {},
		"24h":                  now.Add(-24 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"2026-10-31T08:00:00Z": time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q = %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "-5h", "x7d"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ---------- rate limit ----------

func TestAuthFailureRateLimit(t *testing.T) {
	env := newSafetyEnv(t, nil)
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	env.srv.limiter.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	attacker := from("198.51.100.7:4000")

	// No credential at all is not a failure (a browser opening the UI).
	for i := 0; i < 10; i++ {
		if w := env.do("GET", "/api/status", "", attacker); w.Code != 401 {
			t.Fatalf("anonymous %d: %d", i, w.Code)
		}
	}
	// The same stale credential counts once.
	for i := 0; i < 10; i++ {
		if w := env.do("GET", "/api/status", "", attacker, withCookie("stale-cookie")); w.Code != 401 {
			t.Fatalf("stale cookie %d: %d", i, w.Code)
		} else if c := w.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
			t.Fatalf("a bad cookie must be cleared: %+v", c)
		}
	}
	// A valid agent token on the wrong scope is not a guess either.
	env.do("GET", "/api/status", "", attacker, withBearer(env.srv.cfg.AgentToken()))
	// 4 more distinct guesses use up the 5 per minute; the next one blocks.
	for i := 0; i < 4; i++ {
		if w := env.do("GET", "/api/status", "", attacker, withBearer(fmt.Sprintf("guess-%d", i))); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	w := env.do("GET", "/v1/models", "", attacker, withBearer("guess-final"))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("6th failure: %d %v", w.Code, w.Header())
	}
	// Blocked: a bad or missing credential gets 429 from that address for
	// 10 min, but a valid one still works (nobody sharing the address, such
	// as loopback, can lock the owner or the agents out).
	if w := env.do("GET", "/api/status", "", attacker, withBearer("guess-more")); w.Code != 429 {
		t.Fatalf("blocked guess: %d", w.Code)
	}
	if w := env.do("GET", "/api/status", "", attacker); w.Code != 429 {
		t.Fatalf("blocked anonymous: %d", w.Code)
	}
	if w := env.do("GET", "/api/status", "", attacker, withBearer(testToken)); w.Code != 200 {
		t.Fatalf("blocked with the right token: %d", w.Code)
	}
	if w := env.do("GET", "/v1/models", "", attacker, withBearer(env.srv.cfg.SessionAgentToken("s1"))); w.Code == 429 || w.Code == 401 {
		t.Fatalf("agent token while blocked: %d", w.Code)
	}
	if w := env.do("GET", "/healthz", "", attacker); w.Code != 200 {
		t.Fatalf("public path while blocked: %d", w.Code)
	}
	advance(9 * time.Minute)
	if w := env.do("GET", "/api/status", "", attacker, withBearer("guess-later")); w.Code != 429 {
		t.Fatalf("after 9 min: %d", w.Code)
	}
	advance(2 * time.Minute)
	if w := env.do("GET", "/api/status", "", attacker, withBearer("guess-later")); w.Code != 401 {
		t.Fatalf("after 11 min: %d", w.Code)
	}
	var fails, blocked int
	for _, r := range env.records(t) {
		switch r.Route {
		case "auth.failure":
			fails++
			if r.Actor != audit.ActorSystem || r.Remote != "198.51.100.7" || r.Targets[0] != "GET /api/status" && r.Targets[0] != "/v1/" {
				t.Fatalf("failure record %+v", r)
			}
		case "auth.blocked":
			blocked++
		}
	}
	if fails != 7 || blocked != 1 {
		t.Fatalf("audit: %d failures, %d blocks", fails, blocked)
	}
}

func TestLimitKeyGroupsIPv6(t *testing.T) {
	if a, b := limitKey("[2001:db8:1:2::5]:1"), limitKey("[2001:db8:1:2:ffff::9]:2"); a != b {
		t.Fatalf("%s != %s", a, b)
	}
	if limitKey("192.0.2.1:5") != "192.0.2.1" {
		t.Fatal(limitKey("192.0.2.1:5"))
	}
}

func TestLimiterBounded(t *testing.T) {
	l := newFailLimiter()
	for i := 0; i < maxLimiterEntries+100; i++ {
		l.fail(fmt.Sprintf("10.0.%d.%d", i/256, i%256), "x")
	}
	if len(l.m) > maxLimiterEntries {
		t.Fatalf("%d entries", len(l.m))
	}
}

// ---------- devices ----------

func TestDevicePairingAndRevocation(t *testing.T) {
	env := newSafetyEnv(t, nil)
	tok, d := env.pair(t, "phone")
	if w := env.do("GET", "/api/status", "", withBearer(tok)); w.Code != 200 {
		t.Fatalf("device bearer: %d", w.Code)
	}
	// The pairing URL's ?token= becomes a cookie holding the device token.
	w := env.do("GET", "/?token="+tok, "")
	if c := w.Result().Cookies(); w.Code != 302 || len(c) != 1 || c[0].Value != tok {
		t.Fatalf("device login: %d %+v", w.Code, c)
	}
	if w := env.do("GET", "/api/status", "", withCookie(tok)); w.Code != 200 {
		t.Fatalf("device cookie: %d", w.Code)
	}
	// A device cannot pair more devices; it can list and revoke.
	if w := env.do("POST", "/api/devices", `{"name":"x"}`, withBearer(tok)); w.Code != 403 {
		t.Fatalf("device pairing: %d", w.Code)
	}
	w = env.do("GET", "/api/devices", "", withBearer(tok))
	var list DeviceList
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Devices) != 1 || list.Current != d.ID || strings.Contains(w.Body.String(), "hash") {
		t.Fatalf("list %s", w.Body)
	}
	if w := env.do("DELETE", "/api/devices/nope", "", withBearer(testToken)); w.Code != 404 {
		t.Fatalf("revoke unknown: %d", w.Code)
	}
	if w := env.do("DELETE", "/api/devices/"+d.ID, "", withBearer(testToken)); w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := env.do("GET", "/api/status", "", withBearer(tok)); w.Code != 401 {
		t.Fatalf("revoked device: %d", w.Code)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(env.devices.Path())
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("devices.json mode: %v %v", st, err)
		}
	}
}

// openStream starts GET path as an SSE client and waits for the headers.
func openStream(t *testing.T, base, path, tok string) (*http.Response, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return resp, func() { cancel(); resp.Body.Close() }
}

// streamEnds reports whether the stream body ends within d.
func streamEnds(resp *http.Response, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, bufio.NewReader(resp.Body))
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func TestRevokeCutsLiveStreams(t *testing.T) {
	env := newSafetyEnv(t, nil)
	env.srv.ping = time.Hour
	tok, d := env.pair(t, "tablet")
	ts := httptest.NewServer(env.h)
	defer ts.Close()

	// Two event streams: the device's and the admin's.
	devResp, closeDev := openStream(t, ts.URL, "/api/events", tok)
	defer closeDev()
	if devResp.StatusCode != 200 {
		t.Fatalf("device stream: %d", devResp.StatusCode)
	}
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/devices/"+d.ID, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("revoke: %v %v", resp, err)
	}
	var body struct{ Cut int }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.Cut != 1 {
		t.Fatalf("cut %d", body.Cut)
	}
	if !streamEnds(devResp, 3*time.Second) {
		t.Fatal("the revoked device's stream stayed open")
	}
}

func TestStreamCapPerActor(t *testing.T) {
	env := newSafetyEnv(t, nil)
	env.srv.ping = time.Hour
	for i := 0; i < maxStreamsPerActor; i++ {
		if !env.srv.conns.acquire("owner:admin") {
			t.Fatalf("slot %d refused", i)
		}
	}
	if w := env.do("GET", "/api/events", "", withBearer(testToken)); w.Code != 429 {
		t.Fatalf("33rd stream: %d", w.Code)
	}
	// Another actor is not affected; a non-stream request neither.
	if w := env.do("GET", "/api/status", "", withBearer(testToken)); w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	env.srv.conns.release("owner:admin")
	ts := httptest.NewServer(env.h)
	defer ts.Close()
	resp, closeIt := openStream(t, ts.URL, "/api/events", testToken)
	defer closeIt()
	if resp.StatusCode != 200 {
		t.Fatalf("after a release: %d", resp.StatusCode)
	}
}

// ---------- token rotation ----------

func TestTokenRotation(t *testing.T) {
	env := newSafetyEnv(t, nil)
	env.srv.ping = time.Hour
	devTok, _ := env.pair(t, "phone")
	oldAgent := env.srv.cfg.SessionAgentToken("s1")
	oldShared := env.srv.cfg.AgentToken()
	oldCap := env.srv.previewCap("s1")
	ts := httptest.NewServer(env.h)
	defer ts.Close()
	adminStream, closeStream := openStream(t, ts.URL, "/api/events", testToken)
	defer closeStream()
	env.sessions.infos = []session.Info{{ID: "s1", State: session.StateRunning}}

	w := env.do("POST", "/api/token/rotate", "", withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}
	var res RotateResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !res.OK || res.Token == "" || res.Token == testToken {
		t.Fatalf("result %+v", res)
	}
	if !streamEnds(adminStream, 3*time.Second) {
		t.Fatal("a stream on the old admin token stayed open")
	}
	// The config on disk holds the new token, 0600.
	raw, err := os.ReadFile(env.cfgPath)
	if err != nil || !strings.Contains(string(raw), res.Token) || strings.Contains(string(raw), testToken) {
		t.Fatalf("config file: %v", err)
	}
	if st, _ := os.Stat(env.cfgPath); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", st.Mode().Perm())
	}
	for name, c := range map[string]struct {
		opts []reqOpt
		path string
		want int
	}{
		"old bearer":                {[]reqOpt{withBearer(testToken)}, "/api/status", 401},
		"old cookie":                {[]reqOpt{withCookie(testToken)}, "/api/status", 401},
		"new bearer":                {[]reqOpt{withBearer(res.Token)}, "/api/status", 200},
		"device keeps working":      {[]reqOpt{withBearer(devTok)}, "/api/status", 200},
		"old shared agent token":    {[]reqOpt{withBearer(oldShared)}, "/v1/models", 401},
		"old session token on /v1":  {[]reqOpt{withBearer(oldAgent)}, "/v1/models", 200},
		"old session token scope":   {[]reqOpt{withBearer(oldAgent), withHeader(sessionHeader, "s1")}, "/api/agent/children", 403},
		"new session token scope":   {[]reqOpt{withBearer(env.srv.cfg.SessionAgentToken("s1")), withHeader(sessionHeader, "s1")}, "/api/agent/children", 503},
		"old preview capability":    {nil, "/preview/s1/~" + oldCap + "/index.html", 401},
		"old token for ended agent": {[]reqOpt{withBearer(env.srv.tokenConfigWith(testToken).SessionAgentToken("gone"))}, "/v1/models", 401},
	} {
		if w := env.do("GET", c.path, "", c.opts...); w.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", name, w.Code, c.want, w.Body)
		}
	}
	// Once s1 ends, its old token is dead for good, even if s1 resumes.
	env.sessions.setState(session.StateStopped)
	if w := env.do("GET", "/v1/models", "", withBearer(oldAgent)); w.Code != 401 {
		t.Fatalf("old token after the session ended: %d", w.Code)
	}
	env.sessions.setState(session.StateRunning)
	if w := env.do("GET", "/v1/models", "", withBearer(oldAgent)); w.Code != 401 {
		t.Fatalf("old token after the session resumed: %d", w.Code)
	}
	// A browser on the admin cookie gets a fresh cookie; a device gets no token.
	w = env.do("POST", "/api/token/rotate", "", withCookie(res.Token), withHeader(csrfHeader, "1"))
	var res2 RotateResult
	_ = json.Unmarshal(w.Body.Bytes(), &res2)
	c := w.Result().Cookies()
	if w.Code != 200 || res2.Token != "" || len(c) != 1 || c[0].Value == res.Token || !c[0].HttpOnly {
		t.Fatalf("cookie rotate: %d %+v %+v", w.Code, res2, c)
	}
	w = env.do("POST", "/api/token/rotate", "", withBearer(devTok))
	if w.Code != 200 || strings.Contains(w.Body.String(), "token\"") {
		t.Fatalf("device rotate: %d %s", w.Code, w.Body)
	}
	n := 0
	for _, r := range env.records(t) {
		if r.Route == doctor.RotationRoute && r.Status == 200 {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d rotation records", n)
	}
	if doctor.LastRotation(env.audit).IsZero() {
		t.Fatal("LastRotation found nothing")
	}
}

// tokenConfigWith derives tokens from an arbitrary admin token (tests).
func (s *server) tokenConfigWith(tok string) *config.Config {
	return &config.Config{Server: config.ServerConfig{Token: tok}}
}

// Without a config file, rotation refuses rather than writing the default path.
func TestRotateNeedsConfigFile(t *testing.T) {
	env := newEnv(t, nil)
	if w := env.do("POST", "/api/token/rotate", "", withBearer(testToken)); w.Code != 500 {
		t.Fatalf("status %d", w.Code)
	}
	if env.srv.token() != testToken {
		t.Fatal("token changed without a saved config")
	}
}

// ---------- doctor ----------

func TestDoctorEndpoint(t *testing.T) {
	env := newSafetyEnv(t, nil)
	env.srv.doctorEnv = func(e doctor.Env) doctor.Env {
		e.GOOS = "linux"
		e.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
		e.Output = func(context.Context, string, ...string) (string, error) { return "", os.ErrNotExist }
		e.Bwrap = func(context.Context) (string, error) { return "", os.ErrNotExist }
		e.Reachable = func(context.Context, string) error { return os.ErrNotExist }
		e.DiskFree = func(string) (uint64, error) { return 100 << 30, nil }
		e.Getenv = func(string) string { return "" }
		e.UserConfig = t.TempDir()
		return e
	}
	w := env.do("GET", "/api/doctor", "", withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("doctor: %d", w.Code)
	}
	var rep doctor.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	ids := map[string]doctor.Status{}
	for _, c := range rep.Checks {
		ids[c.ID] = c.Status
		if c.Status != doctor.OK && c.Fix == "" && c.ID != "disk" {
			t.Errorf("%s: %s without a fix", c.ID, c.Status)
		}
	}
	if ids["git"] != doctor.Fail || ids["config"] != doctor.OK || ids["disk"] != doctor.OK || len(ids) != 21 {
		t.Fatalf("checks %v", ids)
	}
	if rep.Counts[doctor.Fail] == 0 {
		t.Fatalf("counts %v", rep.Counts)
	}
}

// A session create names the new session ids as targets and the task
// (masked, clipped) as the detail.
func TestAuditSessionCreateTargets(t *testing.T) {
	env := newSafetyEnv(t, nil)
	w := env.do("POST", "/api/sessions", `{"Agent":"shell","Task":"use hunter2-secret now"}`, withBearer(testToken))
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	rec := env.records(t)[0]
	if rec.Route != "POST /api/sessions" || strings.Join(rec.Targets, ",") != "s1" ||
		rec.Detail != "agent shell: use •••••• now" {
		t.Fatalf("create record %+v", rec)
	}
}
