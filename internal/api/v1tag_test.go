package api

import (
	"net/http"
	"testing"
)

// A person's key cannot tag its /v1 usage onto another user's session.
func TestV1SessionTagOwnedOnly(t *testing.T) {
	e := newUsersEnv(t)
	var tags []string
	e.srv.gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tags = append(tags, r.Header.Get(sessionHeader))
		w.WriteHeader(200)
	})
	e.ss.add("s-cy", e.u["cy"].ID)
	e.ss.add("s-bob", e.u["bob"].ID)
	key, err := e.st.NewAPIKey(e.u["bob"].ID)
	if err != nil {
		t.Fatal(err)
	}
	send := func(id string) string {
		t.Helper()
		w := e.do("POST", "/v1/chat/completions", "{}", withBearer(key), withHeader("Content-Type", "application/json"),
			withHeader(sessionHeader, id))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body)
		}
		return tags[len(tags)-1]
	}
	if got := send("s-cy"); got != "" {
		t.Fatalf("bob's key tagged cy's session: %q", got)
	}
	if got := send("s-bob"); got != "s-bob" {
		t.Fatalf("bob's own session tag dropped: %q", got)
	}
}
