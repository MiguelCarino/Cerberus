package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

// The claude backend, ported from llm-chat.py (argv, start_run,
// handle_event, can_use_tool, control_response). One process per message:
//
//	claude -p --output-format stream-json --verbose --include-partial-messages
//	       --input-format stream-json --permission-prompt-tool stdio
//	       --permission-mode MODE [--resume ID] [--model M]
//
// The user message goes to stdin as one stream-json line, which stays open
// for the permission protocol: a tool that needs a yes arrives on stdout as
//
//	{"type":"control_request","request_id":"…","request":{"subtype":"can_use_tool",
//	 "tool_name":"Bash","input":{…},"permission_suggestions":[…],…}}
//
// and is answered on stdin with
//
//	{"type":"control_response","response":{"subtype":"success","request_id":"…",
//	 "response":{"behavior":"allow","updatedInput":{…},"updatedPermissions":[…]}}}
//	 (or "response":{"behavior":"deny","message":"…"})
//
// A control_cancel_request takes a question back; any other control_request
// subtype gets an error response. stdin closes after the result line.

// cliMode is the --permission-mode for a chat mode.
func cliMode(mode string) string {
	switch mode {
	case "ask", "":
		return "default"
	}
	return mode
}

// claudeModel is the --model for a chat model ("claude" and
// "claude:default" mean the CLI's own default).
func claudeModel(model string) string {
	_, m, _ := strings.Cut(strings.TrimSpace(model), ":")
	if m == "" || strings.EqualFold(m, "default") {
		return ""
	}
	return m
}

// claudeArgv is the command line for one message. settings ("" = none)
// is a --settings JSON (claudeSettings: the protected folders' deny list).
func claudeArgv(bin, mode, session, model, settings string) []string {
	argv := []string{bin, "-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--input-format", "stream-json", "--permission-prompt-tool", "stdio", "--permission-mode", cliMode(mode)}
	if session != "" {
		argv = append(argv, "--resume", session)
	}
	if m := claudeModel(model); m != "" {
		argv = append(argv, "--model", m)
	}
	if settings != "" {
		argv = append(argv, "--settings", settings)
	}
	return argv
}

// claudeSettings is a --settings JSON whose permissions.deny keeps the
// claude CLI out of Cerberus' protected folders (the data folder's chats,
// session records and worktrees, the config folder) and deny_paths, for
// the tools it runs without asking (reads inside its working directory,
// edits in Accept edits mode). Claude Code's rule paths: "//abs" is
// absolute, "~/x" home, anything else relative to the working directory.
func claudeSettings(folders, denyPaths []string) string {
	var deny []string
	add := func(pattern string) {
		for _, tool := range []string{"Read", "Edit"} {
			r := tool + "(" + pattern + ")"
			if !slices.Contains(deny, r) {
				deny = append(deny, r)
			}
		}
	}
	abs := func(p string) string { return "//" + strings.TrimLeft(filepath.ToSlash(p), "/") }
	for _, f := range folders {
		add(abs(f) + "/**")
	}
	for _, p := range denyPaths {
		switch p = strings.TrimSpace(p); {
		case p == "":
		case filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
			add(abs(p))
		default:
			add(p)
		}
	}
	if len(deny) == 0 {
		return ""
	}
	raw, _ := json.Marshal(map[string]any{"permissions": map[string]any{"deny": deny}})
	return string(raw)
}

// claudeGuard is what a claude chat may not do, applied to every
// can_use_tool before a card or a session rule: the phase-3 command policy
// (deny: refused, never asked; ask: a Forced card), deny_paths and the
// protected folders (refused). Reads the CLI makes without asking are
// covered by claudeSettings instead.
type claudeGuard struct {
	paths   *agent.PathGuard
	policy  *agent.Policy
	folders []string // protected folders, for commands that name them
}

func (m *Manager) claudeGuardFor(cwd string) *claudeGuard {
	cfg := m.cfg()
	pg := agent.NewPathGuard(cwd, agent.DefaultDenyPaths(cfg), []string{m.store.dir}, true)
	g := &claudeGuard{paths: pg, policy: agent.NewPolicy(cfg.Agent.Policy)}
	for _, f := range pg.Folders() {
		g.folders = append(g.folders, f)
		if h := m.o.Home; h != "" && under(f, h) {
			if rel, err := filepath.Rel(h, f); err == nil {
				g.folders = append(g.folders, "~/"+filepath.ToSlash(rel))
			}
		}
	}
	if d := configDir(cfg); d != "" {
		g.folders = append(g.folders, d)
	}
	return g
}

// configDir is Cerberus' config folder (the token, API keys), when known.
func configDir(cfg *config.Config) string {
	if cfg.Path() != "" {
		return cfg.Dir()
	}
	if p, err := config.DefaultPath(); err == nil && filepath.IsAbs(p) {
		return filepath.Dir(p)
	}
	return ""
}

// check returns a refusal for the model ("" = go on) and, for a card,
// whether it must always ask (and why).
func (g *claudeGuard) check(tool string, input map[string]any) (refuse string, forced bool, why string) {
	if g == nil {
		return "", false, ""
	}
	var paths []string
	if p := toolPath(input); p != "" {
		paths = append(paths, p)
	}
	if pat := str(input["pattern"]); tool == "Glob" && (strings.HasPrefix(pat, "/") || strings.HasPrefix(pat, "~")) {
		if i := strings.IndexAny(pat, "*?[{"); i >= 0 {
			pat = pat[:i]
		}
		paths = append(paths, pat)
	}
	for _, p := range paths {
		if r := g.paths.Refused(p); r != "" {
			return "Refused: " + p + " is " + r + ". It cannot be approved; do not try to get around it.", false, ""
		}
	}
	if tool != "Bash" {
		return "", false, ""
	}
	cmd := str(input["command"])
	switch v := g.policy.Check(cmd); v.Action {
	case agent.PolicyDeny:
		return "Refused by the command policy: this command " + v.Reason + ". It is never run and cannot be " +
			"approved; do not try to get around it, find another way or ask the user to run it themselves.", false, ""
	case agent.PolicyAsk:
		return "", true, "policy: this command " + truncRunes(v.Reason, 200) + " (always asked)"
	}
	for _, f := range g.folders {
		if f != "" && strings.Contains(cmd, f) {
			return "", true, "this command names Cerberus' protected folder " + f + " (always asked)"
		}
	}
	return "", false, ""
}

// claudeUserLine is the stream-json user message: the images as base64
// blocks, then the words.
func claudeUserLine(text string, images []string) []byte {
	var content any = text
	if len(images) > 0 {
		var blocks []map[string]any
		for _, img := range images {
			media, data, ok := splitDataURL(img)
			if !ok {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": media, "data": data}})
		}
		if text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		content = blocks
	}
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	return append(raw, '\n')
}

// splitDataURL splits data:MEDIA;base64,DATA.
func splitDataURL(u string) (media, data string, ok bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", "", false
	}
	head, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", false
	}
	media, enc, _ := strings.Cut(head, ";")
	return media, data, enc == "base64" && media != ""
}

// claudeEnv is the environment without the nested-session markers.
func claudeEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" || k == "CLAUDE_CODE_ENTRYPOINT" || strings.HasPrefix(k, "CERBERUS_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// claudeRun is one claude process and what it said so far.
type claudeRun struct {
	m     *Manager
	l     *live
	ctx   context.Context
	guard *claudeGuard

	stdinMu sync.Mutex
	stdin   io.WriteCloser
	closed  bool

	streamed map[string]bool // message ids whose text came as deltas
	msgID    string
	tools    map[string]bool // tool_use ids announced
	text     strings.Builder // this run's reply text, for the transcript
	done     agent.Done
	result   bool
	wg       sync.WaitGroup // answer goroutines
}

// runClaude runs one message through the claude CLI.
func (m *Manager) runClaude(ctx context.Context, l *live, bin string, q queued) {
	m.mu.Lock()
	c := l.chat
	cwd := c.Cwd
	guard := m.claudeGuardFor(cwd)
	argv := claudeArgv(bin, c.Mode, c.ClaudeSession, c.Model,
		claudeSettings(guard.paths.Folders(), agent.DefaultDenyPaths(m.cfg())))
	m.emitAgentLocked(l, agent.Event{Type: agent.EvStart, Step: 0})
	m.mu.Unlock()

	started := m.now()
	r := &claudeRun{m: m, l: l, ctx: ctx, guard: guard, streamed: map[string]bool{}, tools: map[string]bool{}}
	err := r.run(argv, cwd, claudeUserLine(q.Text, q.Images))
	// The process is gone: a card still open can never be used, and its
	// answer goroutine would otherwise wait for a person forever.
	m.mu.Lock()
	m.denyAsksLocked(l, "end")
	m.mu.Unlock()
	r.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	d := r.done
	if d.Seconds == 0 {
		d.Seconds = m.now().Sub(started).Seconds()
	}
	d.Tools = true
	switch {
	case ctx.Err() != nil || l.stopped:
		d.OK, d.Stopped, d.Error = false, true, ""
	case err != nil && d.Error == "":
		d.OK, d.Error = false, err.Error()
	case !r.result && d.Error == "":
		d.OK, d.Error = false, "claude ended without a result"
	}
	if t := strings.TrimSpace(r.text.String()); t != "" {
		l.chat.Messages = append(l.chat.Messages, agent.Message{Role: "assistant", Content: t})
	} else if !d.OK && len(l.chat.Messages) > 0 && l.chat.ClaudeSession == "" {
		// Nothing came back and no session exists: the message is not kept.
		l.chat.Messages = l.chat.Messages[:len(l.chat.Messages)-1]
	}
	m.emitAgentLocked(l, agent.Event{Type: agent.EvDone, Done: &d})
	m.endRunLocked(l, d)
}

func (r *claudeRun) run(argv []string, cwd string, first []byte) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = claudeEnv()
	platform.NewGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuf
	cmd.Stderr = &stderr
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return errors.New("could not start claude: " + err.Error())
	}
	r.stdin = stdin
	r.write(first)

	exited := make(chan struct{})
	go func() { // Stop: SIGINT, then SIGTERM, then SIGKILL to the group
		select {
		case <-r.ctx.Done():
			platform.Interrupt(cmd.Process)
			t := time.NewTimer(2 * time.Second)
			defer t.Stop()
			select {
			case <-exited:
			case <-t.C:
				platform.Terminate(cmd.Process, 2*time.Second, exited)
			}
		case <-exited:
		}
	}()

	br := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			r.line(line)
		}
		if err != nil {
			break
		}
	}
	r.closeStdin()
	werr := cmd.Wait()
	close(exited)
	if werr != nil && !r.result && r.ctx.Err() == nil {
		if tail := strings.TrimSpace(reANSI.ReplaceAllString(stderr.String(), "")); tail != "" {
			return errors.New("claude: " + truncRunes(tail, 600))
		}
		return errors.New("claude: " + werr.Error())
	}
	return nil
}

func (r *claudeRun) write(raw []byte) {
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()
	if r.closed || r.stdin == nil {
		return
	}
	_, _ = r.stdin.Write(raw)
}

func (r *claudeRun) closeStdin() {
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()
	if !r.closed && r.stdin != nil {
		r.closed = true
		_ = r.stdin.Close()
	}
}

func (r *claudeRun) emit(ev agent.Event) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.m.emitAgentLocked(r.l, ev)
}

// line handles one stdout line; a surprise in one never ends the run.
func (r *claudeRun) line(raw []byte) {
	var ev map[string]any
	if json.Unmarshal(bytes.TrimSpace(raw), &ev) != nil || ev == nil {
		return
	}
	if sid := str(ev["session_id"]); sid != "" {
		r.m.mu.Lock()
		if r.l.chat.ClaudeSession != sid {
			r.l.chat.ClaudeSession = sid
			r.m.saveLocked(r.l)
		}
		r.m.mu.Unlock()
	}
	switch str(ev["type"]) {
	case "stream_event":
		se, _ := ev["event"].(map[string]any)
		r.stream(se, str(ev["parent_tool_use_id"]))
	case "assistant":
		msg, _ := ev["message"].(map[string]any)
		r.assistant(msg, str(ev["parent_tool_use_id"]))
	case "user":
		msg, _ := ev["message"].(map[string]any)
		r.user(msg)
	case "result":
		r.handleResult(ev)
	case "control_request":
		r.control(ev)
	case "control_cancel_request":
		if id := str(ev["request_id"]); id != "" {
			r.m.mu.Lock()
			if p := r.m.dropAskLocked(r.l, id); p != nil {
				p.ch <- answer{d: agent.Decision{Note: "cancelled"}}
				r.m.emitLocked(r.l, "answered", map[string]any{"req": id, "decision": DecideDeny, "by": "cancel"})
			}
			r.m.mu.Unlock()
		}
	}
}

func (r *claudeRun) stream(se map[string]any, parent string) {
	switch str(se["type"]) {
	case "message_start":
		msg, _ := se["message"].(map[string]any)
		r.msgID = str(msg["id"])
	case "content_block_delta":
		delta, _ := se["delta"].(map[string]any)
		switch str(delta["type"]) {
		case "text_delta":
			if parent == "" {
				if t := str(delta["text"]); t != "" {
					r.streamed[r.msgID] = true
					r.text.WriteString(t)
					r.emit(agent.Event{Type: agent.EvText, Text: t})
				}
			}
		case "thinking_delta":
			if t := str(delta["thinking"]); t != "" && parent == "" {
				r.emit(agent.Event{Type: agent.EvReasoning, Text: t})
			}
		}
	}
}

func (r *claudeRun) assistant(msg map[string]any, parent string) {
	mid := str(msg["id"])
	if u, ok := msg["usage"].(map[string]any); ok && parent == "" {
		in := num(u["input_tokens"]) + num(u["cache_read_input_tokens"]) + num(u["cache_creation_input_tokens"])
		out := num(u["output_tokens"])
		if in+out > 0 {
			r.m.mu.Lock()
			r.l.chat.ContextUsed = in + out
			r.m.mu.Unlock()
			r.done.Usage = agent.Usage{Prompt: in, Completion: out}
		}
	}
	blocks, _ := msg["content"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		switch t := str(block["type"]); t {
		case "text":
			text := str(block["text"])
			if parent != "" || r.streamed[mid] || strings.TrimSpace(text) == "" {
				continue
			}
			r.text.WriteString(text)
			r.emit(agent.Event{Type: agent.EvText, Text: text})
		case "tool_use", "server_tool_use", "mcp_tool_use":
			id := str(block["id"])
			if id == "" || r.tools[id] {
				continue
			}
			r.tools[id] = true
			name := orStr(str(block["name"]), "tool")
			input, _ := block["input"].(map[string]any)
			r.done.Calls++
			r.emit(agent.Event{Type: agent.EvTool, ID: id, Name: name, Canon: name, Input: input, Args: input})
		}
	}
}

func (r *claudeRun) user(msg map[string]any) {
	blocks, _ := msg["content"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if str(block["type"]) != "tool_result" {
			continue
		}
		isErr, _ := block["is_error"].(bool)
		r.emit(agent.Event{Type: agent.EvToolDone, ID: str(block["tool_use_id"]), Output: resultText(block["content"]),
			IsError: isErr})
	}
}

// resultText is a tool_result's content as text.
func resultText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			pm, _ := p.(map[string]any)
			switch str(pm["type"]) {
			case "text":
				parts = append(parts, str(pm["text"]))
			case "image":
				parts = append(parts, "[image]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func (r *claudeRun) handleResult(ev map[string]any) {
	r.result = true
	r.closeStdin() // the turn is over: claude may exit
	isErr, _ := ev["is_error"].(bool)
	sub := str(ev["subtype"])
	d := &r.done
	d.OK = !isErr && (sub == "" || sub == "success")
	if !d.OK {
		detail := str(ev["result"])
		if detail == "" {
			if errs, ok := ev["errors"].([]any); ok {
				var s []string
				for _, e := range errs {
					s = append(s, str(e))
				}
				detail = strings.Join(s, ", ")
			}
		}
		d.Error = "claude reported an error: " + orStr(detail, orStr(sub, "error"))
	}
	if n, ok := ev["num_turns"].(float64); ok {
		d.Steps = int(n)
	}
	if ms, ok := ev["duration_ms"].(float64); ok {
		d.Seconds = ms / 1000
	}
	if u, ok := ev["usage"].(map[string]any); ok {
		d.Total = agent.Usage{Prompt: num(u["input_tokens"]) + num(u["cache_read_input_tokens"]) + num(u["cache_creation_input_tokens"]),
			Completion: num(u["output_tokens"])}
	}
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if mu, ok := ev["modelUsage"].(map[string]any); ok {
		for _, v := range mu {
			if vm, ok := v.(map[string]any); ok {
				if w := num(vm["contextWindow"]); w > r.l.ctxMax {
					r.l.ctxMax = w
				}
			}
		}
	}
	// total_cost_usd is the whole session's: it replaces the chat's cost,
	// and this run's is the step.
	if cost, ok := ev["total_cost_usd"].(float64); ok {
		if prev := r.l.chat.CostUSD; cost >= prev {
			d.CostUSD = cost - prev
		}
		r.l.chat.CostUSD = cost
	}
}

// control handles a control_request.
func (r *claudeRun) control(ev map[string]any) {
	rid := str(ev["request_id"])
	req, _ := ev["request"].(map[string]any)
	if rid == "" || req == nil {
		return
	}
	if str(req["subtype"]) != "can_use_tool" {
		r.writeJSON(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": rid, "error": "this client answers can_use_tool only"}})
		return
	}
	input, _ := req["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	tool := orStr(str(req["tool_name"]), "?")
	ask := agent.AskRequest{ID: rid, Tool: tool, Display: orStr(str(req["display_name"]), orStr(str(req["title"]), tool)),
		Input: input, Suggestions: []map[string]any{}, Blocked: str(req["blocked_path"]),
		Reason:    strings.TrimSpace(reANSI.ReplaceAllString(str(req["decision_reason"]), "")),
		ToolUseID: str(req["tool_use_id"]), Agent: str(req["agent_id"])}
	if s, ok := req["permission_suggestions"].([]any); ok {
		for _, x := range s {
			if xm, ok := x.(map[string]any); ok {
				ask.Suggestions = append(ask.Suggestions, xm)
			}
		}
	}
	if refuse, forced, why := r.guard.check(tool, input); refuse != "" {
		r.m.mu.Lock()
		r.m.emitLocked(r.l, "note", map[string]any{"text": truncRunes(refuse, 300)})
		r.m.mu.Unlock()
		r.writeJSON(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": rid, "response": map[string]any{"behavior": "deny", "message": refuse}}})
		return
	} else if forced {
		ask.Forced = true
		if ask.Reason != "" {
			why += " · " + ask.Reason
		}
		ask.Reason = why
	}
	r.m.mu.Lock()
	if rule, ok := r.m.coveringRuleLocked(r.l, ask); ok {
		r.m.emitLocked(r.l, "note", map[string]any{"text": "allowed by " + coverText(rule)})
		r.m.mu.Unlock()
		r.respond(rid, input, answer{d: agent.Decision{Allow: true}})
		return
	}
	r.m.emitAgentLocked(r.l, agent.Event{Type: agent.EvAsk, Req: &ask})
	r.m.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		a := r.m.wait(r.ctx, r.l, ask)
		if !a.d.Allow && a.d.Note == "cancelled" {
			return // claude took the question back
		}
		r.respond(rid, input, a)
	}()
}

func (r *claudeRun) respond(rid string, input map[string]any, a answer) {
	var resp map[string]any
	if a.d.Allow {
		resp = map[string]any{"behavior": "allow", "updatedInput": input}
		if len(a.updated) > 0 {
			resp["updatedPermissions"] = a.updated
		}
	} else {
		resp = map[string]any{"behavior": "deny", "message": denyMessage(a.d.Note)}
	}
	r.writeJSON(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": rid, "response": resp}})
}

func (r *claudeRun) writeJSON(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	r.write(append(raw, '\n'))
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// tailBuf keeps the last 8 KiB written to it.
type tailBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 8<<10 {
		t.buf = t.buf[len(t.buf)-8<<10:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ---- the claude CLI's saved sessions (the recent list)

// ClaudeSession is one saved claude CLI session.
type ClaudeSession struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"` // "claude"
	Title   string  `json:"title"`
	Cwd     string  `json:"cwd"`
	Updated float64 `json:"updated"`
}

// ClaudeSessions lists the claude CLI's newest sessions from
// ~/.claude/projects (title: the last ai-title, else the first thing
// typed), only when the CLI is installed.
func (m *Manager) ClaudeSessions(limit int) []ClaudeSession {
	if !m.ClaudeAvailable() {
		return []ClaudeSession{}
	}
	root := m.o.ClaudeProjects
	if root == "" {
		root = filepath.Join(m.o.Home, ".claude", "projects")
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	projects, _ := os.ReadDir(root)
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(root, p.Name()))
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if info, err := e.Info(); err == nil {
				files = append(files, file{filepath.Join(root, p.Name(), e.Name()), info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if limit <= 0 {
		limit = 15
	}
	out := []ClaudeSession{}
	for _, f := range files {
		cwd, title, ok := claudeSessionInfo(f.path)
		if !ok {
			continue
		}
		out = append(out, ClaudeSession{ID: strings.TrimSuffix(filepath.Base(f.path), ".jsonl"), Kind: "claude",
			Title: title, Cwd: cwd, Updated: unix(f.mod)})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// claudeSessionInfo reads a session file's cwd and title.
func claudeSessionInfo(path string) (cwd, title string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	first := ""
	for n := 0; n <= 400 && (cwd == "" || first == ""); n++ {
		line, err := br.ReadBytes('\n')
		var ev map[string]any
		if json.Unmarshal(line, &ev) == nil {
			if cwd == "" {
				cwd = str(ev["cwd"])
			}
			if meta, _ := ev["isMeta"].(bool); first == "" && str(ev["type"]) == "user" && !meta {
				msg, _ := ev["message"].(map[string]any)
				first = userWords(msg["content"])
			}
		}
		if err != nil {
			break
		}
	}
	if st, err := f.Stat(); err == nil {
		off := max(0, st.Size()-65536)
		buf := make([]byte, st.Size()-off)
		if _, err := f.ReadAt(buf, off); err == nil || errors.Is(err, io.EOF) {
			for _, line := range bytes.Split(buf, []byte("\n")) {
				if !bytes.Contains(line, []byte(`"ai-title"`)) {
					continue
				}
				var ev map[string]any
				if json.Unmarshal(line, &ev) == nil && str(ev["aiTitle"]) != "" {
					title = str(ev["aiTitle"])
				}
			}
		}
	}
	if title == "" {
		title = first
	}
	if title == "" {
		return "", "", false
	}
	return cwd, truncRunes(strings.Join(strings.Fields(title), " "), 200), true
}

// userWords is what a person typed in a saved user entry: tool results and
// the harness's own tagged lines are not words.
func userWords(content any) string {
	var parts []string
	switch v := content.(type) {
	case string:
		parts = []string{v}
	case []any:
		for _, b := range v {
			bm, _ := b.(map[string]any)
			switch str(bm["type"]) {
			case "text":
				parts = append(parts, str(bm["text"]))
			case "image":
				parts = append(parts, "[image]")
			}
		}
	}
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" && !strings.HasPrefix(p, "<") {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

// ContinueClaude makes a Cerberus chat that resumes a claude CLI session
// (the same chat again when it was continued before).
func (m *Manager) ContinueClaude(sessionID string) (*Chat, error) {
	if !m.ClaudeAvailable() {
		return nil, ErrNoClaude
	}
	for _, s := range m.ClaudeSessions(500) {
		if s.ID != sessionID {
			continue
		}
		m.contMu.Lock()
		defer m.contMu.Unlock()
		if c := m.bySource("claude:" + s.ID); c != nil {
			return c, nil
		}
		cwd := s.Cwd
		if _, err := m.checkCwd(cwd); err != nil {
			cwd = m.o.Home
		}
		now := m.now()
		c := &Chat{ID: newID(now), Title: s.Title, Cwd: cwd, Model: "claude", Created: unix(now), Updated: unix(now),
			Messages: []agent.Message{}, Backend: BackendClaude, Mode: orStr(m.cfg().Chat.DefaultMode, "ask"),
			ClaudeSession: s.ID, Source: "claude:" + s.ID}
		if err := m.store.save(c); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.addLocked(newLive(c))
		m.mu.Unlock()
		return clone(c), nil
	}
	return nil, ErrNotFound
}

// bySource finds a chat continued from source.
func (m *Manager) bySource(source string) *Chat {
	for _, c := range listDir(m.store.dir) {
		if c.Source == source {
			return c
		}
	}
	return nil
}
