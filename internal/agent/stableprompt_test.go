package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// withoutSentNote is a request's user turn without the SentNote the run
// added to it; the test fails when the note is missing.
func withoutSentNote(t *testing.T, content any) string {
	t.Helper()
	s, _ := content.(string)
	i := strings.LastIndex(s, "\n\n[sent ")
	if i < 0 || !strings.HasSuffix(s, " local time]") {
		t.Errorf("no sent note on %q", s)
		return s
	}
	return s[:i]
}

func TestSystemPromptNamesTheDayOnly(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 15, h, m, 0, 0, time.Local) }
	a := systemPrompt("/w", "ask", true, nil, at(9, 1))
	if b := systemPrompt("/w", "ask", true, nil, at(9, 6)); a != b {
		t.Fatalf("the prompt changed within the day:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "Today is Sunday 2026-11-15.") || strings.Contains(a, "09:01") {
		t.Fatalf("prompt %q", a)
	}
	if got := SentNote(at(9, 1)); got != "[sent Sunday 2026-11-15 09:01 local time]" {
		t.Fatalf("SentNote %q", got)
	}
	if NewestUserNote([]Message{{Role: "user"}, {Role: "assistant"}}, at(9, 1)) != nil {
		t.Fatal("a note for a run that answers no user turn")
	}
	if n := NewestUserNote([]Message{{Role: "user"}, {Role: "assistant"}, {Role: "user"}}, at(9, 1)); len(n) != 1 || n[2] == "" {
		t.Fatalf("notes %v", n)
	}
}

// Notes go on the user turns in every request of the run and nowhere else:
// not in the result, not in what OnSave keeps; a note for a message that
// is not a user turn is ignored. The system prompt is the same on every
// request.
func TestNotesOnlyInRequests(t *testing.T) {
	f, base := newFake(t, sse(toolChunks([2]string{"list_dir", `{"path":"."}`})...), sse(content("Done.")))
	o := engineOptions(t, base, ModeBypass, nil)
	o.Messages = []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yes?"}, {Role: "user", Content: "go"}}
	o.Notes = map[int]string{0: "[sent A]", 1: "[not a user turn]", 2: "[sent B]", 9: "[no such message]"}
	var saved []Message
	o.OnSave = func(_ string, all []Message) { saved = all }
	res := Run(context.Background(), o, nil)
	if res.Err != nil || !res.Done.OK {
		t.Fatalf("result %+v", res)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	var system string
	for i, r := range reqs {
		msgs := r["messages"].([]any)
		get := func(j int) string { return msgs[j].(map[string]any)["content"].(string) }
		if i == 0 {
			system = get(0)
		} else if get(0) != system {
			t.Errorf("request %d changed the system prompt", i)
		}
		if get(1) != "hi\n\n[sent A]" || get(2) != "yes?" || get(3) != "go\n\n[sent B]" {
			t.Errorf("request %d: %q %q %q", i, get(1), get(2), get(3))
		}
	}
	for _, ms := range [][]Message{res.Messages, saved} {
		if ms[0].Content != "hi" || ms[2].Content != "go" {
			t.Errorf("a note was kept: %q %q", ms[0].Content, ms[2].Content)
		}
	}
}
