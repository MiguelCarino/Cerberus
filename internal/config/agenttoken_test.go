package config

import "testing"

func TestAgentToken(t *testing.T) {
	c := Defaults()
	if c.AgentToken() != "" {
		t.Fatal("no server token must mean no agent token")
	}
	c.Server.Token = "a"
	a := c.AgentToken()
	c.Server.Token = "b"
	if a == "" || a == "a" || a == c.AgentToken() {
		t.Fatalf("agent token must be derived from, and differ from, the server token: %q", a)
	}
}

func TestSessionAgentToken(t *testing.T) {
	c := Defaults()
	if c.SessionAgentToken("s1") != "" {
		t.Fatal("no server token must mean no session token")
	}
	c.Server.Token = "a"
	tok := c.SessionAgentToken("20261101-120000-abcdef")
	if id, ok := c.VerifySessionAgentToken(tok); !ok || id != "20261101-120000-abcdef" {
		t.Fatalf("verify %q = %q %v", tok, id, ok)
	}
	if tok == c.AgentToken() || c.SessionAgentToken("other") == tok {
		t.Fatal("session tokens must differ per session and from the shared token")
	}
	for _, bad := range []string{"", "a", c.AgentToken(), "cas-s1.", "cas-.abc",
		"cas-other." + tok[len("cas-20261101-120000-abcdef."):], tok + "0"} {
		if _, ok := c.VerifySessionAgentToken(bad); ok {
			t.Fatalf("%q verified", bad)
		}
	}
	c.Server.Token = "b"
	if _, ok := c.VerifySessionAgentToken(tok); ok {
		t.Fatal("a token must stop working when the server token changes")
	}
}
