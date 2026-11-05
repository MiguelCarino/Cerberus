package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/gateway/xlate"
)

// Gateway phase G1: the Anthropic Messages API and the OpenAI Responses API,
// translated to Chat Completions (or passed through to a provider that serves
// them natively).

// pingEvery is how long a translated stream may stay quiet before a
// keepalive event goes out (a long prompt read sends nothing).
var pingEvery = 15 * time.Second

func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, func(st int, msg string) []byte { return (&xlate.Messages{}).Error(st, msg) })
	if !ok {
		return
	}
	xr, err := xlate.ParseMessages(body)
	if err != nil {
		writeXError(w, &xlate.Messages{}, err)
		return
	}
	g.serveTranslated(w, r, "messages", "/messages", body, xr, xlate.NewMessages(xr))
}

func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, func(st int, msg string) []byte { return (&xlate.Responses{}).Error(st, msg) })
	if !ok {
		return
	}
	xr, err := xlate.ParseResponses(body)
	if err != nil {
		writeXError(w, &xlate.Responses{}, err)
		return
	}
	g.serveTranslated(w, r, "responses", "/responses", body, xr, xlate.NewResponses(xr))
}

func readBody(w http.ResponseWriter, r *http.Request, shape func(int, string) []byte) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		st := http.StatusBadRequest
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			st = http.StatusRequestEntityTooLarge
		}
		writeRaw(w, st, shape(st, err.Error()))
		return nil, false
	}
	return body, true
}

func writeXError(w http.ResponseWriter, api xlate.API, err error) {
	st := http.StatusBadRequest
	var xe *xlate.Error
	if errors.As(err, &xe) {
		st = xe.Status
	}
	writeRaw(w, st, api.Error(st, err.Error()))
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// serveTranslated runs a translated request through serve, with the reply
// translated back by api, or passed through when the endpoint's provider
// serves nativePath itself.
func (g *Gateway) serveTranslated(w http.ResponseWriter, r *http.Request, apiName, nativePath string,
	body []byte, xr *xlate.Request, api xlate.API) {
	for _, n := range xr.Notes {
		g.noteOnce("xlate|"+apiName+"|"+n, "gateway: field accepted but not used", "api", apiName, "field", n)
	}
	req := &request{fields: xr.Fields, model: xr.Model, stream: xr.Stream, api: apiName,
		thinking: xr.Thinking, effort: xr.Effort, resultErrors: xr.ToolErrors}
	xw := newXWriter(w, api, xr.Stream)
	defer xw.finish()
	g.serve(xw, r, "/chat/completions", req, func(id string, ep engine.Endpoint) string {
		if !g.providerServes(id, ep, nativePath) {
			return ""
		}
		// Native: the client's own body (model rewritten) and its own reply.
		var f map[string]json.RawMessage
		if json.Unmarshal(body, &f) != nil {
			return ""
		}
		req.fields = f
		xw.passThrough()
		return nativePath
	})
}

// hints applies a translated request's thinking switch and effort, before
// the model's defaults (which then see them as named).
func (req *request) hints(local bool) {
	if req.effort != "" {
		if _, ok := req.fields["reasoning_effort"]; !ok {
			req.fields["reasoning_effort"] = mustJSON(req.effort)
		}
	}
	if req.thinking != nil && local {
		req.mergeKwarg("enable_thinking", *req.thinking)
	}
}

// SetProviders takes the native APIs each provider declares, and the model
// name map (copied; Reload calls it again).
func (g *Gateway) SetProviders(providers []config.ProviderConfig, modelMap map[string]string) {
	apis := map[string][]string{}
	for _, p := range providers {
		if len(p.APIs) > 0 {
			apis[p.Name] = slices.Clone(p.APIs)
		}
	}
	type pair struct{ pat, id string }
	var mm []pair
	for k, v := range modelMap {
		if k != "" && v != "" {
			mm = append(mm, pair{k, v})
		}
	}
	// Most specific first: longer patterns before shorter, then by name.
	sort.Slice(mm, func(i, j int) bool {
		if len(mm[i].pat) != len(mm[j].pat) {
			return len(mm[i].pat) > len(mm[j].pat)
		}
		return mm[i].pat < mm[j].pat
	})
	g.priceMu.Lock()
	g.providerAPIs = apis
	g.modelMap = g.modelMap[:0]
	for _, p := range mm {
		g.modelMap = append(g.modelMap, [2]string{p.pat, p.id})
	}
	g.priceMu.Unlock()
}

// mapModel returns the model a hard-coded client name maps to, or "".
func (g *Gateway) mapModel(name string) string {
	if name == "" {
		return ""
	}
	g.priceMu.Lock()
	defer g.priceMu.Unlock()
	for _, p := range g.modelMap {
		if ok, _ := path.Match(p[0], name); ok {
			return p[1]
		}
	}
	return ""
}

// providerServes reports whether the provider of a cloud model id
// ("anthropic/claude-…") declared nativePath ("/messages", "/responses").
func (g *Gateway) providerServes(id string, ep engine.Endpoint, nativePath string) bool {
	if ep.Local {
		return false
	}
	prov := providerOf(id, false, ep.Backend)
	g.priceMu.Lock()
	defer g.priceMu.Unlock()
	return slices.Contains(g.providerAPIs[prov], strings.TrimPrefix(nativePath, "/"))
}

// nativeHeaders adapts a pass-through request's headers: the Anthropic API
// takes its key as x-api-key and needs anthropic-version.
func nativeHeaders(up, client http.Header, upstreamPath string) {
	if upstreamPath != "/messages" && upstreamPath != "/messages/count_tokens" {
		return
	}
	if a := up.Get("Authorization"); strings.HasPrefix(a, "Bearer ") && up.Get("X-Api-Key") == "" {
		up.Set("X-Api-Key", strings.TrimPrefix(a, "Bearer "))
		up.Del("Authorization")
	}
	up.Set("Anthropic-Version", orDefault(client.Get("Anthropic-Version"), "2023-06-01"))
	if b := client.Get("Anthropic-Beta"); b != "" {
		up.Set("Anthropic-Beta", b)
	}
}

func orDefault(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

// xwriter sits between serve and the client: it receives the upstream's
// Chat Completions reply (as serve relays it) and writes the client API's.
type xwriter struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	api    xlate.API
	stream bool // the client asked for a stream
	hdr    http.Header

	mu     sync.Mutex
	status int
	mode   int // 0 = undecided, 1 = translated SSE, 2 = translated body, 3 = pass-through
	st     xlate.Stream
	body   bytes.Buffer // mode 2
	line   []byte       // mode 1: a partial line
	last   time.Time
	native bool
	err    error // a failed write to the client

	stop chan struct{}
	done chan struct{}
}

func newXWriter(w http.ResponseWriter, api xlate.API, stream bool) *xwriter {
	return &xwriter{w: w, rc: http.NewResponseController(w), api: api, stream: stream, hdr: http.Header{}}
}

// passThrough relays the upstream reply untouched (a native provider API).
func (x *xwriter) passThrough() {
	x.mu.Lock()
	x.native = true
	x.mu.Unlock()
}

func (x *xwriter) Header() http.Header { return x.hdr }

func (x *xwriter) WriteHeader(code int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.status != 0 {
		return
	}
	x.status = code
	switch {
	case x.native:
		x.mode = 3
		copyHeaders(x.w.Header(), x.hdr)
		x.w.WriteHeader(code)
	case code/100 == 2 && strings.HasPrefix(x.hdr.Get("Content-Type"), "text/event-stream"):
		x.mode = 1
		h := x.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		x.w.WriteHeader(code)
		x.st = x.api.NewStream()
		x.writeLocked(x.st.Start())
		x.stop, x.done = make(chan struct{}), make(chan struct{})
		go x.pinger()
	default:
		x.mode = 2
	}
}

func (x *xwriter) writeLocked(b []byte) {
	if len(b) == 0 || x.err != nil {
		return
	}
	if _, err := x.w.Write(b); err != nil {
		x.err = err
		return
	}
	x.last = time.Now()
	_ = x.rc.Flush()
}

func (x *xwriter) Write(p []byte) (int, error) {
	if x.status == 0 {
		x.WriteHeader(http.StatusOK)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	switch x.mode {
	case 3:
		if _, err := x.w.Write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	case 2:
		x.body.Write(p)
		return len(p), nil
	}
	x.line = append(x.line, p...)
	for {
		i := bytes.IndexByte(x.line, '\n')
		if i < 0 {
			break
		}
		x.event(x.line[:i])
		x.line = x.line[i+1:]
	}
	if x.err != nil {
		return 0, x.err
	}
	return len(p), nil
}

// event translates one upstream SSE line.
func (x *xwriter) event(line []byte) {
	payload, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if string(payload) == "[DONE]" {
		x.writeLocked(x.st.End())
		return
	}
	if len(payload) > 0 && payload[0] == '{' {
		x.writeLocked(x.st.Chunk(payload))
	}
}

func (x *xwriter) Flush() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.mode == 3 {
		_ = x.rc.Flush()
	}
}

func (x *xwriter) pinger() {
	defer close(x.done)
	t := time.NewTicker(pingEvery / 3)
	defer t.Stop()
	for {
		select {
		case <-x.stop:
			return
		case <-t.C:
			x.mu.Lock()
			if time.Since(x.last) >= pingEvery {
				x.writeLocked(x.st.Ping())
			}
			x.mu.Unlock()
		}
	}
}

// finish ends the client's reply once serve returned: closes a stream that
// ended without [DONE], or translates a buffered body.
func (x *xwriter) finish() {
	if x.stop != nil {
		close(x.stop)
		<-x.done
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	switch x.mode {
	case 1:
		if len(x.line) > 0 {
			x.event(x.line)
			x.line = nil
		}
		x.writeLocked(x.st.End())
	case 2:
		b := x.body.Bytes()
		if x.status/100 == 2 {
			out, err := x.api.Reply(b)
			if err != nil {
				x.writeBody(http.StatusBadGateway, x.api.Error(http.StatusBadGateway, "upstream reply: "+err.Error()))
				return
			}
			x.writeBody(x.status, out)
			return
		}
		x.writeBody(x.status, x.api.Error(x.status, xlate.ErrorMessage(b)))
	}
}

func (x *xwriter) writeBody(status int, b []byte) {
	x.w.Header().Set("Content-Type", "application/json")
	x.w.WriteHeader(status)
	_, _ = x.w.Write(b)
}

// handleCountTokens answers POST /messages/count_tokens: the engine's own
// count when the model is a loaded llama.cpp model, the provider's for a
// native Messages provider, else an estimate (4 bytes a token), marked.
func (g *Gateway) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	m := &xlate.Messages{}
	body, ok := readBody(w, r, m.Error)
	if !ok {
		return
	}
	xr, err := xlate.ParseMessages(body)
	if err != nil {
		writeXError(w, m, err)
		return
	}
	estimate := func() {
		b, _ := json.Marshal(map[string]any{"messages": xr.Fields["messages"], "tools": xr.Fields["tools"]})
		writeJSON(w, http.StatusOK, map[string]any{"input_tokens": (len(b) + 3) / 4, "estimated": true})
	}
	if g.src == nil {
		estimate()
		return
	}
	model := xr.Model
	id, info, known := g.resolve(r.Context(), model)
	if !known {
		if mm := g.mapModel(model); mm != "" {
			id, info, known = g.resolve(r.Context(), mm)
		}
	}
	if !known || !info.Loaded || !info.Local || info.Backend != engine.BackendLlamaCpp {
		if known && !info.Local {
			if g.countNative(w, r, id, body) {
				return
			}
		}
		estimate()
		return
	}
	ep, release, err := g.src.Acquire(r.Context(), id)
	if err != nil {
		estimate()
		return
	}
	defer release()
	if n, err := llamaCount(r.Context(), g.client, ep, xr.Fields); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
		return
	}
	estimate()
}

// countNative passes count_tokens to a provider that serves Messages.
func (g *Gateway) countNative(w http.ResponseWriter, r *http.Request, id string, body []byte) bool {
	ep, release, err := g.src.Acquire(r.Context(), id)
	if err != nil {
		return false
	}
	defer release()
	if !g.providerServes(id, ep, "/messages") {
		return false
	}
	var f map[string]json.RawMessage
	if json.Unmarshal(body, &f) != nil {
		return false
	}
	f["model"] = mustJSON(ep.Model)
	b, _ := json.Marshal(f)
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(ep.BaseURL, "/")+"/messages/count_tokens", bytes.NewReader(b))
	if err != nil {
		return false
	}
	up.Header.Set("Content-Type", "application/json")
	for k, v := range ep.Headers {
		up.Header.Set(k, v)
	}
	nativeHeaders(up.Header, r.Header, "/messages/count_tokens")
	resp, err := g.client.Do(up)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
	return true
}

// llamaCount renders the chat through llama-server's /apply-template and
// counts it with /tokenize.
func llamaCount(ctx context.Context, hc *http.Client, ep engine.Endpoint, fields map[string]json.RawMessage) (int, error) {
	root := strings.TrimSuffix(strings.TrimRight(ep.BaseURL, "/"), "/v1")
	post := func(p string, in, out any) error {
		b, _ := json.Marshal(in)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+p, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.New(resp.Status)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
	}
	var tpl struct {
		Prompt string `json:"prompt"`
	}
	in := map[string]json.RawMessage{"messages": fields["messages"]}
	if t, ok := fields["tools"]; ok {
		in["tools"] = t
	}
	if err := post("/apply-template", in, &tpl); err != nil {
		return 0, err
	}
	var tok struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := post("/tokenize", map[string]any{"content": tpl.Prompt, "add_special": true, "parse_special": true}, &tok); err != nil {
		return 0, err
	}
	return len(tok.Tokens), nil
}
