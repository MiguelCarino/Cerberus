package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/inbox"
)

type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) run(_ context.Context, argv []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, argv)
	return nil
}

func newTest(cfg config.NotifyConfig) (*Notifier, *recorder, *time.Time) {
	n := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := &recorder{}
	n.run = rec.run
	clock := time.Unix(1000, 0)
	n.now = func() time.Time { return clock }
	return n, rec, &clock
}

func TestCommandSubstitutionAndRateLimit(t *testing.T) {
	n, rec, clock := newTest(config.NotifyConfig{Command: []string{"/bin/notify", "{title}", "[{urgency}] {body}"}})
	n.Send(Message{Title: "s1 waiting", Body: "needs input; $(rm -rf ~)", Urgency: "critical", SessionID: "s1"})
	n.Send(Message{Title: "s1 waiting", Body: "dup", SessionID: "s1"})             // rate-limited
	n.Send(Message{Title: "s1 passed", Body: "ok", SessionID: "s1", Urgency: "x"}) // other state
	n.Send(Message{Title: "s1 waiting", Body: "other session", SessionID: "s2"})
	*clock = clock.Add(11 * time.Second)
	n.Send(Message{Title: "s1 waiting", Body: "later", SessionID: "s1", Urgency: "low"})
	n.wg.Wait()

	want := [][]string{
		{"/bin/notify", "s1 waiting", "[critical] needs input; $(rm -rf ~)"},
		{"/bin/notify", "s1 passed", "[normal] ok"},
		{"/bin/notify", "s1 waiting", "[normal] other session"},
		{"/bin/notify", "s1 waiting", "[low] later"},
	}
	if len(rec.calls) != len(want) {
		t.Fatalf("calls = %q", rec.calls)
	}
	for _, w := range want {
		if !slices.ContainsFunc(rec.calls, func(c []string) bool { return slices.Equal(c, w) }) {
			t.Errorf("missing call %q in %q", w, rec.calls)
		}
	}
}

func TestNtfy(t *testing.T) {
	type got struct {
		body, title, prio, click string
	}
	ch := make(chan got, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		ch <- got{string(b), r.Header.Get("Title"), r.Header.Get("Priority"), r.Header.Get("Click")}
	}))
	defer srv.Close()

	tests := []struct {
		msg  Message
		want got
	}{
		{Message{Title: "Done\nnow", Body: "tests passed", Urgency: "low", URL: "http://x/s/1"}, got{"tests passed", "Done now", "2", "http://x/s/1"}},
		{Message{Title: "B", Body: "b", SessionID: "s"}, got{"b", "B", "3", ""}},
		{Message{Title: "C", Body: "c", Urgency: "critical"}, got{"c", "C", "5", ""}},
	}
	for _, tc := range tests {
		n, rec, _ := newTest(config.NotifyConfig{NtfyURL: srv.URL})
		n.notifySend = "" // keep the test independent of the host
		n.Send(tc.msg)
		n.wg.Wait()
		if g := <-ch; g != tc.want {
			t.Errorf("ntfy got %+v, want %+v", g, tc.want)
		}
		if len(rec.calls) != 0 {
			t.Errorf("unexpected command calls %q", rec.calls)
		}
	}
}

func TestNotifySendFallbackAndDisabled(t *testing.T) {
	n, rec, _ := newTest(config.NotifyConfig{})
	n.notifySend = "/usr/bin/notify-send"
	n.Send(Message{Title: "T", Body: "B"})
	n.wg.Wait()
	want := []string{"/usr/bin/notify-send", "-a", "Cerberus", "-u", "normal", "T", "B"}
	if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], want) {
		t.Fatalf("calls = %q", rec.calls)
	}

	n, rec, _ = newTest(config.NotifyConfig{Disabled: true, Command: []string{"x"}})
	n.Send(Message{Title: "T"})
	n.wg.Wait()
	if len(rec.calls) != 0 {
		t.Fatalf("disabled notifier ran %q", rec.calls)
	}

	var nilN *Notifier
	nilN.Send(Message{Title: "safe"})
}

func TestSendDoesNotBlock(t *testing.T) {
	n, _, _ := newTest(config.NotifyConfig{Command: []string{"x"}})
	release := make(chan struct{})
	n.run = func(context.Context, []string) error { <-release; return nil }
	done := make(chan struct{})
	go func() { n.Send(Message{Title: "slow"}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked")
	}
	close(release)
	n.wg.Wait()
}

func TestRateMapPruned(t *testing.T) {
	n, _, clock := newTest(config.NotifyConfig{})
	for i := range maxKeys {
		n.allow(string(rune('a' + i)))
	}
	*clock = clock.Add(time.Minute)
	n.allow("new")
	if len(n.last) != 1 {
		t.Fatalf("map size %d after prune", len(n.last))
	}
}

func TestRunCommandReal(t *testing.T) {
	if err := runCommand(context.Background(), []string{"go", "version"}); err != nil {
		t.Skip("go not on PATH:", err)
	}
}

type fakeInbox struct {
	mu    sync.Mutex
	items []inbox.Item
}

func (f *fakeInbox) Add(it inbox.Item) inbox.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, it)
	return it
}

func TestInboxReceivesEverySentMessage(t *testing.T) {
	n, rec, _ := newTest(config.NotifyConfig{Command: []string{"/bin/notify", "{title}"}})
	ib := &fakeInbox{}
	n.SetInbox(ib)
	n.Send(Message{Title: "Checks passed: x", Body: "ready", SessionID: "s1", URL: "https://h.ts.net/#/s/s1"})
	n.Send(Message{Title: "Checks passed: x", Body: "dup", SessionID: "s1"}) // rate-limited: not stored either
	n.Send(Message{Title: "Chat finished", SessionID: "chat-20261101-1-ab"})
	n.Send(Message{Title: "Cerberus: openai budget", Body: "past soft cap"})
	n.Send(Message{Title: "DAC tariff risk", Urgency: "critical", Kind: "alert"})
	n.Send(Message{Title: "Doctor", Kind: "doctor", Level: "warn", Link: "#/doctor"})
	n.wg.Wait()
	if len(rec.calls) != 5 {
		t.Fatalf("calls = %q", rec.calls)
	}
	want := []inbox.Item{
		{Kind: "session", Level: "info", Title: "Checks passed: x", Body: "ready", Link: "#/s/s1"},
		{Kind: "chat", Level: "info", Title: "Chat finished", Link: "#/chat/chat-20261101-1-ab"},
		{Kind: "budget", Level: "warn", Title: "Cerberus: openai budget", Body: "past soft cap"},
		{Kind: "alert", Level: "crit", Title: "DAC tariff risk"},
		{Kind: "doctor", Level: "warn", Title: "Doctor", Link: "#/doctor"},
	}
	if len(ib.items) != len(want) {
		t.Fatalf("inbox = %+v", ib.items)
	}
	for i, w := range want {
		if ib.items[i] != w {
			t.Errorf("item %d = %+v, want %+v", i, ib.items[i], w)
		}
	}
}

func TestInboxKeepsMessagesWhenChannelsDisabled(t *testing.T) {
	n, rec, _ := newTest(config.NotifyConfig{Command: []string{"/bin/notify"}, Disabled: true})
	ib := &fakeInbox{}
	n.SetInbox(ib)
	n.Send(Message{Title: "Session failed: y", SessionID: "s9"})
	n.wg.Wait()
	if len(rec.calls) != 0 {
		t.Fatalf("disabled still ran %q", rec.calls)
	}
	if len(ib.items) != 1 || ib.items[0].Level != "warn" || ib.items[0].Link != "#/s/s9" {
		t.Fatalf("inbox = %+v", ib.items)
	}
	n.SetInbox(nil)
	n.Send(Message{Title: "other"})
	if len(ib.items) != 1 {
		t.Fatal("stored after SetInbox(nil)")
	}
}

func TestInboxRealStore(t *testing.T) {
	st, err := inbox.Open(t.TempDir()+"/inbox.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, _, _ := newTest(config.NotifyConfig{Disabled: true})
	n.SetInbox(st)
	ch, cancel := st.Subscribe()
	defer cancel()
	n.Send(Message{Title: "Downloaded qwen", Kind: "download", Link: "#/models"})
	ev := <-ch
	if ev.Item == nil || ev.Item.Kind != "download" || ev.Item.ID == "" || ev.Unread != 1 {
		t.Fatalf("ev = %+v", ev)
	}
}
