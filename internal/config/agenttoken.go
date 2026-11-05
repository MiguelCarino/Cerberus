package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// AgentToken is the credential agents get as OPENAI_API_KEY. It is derived
// from Server.Token (HMAC-SHA256), so it changes when the token does, and the
// API accepts it for /v1 only: an agent, or a script it runs, can use the
// gateway but never the control API (accept, input, new sessions). It is ""
// when there is no server token.
func (c *Config) AgentToken() string {
	key := c.TokenSnapshot()
	if key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("cerberus agent gateway token v1"))
	return "cag-" + hex.EncodeToString(mac.Sum(nil))
}

// sessionTokenPrefix starts every per-session agent token.
const sessionTokenPrefix = "cas-"

// SessionAgentToken is the agent credential of one session, what the
// session manager injects as OPENAI_API_KEY: "cas-<id>.<mac>", where mac is
// HMAC-SHA256 keyed with Server.Token over the session id. Like AgentToken
// it works for /v1; in addition it proves which session holds it, so it
// also opens the restricted /api/agent/* scope for that session (and only
// for its descendants). It is "" when there is no server token or no id.
func (c *Config) SessionAgentToken(id string) string {
	key := c.TokenSnapshot()
	if key == "" || id == "" {
		return ""
	}
	return sessionTokenPrefix + id + "." + sessionMAC(key, id)
}

// VerifySessionAgentToken checks a token made by SessionAgentToken and
// returns the session id it belongs to. The comparison is constant-time.
func (c *Config) VerifySessionAgentToken(tok string) (string, bool) {
	key := c.TokenSnapshot()
	if key == "" || !strings.HasPrefix(tok, sessionTokenPrefix) {
		return "", false
	}
	id, mac, ok := strings.Cut(strings.TrimPrefix(tok, sessionTokenPrefix), ".")
	if !ok || id == "" || mac == "" {
		return "", false
	}
	want := sessionMAC(key, id)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) != 1 {
		return "", false
	}
	return id, true
}

func sessionMAC(key, id string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("cerberus session agent token v1\x00"))
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))
}
