package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestExchangeCapture(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo.\"}}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Plain answer."}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer up.Close()
	var on atomic.Bool
	on.Store(true)
	var mu sync.Mutex
	var got []Exchange
	gw := newGateway(&cloudSource{base: up.URL}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithExchange(on.Load, func(e Exchange) { mu.Lock(); got = append(got, e); mu.Unlock() }))
	send := func(model, body string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/chat/completions", strings.NewReader(body))
		req.Header.Set("X-Cerberus-Session", "s1")
		gw.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", model, rec.Code, rec.Body)
		}
	}
	msgs := `"messages":[{"role":"user","content":"hi"}]`
	send("acme/big", `{"model":"acme/big",`+msgs+`}`)
	send("acme/big", `{"model":"acme/big","stream":true,`+msgs+`}`)
	send("qwen", `{"model":"qwen",`+msgs+`}`) // local: never captured
	on.Store(false)
	send("acme/big", `{"model":"acme/big",`+msgs+`}`) // switched off
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Answer != "Plain answer." || got[1].Answer != "Hello." || got[0].Session != "s1" ||
		got[0].Model != "acme/big" || !strings.Contains(string(got[0].Messages), `"hi"`) {
		t.Fatalf("exchanges %+v", got)
	}
}
