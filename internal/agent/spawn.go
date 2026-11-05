package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Agents managing agents: the six cerberus_* operations on the daemon's
// restricted /api/agent scope. The built-in agent offers them as extra
// tools (when CERBERUS_URL is set and agent.spawn.enabled), and
// `cerberus mcp` serves the same definitions over MCP for other agents.
// Both authenticate with the session's own agent token plus
// X-Cerberus-Session; the daemon decides what the session may see and do.

const (
	spawnCallTimeout = 60 * time.Second
	waitMaxS         = 600
	waitDefaultS     = 60
	readMaxKiB       = 64
)

// Spawn tool names.
const (
	ToolSpawn = "cerberus_spawn"
	ToolList  = "cerberus_list"
	ToolRead  = "cerberus_read"
	ToolSend  = "cerberus_send"
	ToolWait  = "cerberus_wait"
	ToolStop  = "cerberus_stop"
)

// SpawnTool describes one cerberus_* tool: Schema is its JSON Schema
// (an object schema), used both as OpenAI function parameters and as the
// MCP inputSchema.
type SpawnTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

// SpawnTools are the definitions of the cerberus_* tools: the six session
// tools, then the delegation tools (models, ask, rate).
func SpawnTools() []SpawnTool {
	return append(sessionTools(), DelegateTools()...)
}

// sessionTools are the six tools that start and steer child sessions.
func sessionTools() []SpawnTool {
	id := prop("string", "The child session's id (from cerberus_spawn or cerberus_list).")
	return []SpawnTool{
		{ToolSpawn, "Start a child agent session that works on a task in parallel. It runs on its own; use " +
			"cerberus_wait to wait for it and cerberus_read to see what it did. Limits on depth and count apply, " +
			"and a person may have to approve it first.", schema(map[string]any{
			"task":  prop("string", "The instruction for the child agent, complete and self-contained."),
			"agent": prop("string", "Agent profile to run (optional; the default agent when left out)."),
			"model": prop("string", "Model id for agents that take one (optional)."),
			"workspace": map[string]any{"type": "string", "enum": []string{"scratch", "same"},
				"description": `"scratch" (default): a fresh empty folder. "same": your own workspace ` +
					`(a repo child gets its own worktree of the same repo; a folder child edits your folder in place).`},
			"name": prop("string", "A short label for the child (optional)."),
		}, "task")},
		{ToolList, "List the sessions you started (children and their children) with state and last activity.",
			schema(map[string]any{})},
		{ToolRead, "Read the end of a child session's transcript (plain text).", schema(map[string]any{
			"id":       id,
			"tail_kib": map[string]any{"type": "integer", "minimum": 1, "maximum": readMaxKiB, "description": "How much of the end to return, in KiB (default 16, at most 64)."},
		}, "id")},
		{ToolSend, "Send a follow-up to a child session that has finished or stopped: it starts a new run " +
			"with this text as the task. A running child refuses it (wait for it first).", schema(map[string]any{
			"id": id, "text": prop("string", "The message."),
		}, "id", "text")},
		{ToolWait, "Wait until a child session stops working (it finished, failed, stopped or was discarded) " +
			"or the timeout passes.", schema(map[string]any{
			"id":        id,
			"timeout_s": map[string]any{"type": "integer", "minimum": 0, "maximum": waitMaxS, "description": "Seconds to wait at most (default 60, at most 600)."},
		}, "id")},
		{ToolStop, "Stop a child session's agent.", schema(map[string]any{"id": id}, "id")},
	}
}

// isSpawnTool reports whether name is one of the cerberus_* tools.
func isSpawnTool(name string) bool {
	switch name {
	case ToolSpawn, ToolList, ToolRead, ToolSend, ToolWait, ToolStop:
		return true
	}
	return isDelegateTool(name)
}

// SpawnClient calls the daemon's /api/agent scope for one session.
type SpawnClient struct {
	base, key, session string
	http               *http.Client

	// local: cerberus_ask reads its files on this side (an MCP client,
	// which has no workspace on the daemon); see LocalFiles.
	local *localFiles
}

// NewSpawnClient talks to base (CERBERUS_URL, e.g. http://127.0.0.1:7340)
// as session, with key (the session's agent token).
func NewSpawnClient(base, key, session string) *SpawnClient {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSHandshakeTimeout: connectTimeout,
		MaxIdleConns:        2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &SpawnClient{base: strings.TrimRight(base, "/"), key: key, session: session,
		http: &http.Client{Transport: tr}}
}

// Call runs one cerberus_* tool and returns the text the model gets. An
// error carries the daemon's refusal (limits, unknown child, bad input).
func (c *SpawnClient) Call(ctx context.Context, name string, a map[string]any) (string, error) {
	ar := args(a)
	need := func(k string) (string, error) {
		v := strings.TrimSpace(ar.str(k))
		if v == "" {
			return "", fmt.Errorf("%s is required", k)
		}
		return v, nil
	}
	if isDelegateTool(name) {
		return c.callDelegate(ctx, name, ar)
	}
	switch name {
	case ToolSpawn:
		task, err := need("task")
		if err != nil {
			return "", err
		}
		body := map[string]string{"task": task, "agent": ar.str("agent"), "model": ar.str("model"),
			"workspace": ar.str("workspace"), "name": ar.str("name")}
		var info childInfo
		if err := c.do(ctx, http.MethodPost, "/api/agent/spawn", body, &info, spawnCallTimeout); err != nil {
			return "", err
		}
		s := fmt.Sprintf("Started child session %s (%s): %s", info.ID, info.label(), info.State)
		if info.WaitingReason != "" {
			s += " — " + info.WaitingReason
		}
		return s + ". Use cerberus_wait with this id to wait for it.", nil
	case ToolList:
		var kids []childInfo
		if err := c.do(ctx, http.MethodGet, "/api/agent/children", nil, &kids, spawnCallTimeout); err != nil {
			return "", err
		}
		if len(kids) == 0 {
			return "You have no child sessions.", nil
		}
		var b strings.Builder
		for _, k := range kids {
			b.WriteString(k.line(c.session))
			b.WriteByte('\n')
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case ToolRead:
		id, err := need("id")
		if err != nil {
			return "", err
		}
		kib := 16
		if n, ok := ar.int("tail_kib"); ok && n > 0 {
			kib = min(n, readMaxKiB)
		}
		var t struct {
			ID, State, Text string
			Truncated       bool
		}
		p := "/api/agent/sessions/" + url.PathEscape(id) + "/tail?kib=" + strconv.Itoa(kib)
		if err := c.do(ctx, http.MethodGet, p, nil, &t, spawnCallTimeout); err != nil {
			return "", err
		}
		head := fmt.Sprintf("Session %s is %s.", id, t.State)
		if t.Truncated {
			head += fmt.Sprintf(" Last %d KiB of its transcript:", kib)
		} else {
			head += " Its transcript:"
		}
		if strings.TrimSpace(t.Text) == "" {
			return fmt.Sprintf("Session %s is %s. It has printed nothing yet.", id, t.State), nil
		}
		return head + "\n" + t.Text, nil
	case ToolSend:
		id, err := need("id")
		if err != nil {
			return "", err
		}
		text, err := need("text")
		if err != nil {
			return "", err
		}
		p := "/api/agent/sessions/" + url.PathEscape(id) + "/send"
		if err := c.do(ctx, http.MethodPost, p, map[string]string{"text": text}, nil, spawnCallTimeout); err != nil {
			return "", err
		}
		return "Sent to " + id + ".", nil
	case ToolWait:
		id, err := need("id")
		if err != nil {
			return "", err
		}
		secs := waitDefaultS
		if n, ok := ar.int("timeout_s"); ok {
			secs = min(max(n, 0), waitMaxS)
		}
		var res struct {
			Done    bool
			Session childInfo
		}
		p := "/api/agent/sessions/" + url.PathEscape(id) + "/wait?timeout=" + strconv.Itoa(secs)
		if err := c.do(ctx, http.MethodGet, p, nil, &res, time.Duration(secs)*time.Second+spawnCallTimeout); err != nil {
			return "", err
		}
		if !res.Done {
			return fmt.Sprintf("Session %s is still %s after %d s.", id, res.Session.State, secs), nil
		}
		return "Session " + res.Session.finished() + ". Use cerberus_read to see its output.", nil
	case ToolStop:
		id, err := need("id")
		if err != nil {
			return "", err
		}
		p := "/api/agent/sessions/" + url.PathEscape(id) + "/stop"
		if err := c.do(ctx, http.MethodPost, p, nil, nil, spawnCallTimeout); err != nil {
			return "", err
		}
		return "Stopping " + id + ".", nil
	}
	return "", fmt.Errorf("there is no tool %q", name)
}

// childInfo is the part of a session's Info the tools show.
type childInfo struct {
	ID, Title, Name, State, Agent, Model, Parent string
	WaitingReason, Error, Activity               string
	ExitCode                                     *int
	Depth                                        int
}

func (i childInfo) label() string {
	if i.Name != "" {
		return i.Name
	}
	return i.Title
}

// line is one row of cerberus_list.
func (i childInfo) line(self string) string {
	s := fmt.Sprintf("%s · %s · %s · %s", i.ID, i.label(), i.State, i.Agent)
	if i.Parent != "" && i.Parent != self {
		s += " · child of " + i.Parent
	}
	if i.WaitingReason != "" {
		s += " · " + i.WaitingReason
	}
	if i.Activity != "" {
		s += " · " + truncRunes(i.Activity, 120)
	}
	return s
}

// finished describes a session that stopped working.
func (i childInfo) finished() string {
	s := i.ID + " is " + i.State
	if i.ExitCode != nil {
		s += fmt.Sprintf(" (exit %d)", *i.ExitCode)
	}
	if i.Error != "" {
		s += ": " + truncRunes(i.Error, 200)
	}
	return s
}

// do sends one request and decodes the JSON answer into out (when not nil).
func (c *SpawnClient) do(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	if c.base == "" {
		return errors.New("CERBERUS_URL is not set: this agent is not running in a Cerberus session")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if c.session != "" {
		req.Header.Set("X-Cerberus-Session", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("the Cerberus daemon did not answer within %s", timeout)
		}
		return fmt.Errorf("could not reach the Cerberus daemon: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &daemonError{status: resp.StatusCode, msg: truncRunes(msg, 400)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// daemonError is a refusal from the daemon: its status and message.
type daemonError struct {
	status int
	msg    string
}

func (e *daemonError) Error() string { return fmt.Sprintf("refused (%d): %s", e.status, e.msg) }
