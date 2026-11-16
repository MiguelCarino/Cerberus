package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/engine"
)

// TestPrefillBusyAndHints: a llama.cpp stream's prompt_progress frames show
// in Busy() while the prompt is read, go out as hints, and end with a Done
// hint at the first output (web-api-ops-8).
func TestPrefillBusyAndHints(t *testing.T) {
	release := make(chan struct{})
	reading := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[],"prompt_progress":{"total":48000,"cache":0,"processed":12000,"time_ms":900}}

`)
		w.(http.Flusher).Flush()
		close(reading)
		<-release
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: [DONE]

`)
	})
	h.gw.SetStall(120)
	hints, cancel := h.gw.SubscribePrefill()
	defer cancel()

	done := make(chan string)
	go func() { done <- readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`).Body) }()
	<-reading
	var b Busy
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b = h.gw.Busy()["qwen"]; b.Prefill != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b.Prefill == nil || b.Prefill.Processed != 12000 || b.Prefill.Total != 48000 || b.Prefill.Session != "s1" {
		t.Fatalf("busy while reading: %+v %+v", b, b.Prefill)
	}
	first := <-hints
	if first.Done || first.Processed != 12000 || first.Model != "qwen" || first.Session != "s1" {
		t.Fatalf("first hint %+v", first)
	}
	close(release)
	body := <-done
	if strings.Contains(body, "prompt_progress") {
		t.Errorf("progress reached a client that did not ask:\n%s", body)
	}
	var last engine.Prefill
	select {
	case last = <-hints:
	case <-time.After(5 * time.Second):
		t.Fatal("no Done hint")
	}
	if !last.Done || last.Total != 48000 {
		t.Fatalf("last hint %+v", last)
	}
	if b := h.gw.Busy()["qwen"]; b.Prefill != nil {
		t.Fatalf("prefill after the first output: %+v", b.Prefill)
	}
	// Exactly one Done.
	select {
	case extra := <-hints:
		t.Fatalf("extra hint %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestReturnProgressStrippedForCloud: llama.cpp's own field never reaches
// another engine or a cloud API.
func TestReturnProgressStrippedForCloud(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	h.src.local = false
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true,"return_progress":true}`).Body)
	if _, ok := h.up.body["return_progress"]; ok {
		t.Fatalf("return_progress sent to a cloud endpoint: %v", h.up.body)
	}
}
