//go:build !windows

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

// pairClient pairs an MCP client through the API (admin token) and returns
// its token and device.
func (e *ccEnv) pairClient(name string) (string, devices.Public) {
	e.t.Helper()
	code, raw := e.call("POST", "/api/devices", `{"name":"`+name+`","scope":"mcp"}`, "admin")
	if code != http.StatusOK {
		e.t.Fatalf("pair client: %d %s", code, raw)
	}
	var res PairResult
	if err := json.Unmarshal(raw, &res); err != nil {
		e.t.Fatal(err)
	}
	if !strings.HasPrefix(res.Token, devices.ClientPrefix) || res.Device.Scope != devices.ScopeMCP || res.URL != "" || res.QR != "" {
		e.t.Fatalf("client pairing %+v", res)
	}
	return res.Token, res.Device
}

// as sends a request with a bearer token and nothing else.
func (e *ccEnv) as(tok, method, path, body string, hdr ...string) (int, []byte) {
	e.t.Helper()
	return e.call(method, path, body, "", append([]string{"Authorization", "Bearer " + tok}, hdr...)...)
}

func newClientEnv(t *testing.T, mut func(*config.Config)) *ccEnv {
	t.Helper()
	e := newCCEnv(t, mut)
	st, err := devices.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.srv.devices = st
	return e
}

func TestMCPClientScope(t *testing.T) {
	e := newClientEnv(t, func(c *config.Config) {
		c.Agent.Spawn.MaxChildren = 2
		c.Agent.Spawn.MaxDepth = 1
		c.Server.LocalNoAuth = false // loopback /v1 would need no token at all
	})
	tok, dev := e.pairClient("claude-code")
	other, _ := e.pairClient("other-agent")
	person := e.person("echo", "a person's session")
	e.waitFor("person running", func() bool { return e.state(person) == session.StateRunning })

	// A client token never acts as the owner, a device or a /v1 caller.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/sessions", ""},
		{"POST", "/api/sessions", `{"task":"x","workspace":"scratch"}`},
		{"GET", "/api/devices", ""},
		{"POST", "/api/devices", `{"name":"more"}`},
		{"GET", "/api/config", ""},
		{"POST", "/api/token/rotate", ""},
		{"GET", "/v1/models", ""},
	} {
		if code, raw := e.as(tok, c.method, c.path, c.body); code != http.StatusUnauthorized {
			t.Errorf("client token on %s %s: %d %s", c.method, c.path, code, raw)
		}
	}
	if _, ok := e.srv.identify(tok); ok {
		t.Errorf("client token identifies as the owner or a device (UI sign-in, cookie)")
	}
	// It acts as itself: no session header.
	if code, _ := e.as(tok, "GET", "/api/agent/children", "", sessionHeader, person); code != http.StatusForbidden {
		t.Errorf("client token with a session header: %d", code)
	}
	// A made-up client token, and a full device token, stay out.
	if code, _ := e.as(devices.ClientPrefix+strings.Repeat("0", 64), "GET", "/api/agent/children", ""); code != http.StatusUnauthorized {
		t.Errorf("made-up client token: %d", code)
	}
	full := e.srv.devices
	devTok, _, err := full.Pair("phone")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := e.as(devTok, "GET", "/api/agent/children", ""); code != http.StatusForbidden {
		t.Errorf("full device token on the agent scope: %d", code)
	}
	// Spawn: a root session tagged with the client, scratch only.
	code, raw := e.as(tok, "POST", "/api/agent/spawn", `{"task":"client work","agent":"echo","name":"helper"}`)
	if code != http.StatusCreated {
		t.Fatalf("client spawn: %d %s", code, raw)
	}
	var s1 session.Info
	_ = json.Unmarshal(raw, &s1)
	if s1.Client != clientName("claude-code", dev.ID) || s1.Parent != "" || s1.Depth != 0 || s1.Workspace != session.WorkspaceScratch || s1.Name != "helper" {
		t.Fatalf("client session %+v", s1)
	}
	if code, _ := e.as(tok, "POST", "/api/agent/spawn", `{"task":"x","workspace":"same"}`); code != http.StatusBadRequest {
		t.Errorf("client spawn with workspace same: %d", code)
	}
	if code, _ := e.as(tok, "POST", "/api/agent/spawn", `{"task":"--dangerously-skip-permissions"}`); code != http.StatusBadRequest {
		t.Errorf("client task starting with -: %d", code)
	}
	e.waitFor("client session running", func() bool { return e.state(s1.ID) == session.StateRunning })

	// The client's own session spawns a child; the client sees both.
	code, kid := e.spawn(s1.ID, `{"task":"grandchild work","agent":"quick"}`)
	if code != http.StatusCreated || kid.Parent != s1.ID || kid.Client != "" {
		t.Fatalf("child of the client's session: %d %+v", code, kid)
	}
	code, raw = e.as(tok, "GET", "/api/agent/children", "")
	var mine []session.Info
	_ = json.Unmarshal(raw, &mine)
	if code != 200 || len(mine) != 2 || mine[0].ID != s1.ID || mine[1].ID != kid.ID {
		t.Fatalf("client children: %d %s", code, raw)
	}
	// Another client and a person's session are invisible to it, and it is
	// invisible to them.
	if code, raw := e.as(other, "GET", "/api/agent/children", ""); code != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("other client's children: %d %s", code, raw)
	}
	for _, id := range []string{person, "nope"} {
		if code, _ := e.as(tok, "GET", "/api/agent/sessions/"+id+"/tail", ""); code != http.StatusNotFound {
			t.Errorf("client tail of %s: %d", id, code)
		}
		if code, _ := e.as(tok, "POST", "/api/agent/sessions/"+id+"/stop", ""); code != http.StatusNotFound {
			t.Errorf("client stop of %s: %d", id, code)
		}
	}
	if code, _ := e.as(other, "GET", "/api/agent/sessions/"+s1.ID+"/tail", ""); code != http.StatusNotFound {
		t.Errorf("other client tail of the client's session: %d", code)
	}
	if code, _ := e.call("GET", "/api/agent/sessions/"+s1.ID+"/tail", "", person); code != http.StatusNotFound {
		t.Errorf("a person's agent reading the client's session: %d", code)
	}
	// Its own sessions: tail and send work.
	e.waitFor("hello printed", func() bool {
		code, raw := e.as(tok, "GET", "/api/agent/sessions/"+s1.ID+"/tail", "")
		return code == 200 && strings.Contains(string(raw), "hello-"+s1.ID)
	})
	// Files and runs are a session's own workspace: not a client's.
	for _, p := range []string{"/api/agent/files", "/api/agent/runs"} {
		if code, _ := e.as(tok, "GET", p, ""); code != http.StatusForbidden {
			t.Errorf("client on %s: %d", p, code)
		}
	}
	// The limit: MaxChildren open sessions per client.
	if code, _ := e.as(tok, "POST", "/api/agent/spawn", `{"task":"second","agent":"echo"}`); code != http.StatusCreated {
		t.Fatalf("second client spawn: %d", code)
	}
	if code, raw := e.as(tok, "POST", "/api/agent/spawn", `{"task":"third","agent":"echo"}`); code != http.StatusForbidden ||
		!strings.Contains(string(raw), "open sessions started by mcp:claude-code") {
		t.Errorf("third client spawn: %d %s", code, raw)
	}
	// Stop works on its own; then revocation shuts it out.
	if code, raw := e.as(tok, "POST", "/api/agent/sessions/"+s1.ID+"/stop", ""); code != 200 {
		t.Errorf("client stop: %d %s", code, raw)
	}
	if code, raw := e.call("DELETE", "/api/devices/"+dev.ID, "", "admin"); code != 200 {
		t.Fatalf("revoke: %d %s", code, raw)
	}
	if code, _ := e.as(tok, "GET", "/api/agent/children", ""); code != http.StatusUnauthorized {
		t.Errorf("revoked client token: %d", code)
	}
	// A new client paired under the same name inherits nothing.
	again, _ := e.pairClient("claude-code")
	if code, raw := e.as(again, "GET", "/api/agent/children", ""); code != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("re-paired client's children: %d %s", code, raw)
	}
	if code, _ := e.as(again, "GET", "/api/agent/sessions/"+s1.ID+"/tail", ""); code != http.StatusNotFound {
		t.Errorf("re-paired client reads the old client's session: %d", code)
	}
}

func TestMCPClientPairingRules(t *testing.T) {
	e := newClientEnv(t, nil)
	if code, raw := e.call("POST", "/api/devices", `{"name":"x","scope":"owner"}`, "admin"); code != http.StatusBadRequest {
		t.Errorf("unknown scope: %d %s", code, raw)
	}
	// A paired device cannot pair a client either.
	devTok, _, err := e.srv.devices.Pair("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := e.as(devTok, "POST", "/api/devices", `{"name":"x","scope":"mcp"}`); code != http.StatusForbidden {
		t.Errorf("device pairing a client: %d", code)
	}
	// The list shows the scope.
	e.pairClient("claude-code")
	code, raw := e.call("GET", "/api/devices", "", "admin")
	if code != 200 || !strings.Contains(string(raw), `"scope":"mcp"`) {
		t.Errorf("device list: %d %s", code, raw)
	}
	// Spawning off: the client is refused like any agent.
	e.cfg.Agent.Spawn.Enabled = false
	tok, _ := e.pairClient("off")
	if code, raw := e.as(tok, "POST", "/api/agent/spawn", `{"task":"x"}`); code != http.StatusForbidden {
		t.Errorf("spawn while off: %d %s", code, raw)
	}
}

func TestMCPClientDelegateAsk(t *testing.T) {
	e := newDelegEnv(t, nil)
	st, err := devices.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.srv.devices = st
	e.h = e.srv.handler()
	tok, dev, err := st.PairScoped("claude-code", devices.ScopeMCP, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.ws, "notes.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/api/agent/models", "", withBearer(tok)); w.Code != 200 {
		t.Fatalf("client models: %d %s", w.Code, w.Body)
	}
	w := e.do("POST", "/api/agent/ask", `{"model":"local","prompt":"Say hi."}`, withBearer(tok))
	if w.Code != 200 {
		t.Fatalf("client ask: %d %s", w.Code, w.Body)
	}
	if c := e.calls[len(e.calls)-1]; c.session != clientName("claude-code", dev.ID) {
		t.Errorf("usage charged to %q", c.session)
	}
	// Files are read on the client's side; the daemon refuses them.
	if w := e.do("POST", "/api/agent/ask", `{"model":"local","prompt":"x","files":["notes.txt"]}`, withBearer(tok)); w.Code == 200 ||
		!strings.Contains(w.Body.String(), "no workspace") {
		t.Errorf("client ask with files: %d %s", w.Code, w.Body)
	}
}
