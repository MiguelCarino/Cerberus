package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpPipe runs an mcpServer over pipes and exchanges lines with it.
type mcpPipe struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	done chan error
}

func startMCP(t *testing.T, tools mcpCaller) *mcpPipe {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := newMCPServer(tools, outW)
	done := make(chan error, 1)
	go func() {
		done <- srv.serve(context.Background(), inR)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	p := &mcpPipe{t: t, in: inW, out: sc, done: done}
	t.Cleanup(func() {
		inW.Close()
		go func() { _, _ = io.Copy(io.Discard, outR) }()
		<-done
	})
	return p
}

func (p *mcpPipe) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, line+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

// recv reads one response (fails after 30 s).
func (p *mcpPipe) recv() map[string]any {
	p.t.Helper()
	got := make(chan map[string]any, 1)
	go func() {
		if !p.out.Scan() {
			got <- nil
			return
		}
		var m map[string]any
		if err := json.Unmarshal(p.out.Bytes(), &m); err != nil {
			m = map[string]any{"unparsed": p.out.Text()}
		}
		got <- m
	}()
	select {
	case m := <-got:
		if m == nil {
			p.t.Fatal("server output ended")
		}
		return m
	case <-time.After(30 * time.Second):
		p.t.Fatal("no response within 30 s")
	}
	return nil
}

func (p *mcpPipe) rpc(id int, method string, params any) map[string]any {
	p.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, _ := json.Marshal(req)
	p.send(string(raw))
	m := p.recv()
	if got, _ := m["id"].(float64); int(got) != id || m["jsonrpc"] != "2.0" {
		p.t.Fatalf("response to %s: %v", method, m)
	}
	return m
}

// toolText returns a tools/call result's text and isError.
func toolText(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content %v", res)
	}
	c := content[0].(map[string]any)
	if c["type"] != "text" {
		t.Fatalf("content type %v", c)
	}
	isErr, _ := res["isError"].(bool)
	return c["text"].(string), isErr
}

type fakeCaller struct {
	mu    sync.Mutex
	calls []string
	block chan struct{}
}

func (f *fakeCaller) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	switch name {
	case "cerberus_stop":
		return "", errors.New("refused (404): no such child session")
	case "cerberus_wait":
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	b, _ := json.Marshal(args)
	return name + " " + string(b), nil
}

func TestMCPProtocol(t *testing.T) {
	f := &fakeCaller{block: make(chan struct{})}
	p := startMCP(t, f)

	init := p.rpc(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol %v", res)
	}
	caps := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"].(map[string]any); !ok || len(caps) != 1 {
		t.Fatalf("capabilities %v", caps)
	}
	if si := res["serverInfo"].(map[string]any); si["name"] != "cerberus" || si["version"] == "" {
		t.Fatalf("serverInfo %v", si)
	}
	// Notifications get no answer: the next line must be the ping's.
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if m := p.rpc(2, "ping", nil); m["result"] == nil || m["error"] != nil {
		t.Fatalf("ping %v", m)
	}

	list := p.rpc(3, "tools/list", map[string]any{})
	tools := list["result"].(map[string]any)["tools"].([]any)
	want := []string{"cerberus_spawn", "cerberus_list", "cerberus_read", "cerberus_send", "cerberus_wait", "cerberus_stop", "cerberus_models", "cerberus_ask", "cerberus_rate"}
	if len(tools) != len(want) {
		t.Fatalf("tools %v", tools)
	}
	for i, raw := range tools {
		tool := raw.(map[string]any)
		schema, _ := tool["inputSchema"].(map[string]any)
		if tool["name"] != want[i] || tool["description"] == "" || schema["type"] != "object" || schema["properties"] == nil {
			t.Fatalf("tool %d: %v", i, tool)
		}
	}
	spawn := tools[0].(map[string]any)["inputSchema"].(map[string]any)
	if req := spawn["required"].([]any); len(req) != 1 || req[0] != "task" {
		t.Fatalf("spawn schema %v", spawn)
	}

	text, isErr := toolText(t, p.rpc(4, "tools/call", map[string]any{"name": "cerberus_list"}))
	if isErr || text != "cerberus_list {}" {
		t.Fatalf("list call %q %v", text, isErr)
	}
	text, isErr = toolText(t, p.rpc(5, "tools/call", map[string]any{"name": "cerberus_stop", "arguments": map[string]any{"id": "x"}}))
	if !isErr || !strings.Contains(text, "no such child") {
		t.Fatalf("failing call %q %v", text, isErr)
	}

	// A blocked wait does not hold up other requests; cancelling it sends
	// no response.
	p.send(`{"jsonrpc":"2.0","id":"w1","method":"tools/call","params":{"name":"cerberus_wait","arguments":{"id":"c"}}}`)
	if m := p.rpc(6, "ping", nil); m["error"] != nil {
		t.Fatalf("ping during wait %v", m)
	}
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"w1"}}`)
	p.send(`{"jsonrpc":"2.0","id":"w2","method":"tools/call","params":{"name":"cerberus_wait","arguments":{"id":"d"}}}`)
	close(f.block)
	m := p.recv()
	if m["id"] != "w2" {
		t.Fatalf("expected only w2's answer, got %v", m)
	}

	for _, c := range []struct {
		line string
		code float64
	}{
		{`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`, rpcMethodNotFound},
		{`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"rm_rf"}}`, rpcInvalidParams},
		{`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":"nope"}`, rpcInvalidParams},
		{`{"jsonrpc":"1.0","id":10,"method":"ping"}`, rpcInvalidRequest},
		{`{not json`, rpcParseError},
		{`[{"jsonrpc":"2.0","id":11,"method":"ping"}]`, rpcInvalidRequest},
	} {
		p.send(c.line)
		m := p.recv()
		e, _ := m["error"].(map[string]any)
		if e == nil || e["code"].(float64) != c.code {
			t.Errorf("%s → %v", c.line, m)
		}
	}
}

func TestMCPEndsOnEOF(t *testing.T) {
	var out strings.Builder
	code := mcpCmd(nil, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &out, io.Discard)
	if code != 0 || !strings.Contains(out.String(), `"id":1`) || !strings.Contains(out.String(), `"result":{}`) {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}

// modernParams returns params carrying 2026-07-28 per-request metadata.
func modernParams(version string, extra map[string]any) map[string]any {
	p := map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    version,
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// checkModern asserts a 2026-07-28 result: resultType and serverInfo in _meta.
func checkModern(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	if res["resultType"] != "complete" {
		t.Fatalf("resultType %v", res)
	}
	meta, _ := res["_meta"].(map[string]any)
	si, _ := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if si["name"] != "cerberus" || si["version"] == "" {
		t.Fatalf("serverInfo %v", res)
	}
	return res
}

// TestMCPModern serves a 2026-07-28 client that never sends initialize.
func TestMCPModern(t *testing.T) {
	p := startMCP(t, &fakeCaller{block: make(chan struct{})})

	res := checkModern(t, p.rpc(1, "server/discover", modernParams("2026-07-28", nil)))
	vers, _ := res["supportedVersions"].([]any)
	if len(vers) != 2 || vers[0] != "2026-07-28" || vers[1] != "2025-06-18" {
		t.Fatalf("supportedVersions %v", res)
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"].(map[string]any); !ok || len(caps) != 1 {
		t.Fatalf("capabilities %v", res)
	}
	if s, _ := res["instructions"].(string); s == "" {
		t.Fatalf("instructions %v", res)
	}
	// A probe without _meta (a dual-era client's first message) is answered too.
	checkModern(t, p.rpc(2, "server/discover", nil))

	list := checkModern(t, p.rpc(3, "tools/list", modernParams("2026-07-28", nil)))
	if tools, _ := list["tools"].([]any); len(tools) != 9 {
		t.Fatalf("tools %v", list)
	}
	m := p.rpc(4, "tools/call", modernParams("2026-07-28", map[string]any{"name": "cerberus_list", "arguments": map[string]any{"x": 1}}))
	checkModern(t, m)
	if text, isErr := toolText(t, m); isErr || text != `cerberus_list {"x":1}` {
		t.Fatalf("call %q %v", text, isErr)
	}
	checkModern(t, p.rpc(5, "ping", modernParams("2026-07-28", nil)))

	// A supported legacy version named in _meta is served the legacy way.
	legacy := p.rpc(6, "tools/list", modernParams("2025-06-18", nil))["result"].(map[string]any)
	if _, ok := legacy["resultType"]; ok {
		t.Fatalf("legacy result stamped: %v", legacy)
	}

	// An unsupported version is refused with the supported list, on any method.
	for i, method := range []string{"tools/list", "server/discover", "tools/call"} {
		m := p.rpc(10+i, method, modernParams("1900-01-01", map[string]any{"name": "cerberus_list"}))
		e, _ := m["error"].(map[string]any)
		if e == nil || e["code"].(float64) != rpcUnsupportedVersion || e["message"] != "Unsupported protocol version" {
			t.Fatalf("%s: %v", method, m)
		}
		data := e["data"].(map[string]any)
		if data["requested"] != "1900-01-01" || len(data["supported"].([]any)) != 2 {
			t.Fatalf("%s data %v", method, data)
		}
	}
	// A malformed version is invalid params.
	m = p.rpc(20, "tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": 7}})
	if e, _ := m["error"].(map[string]any); e == nil || e["code"].(float64) != rpcInvalidParams {
		t.Fatalf("bad version %v", m)
	}
}

// TestMCPLegacyWithoutInitialize serves a 2025-06-18 client that skips the
// handshake: no _meta, plain results.
func TestMCPLegacyWithoutInitialize(t *testing.T) {
	p := startMCP(t, &fakeCaller{block: make(chan struct{})})
	res := p.rpc(1, "tools/list", nil)["result"].(map[string]any)
	if tools, _ := res["tools"].([]any); len(tools) != 9 {
		t.Fatalf("tools %v", res)
	}
	if _, ok := res["resultType"]; ok {
		t.Fatalf("legacy result stamped: %v", res)
	}
	m := p.rpc(2, "tools/call", map[string]any{"name": "cerberus_list"})
	if text, isErr := toolText(t, m); isErr || text != "cerberus_list {}" {
		t.Fatalf("call %q %v", text, isErr)
	}
	if _, ok := m["result"].(map[string]any)["_meta"]; ok {
		t.Fatalf("legacy call stamped: %v", m)
	}
}

// TestMCPInitializeNegotiation echoes a supported version and answers
// anything else with the newest handshake version.
func TestMCPInitializeNegotiation(t *testing.T) {
	p := startMCP(t, &fakeCaller{block: make(chan struct{})})
	for i, c := range []struct{ asked, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2026-07-28", "2026-07-28"},
		{"2025-11-25", "2025-06-18"},
		{"2024-11-05", "2025-06-18"},
		{"", "2025-06-18"},
	} {
		res := p.rpc(i+1, "initialize", map[string]any{"protocolVersion": c.asked, "capabilities": map[string]any{}})["result"].(map[string]any)
		if res["protocolVersion"] != c.want {
			t.Errorf("asked %q → %v, want %q", c.asked, res["protocolVersion"], c.want)
		}
	}
}

func TestMCPVersionHelpers(t *testing.T) {
	for _, c := range []struct {
		params string
		ver    string
		modern bool
		err    bool
	}{
		{``, "", false, false},
		{`"nope"`, "", false, false},
		{`{}`, "", false, false},
		{`{"_meta":{"progressToken":1}}`, "", false, false},
		{`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, "2026-07-28", true, false},
		{`{"_meta":{"io.modelcontextprotocol/protocolVersion":""}}`, "", true, true},
		{`{"_meta":{"io.modelcontextprotocol/protocolVersion":null}}`, "", true, true},
	} {
		v, modern, err := requestVersion(json.RawMessage(c.params))
		if v != c.ver || modern != c.modern || (err != nil) != c.err {
			t.Errorf("%s → %q %v %v", c.params, v, modern, err)
		}
	}
}
