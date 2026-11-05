package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pngBytes is a 1×1 PNG.
var pngBytes, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

// newVisionFake is newFake plus GET /v1/models answering models (nil = 404).
func newVisionFake(t *testing.T, models any, replies ...func(http.ResponseWriter, map[string]any)) (*fakeServer, string) {
	f := &fakeServer{t: t, replies: replies}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if models == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models)
	})
	mux.HandleFunc("/", f.serve)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL + "/v1"
}

func inboxOptions(t *testing.T, base string) options {
	o := testOptions(t, base)
	o.Inbox = "inbox"
	o.InboxSeen = filepath.Join(t.TempDir(), inboxSeenFile)
	dir := filepath.Join(o.Cwd, "inbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), bytes.Repeat([]byte("n"), 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	return o
}

// userParts returns the first user message's content: a string, or parts.
func userContent(t *testing.T, req map[string]any) any {
	t.Helper()
	for _, m := range req["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] == "user" {
			return mm["content"]
		}
	}
	t.Fatal("no user message")
	return nil
}

func TestInboxImageReachesVisionServer(t *testing.T) {
	models := map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "test-model", "local": true, "vision": true}}}
	f, base := newVisionFake(t, models, sse(content("I see a pixel.")))
	o := inboxOptions(t, base)
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	parts, ok := userContent(t, reqs[0]).([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content %#v", userContent(t, reqs[0]))
	}
	text := parts[0].(map[string]any)
	img := parts[1].(map[string]any)
	if text["type"] != "text" || !strings.HasPrefix(text["text"].(string), "do the thing\n\n") ||
		!strings.Contains(text["text"].(string), "The user added files to ./inbox: a.png (") ||
		!strings.Contains(text["text"].(string), "notes.txt (2 KB)") {
		t.Fatalf("text part %#v", text)
	}
	url := img["image_url"].(map[string]any)["url"].(string)
	if img["type"] != "image_url" || url != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("image part %#v", img)
	}
	// The conversation keeps the image for a resume, as parts.
	raw, _ := json.Marshal(message{Role: "user", Content: "x", Images: []string{url}})
	var back message
	if err := json.Unmarshal(raw, &back); err != nil || back.Content != "x" || len(back.Images) != 1 || back.Images[0] != url {
		t.Fatalf("round trip %+v %v", back, err)
	}
	// Announced once: the next run sees nothing new.
	if again := scanInbox(o.Cwd, o.Inbox, o.InboxSeen, nil); len(again) != 0 {
		t.Fatalf("announced again: %+v", again)
	}
}

func TestInboxImageRefusedFallsBackToText(t *testing.T) {
	f, base := newVisionFake(t, nil,
		func(w http.ResponseWriter, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"image input is not supported by this model"}}`))
		},
		sse(content("Read it from the inbox.")),
	)
	o := inboxOptions(t, base)
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	if _, ok := userContent(t, reqs[0]).([]any); !ok {
		t.Fatal("first request carried no image")
	}
	text, ok := userContent(t, reqs[1]).(string)
	if !ok || !strings.Contains(text, "a.png") || !strings.Contains(text, "could not be shown") || strings.Contains(text, "is attached") {
		t.Fatalf("retry content %#v", userContent(t, reqs[1]))
	}
	if reqs[1]["tools"] == nil {
		t.Fatal("an image refusal turned tools off")
	}
	if !strings.Contains(out.String(), "takes no images") {
		t.Fatalf("no notice:\n%s", out.String())
	}
}

func TestInboxLocalTextModelGetsMentionOnly(t *testing.T) {
	models := map[string]any{"data": []any{map[string]any{"id": "test-model", "local": true}}}
	f, base := newVisionFake(t, models, sse(content("ok")))
	o := inboxOptions(t, base)
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	text, ok := userContent(t, f.requests()[0]).(string)
	if !ok || !strings.Contains(text, "./inbox: a.png") || strings.Contains(text, "attached") {
		t.Fatalf("content %#v", userContent(t, f.requests()[0]))
	}
}

func TestScanInboxLedger(t *testing.T) {
	cwd := t.TempDir()
	seen := filepath.Join(t.TempDir(), inboxSeenFile)
	dir := filepath.Join(cwd, "inbox")
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := scanInbox(cwd, "inbox", seen, nil); got != nil {
		t.Fatalf("no inbox: %v", got)
	}
	write("a.txt", "1")
	write("sub/b.txt", "2")
	write(".hidden/c.txt", "3")
	write("secrets.json", "{}")
	got := scanInbox(cwd, "inbox", seen, []string{"**/secrets.json"})
	if len(got) != 2 || got[0].Rel != "a.txt" || got[1].Rel != "sub/b.txt" {
		t.Fatalf("first scan %+v", got)
	}
	if got := scanInbox(cwd, "inbox", seen, nil); len(got) != 1 || got[0].Rel != "secrets.json" {
		t.Fatalf("second scan %+v", got) // only the file deny_paths hid before, now allowed
	}
	future := time.Now().Add(time.Hour)
	write("a.txt", "changed")
	_ = os.Chtimes(filepath.Join(dir, "a.txt"), future, future)
	write("new.png", "x")
	got = scanInbox(cwd, "inbox", seen, nil)
	if len(got) != 2 || got[0].Rel != "a.txt" || got[1].Rel != "new.png" {
		t.Fatalf("after changes %+v", got)
	}
	if n := inboxNote("inbox", got, 0); n != "The user added files to ./inbox: a.txt (7 B), new.png (1 B)." {
		t.Fatalf("note %q", n)
	}
	if os.Symlink("/etc/passwd", filepath.Join(dir, "link")) == nil {
		for _, f := range scanInbox(cwd, "inbox", seen, nil) {
			if f.Rel == "link" {
				t.Fatal("symlink listed")
			}
		}
	}
}

func TestImageRefusalClassifier(t *testing.T) {
	for _, c := range []struct {
		err  *ServerError
		want bool
	}{
		{&ServerError{Status: 400, Message: "Invalid content: expected string, got array"}, true},
		{&ServerError{Status: 400, Message: "model does not support vision"}, true},
		{&ServerError{Status: 400, Message: "context length exceeded"}, false},
		{&ServerError{Status: 401, Message: "image"}, false},
		{&ServerError{Status: 429, Message: "slow down"}, false},
		{&ServerError{Status: 500, Message: "boom"}, true},
	} {
		if got := imageRefusal(c.err); got != c.want {
			t.Errorf("imageRefusal(%v) = %v", c.err, got)
		}
	}
}

func TestInboxLedgerWaitsForSuccessfulRequest(t *testing.T) {
	f, base := newVisionFake(t, nil, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	})
	o := inboxOptions(t, base)
	var out bytes.Buffer
	if code := run(context.Background(), o, strings.NewReader(""), &out); code == exitOK {
		t.Fatalf("a failed request exited OK:\n%s", out.String())
	}
	if len(f.requests()) == 0 {
		t.Fatal("no request sent")
	}
	// The model never got the note: the next run announces the files again.
	if again := scanInbox(o.Cwd, o.Inbox, o.InboxSeen, nil); len(again) != 2 {
		t.Fatalf("files marked seen after a failed run: %+v", again)
	}
}
