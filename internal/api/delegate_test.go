package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
)

// delegModels: two local models (one loaded) and a cloud one.
type delegModels struct{ fakeModels }

func (*delegModels) Models(context.Context) []engine.ModelInfo {
	return []engine.ModelInfo{
		{ID: "qwen3-coder:30b", Backend: "ollama", Local: true, Loaded: true, Ctx: 32768},
		{ID: "gemma3:12b", Backend: "ollama", Local: true},
		{ID: "openrouter/big", Backend: "openai"},
	}
}

// workspaceSessions gives the fake sessions a workspace folder.
type workspaceSessions struct {
	*fakeSessions
	dir string
}

func (w workspaceSessions) Get(id string) (session.Info, bool) {
	i, ok := w.fakeSessions.Get(id)
	i.Worktree = w.dir
	return i, ok
}

type upstreamCall struct {
	body    map[string]any
	session string
	client  string
}

type delegEnv struct {
	*testEnv
	ws    string
	mu    sync.Mutex
	calls []upstreamCall
	reply func(w http.ResponseWriter)
	ft    *finetune.Store
	agent []reqOpt
}

func newDelegEnv(t *testing.T, mut func(*config.Config)) *delegEnv {
	t.Helper()
	env := newEnv(t, func(c *config.Config) {
		c.Finetune.Capture = config.CaptureDelegated
		c.Prices = map[string]config.Price{"openrouter/*": {In: 3, Out: 15}}
		c.Delegate.ReferenceModel = "openrouter/big"
		if mut != nil {
			mut(c)
		}
	})
	e := &delegEnv{testEnv: env, ws: t.TempDir()}
	e.srv.sessions = workspaceSessions{fakeSessions: env.sessions, dir: e.ws}
	e.srv.models = &delegModels{}
	e.reply = func(w http.ResponseWriter) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"<think>hm</think>Three Go files."},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":30}}`)
	}
	e.srv.gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		e.calls = append(e.calls, upstreamCall{body: body, session: r.Header.Get("X-Cerberus-Session"), client: r.Header.Get("X-Cerberus-Client")})
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		e.reply(w)
	})
	ft, err := finetune.New(filepath.Join(t.TempDir(), "ft"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.ft, e.srv.finetune = ft, ft
	e.h = e.srv.handler()
	e.agent = []reqOpt{withBearer(env.srv.cfg.SessionAgentToken("s1")), withHeader(sessionHeader, "s1")}
	return e
}

func TestDelegateModels(t *testing.T) {
	e := newDelegEnv(t, func(c *config.Config) { c.Delegate.Tags = map[string][]string{"qwen3-coder:30b": {"code"}} })
	w := e.do("GET", "/api/agent/models", "", e.agent...)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res agent.DelegateModels
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Models) != 2 || res.Models[0].ID != "qwen3-coder:30b" || !res.Models[0].Loaded || res.Models[0].Tags[0] != "code" ||
		res.Models[1].Loaded || res.Guidance == "" || res.Load {
		t.Fatalf("models %+v", res)
	}
	if txt := agent.FormatDelegateModels(res); !strings.Contains(txt, "qwen3-coder:30b · loaded · 32k ctx · code") || !strings.Contains(txt, "Guidance:") {
		t.Errorf("text %q", txt)
	}
	// owner tokens are not agents
	if w := e.do("GET", "/api/agent/models", "", withBearer(testToken)); w.Code == 200 {
		t.Errorf("admin token on the agent scope: %d", w.Code)
	}
}

func TestDelegateAsk(t *testing.T) {
	e := newDelegEnv(t, nil)
	if err := os.WriteFile(filepath.Join(e.ws, "notes.txt"), []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := e.do("POST", "/api/agent/ask", `{"model":"local","prompt":"Count the lines.","files":["notes.txt"],"max_tokens":200}`, e.agent...)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res agent.DelegateResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Model != "qwen3-coder:30b" || res.Text != "Three Go files." || res.PromptTokens != 120 || res.CompletionTokens != 30 ||
		!strings.HasPrefix(res.ID, "ex-") {
		t.Fatalf("result %+v", res)
	}
	// 120 × $3/M + 30 × $15/M
	if res.SavedUSD < 0.00080 || res.SavedUSD > 0.00082 {
		t.Errorf("saved %v", res.SavedUSD)
	}
	c := e.calls[0]
	msgs := c.body["messages"].([]any)
	user := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if c.session != "s1" || c.client != usage.ClientDelegate || c.body["model"] != "qwen3-coder:30b" || c.body["max_tokens"].(float64) != 200 ||
		!strings.Contains(user, "--- notes.txt ---\nalpha\nbeta") || c.body["stream"] != false {
		t.Errorf("upstream call %+v", c)
	}
	// the example was kept and can be rated
	if w := e.do("POST", "/api/agent/rate", `{"id":"`+res.ID+`","good":false,"better":"Two lines."}`, e.agent...); w.Code != 200 || !strings.Contains(w.Body.String(), `"kept":true`) {
		t.Fatalf("rate: %d %s", w.Code, w.Body)
	}
	// Made-up ids and the cap are refused, so an agent cannot flood
	// ratings.jsonl.
	if w := e.do("POST", "/api/agent/rate", `{"id":"ex-0123456789abcdef","good":true,"better":"spam"}`, e.agent...); w.Code != 404 {
		t.Errorf("made-up id: %d %s", w.Code, w.Body)
	}
	for i := 1; i < finetune.MaxAgentRatings; i++ {
		if w := e.do("POST", "/api/agent/rate", `{"id":"`+res.ID+`","good":false,"better":"Two lines."}`, e.agent...); w.Code != 200 {
			t.Fatalf("rate again: %d %s", w.Code, w.Body)
		}
	}
	if w := e.do("POST", "/api/agent/rate", `{"id":"`+res.ID+`","good":true}`, e.agent...); w.Code != 429 {
		t.Errorf("over the cap: %d %s", w.Code, w.Body)
	}
	st, _ := e.ft.Stats()
	if st.Examples != 1 || st.Corrected != 1 || st.Usable != 1 {
		t.Errorf("finetune stats %+v", st)
	}
	// the owner can see it
	if w := e.do("GET", "/api/finetune", "", withBearer(testToken)); w.Code != 200 || !strings.Contains(w.Body.String(), `"capture":"delegated"`) {
		t.Errorf("finetune stats: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/finetune/export", "", withBearer(testToken)); w.Code != 200 || !strings.Contains(w.Body.String(), "Two lines.") || w.Header().Get("X-Cerberus-Rows") != "1" {
		t.Errorf("export: %d %s", w.Code, w.Body)
	}
}

func TestDelegateRefusals(t *testing.T) {
	e := newDelegEnv(t, nil)
	cases := []struct {
		body string
		code int
		want string
	}{
		{`{"model":"gemma3:12b","prompt":"x"}`, 409, "not loaded"},
		{`{"model":"openrouter/big","prompt":"x"}`, 400, "not a local model"},
		{`{"model":"local","prompt":"  "}`, 400, "prompt"},
		{`{"model":"local","prompt":"x","files":["../secret"]}`, 400, "inside the workspace"},
		{`{"model":"local","prompt":"x","files":["/etc/passwd"]}`, 400, "inside the workspace"},
		{`{"model":"local","prompt":"x","files":["missing.txt"]}`, 400, "no such file"},
		{`{"model":"local","prompt":"x","files":["a","b","c","d","e","f","g","h","i"]}`, 400, "at most 8"},
	}
	for _, c := range cases {
		w := e.do("POST", "/api/agent/ask", c.body, e.agent...)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.body, w.Code, w.Body)
		}
	}
	if runtime.GOOS != "windows" {
		outside := filepath.Join(t.TempDir(), "secret.txt")
		os.WriteFile(outside, []byte("TOP SECRET"), 0o600)
		os.Symlink(outside, filepath.Join(e.ws, "link.txt"))
		w := e.do("POST", "/api/agent/ask", `{"model":"local","prompt":"x","files":["link.txt"]}`, e.agent...)
		if w.Code != 400 || len(e.calls) != 0 {
			t.Errorf("symlink out of the workspace: %d %s (calls %d)", w.Code, w.Body, len(e.calls))
		}
	}
	// busy
	e.srv.delegate.inFlight.Store(2)
	if w := e.do("POST", "/api/agent/ask", `{"model":"local","prompt":"x"}`, e.agent...); w.Code != 429 {
		t.Errorf("busy: %d %s", w.Code, w.Body)
	}
	e.srv.delegate.inFlight.Store(0)
	// the engine lost the model: a clear 409 the tool turns into "do it yourself"
	e.reply = func(w http.ResponseWriter) {
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"message":"unknown model: qwen3-coder:30b"}}`)
	}
	if w := e.do("POST", "/api/agent/ask", `{"model":"local","prompt":"x"}`, e.agent...); w.Code != 409 || !strings.Contains(w.Body.String(), "unknown model") {
		t.Errorf("upstream 404: %d %s", w.Code, w.Body)
	}
}

func TestDelegateOff(t *testing.T) {
	e := newDelegEnv(t, func(c *config.Config) {
		c.Delegate.Enabled = false
		c.Finetune.Capture = config.CaptureOff
	})
	if w := e.do("POST", "/api/agent/ask", `{"model":"local","prompt":"x"}`, e.agent...); w.Code != 403 || !strings.Contains(w.Body.String(), "delegate.enabled") {
		t.Errorf("off: %d %s", w.Code, w.Body)
	}
	e2 := newDelegEnv(t, func(c *config.Config) { c.Finetune.Capture = config.CaptureOff })
	w := e2.do("POST", "/api/agent/ask", `{"model":"local","prompt":"x"}`, e2.agent...)
	var res agent.DelegateResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || !strings.HasPrefix(res.ID, "ask-") {
		t.Fatalf("capture off: %d %s", w.Code, w.Body)
	}
	if w := e2.do("POST", "/api/agent/rate", `{"id":"`+res.ID+`","good":true}`, e2.agent...); w.Code != 200 || !strings.Contains(w.Body.String(), `"kept":false`) {
		t.Errorf("rate with capture off: %d %s", w.Code, w.Body)
	}
	if st, _ := e2.ft.Stats(); st.Examples != 0 {
		t.Errorf("capture off kept %d examples", st.Examples)
	}
}
