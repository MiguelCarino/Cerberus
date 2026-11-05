package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/usage"
)

// fakeSource is a modelSource backed by one upstream URL.
type fakeSource struct {
	base     string
	local    bool
	headers  map[string]string
	acquired atomic.Int32
	released atomic.Int32
}

func (f *fakeSource) Models(context.Context) []engine.ModelInfo {
	return []engine.ModelInfo{
		{ID: "qwen", Backend: engine.BackendLlamaCpp, Local: true, Loaded: true, Ctx: 8192},
		{ID: "cloud/gpt", Backend: engine.BackendOpenAI},
	}
}

func (f *fakeSource) Acquire(_ context.Context, id string) (engine.Endpoint, func(), error) {
	switch id {
	case "qwen", "default":
	case "guard": // G2: the guard's classifier, served by the same fake engine
		f.acquired.Add(1)
		return engine.Endpoint{BaseURL: f.base + "/v1/", Model: "backend-guard", Local: true, Backend: engine.BackendLlamaCpp},
			func() { f.released.Add(1) }, nil
	case "full":
		return engine.Endpoint{}, nil, engine.ErrNoRoom
	default:
		return engine.Endpoint{}, nil, fmt.Errorf("%w: %s", engine.ErrUnknownModel, id)
	}
	f.acquired.Add(1)
	return engine.Endpoint{BaseURL: f.base + "/v1/", Model: "backend-qwen", Headers: f.headers,
		Local: f.local, Backend: engine.BackendLlamaCpp}, func() { f.released.Add(1) }, nil
}

// memStore records usage in memory.
type memStore struct {
	mu   sync.Mutex
	recs []usage.Record
}

func (m *memStore) Add(r usage.Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
}

func (m *memStore) last(t *testing.T) usage.Record {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recs) != 1 {
		t.Fatalf("got %d usage records, want 1", len(m.recs))
	}
	return m.recs[0]
}

// upstream captures what the gateway sent.
type upstream struct {
	mu      sync.Mutex
	path    string
	body    map[string]any
	headers http.Header
}

func (u *upstream) capture(r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(raw))) // the handler reads it too
	_ = json.Unmarshal(raw, &body)
	if body["model"] == "backend-guard" {
		return // the guard's classifier (G2): keep the main request's body
	}
	u.mu.Lock()
	u.path, u.body, u.headers = r.URL.Path, body, r.Header.Clone()
	u.mu.Unlock()
}

type harness struct {
	gw    *Gateway
	src   *fakeSource
	store *memStore
	up    *upstream
	srv   *httptest.Server // the gateway
}

// newHarness starts a fake upstream running h and a gateway in front of it.
// The energy meter advances 10 J per reading.
func newHarness(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *harness {
	t.Helper()
	up := &upstream{}
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.capture(r)
		h(w, r)
	}))
	t.Cleanup(us.Close)
	src := &fakeSource{base: us.URL, local: true, headers: map[string]string{"Authorization": "Bearer upstream-key"}}
	var joules atomic.Int64
	energy := func() float64 { return float64(joules.Add(10)) }
	store := &memStore{}
	gw := newGateway(src, energy, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return &harness{gw: gw, src: src, store: store, up: up, srv: srv}
}

func (h *harness) recorded() int {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return len(h.store.recs)
}

func (h *harness) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer cerberus-token")
	req.Header.Set("X-Cerberus-Client", "test")
	req.Header.Set("X-Cerberus-Session", "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const sseChunks = `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

`

const sseUsage = `data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5}}

`

func sseHandler(withUsage bool, flushes *atomic.Int32) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive, X-Secret")
		w.Header().Set("X-Secret", "hop")
		body := sseChunks
		if withUsage {
			body += sseUsage
		}
		body += "data: [DONE]\n\n"
		for _, line := range strings.SplitAfter(body, "\n") {
			_, _ = io.WriteString(w, line)
			w.(http.Flusher).Flush()
			if flushes != nil {
				flushes.Add(1)
			}
		}
	}
}

func TestStreamingStripsUsageAndRecords(t *testing.T) {
	h := newHarness(t, sseHandler(true, nil))
	resp := h.post(t, "/chat/completions", `{"model":"default","stream":true,"messages":[]}`)
	got := readAll(t, resp.Body)

	if want := sseChunks + "data: [DONE]\n\n"; got != want {
		t.Fatalf("client stream:\n%q\nwant\n%q", got, want)
	}
	if v := resp.Header.Get("X-Secret"); v != "" {
		t.Errorf("Connection-listed header forwarded: %q", v)
	}
	if h.up.path != "/v1/chat/completions" {
		t.Errorf("upstream path %q", h.up.path)
	}
	if h.up.body["model"] != "backend-qwen" {
		t.Errorf("model not rewritten: %v", h.up.body["model"])
	}
	so, _ := h.up.body["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Errorf("include_usage not set upstream: %v", h.up.body["stream_options"])
	}
	if a := h.up.headers.Get("Authorization"); a != "Bearer upstream-key" {
		t.Errorf("upstream Authorization %q (client token must not leak)", a)
	}

	rec := h.store.last(t)
	if rec.Status != 200 || rec.Error != "" {
		t.Errorf("status %d err %q", rec.Status, rec.Error)
	}
	if rec.PromptTokens != 7 || rec.CompletionTokens != 5 {
		t.Errorf("tokens %d/%d, want 7/5", rec.PromptTokens, rec.CompletionTokens)
	}
	if rec.Client != "test" || rec.SessionID != "s1" || rec.Backend != engine.BackendLlamaCpp || !rec.Local {
		t.Errorf("record identity wrong: %+v", rec)
	}
	if rec.EnergyJ != 10 || rec.TokPerJ != 0.5 {
		t.Errorf("energy %v tok/J %v, want 10 and 0.5", rec.EnergyJ, rec.TokPerJ)
	}
	// The whole stream arrives at once here: no measurable speed.
	if rec.TokPerSec != 0 || rec.TTFTMs < 0 || rec.DurationMs < rec.TTFTMs {
		t.Errorf("timings: %+v", rec)
	}
	if h.src.released.Load() != 1 {
		t.Errorf("release called %d times", h.src.released.Load())
	}
}

func TestStreamingKeepsUsageWhenClientAsked(t *testing.T) {
	h := newHarness(t, sseHandler(true, nil))
	resp := h.post(t, "/chat/completions",
		`{"model":"qwen","stream":true,"stream_options":{"include_usage":true}}`)
	if got := readAll(t, resp.Body); !strings.Contains(got, `"usage":{"prompt_tokens":7`) {
		t.Fatalf("usage chunk stripped although the client asked for it:\n%s", got)
	}
}

func TestStreamingCountsDeltasWithoutUsage(t *testing.T) {
	h := newHarness(t, sseHandler(false, nil))
	resp := h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`)
	readAll(t, resp.Body)
	if rec := h.store.last(t); rec.CompletionTokens != 2 || rec.PromptTokens != 0 {
		t.Errorf("tokens %d/%d, want 0/2 (delta count)", rec.PromptTokens, rec.CompletionTokens)
	}
}

// A stream whose deltas are spread over time gets a speed; one delivered in
// a single burst (a buffering proxy) does not get millions of tok/s.
func TestStreamingSpeedNeedsASpan(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{"a", "b", "c"} {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+c+`"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`).Body)
	rec := h.store.last(t)
	if rec.TokPerSec <= 0 || rec.TokPerSec > 1000 {
		t.Fatalf("spread stream: TokPerSec = %v", rec.TokPerSec)
	}
}

// TestStreamingFlushesEachLine checks the client sees a line before the
// upstream sends the next one.
func TestStreamingFlushesEachLine(t *testing.T) {
	next := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n")
		w.(http.Flusher).Flush()
		<-next
		_, _ = io.WriteString(w, "\ndata: [DONE]\n\n")
	})
	resp := h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`)
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, `"a"`) {
		t.Fatalf("first line %q, %v", line, err)
	}
	close(next)
	readAll(t, br)
}

func TestNonStreaming(t *testing.T) {
	const out = `{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, out)
	})
	resp := h.post(t, "/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`)
	if got := readAll(t, resp.Body); got != out {
		t.Fatalf("body %q", got)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("upstream header not forwarded")
	}
	if _, ok := h.up.body["stream_options"]; ok {
		t.Error("stream_options added to a non-streaming request")
	}
	rec := h.store.last(t)
	if rec.PromptTokens != 3 || rec.CompletionTokens != 4 || rec.Status != 200 {
		t.Errorf("record %+v", rec)
	}
	// An instant one-piece reply has no measurable speed (not millions of tok/s).
	if rec.TokPerSec != 0 {
		t.Errorf("TokPerSec = %v for an instant non-streamed reply", rec.TokPerSec)
	}
}

func TestEmbeddingsNeverGetStreamOptions(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	})
	readAll(t, h.post(t, "/embeddings", `{"model":"qwen","input":"x","stream":true}`).Body)
	if h.up.path != "/v1/embeddings" {
		t.Errorf("path %q", h.up.path)
	}
	if _, ok := h.up.body["stream_options"]; ok {
		t.Error("stream_options sent to embeddings")
	}
	if rec := h.store.last(t); rec.PromptTokens != 2 {
		t.Errorf("prompt tokens %d", rec.PromptTokens)
	}
}

func TestUpstreamErrorPassThrough(t *testing.T) {
	const errBody = `{"error":{"message":"context too long","type":"invalid_request_error"}}`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errBody)
	})
	resp := h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := readAll(t, resp.Body); got != errBody {
		t.Errorf("body %q", got)
	}
	if rec := h.store.last(t); rec.Status != 400 || rec.Error != "context too long" {
		t.Errorf("record status %d err %q", rec.Status, rec.Error)
	}
}

func TestGatewayErrors(t *testing.T) {
	tests := []struct {
		name, path, body string
		status           int
		typ              string
		recorded         bool
	}{
		{"unknown model", "/chat/completions", `{"model":"nope"}`, 404, "model_not_found", true},
		{"no room", "/completions", `{"model":"full"}`, 503, "insufficient_vram", true},
		{"bad json", "/chat/completions", `not json`, 400, "invalid_request_error", false},
		{"model not string", "/chat/completions", `{"model":5}`, 400, "invalid_request_error", false},
		{"too large", "/chat/completions", `{"x":"` + strings.Repeat("a", maxBody) + `"}`, 413, "invalid_request_error", false},
		{"unknown path", "/files", `{}`, 404, "not_found", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(http.ResponseWriter, *http.Request) { t.Error("upstream called") })
			resp := h.post(t, tt.path, tt.body)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.status)
			}
			var e struct {
				Error struct{ Message, Type string }
			}
			if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error.Type != tt.typ || e.Error.Message == "" {
				t.Errorf("error body %+v, %v", e, err)
			}
			if n := h.recorded(); (n == 1) != tt.recorded {
				t.Errorf("records %d, recorded=%v", n, tt.recorded)
			}
		})
	}
}

func TestUpstreamDown(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.src.base = "http://127.0.0.1:1" // nothing listens here
	h.src.local = false               // a remote upstream: 502 (a local one is an engine failure, see crash_test.go)
	resp := h.post(t, "/chat/completions", `{"model":"qwen"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if rec := h.store.last(t); rec.Status != 502 || rec.Error == "" {
		t.Errorf("record %+v", rec)
	}
	if h.src.released.Load() != 1 {
		t.Error("model not released")
	}
}

func TestClientCancelPropagates(t *testing.T) {
	upCancelled := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(upCancelled)
		case <-time.After(10 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/chat/completions",
		strings.NewReader(`{"model":"qwen","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-upCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not cancelled")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.recorded() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rec := h.store.last(t)
	if rec.Status != statusClientClosed {
		t.Errorf("status %d, want %d", rec.Status, statusClientClosed)
	}
}

func TestEnergySplitAcrossConcurrentLocal(t *testing.T) {
	h := newHarness(t, nil)
	h.gw.localInFlight.Add(2) // ours and one other local request
	e0 := h.gw.energyNow()
	if got := h.gw.energyShare(e0); got != 5 {
		t.Errorf("energy share %v, want 5 (10 J over 2 requests)", got)
	}
	if got := newGateway(h.src, nil, nil, nil).energyShare(0); got != 0 {
		t.Errorf("no monitor: energy %v, want 0", got)
	}
}

func TestCloudRequestsHaveNoEnergy(t *testing.T) {
	h := newHarness(t, sseHandler(true, nil))
	h.src.local = false
	readAll(t, h.post(t, "/chat/completions", `{"model":"qwen","stream":true}`).Body)
	if rec := h.store.last(t); rec.EnergyJ != 0 || rec.Local {
		t.Errorf("cloud record energy %v local %v", rec.EnergyJ, rec.Local)
	}
}

func TestModels(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := http.Get(h.srv.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Object string
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("list %+v", list)
	}
	if d := list.Data[1]; d.ID != "cloud/gpt" || d.OwnedBy != "openai" || d.Object != "model" {
		t.Errorf("entry %+v", d)
	}
}

func TestEncodePreservesStreamOptions(t *testing.T) {
	req := &request{fields: map[string]json.RawMessage{
		"stream_options": json.RawMessage(`{"other":1}`), "temperature": json.RawMessage(`0.2`)}, stream: true}
	b, err := req.encode("m", true)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","stream_options":{"include_usage":true,"other":1},"temperature":0.2}`
	if string(b) != want {
		t.Errorf("got %s\nwant %s", b, want)
	}
}

func TestNewWithNilDeps(t *testing.T) {
	gw := New(nil, nil, nil, nil)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/chat/completions", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d", resp.StatusCode)
	}
}
