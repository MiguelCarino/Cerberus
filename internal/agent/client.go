package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const (
	connectTimeout = 15 * time.Second
	// readIdle is how long a stream may stay silent: a model may think for
	// minutes before its first token (Atrium's READ_S).
	readIdle = 900 * time.Second
)

// ServerError is an error the server (or the connection to it) reported.
// Status is 0 when there was no HTTP status, for example a refused connection
// or an error chunk inside the stream.
type ServerError struct {
	Status  int
	Message string
}

func (e *ServerError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// message is one OpenAI chat message. Images (data: URLs) make a user
// message multimodal: its content is then sent as parts, the text first and
// one image_url part per image.
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Images     []string   `json:"-"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// contentPart is one element of a multimodal content array.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// wireMessage is message as JSON, with content as a string or as parts.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func (m message) MarshalJSON() ([]byte, error) {
	w := wireMessage{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if len(m.Images) > 0 {
		parts := []contentPart{{Type: "text", Text: m.Content}}
		for _, u := range m.Images {
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: u}})
		}
		w.Content = parts
	}
	return json.Marshal(w)
}

func (m *message) UnmarshalJSON(raw []byte) error {
	var w struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []toolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	*m = message{Role: w.Role, ToolCalls: w.ToolCalls, ToolCallID: w.ToolCallID}
	c := bytes.TrimSpace(w.Content)
	switch {
	case len(c) == 0 || bytes.Equal(c, []byte("null")):
	case c[0] == '"':
		return json.Unmarshal(c, &m.Content)
	case c[0] == '[':
		var parts []contentPart
		if err := json.Unmarshal(c, &parts); err != nil {
			return err
		}
		var text []string
		for _, p := range parts {
			switch {
			case p.Type == "text":
				text = append(text, p.Text)
			case p.Type == "image_url" && p.ImageURL != nil:
				m.Images = append(m.Images, p.ImageURL.URL)
			}
		}
		m.Content = strings.Join(text, "\n")
	default:
		return fmt.Errorf("message content is neither text nor parts")
	}
	return nil
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []message       `json:"messages"`
	Stream        bool            `json:"stream"`
	StreamOptions map[string]bool `json:"stream_options"`
	Tools         []toolSpec      `json:"tools,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
}

// chunk is one streamed chat.completion.chunk (or a whole non-streamed reply,
// whose choices carry "message" instead of "delta").
type chunk struct {
	Choices []struct {
		Delta        delta  `json:"delta"`
		Message      *delta `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// delta keeps loosely typed fields raw: servers disagree on null, strings and
// objects here, and one odd field must not drop the whole chunk.
type delta struct {
	Content          json.RawMessage `json:"content"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
	ToolCalls        []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// rawString returns raw as a string when it is a JSON string, else "".
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// client talks to one OpenAI-compatible server.
type client struct {
	base, key, session string
	name               string // X-Cerberus-Client ("" = cerberus-agent)
	owner              string // X-Cerberus-Owner: the user charged (honoured for the admin token only)
	http               *http.Client
	idle               time.Duration
}

func newClient(base, key, session string) *client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSHandshakeTimeout: connectTimeout,
		MaxIdleConns:        2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &client{base: strings.TrimRight(base, "/"), key: key, session: session,
		http: &http.Client{Transport: tr}, idle: readIdle}
}

// stream POSTs body to /chat/completions with stream on and calls fn for each
// chunk as it arrives. It returns a *ServerError for anything the server or
// the connection got wrong, or ctx's error when ctx ended.
func (c *client) stream(ctx context.Context, body chatRequest, fn func(*chunk)) error {
	body.Stream = true
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var silent atomic.Bool
	watchdog := time.AfterFunc(c.idle, func() { silent.Store(true); cancel() })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return &ServerError{Message: err.Error()}
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return c.connError(ctx, &silent, "could not reach "+c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &ServerError{Status: resp.StatusCode, Message: errorText(raw)}
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		return readWhole(resp.Body, fn)
	}
	err = readSSE(resp.Body, func() { watchdog.Reset(c.idle) }, fn)
	if err != nil && !isServerError(err) {
		return c.connError(ctx, &silent, "the stream broke off", err)
	}
	return err
}

func (c *client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("X-Cerberus-Client", orStr(c.name, "cerberus-agent"))
	if c.session != "" {
		req.Header.Set("X-Cerberus-Session", c.session)
	}
	if c.owner != "" {
		req.Header.Set("X-Cerberus-Owner", c.owner)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
}

// connError tells a cancelled run, a silent server and a broken connection apart.
func (c *client) connError(ctx context.Context, silent *atomic.Bool, what string, err error) error {
	switch {
	case silent.Load():
		return &ServerError{Message: fmt.Sprintf("%s: no data for %s", what, c.idle)}
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return &ServerError{Message: fmt.Sprintf("%s: %v", what, err)}
}

func isServerError(err error) bool {
	var se *ServerError
	return errors.As(err, &se)
}

// readWhole handles a server that answered with one JSON body anyway.
func readWhole(r io.Reader, fn func(*chunk)) error {
	raw, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return err
	}
	var ch chunk
	if err := json.Unmarshal(raw, &ch); err != nil {
		return &ServerError{Message: "the reply was not JSON: " + errorText(raw)}
	}
	if hasError(ch.Error) {
		return &ServerError{Message: errorText(raw)}
	}
	fn(&ch)
	return nil
}

// readSSE parses Server-Sent Events: data lines joined until a blank line,
// comments and event names ignored, [DONE] ends it. alive is called for every
// line read so the idle watchdog only fires on real silence.
func readSSE(r io.Reader, alive func(), fn func(*chunk)) error {
	br := bufio.NewReaderSize(r, 32<<10)
	var data [][]byte
	emit := func() error {
		joined := bytes.Join(data, []byte("\n"))
		data = data[:0]
		var ch chunk
		if json.Unmarshal(joined, &ch) != nil {
			return nil
		}
		if hasError(ch.Error) {
			return &ServerError{Message: errorText(joined)}
		}
		fn(&ch)
		return nil
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			alive()
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			payload := bytes.TrimLeft(line[5:], " ")
			if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
				return nil
			}
			data = append(data, append([]byte(nil), payload...))
		case len(line) == 0 && len(data) > 0:
			if e := emit(); e != nil {
				return e
			}
		}
		if err != nil {
			if len(data) > 0 {
				if e := emit(); e != nil {
					return e
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func hasError(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "false"
}

// errorText pulls a readable message out of an error body.
func errorText(raw []byte) string {
	var data any
	if json.Unmarshal(raw, &data) != nil {
		s := strings.TrimSpace(strings.ToValidUTF8(string(raw), "�"))
		if s == "" {
			return "no details"
		}
		return truncRunes(s, 500)
	}
	if m, ok := data.(map[string]any); ok {
		if e, ok := m["error"]; ok && e != nil {
			if em, ok := e.(map[string]any); ok && em["message"] != nil {
				return truncRunes(fmt.Sprint(em["message"]), 500)
			}
			return truncRunes(fmt.Sprint(e), 500)
		}
	}
	return truncRunes(fmt.Sprint(data), 500)
}

var (
	reNotTools = regexp.MustCompile(`context|too long|token`)
	reTools    = regexp.MustCompile(`\btools?\b|function.?call|jinja`)
	// reNoModel: the model itself is missing (not loaded, unknown). Tools are
	// not the problem, so they stay on and the error is shown as it is.
	reNoModel = regexp.MustCompile(`no models? (are |is )?loaded|model\b.*\bnot (loaded|found)|model_not_found|unknown model`)
)

// noToolSupport reports whether err means "no function calling here": the
// server says so (tools, function calling, llama-server's --jinja), or a
// 400/404/422/500/501 that says nothing about the context size. 401, 403 and
// 429 never count.
func noToolSupport(err *ServerError) bool {
	text := strings.ToLower(err.Message)
	switch {
	case err.Status == 401 || err.Status == 403 || err.Status == 429 || reNotTools.MatchString(text) || reNoModel.MatchString(text):
		return false
	case reTools.MatchString(text):
		return true
	}
	switch err.Status {
	case 400, 404, 422, 500, 501:
		return true
	}
	return false
}
