package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/usage"
)

// statusClientClosed is recorded when the client went away (nginx's 499).
const statusClientClosed = 499

// maxUsageScan bounds how much of a non-streaming body is kept to read usage.
const maxUsageScan = 32 << 20

// hopHeaders are connection-level headers a proxy must not forward.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// request is a parsed client body plus the flags the proxy needs.
type request struct {
	fields      map[string]json.RawMessage
	model       string
	stream      bool
	clientUsage bool // the client itself asked for stream_options.include_usage
}

// call tracks one proxied request for the usage record.
type call struct {
	start  time.Time
	sent   time.Time // upstream request sent (after model acquisition)
	first  time.Time // first content byte
	rec    usage.Record
	deltas int

	capture  bool            // keep the answer (WithExchange)
	messages json.RawMessage // the request's messages, when capturing
	text     strings.Builder // the answer so far, when capturing
}

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, path string) {
	c := &call{start: time.Now()}
	c.rec = usage.Record{Time: c.start, Client: r.Header.Get("X-Cerberus-Client"),
		SessionID: r.Header.Get("X-Cerberus-Session"), User: r.Header.Get("X-Cerberus-User")}

	req, status, err := readRequest(w, r)
	if err != nil {
		writeError(w, status, "invalid_request_error", err.Error())
		return
	}
	c.rec.Model = req.model
	defer func() { g.finish(c) }()

	if g.src == nil {
		g.fail(w, c, http.StatusServiceUnavailable, "server_error", "no model engine configured")
		return
	}
	ctx := r.Context()
	id, info, known := g.resolve(ctx, req.model)
	c.rec.Model = id
	checked := false
	if known && !info.Local {
		c.rec.Provider = providerOf(id, false, info.Backend)
		if g.refuseOverBudget(w, c) {
			return
		}
		checked = true
	}
	ep, release, err := g.src.Acquire(ctx, id)
	if err != nil {
		st, typ := acquireStatus(err)
		g.fail(w, c, st, typ, err.Error())
		return
	}
	defer func() { release() }()
	c.rec.Backend, c.rec.Local = ep.Backend, ep.Local
	c.rec.Provider = providerOf(id, ep.Local, ep.Backend)
	if g.captureFor(ep.Local, path) {
		c.capture, c.messages = true, req.fields["messages"]
	}
	if !ep.Local && !checked && g.refuseOverBudget(w, c) {
		return
	}

	body, err := req.encode(ep.Model, path != "/embeddings")
	if err != nil {
		g.fail(w, c, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if ep.Local {
		g.localInFlight.Add(1)
		defer g.localInFlight.Add(-1)
	}
	energy0 := g.energyNow()
	fg, canForget := g.src.(forgetter)
	if g.forward(ctx, w, r, c, req, ep, path, body, ep.Local && canForget) {
		// The engine had dropped the model behind our back: load it and try once more.
		g.log.Info("model was not loaded upstream; loading and retrying", "model", id, "backend", ep.Backend)
		fg.Forget(id)
		release()
		ep, release, err = g.src.Acquire(ctx, id)
		if err != nil {
			st, typ := acquireStatus(err)
			g.fail(w, c, st, typ, err.Error())
			return
		}
		if body, err = req.encode(ep.Model, path != "/embeddings"); err != nil {
			g.fail(w, c, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		g.forward(ctx, w, r, c, req, ep, path, body, false)
	}
	if ep.Local {
		c.rec.EnergyJ = g.energyShare(energy0)
	}
}

// energyShare is the GPU energy used since energy0 divided by the local
// requests in flight now (this one included): concurrent requests share the
// GPU, so each is charged an equal share.
func (g *Gateway) energyShare(energy0 float64) float64 {
	if g.energy == nil {
		return 0
	}
	n := max(g.localInFlight.Load(), 1)
	return max(g.energyNow()-energy0, 0) / float64(n)
}

// resolve maps id to the engine's model when the source can, so usage
// aggregates per model rather than per alias and budgets can be checked
// before a model is acquired.
func (g *Gateway) resolve(ctx context.Context, id string) (string, engine.ModelInfo, bool) {
	if rs, ok := g.src.(resolver); ok {
		if m, err := rs.Resolve(ctx, id); err == nil {
			return m.ID, m, true
		}
	}
	return id, engine.ModelInfo{}, false
}

func (g *Gateway) energyNow() float64 {
	if g.energy == nil {
		return 0
	}
	return g.energy()
}

// forward sends body upstream and relays the response to w.
// With retry set, an upstream "model not loaded" error is not relayed: it
// returns true (nothing written) so the caller can load the model and retry.
func (g *Gateway) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, c *call,
	req *request, ep engine.Endpoint, path string, body []byte, retry bool) bool {
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		g.fail(w, c, http.StatusInternalServerError, "server_error", err.Error())
		return false
	}
	up.Header.Set("Content-Type", "application/json")
	if a := r.Header.Get("Accept"); a != "" {
		up.Header.Set("Accept", a)
	}
	// The client's Authorization is the Cerberus token and is never forwarded;
	// cloud keys come from the endpoint's headers.
	for k, v := range ep.Headers {
		up.Header.Set(k, v)
	}
	c.sent = time.Now()
	resp, err := g.client.Do(up)
	if err != nil {
		if ctx.Err() != nil {
			c.rec.Status, c.rec.Error = statusClientClosed, "client cancelled"
			return false
		}
		if ep.Local && engine.IsTransient(err) {
			g.engineFailed(w, c, ep.Gen, "upstream: "+err.Error())
			return false
		}
		g.fail(w, c, http.StatusBadGateway, "upstream_error", "upstream: "+err.Error())
		return false
	}
	defer resp.Body.Close()

	var src io.Reader = resp.Body
	if ep.Local && resp.StatusCode/100 != 2 {
		head, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		msg := errorMessage(head)
		if retry && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound) && modelNotLoaded(msg) {
			return true
		}
		if engineCrashed(resp.StatusCode, msg) {
			g.engineFailed(w, c, ep.Gen, msg)
			return false
		}
		src = io.MultiReader(bytes.NewReader(head), resp.Body)
	}

	c.rec.Status = resp.StatusCode
	copyHeaders(w.Header(), resp.Header)
	if resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		err = g.relayStream(w, src, c, req.clientUsage)
	} else {
		w.WriteHeader(resp.StatusCode)
		err = relayBody(w, src, c)
	}
	if err != nil && c.rec.Error == "" {
		if ctx.Err() != nil {
			c.rec.Status, c.rec.Error = statusClientClosed, "client cancelled"
		} else {
			c.rec.Error = err.Error()
		}
	}
	// A stream already answered 200, so the status stays; but an engine
	// that died mid-reply (or sent a crash as an error event) is reloaded.
	// A client that hung up (a failed write to it) is not the engine.
	var cw *clientWriteError
	if ep.Local && ctx.Err() == nil && !errors.As(err, &cw) &&
		(engine.IsTransient(err) || (c.rec.Error != "" && engine.IsTransientText(0, c.rec.Error))) {
		g.markCrashed(c.rec.Model, ep.Gen)
	}
	return false
}

// reEngineLoading is llama-server's 503 while it is still loading, which is
// not a crash.
var reEngineLoading = regexp.MustCompile(`(?i)loading model`)

// engineCrashed reports whether a local engine's error reply means the
// engine fell over: 502/503 (other than "loading model"), or crash words in
// any status (LM Studio answers a crashed llama.cpp with 400 "terminated" or
// "Channel Error").
func engineCrashed(status int, msg string) bool {
	if (status == http.StatusBadGateway || status == http.StatusServiceUnavailable) && !reEngineLoading.MatchString(msg) {
		return true
	}
	return engine.IsTransientText(0, msg)
}

// engineFailed answers a local engine failure. The gateway does not retry
// (the flows runner retries a failed item once, and a client's stream cannot
// be replayed); it tells the engine, which reloads the model on the next
// request with one slot fewer, and answers 503 so callers know a retry may
// work (engine.IsTransientText matches this text).
func (g *Gateway) engineFailed(w http.ResponseWriter, c *call, gen uint64, upstream string) {
	g.markCrashed(c.rec.Model, gen)
	g.fail(w, c, http.StatusServiceUnavailable, "engine_failed",
		"local engine failed: "+upstream+" (it will be reloaded)")
}

// markCrashed tells the engine; gen is the load that served the request
// (Endpoint.Gen), so a failure of an engine that was already reloaded is
// not counted twice.
func (g *Gateway) markCrashed(id string, gen uint64) {
	ok := false
	switch cm := g.src.(type) {
	case genCrashMarker:
		ok = cm.MarkCrashedGen(id, gen)
	case crashMarker:
		ok = cm.MarkCrashed(id)
	}
	if ok {
		g.log.Warn("local engine failed; marked for reload", "model", id)
	}
}

// clientWriteError is a failed write to the client (it hung up), as
// opposed to a failed read from the engine.
type clientWriteError struct{ err error }

func (e *clientWriteError) Error() string { return e.err.Error() }

// clientWriter tags w's write errors as clientWriteError.
type clientWriter struct{ w io.Writer }

func (c clientWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil {
		err = &clientWriteError{err}
	}
	return n, err
}

// reNotLoaded matches the engines' "this model is not loaded" errors: LM
// Studio's "No models loaded", Ollama's and llama.cpp's "model ... not found",
// and OpenAI-style model_not_found.
var reNotLoaded = regexp.MustCompile(`(?i)no models? (are |is )?loaded|model\b.*\bnot (loaded|found)|model_not_found|not loaded`)

// modelNotLoaded reports whether an upstream error message means the model
// is not in memory (as opposed to a bad request).
func modelNotLoaded(msg string) bool { return msg != "" && reNotLoaded.MatchString(msg) }

// relayBody copies a non-streaming body and reads usage (or the upstream
// error message) from it.
func relayBody(w http.ResponseWriter, body io.Reader, c *call) error {
	var buf capBuffer
	buf.limit = maxUsageScan
	c.first = time.Now()
	if _, err := io.Copy(clientWriter{w}, io.TeeReader(body, &buf)); err != nil {
		return err
	}
	if buf.over {
		return nil
	}
	if c.rec.Status/100 != 2 {
		c.rec.Error = errorMessage(buf.b)
		return nil
	}
	if c.capture {
		c.addText(bodyText(buf.b))
	}
	var res struct {
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(buf.b, &res) == nil && res.Usage != nil {
		res.Usage.apply(&c.rec)
	}
	return nil
}

// fail writes an error response and marks the record failed.
func (g *Gateway) fail(w http.ResponseWriter, c *call, status int, typ, msg string) {
	c.rec.Status, c.rec.Error = status, msg
	if status != statusClientClosed {
		writeError(w, status, typ, msg)
	}
}

// minSpeedWindow is the shortest span a tokens-per-second figure is taken over.
const minSpeedWindow = 50 * time.Millisecond

// finish computes the derived figures and stores the record.
func (g *Gateway) finish(c *call) {
	defer g.deliver(c)
	end := time.Now()
	c.rec.DurationMs = end.Sub(c.start).Milliseconds()
	if !c.first.IsZero() {
		c.rec.TTFTMs = c.first.Sub(c.start).Milliseconds()
	}
	if c.rec.CompletionTokens == 0 && c.deltas > 0 {
		c.rec.CompletionTokens = c.deltas
	}
	if toks := float64(c.rec.CompletionTokens); toks > 0 {
		// Generation speed excludes prompt processing (time to first
		// token) when the reply streams. A non-streamed body arrives in one
		// piece, so it is measured from the request. Either way only a span
		// of at least minSpeedWindow counts: an upstream or proxy that
		// buffers a stream delivers every delta in one read, and a figure
		// over microseconds (millions of tok/s) would skew the averages.
		from := c.first
		if c.deltas == 0 {
			from = c.sent
		}
		if from.IsZero() || !end.After(from) {
			from = c.sent
		}
		if span := end.Sub(from); !from.IsZero() && span >= minSpeedWindow && c.deltas != 1 {
			c.rec.TokPerSec = toks / span.Seconds()
		}
		if c.rec.EnergyJ > 0 {
			c.rec.TokPerJ = toks / c.rec.EnergyJ
		}
	}
	if c.rec.Status >= 400 || c.rec.Error != "" {
		g.log.Debug("gateway request failed", "model", c.rec.Model, "status", c.rec.Status, "err", c.rec.Error)
	}
	g.priceCall(c)
	if g.store != nil {
		g.store.Add(c.rec)
		if c.rec.CostUSD > 0 {
			g.checkBudgets(c.rec.Provider) // announces a soft cap this request crossed
		}
	}
}

// readRequest reads and parses the client body (limit maxBody).
func readRequest(w http.ResponseWriter, r *http.Request) (*request, int, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return nil, http.StatusRequestEntityTooLarge, errors.New("request body exceeds 32 MiB")
		}
		return nil, http.StatusBadRequest, err
	}
	req := &request{}
	if err := json.Unmarshal(raw, &req.fields); err != nil || req.fields == nil {
		return nil, http.StatusBadRequest, errors.New("request body must be a JSON object")
	}
	if v, ok := req.fields["model"]; ok {
		if err := json.Unmarshal(v, &req.model); err != nil {
			return nil, http.StatusBadRequest, errors.New("model must be a string")
		}
	}
	if v, ok := req.fields["stream"]; ok {
		_ = json.Unmarshal(v, &req.stream)
	}
	if v, ok := req.fields["stream_options"]; ok {
		var so struct {
			IncludeUsage bool `json:"include_usage"`
		}
		_ = json.Unmarshal(v, &so)
		req.clientUsage = so.IncludeUsage
	}
	return req, 0, nil
}

// encode rewrites the model and, for streaming generation, asks for usage.
func (req *request) encode(model string, generation bool) ([]byte, error) {
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	req.fields["model"] = m
	if generation && req.stream && !req.clientUsage {
		opts := map[string]json.RawMessage{}
		if v, ok := req.fields["stream_options"]; ok {
			_ = json.Unmarshal(v, &opts)
			if opts == nil {
				opts = map[string]json.RawMessage{}
			}
		}
		opts["include_usage"] = json.RawMessage("true")
		b, err := json.Marshal(opts)
		if err != nil {
			return nil, err
		}
		req.fields["stream_options"] = b
	}
	return json.Marshal(req.fields)
}

// copyHeaders copies upstream response headers minus hop-by-hop ones and
// Content-Length (the stream may be altered).
func copyHeaders(dst, src http.Header) {
	drop := map[string]bool{"Content-Length": true}
	for _, h := range hopHeaders {
		drop[h] = true
	}
	for _, f := range src.Values("Connection") {
		for _, h := range strings.Split(f, ",") {
			if h = strings.TrimSpace(h); h != "" {
				drop[http.CanonicalHeaderKey(h)] = true
			}
		}
	}
	for k, vs := range src {
		if !drop[k] {
			dst[k] = append([]string(nil), vs...)
		}
	}
}

// errorMessage extracts error.message from an OpenAI-style error body, or
// returns the body itself, clipped.
func errorMessage(b []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &obj) == nil && obj.Message != "" {
			return clip(obj.Message)
		}
		var s string
		if json.Unmarshal(e.Error, &s) == nil && s != "" {
			return clip(s)
		}
	}
	return clip(strings.TrimSpace(string(b)))
}

func clip(s string) string {
	const n = 300
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// capBuffer collects writes up to limit bytes, then stops collecting.
type capBuffer struct {
	b     []byte
	limit int
	over  bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if !c.over {
		if len(c.b)+len(p) > c.limit {
			c.over, c.b = true, nil
		} else {
			c.b = append(c.b, p...)
		}
	}
	return len(p), nil
}
