package api

import (
	"net/http/httptest"
	"testing"
)

// The Anthropic SDKs and Claude Code send their key as x-api-key (G1); it
// counts on /v1 only.
func TestBearerXAPIKey(t *testing.T) {
	cases := []struct{ path, auth, key, want string }{
		{"/v1/messages", "", "tok", "tok"},
		{"/v1/messages", "Bearer abc", "tok", "abc"},
		{"/api/status", "", "tok", ""},
		{"/v1/models", "", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if c.key != "" {
			r.Header.Set("X-Api-Key", c.key)
		}
		if got := bearer(r); got != c.want {
			t.Errorf("%s auth=%q key=%q: got %q want %q", c.path, c.auth, c.key, got, c.want)
		}
	}
}
