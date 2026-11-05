package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"

	"carino.systems/cerberus/internal/agent"
)

// `cerberus mcp` is a stdio MCP server (JSON-RPC 2.0, one message per line)
// that lets any MCP-capable agent running in a Cerberus session start and
// steer child sessions: the six cerberus_* tools, which call the daemon's
// restricted /api/agent scope with the session's own agent token. It reads
// CERBERUS_URL, OPENAI_API_KEY (the session agent token, never the admin
// token) and CERBERUS_SESSION, as written into the session's mcp.json.
//
// Only the tools capability is offered. Requests are answered as they
// finish (a long cerberus_wait does not hold up a ping), notifications are
// ignored except notifications/cancelled, and stdout carries nothing but
// protocol messages; diagnostics go to stderr.
//
// The server is dual-era. Legacy clients (MCP 2025-06-18) open with an
// initialize handshake, which negotiates the version: a supported requested
// version is echoed, anything else gets 2025-06-18, the newest version that
// has a handshake. Modern clients (MCP 2026-07-28) skip the handshake and
// name their version in every request's params._meta; such a request is
// served on its own, its result carries resultType and the server's identity
// in _meta, and an unsupported version is refused with
// UnsupportedProtocolVersion (-32022) listing what is supported.
// server/discover reports identity, capabilities and supported versions.
// No state is kept between requests either way, so tools/list and
// tools/call work without a prior initialize.

const (
	mcpProtocolLegacy = "2025-06-18" // the newest version with an initialize handshake
	mcpProtocolModern = "2026-07-28" // per-request _meta, server/discover
	mcpMaxLine        = 8 << 20

	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaServerInfo      = "io.modelcontextprotocol/serverInfo"

	mcpInstructions = "Start and steer child agent sessions on this Cerberus daemon. You see only the " +
		"sessions you started; depth and count limits apply, and a person may have to approve a spawn."

	mcpClientInstructions = "Cerberus, the owner's self-hosted LLM runtime. Hand bulk, low-risk text jobs " +
		"(summaries of long logs or files, drafts, translations, boilerplate) to a local model with " +
		"cerberus_ask (cerberus_models lists them; files are read on this machine, so their text never " +
		"enters your context) and check what comes back. cerberus_spawn starts an agent session in a " +
		"scratch folder; you see and steer only the sessions you started, and limits and approval apply."
)

// mcpSupportedVersions lists the protocol versions served, newest first.
var mcpSupportedVersions = []string{mcpProtocolModern, mcpProtocolLegacy}

// JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602

	rpcUnsupportedVersion = -32022 // MCP 2026-07-28 UnsupportedProtocolVersion
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpCaller runs one tool; *agent.SpawnClient in production.
type mcpCaller interface {
	Call(ctx context.Context, name string, args map[string]any) (string, error)
}

func mcpCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cerberus mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	base := fs.String("url", os.Getenv("CERBERUS_URL"), "the daemon's URL (default $CERBERUS_URL)")
	key := fs.String("key", os.Getenv("OPENAI_API_KEY"), "the session's agent token (default $OPENAI_API_KEY)")
	sess := fs.String("session", os.Getenv("CERBERUS_SESSION"), "the calling session (default $CERBERUS_SESSION)")
	clientName := fs.String("client", "", "run for an outside agent as the MCP client NAME (cerberus pair --mcp --name NAME)")
	cfgPath := fs.String("config", "", "config file, with --client (default ~/.config/cerberus/config.json)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var caller *agent.SpawnClient
	if *clientName != "" {
		c, err := mcpClientCaller(*clientName, *cfgPath, *base, *key)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus mcp:", err)
			return 1
		}
		caller = c
	} else {
		if *base == "" || *sess == "" {
			fmt.Fprintln(stderr, "cerberus mcp: CERBERUS_URL or CERBERUS_SESSION is not set; tool calls will fail")
		}
		caller = agent.NewSpawnClient(*base, *key, *sess)
	}
	srv := newMCPServer(caller, stdout)
	if *clientName != "" {
		srv.instructions = mcpClientInstructions
	}
	if err := srv.serve(context.Background(), stdin); err != nil {
		fmt.Fprintln(stderr, "cerberus mcp:", err)
		return 1
	}
	return 0
}

// mcpServer answers MCP requests on one stream.
type mcpServer struct {
	tools        mcpCaller
	out          io.Writer
	instructions string

	wmu     sync.Mutex // one message per write
	mu      sync.Mutex
	pending map[string]context.CancelFunc // in-flight tools/call by request id
	wg      sync.WaitGroup
}

func newMCPServer(tools mcpCaller, out io.Writer) *mcpServer {
	return &mcpServer{tools: tools, out: out, instructions: mcpInstructions, pending: map[string]context.CancelFunc{}}
}

// serve reads messages until in ends, then cancels what is still running
// and waits for it.
func (s *mcpServer) serve(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		s.wg.Wait()
	}()
	rd := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := readMessage(rd)
		if len(bytes.TrimSpace(line)) > 0 {
			s.handle(ctx, line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readMessage reads one newline-terminated message, refusing absurd sizes.
func readMessage(rd *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := rd.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > mcpMaxLine {
			// Skip the rest of the line and report it as unparseable.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = rd.ReadSlice('\n')
			}
			return []byte("\x00too long"), err
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return buf, err
		}
	}
}

func (s *mcpServer) write(r rpcResponse) {
	r.JSONRPC = "2.0"
	if r.ID == nil {
		r.ID = json.RawMessage("null")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		raw, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: r.ID, Error: &rpcError{Code: -32603, Message: err.Error()}})
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.out.Write(append(raw, '\n'))
}

func (s *mcpServer) fail(id json.RawMessage, code int, msg string) {
	s.write(rpcResponse{ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// handle dispatches one message.
func (s *mcpServer) handle(ctx context.Context, line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		if t := bytes.TrimSpace(line); len(t) > 0 && t[0] == '[' {
			s.fail(nil, rpcInvalidRequest, "batches are not supported")
			return
		}
		s.fail(nil, rpcParseError, "parse error")
		return
	}
	notification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.Method == "" {
		return // a response from the client: this server never asks for one
	}
	if notification {
		if req.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) == nil {
				s.mu.Lock()
				if c := s.pending[string(p.RequestID)]; c != nil {
					c()
				}
				s.mu.Unlock()
			}
		}
		return // every other notification is ignored
	}
	if req.JSONRPC != "2.0" {
		s.fail(req.ID, rpcInvalidRequest, `jsonrpc must be "2.0"`)
		return
	}
	requested, modern, err := requestVersion(req.Params)
	if err != nil {
		s.fail(req.ID, rpcInvalidParams, err.Error())
		return
	}
	if modern && !mcpSupported(requested) && req.Method != "initialize" {
		s.write(rpcResponse{ID: req.ID, Error: &rpcError{
			Code:    rpcUnsupportedVersion,
			Message: "Unsupported protocol version",
			Data:    map[string]any{"supported": mcpSupportedVersions, "requested": requested},
		}})
		return
	}
	// Only 2026-07-28 results carry resultType and _meta; a legacy version
	// named in _meta is served the legacy way.
	stamp := modern && requested == mcpProtocolModern
	reply := func(res map[string]any) {
		if stamp {
			res = stampModern(res)
		}
		s.write(rpcResponse{ID: req.ID, Result: res})
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.write(rpcResponse{ID: req.ID, Result: map[string]any{
			"protocolVersion": negotiateInitialize(p.ProtocolVersion),
			"capabilities":    mcpCapabilities(),
			"serverInfo":      mcpServerInfo(),
			"instructions":    s.instructions,
		}})
	case "server/discover":
		s.write(rpcResponse{ID: req.ID, Result: stampModern(map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      mcpCapabilities(),
			"instructions":      s.instructions,
		})})
	case "ping":
		reply(map[string]any{})
	case "tools/list":
		tools := make([]map[string]any, 0, 6)
		for _, t := range agent.SpawnTools() {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		reply(map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			s.fail(req.ID, rpcInvalidParams, "tools/call needs a tool name and object arguments")
			return
		}
		if !knownTool(p.Name) {
			s.fail(req.ID, rpcInvalidParams, "unknown tool "+p.Name)
			return
		}
		cctx, cancel := context.WithCancel(ctx)
		key := string(req.ID)
		s.mu.Lock()
		s.pending[key] = cancel
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() {
				s.mu.Lock()
				delete(s.pending, key)
				s.mu.Unlock()
				cancel()
			}()
			text, err := s.tools.Call(cctx, p.Name, nonNilArgs(p.Arguments))
			if cctx.Err() != nil && ctx.Err() == nil {
				return // cancelled by the client: no response is sent
			}
			res := map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": false}
			if err != nil {
				res = map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
			}
			reply(res)
		})
	default:
		s.fail(req.ID, rpcMethodNotFound, "method not found: "+req.Method)
	}
}

// requestVersion reads the per-request protocol version a 2026-07-28 client
// puts in params._meta. modern is false when there is none (a legacy
// request, or params that are not an object: the method reports those).
func requestVersion(params json.RawMessage) (version string, modern bool, err error) {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return "", false, nil
	}
	raw, ok := p.Meta[metaProtocolVersion]
	if !ok {
		return "", false, nil
	}
	if json.Unmarshal(raw, &version) != nil || version == "" {
		return "", true, errors.New(metaProtocolVersion + " must be a non-empty string")
	}
	return version, true, nil
}

func mcpSupported(v string) bool {
	for _, s := range mcpSupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

// negotiateInitialize picks the version an initialize answer names: the
// client's when supported, else the newest version that has a handshake
// (a legacy client cannot speak 2026-07-28 anyway).
func negotiateInitialize(requested string) string {
	if mcpSupported(requested) {
		return requested
	}
	return mcpProtocolLegacy
}

// stampModern adds what every 2026-07-28 result carries: resultType and the
// server's identity in _meta.
func stampModern(res map[string]any) map[string]any {
	res["resultType"] = "complete"
	res["_meta"] = map[string]any{metaServerInfo: mcpServerInfo()}
	return res
}

func mcpCapabilities() map[string]any {
	return map[string]any{"tools": map[string]any{}}
}

func mcpServerInfo() map[string]any {
	return map[string]any{"name": "cerberus", "title": "Cerberus sessions", "version": version}
}

func knownTool(name string) bool {
	for _, t := range agent.SpawnTools() {
		if t.Name == name {
			return true
		}
	}
	return false
}

func nonNilArgs(a map[string]any) map[string]any {
	if a == nil {
		return map[string]any{}
	}
	return a
}
