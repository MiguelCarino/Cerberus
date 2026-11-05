package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"carino.systems/cerberus/internal/config"
	"testing"
	"time"
)

// TestTrackRefusesAfterCut: a request whose credential was checked before
// a revocation, but which registers after the cut, is refused instead of
// running on the revoked credential.
func TestTrackRefusesAfterCut(t *testing.T) {
	c := newConnTracker()
	since := c.epoch() // credential checked here
	c.cut("device:d1") // revocation lands before the request is tracked
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, live := c.track("device:d1", cancel, since); live {
		t.Fatal("tracked a request on a credential revoked after it was checked")
	}
	// Another key is unaffected, and a request checked after the cut runs.
	if rel, live := c.track("admin", cancel, since); !live {
		t.Fatal("unrelated key refused")
	} else {
		rel()
	}
	if rel, live := c.track("device:d1", cancel, c.epoch()); !live {
		t.Fatal("request checked after the cut refused")
	} else {
		rel()
	}
}

// TestAdmitRefusesRevokedBetweenCheckAndTrack drives admit with an epoch
// read before a device revocation.
func TestAdmitRefusesRevokedBetweenCheckAndTrack(t *testing.T) {
	env := newSafetyEnv(t, nil)
	since := env.srv.conns.epoch()
	env.srv.conns.cut("device:dX")
	r := env.newRequest("GET", "/api/status")
	r = r.WithContext(context.WithValue(r.Context(), epochKey{}, since))
	w := env.serveAdmit(r, principal{Actor: "owner:x", Kind: kindDevice, Device: "dX"})
	if w != 401 {
		t.Fatalf("admit after revocation: %d", w)
	}
}

// TestCrossSiteTokenLoginNotCounted: <img src="http://127.0.0.1:7340/?token=…">
// on a web page cannot use up the address's failure budget.
func TestCrossSiteTokenLoginNotCounted(t *testing.T) {
	env := newSafetyEnv(t, nil)
	victim := from("127.0.0.1:4000")
	for i := 0; i < 10; i++ {
		env.do("GET", fmt.Sprintf("/?token=x%d", i), "", victim, withHost("127.0.0.1:7340"),
			withHeader("Sec-Fetch-Site", "cross-site"))
	}
	if w := env.do("GET", "/api/status", "", victim, withHost("127.0.0.1:7340")); w.Code == 429 {
		t.Fatal("cross-site ?token= failures blocked the address")
	}
	// Even after a real block, valid credentials from loopback still work.
	for i := 0; i < 6; i++ {
		env.do("GET", "/api/status", "", victim, withHost("127.0.0.1:7340"), withBearer(fmt.Sprintf("junk-%d", i)))
	}
	if w := env.do("GET", "/api/status", "", victim, withHost("127.0.0.1:7340"), withBearer("junk-9")); w.Code != 429 {
		t.Fatalf("junk after the block: %d", w.Code)
	}
	if w := env.do("GET", "/api/status", "", victim, withHost("127.0.0.1:7340"), withBearer(testToken)); w.Code != 200 {
		t.Fatalf("admin token on a blocked loopback: %d", w.Code)
	}
}

func (e *safetyEnv) newRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "192.0.2.10:5555"
	return r
}

// serveAdmit runs admit with a handler that answers 200.
func (e *safetyEnv) serveAdmit(r *http.Request, pr principal) int {
	w := httptest.NewRecorder()
	e.srv.admit(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), pr)
	return w.Code
}

// TestPairURLsPreferReachable: a pairing made from the desktop UI on
// 127.0.0.1 gets a QR link a phone can open (LAN or tailnet), not loopback.
func TestPairURLsPreferReachable(t *testing.T) {
	sc := config.ServerConfig{Listen: "127.0.0.1:7340", ExtraListen: []string{"100.64.1.2:7340"}}
	got := pairURLs(sc, "http://127.0.0.1:7340", nil)
	if len(got) != 2 || got[0] != "http://100.64.1.2:7340" || got[1] != "http://127.0.0.1:7340" {
		t.Fatalf("pairURLs = %v", got)
	}
	sc = config.ServerConfig{Listen: "0.0.0.0:7340"}
	if got := pairURLs(sc, "http://localhost:7340", []string{"192.168.1.5"}); got[0] != "http://192.168.1.5:7340" {
		t.Fatalf("wildcard: %v", got)
	}
	sc.PublicURL = "https://box.tail.ts.net/"
	if got := pairURLs(sc, "http://192.168.1.5:7340", nil); got[0] != "https://box.tail.ts.net" || got[1] != "http://192.168.1.5:7340" {
		t.Fatalf("public url: %v", got)
	}
	if got := pairURLs(config.ServerConfig{Listen: "127.0.0.1:7340"}, "http://127.0.0.1:7340", nil); len(got) != 1 || got[0] != "http://127.0.0.1:7340" {
		t.Fatalf("loopback only: %v", got)
	}
}

// TestPairEndpointUsesReachableURL drives POST /api/devices from loopback.
func TestPairEndpointUsesReachableURL(t *testing.T) {
	env := newSafetyEnv(t, func(c *config.Config) {
		c.Server.Listen = "127.0.0.1:7340"
		c.Server.ExtraListen = []string{"192.168.7.9:7340"}
	})
	env.srv.ifaceIPs = func() []string { return nil }
	w := env.do("POST", "/api/devices", `{"name":"phone"}`, withBearer(testToken), local("127.0.0.1:5000"))
	if w.Code != 200 {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	var res PairResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !strings.HasPrefix(res.URL, "http://192.168.7.9:7340/?token=cdv-") || len(res.URLs) != 2 {
		t.Fatalf("pair url %q, urls %v", res.URL, res.URLs)
	}
}

// TestTerminalInputAuditCoalesced: keys sent one request each become one
// audit record per line, so a vault value typed by hand is redacted whole.
func TestTerminalInputAuditCoalesced(t *testing.T) {
	env := newSafetyEnv(t, nil)
	env.srv.inAudit.idle = 50 * time.Millisecond
	env.srv.control = visibleInput{}
	for _, k := range []string{"echo ", "hun", "ter2-", "sec", "ret", "\r"} {
		body, _ := json.Marshal(map[string]string{"data": k})
		if w := env.do("POST", "/api/sessions/s1/input", string(body), withBearer(testToken)); w.Code != 200 {
			t.Fatalf("input %q: %d", k, w.Code)
		}
	}
	// A partial line is written after the idle pause.
	env.do("POST", "/api/sessions/s1/input", `{"data":"l"}`, withBearer(testToken))
	env.do("POST", "/api/sessions/s1/input", `{"data":"s"}`, withBearer(testToken))
	deadline := time.Now().Add(3 * time.Second)
	var inputs []string
	for time.Now().Before(deadline) {
		inputs = inputs[:0]
		for _, r := range env.records(t) {
			if strings.HasSuffix(r.Route, "/{action}") || strings.Contains(r.Route, "input") {
				inputs = append(inputs, r.Detail)
			}
		}
		if len(inputs) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(inputs) != 2 {
		t.Fatalf("input records: %q", inputs)
	}
	if strings.HasPrefix(inputs[1], "input: echo") { // newest first
		inputs[0], inputs[1] = inputs[1], inputs[0]
	}
	if strings.Contains(inputs[0], "hunter2") || !strings.HasPrefix(inputs[0], "input: echo ") {
		t.Fatalf("line record not joined or not redacted: %q", inputs[0])
	}
	if inputs[1] != "input: ls" {
		t.Fatalf("idle record: %q", inputs[1])
	}
}

// visibleInput is a control service whose terminals echo (input not hidden).
type visibleInput struct{ controlService }

func (visibleInput) InputHidden(string) bool { return false }

func (visibleInput) Record(string, string, string) {}
