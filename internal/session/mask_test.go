//go:build !windows

package session

import "testing"

// TestMaskSecretsHidesVaultValues: the Monitor's process command lines go
// through MaskSecrets, so a secret an agent expanded into argv is masked.
func TestMaskSecretsHidesVaultValues(t *testing.T) {
	const secret = "ghp_0123456789abcdefXYZ"
	e := newTestEnv(t, nil, func(e *testEnv) {
		withFileVault(e, map[string]string{"gh": secret, "short": "abc"})
	})
	got := e.m.MaskSecrets([]string{"curl -H Authorization: token " + secret + " https://x", "ls abc"})
	if got[0] != "curl -H Authorization: token "+redactMask+" https://x" || got[1] != "ls abc" {
		t.Fatalf("masked = %q", got)
	}
}
