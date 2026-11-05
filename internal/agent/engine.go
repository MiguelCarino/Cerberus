package agent

// The tool loop as a library (day phase 5). Run drives one message's whole
// turn against an OpenAI-compatible server: the request, its stream, the
// tools the reply calls, and again, until an answer without tools. It
// reports everything through an event callback in Atrium's wire protocol
// (llm_chat_engine.Run), so the terminal agent (loop.go) and the web chat
// (internal/chat) are two thin clients of the same engine.
//
// Events, in order of a typical turn:
//
//	start      {step}                        a request goes out
//	reasoning  {text}                        thinking words, as they come
//	text       {text}                        reply words, as they come
//	usage      {prompt, completion}          one request's token counts
//	tool       {id, name, canon, input}      a call begins (canon: Claude Code's tool name and input shape)
//	ask        {req}                         a yes is needed: the Asker answers
//	tool_done  {id, output, is_error}
//	no_tools   {why}                         the model takes no tools: plain chat from now on
//	note       {text}                        a one-line remark (Cerberus' addition)
//	done       {ok, stopped, error, steps, seconds, usage, total, calls, tools}
//
// Permissions follow Atrium's four modes (ask, auto, acceptEdits, plan)
// plus ModeBypass for `cerberus agent --approve auto`. The phase-3 command
// policy stays authoritative in every mode: Deny is never run and never
// asked, AskAlways is asked even when the mode or a session rule would let
// the command through.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/permits"
	"carino.systems/cerberus/internal/platform"
)

// Message is one OpenAI chat message (role, content, images as data: URLs,
// tool calls, tool_call_id). ToolCall and FunctionCall are its tool calls.
type (
	Message      = message
	ToolCall     = toolCall
	FunctionCall = functionCall
)

// Mode is a permission mode.
type Mode string

const (
	ModeAsk         Mode = "ask"         // every write and command asks
	ModeAuto        Mode = "auto"        // read-only commands (SafeCommand) go, the rest asks
	ModeAcceptEdits Mode = "acceptEdits" // file edits inside the working directory go, commands ask
	ModePlan        Mode = "plan"        // reading tools only; writes and commands are refused
	ModeBypass      Mode = "bypass"      // everything runs without asking (`--approve auto`); the policy still applies
)

// ParseMode accepts a mode's name and the common spellings of it ("default"
// and "manual" are ask, "accept-edits" is acceptEdits).
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ask", "default", "manual", "":
		return ModeAsk, true
	case "auto":
		return ModeAuto, true
	case "acceptedits", "accept-edits", "accept_edits":
		return ModeAcceptEdits, true
	case "plan":
		return ModePlan, true
	case "bypass", "bypasspermissions":
		return ModeBypass, true
	}
	return ModeAsk, false
}

// Event types.
const (
	EvStart     = "start"
	EvText      = "text"
	EvReasoning = "reasoning"
	EvTool      = "tool"
	EvToolDone  = "tool_done"
	EvAsk       = "ask"
	EvNoTools   = "no_tools"
	EvUsage     = "usage"
	EvNote      = "note"
	EvDone      = "done"
)

// Event is one thing that happened in a run. Which fields are set depends
// on Type; MarshalJSON writes exactly the wire fields of that type.
type Event struct {
	Type    string
	Step    int
	Text    string
	ID      string
	Name    string
	Canon   string
	Input   map[string]any
	Args    map[string]any // the call's own arguments (not on the wire); nil when they were not valid JSON
	Output  string
	IsError bool
	Req     *AskRequest
	Why     string
	Usage   Usage
	Done    *Done
}

// Usage is token counts.
type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
}

// Done ends every run. Usage is the last request's (the context the next
// message starts from), Total the sum over the run's requests.
type Done struct {
	OK      bool    `json:"ok"`
	Stopped bool    `json:"stopped"`
	Error   string  `json:"error"`
	Steps   int     `json:"steps"`
	Seconds float64 `json:"seconds"`
	Usage   Usage   `json:"usage"`
	Total   Usage   `json:"total"`
	Calls   int     `json:"calls"`
	Tools   bool    `json:"tools"`
	CostUSD float64 `json:"cost,omitempty"` // set by backends that report a price (the claude CLI)
}

// MarshalJSON writes the event in Atrium's wire shape: {"type": …, fields}.
func (e Event) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": e.Type}
	switch e.Type {
	case EvStart:
		m["step"] = e.Step
	case EvText, EvReasoning, EvNote:
		m["text"] = e.Text
	case EvTool:
		in := e.Input
		if in == nil {
			in = map[string]any{}
		}
		m["id"], m["name"], m["canon"], m["input"] = e.ID, e.Name, e.Canon, in
	case EvToolDone:
		m["id"], m["output"], m["is_error"] = e.ID, e.Output, e.IsError
	case EvAsk:
		m["req"] = e.Req
	case EvNoTools:
		m["why"] = e.Why
	case EvUsage:
		m["prompt"], m["completion"] = e.Usage.Prompt, e.Usage.Completion
	case EvDone:
		if e.Done != nil {
			raw, err := json.Marshal(e.Done)
			if err != nil {
				return nil, err
			}
			var dm map[string]any
			if err := json.Unmarshal(raw, &dm); err != nil {
				return nil, err
			}
			for k, v := range dm {
				m[k] = v
			}
		}
	default:
		if e.Text != "" {
			m["text"] = e.Text
		}
	}
	return json.Marshal(m)
}

// AskRequest is a permission card: Atrium's req {id, tool, display, input,
// suggestions, blocked, reason, tool_use_id, agent, native}. Tool and Input
// are the canonical (Claude Code) shapes so session rules work the same for
// every backend.
type AskRequest struct {
	ID          string           `json:"id"`
	Tool        string           `json:"tool"`
	Display     string           `json:"display"`
	Input       map[string]any   `json:"input"`
	Suggestions []map[string]any `json:"suggestions"`
	Blocked     string           `json:"blocked,omitempty"`
	Reason      string           `json:"reason,omitempty"`
	ToolUseID   string           `json:"tool_use_id"`
	Agent       string           `json:"agent,omitempty"`
	Native      bool             `json:"native"`
	// Forced is set when the command policy says "always ask": session
	// rules and "always" answers never answer it.
	Forced bool `json:"forced,omitempty"`
	// Always are the rules "Always allow" may save for this request
	// (permits.Suggest, filled by the asker that shows it; none when Forced).
	Always []permits.Rule `json:"always,omitempty"`
	// Args are the native tool's own arguments (the terminal card shows them).
	Args map[string]any `json:"-"`
}

// Decision answers an AskRequest: Allow once, Allow for the session
// (Session, the asker keeps the rule), or deny with an optional Note that
// reaches the model.
type Decision struct {
	Allow   bool
	Session bool
	Note    string
}

// Asker answers permission cards. Ask blocks until a person answers or ctx
// ends (then it returns a denial).
type Asker interface {
	Ask(ctx context.Context, req AskRequest) Decision
}

// RuleChecker is an optional Asker extension: a session rule that already
// covers the request answers it without a card (no ask event).
type RuleChecker interface {
	Covered(req AskRequest) (Decision, bool)
}

// RunOptions is one run's setup.
type RunOptions struct {
	Base, Model, Key, Session string
	Client                    string // X-Cerberus-Client (default "cerberus-agent")
	// Owner is the user a daemon-internal run is charged to
	// (X-Cerberus-Owner; the gateway honours it only for the admin token).
	Owner  string
	Cwd    string // working directory (writes only under it)
	Origin string // the repo Cwd is a worktree of ("" = none)
	// Messages is the conversation so far, ending in the new user message;
	// without the system prompt (rebuilt per request).
	Messages []Message
	Mode     Mode
	MaxSteps int  // tool rounds; the last one goes out without tools
	NoTools  bool // plain chat: no tools offered at all
	// DenyPaths are globs never read or written (DefaultDenyPaths adds the
	// config folder).
	DenyPaths []string
	// ProtectData hides Cerberus' data folder (session records, worktrees,
	// chats) from the tools, except the working directory.
	ProtectData bool
	// Protect lists more folders the tools may not touch (except Cwd), such
	// as a chat store kept outside the default data folder.
	Protect        []string
	CommandTimeout time.Duration // run_command default (the model may ask up to 600 s)
	ResultClip     int           // characters of one tool result (default 12 000)
	// AskOutside makes reads outside Cwd ask (the chat's folder access).
	AskOutside bool
	// Jail refuses every read and write outside Cwd (no card to ask): a
	// non-admin user's chat works in its own folder only (Phase 4a).
	Jail bool
	// Wrap, when set, runs run_command's argv inside a sandbox (bwrap with
	// only Cwd writable). With Jail and no Wrap, run_command is not
	// offered at all.
	Wrap        func(argv []string) []string
	MaxTokens   int
	Temperature *float64
	// System builds the system prompt per request (tools: whether tools go
	// with it). Nil uses the terminal agent's prompt.
	System func(tools bool) string
	// Refusal is what the model reads for a denied card (nil: "The user
	// refused this." with the note).
	Refusal func(note string) string
	Spawn   *SpawnClient
	Policy  *Policy // nil: built-in rules only
	Asker   Asker   // nil: every card is denied
	// OnSave is called with the conversation whenever it changed (before
	// the first request too) and the system prompt last built.
	OnSave func(system string, messages []Message)
	// OnReply is called after each request the server answered.
	OnReply func()

	private privateArea // set by the terminal client
	// reapCommands ends what the run's commands left running when the run
	// ends (the terminal agent: its process ends with the run anyway).
	reapCommands bool
}

// Result is what a run leaves: the conversation (with this run's replies
// and tool results), the done summary and the error, if any.
type Result struct {
	Messages []Message
	System   string
	Done     Done
	Err      error
}

// DefaultDenyPaths is cfg's deny_paths plus Cerberus' own config folder.
func DefaultDenyPaths(cfg *config.Config) []string {
	return append(append([]string(nil), cfg.Agent.DenyPaths...), configDeny(cfg)...)
}

// engine is one run in progress.
type engine struct {
	o      RunOptions
	emit   func(Event)
	client *client
	tools  *toolbox

	messages []Message
	system   string
	toolsOn  bool
	steps    int
	calls    int
	last     Usage
	total    Usage
}

// Run executes one turn and returns when the model answered without tools,
// the step limit was used up, the server failed or ctx ended. emit is
// called from Run's goroutine; the last event is always done.
func Run(ctx context.Context, o RunOptions, emit func(Event)) Result {
	if emit == nil {
		emit = func(Event) {}
	}
	if o.Client == "" {
		o.Client = "cerberus-agent"
	}
	if o.Mode == "" {
		o.Mode = ModeAsk
	}
	if o.ResultClip <= 0 {
		o.ResultClip = outputMax
	}
	if o.MaxSteps < 0 {
		o.MaxSteps = 0
	}
	if o.Refusal == nil {
		o.Refusal = refusal
	}
	if o.ProtectData && len(o.private.roots) == 0 {
		o.private = dataArea(o.Cwd)
	}
	for _, p := range o.Protect {
		if filepath.IsAbs(p) {
			o.private.roots = append(o.private.roots, filepath.Clean(p))
		}
	}
	if o.ProtectData || len(o.Protect) > 0 {
		o.private.own = ownInside(o.Cwd, o.private.roots)
	}
	tools := newToolbox(o.Cwd, o.DenyPaths, o.CommandTimeout)
	tools.origin = o.Origin
	tools.jail, tools.wrap = o.Jail, o.Wrap
	tools.private = o.private.resolved()
	tools.resultMax = o.ResultClip
	if o.reapCommands {
		defer tools.reap()
	}
	c := newClient(o.Base, o.Key, o.Session)
	c.name = o.Client
	c.owner = o.Owner
	e := &engine{o: o, emit: emit, client: c, tools: tools,
		messages: append([]Message(nil), o.Messages...), toolsOn: !o.NoTools}
	started := time.Now()
	err := e.loop(ctx)
	d := Done{Steps: e.steps, Seconds: time.Since(started).Seconds(), Usage: e.last, Total: e.total,
		Calls: e.calls, Tools: e.toolsOn}
	switch {
	case err == nil:
		d.OK = true
	case ctx.Err() != nil:
		d.Stopped = true
	default:
		d.Error = describeError(err)
	}
	emit(Event{Type: EvDone, Done: &d})
	return Result{Messages: e.messages, System: e.system, Done: d, Err: err}
}

// dataArea hides the data folder's records from a run outside a session.
func dataArea(cwd string) privateArea {
	data, err := platform.DataDir()
	if err != nil || !filepath.IsAbs(data) {
		return privateArea{}
	}
	return privateArea{roots: []string{filepath.Join(data, "sessions"), filepath.Join(data, "worktrees"),
		filepath.Join(data, "chats")}}
}

// ownInside is cwd when it lies inside one of roots (a session's worktree
// opened as a chat folder stays usable), else nothing: a folder above the
// data (the home folder, the default) must not unprotect it.
func ownInside(cwd string, roots []string) []string {
	for _, r := range roots {
		if cwd != r && under(cwd, r) {
			return []string{cwd}
		}
	}
	return nil
}

func (e *engine) prompt(tools bool) string {
	if e.o.System != nil {
		return e.o.System(tools)
	}
	return systemPrompt(e.o.Cwd, string(e.o.Mode), tools, nil)
}

func (e *engine) save() {
	if e.o.OnSave == nil {
		return
	}
	if e.system == "" {
		e.system = e.prompt(e.toolsOn)
	}
	e.o.OnSave(e.system, append([]Message(nil), e.messages...))
}

// loop runs requests until an answer without tool calls. The last allowed
// step goes out without tools, so the model has to answer.
func (e *engine) loop(ctx context.Context) error {
	// Saved before the first request too, so a run that fails at once
	// still has its task on record.
	e.save()
	for step := 0; step <= e.o.MaxSteps; {
		if err := ctx.Err(); err != nil {
			return err
		}
		last := step == e.o.MaxSteps
		e.emit(Event{Type: EvStart, Step: step})
		body := e.request(last)
		text, calls, streamed, err := e.one(ctx, body)
		if err != nil {
			if ctx.Err() != nil && strings.TrimSpace(text) != "" {
				// What was on screen stays said.
				e.messages = append(e.messages, Message{Role: "assistant", Content: text})
				e.save()
			}
			if ctx.Err() == nil && !streamed && hasImages(e.messages) && imageRefusal(err) {
				dropImages(e.messages)
				e.emit(Event{Type: EvNote, Text: "the model takes no images (" + truncRunes(describeError(err), 160) + "): mentioned as text"})
				continue
			}
			var se *ServerError
			if ctx.Err() == nil && body.Tools != nil && !streamed && errors.As(err, &se) && noToolSupport(se) {
				e.toolsOn = false
				e.emit(Event{Type: EvNoTools, Why: se.Message})
				continue
			}
			return err
		}
		e.steps = step + 1
		if e.o.OnReply != nil {
			e.o.OnReply()
		}
		e.messages = append(e.messages, Message{Role: "assistant", Content: text, ToolCalls: calls})
		e.save() // before the tools too: a stop mid-tool keeps the reply
		if len(calls) == 0 {
			return nil
		}
		for _, c := range calls {
			var out string
			if ctx.Err() != nil {
				out = "stopped by the user before it ran"
				e.emit(Event{Type: EvToolDone, ID: c.ID, Output: out, IsError: true})
			} else {
				out = e.call(ctx, c)
			}
			e.messages = append(e.messages, Message{Role: "tool", ToolCallID: c.ID, Content: out})
		}
		e.save()
		step++
	}
	return nil
}

func (e *engine) request(last bool) chatRequest {
	tools := e.toolsOn && !last
	msgs := make([]Message, 0, len(e.messages)+2)
	e.system = e.prompt(tools)
	msgs = append(msgs, Message{Role: "system", Content: e.system})
	msgs = append(msgs, e.messages...)
	if last && e.toolsOn {
		msgs = append(msgs, Message{Role: "user", Content: lastStepNudge})
	}
	body := chatRequest{Model: e.o.Model, Messages: msgs, StreamOptions: map[string]bool{"include_usage": true}}
	if e.o.MaxTokens > 0 {
		body.MaxTokens = e.o.MaxTokens
	}
	body.Temperature = e.o.Temperature
	if tools {
		body.Tools = e.toolSpecs()
	}
	return body
}

// toolSpecs are the tools offered: the reading tools only in plan mode,
// else the file and command tools plus the cerberus_* tools when spawning
// is available.
func (e *engine) toolSpecs() []toolSpec {
	var out []toolSpec
	for _, t := range toolSpecs {
		if e.o.Mode == ModePlan && !readTools[t.Function.Name] {
			continue
		}
		if t.Function.Name == "run_command" && e.o.Jail && e.o.Wrap == nil {
			continue // no sandbox for commands: not offered
		}
		out = append(out, t)
	}
	if e.o.Spawn == nil || e.o.Mode == ModePlan {
		return out
	}
	for _, t := range SpawnTools() {
		out = append(out, toolSpec{Type: "function", Function: toolDef{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
	}
	return out
}

// one sends one streamed request, emitting as it goes. streamed reports
// whether any reply text arrived (then a tool-less retry would repeat it).
func (e *engine) one(ctx context.Context, body chatRequest) (text string, calls []toolCall, streamed bool, err error) {
	var reply strings.Builder
	pending := map[int]*pendingCall{}
	var split thinkSplitter
	finish, thought := "", false
	show := func(words, think string) {
		if think != "" {
			thought = true
			e.emit(Event{Type: EvReasoning, Text: think})
		}
		if words != "" {
			reply.WriteString(words)
			e.emit(Event{Type: EvText, Text: words})
		}
	}
	err = e.client.stream(ctx, body, func(ch *chunk) {
		if ch.Usage != nil {
			e.last = Usage{Prompt: ch.Usage.PromptTokens, Completion: ch.Usage.CompletionTokens}
			e.total.Prompt += ch.Usage.PromptTokens
			e.total.Completion += ch.Usage.CompletionTokens
			e.emit(Event{Type: EvUsage, Usage: e.last})
		}
		for _, c := range ch.Choices {
			d := c.Delta
			if c.Message != nil {
				d = *c.Message
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
			if s := rawString(d.ReasoningContent) + rawString(d.Reasoning); s != "" {
				thought = true
				e.emit(Event{Type: EvReasoning, Text: s})
			}
			if piece := rawString(d.Content); piece != "" {
				show(split.feed(piece))
			}
			for _, tc := range d.ToolCalls {
				idx := len(pending)
				if tc.Index != nil {
					idx = *tc.Index
				}
				p := pending[idx]
				if p == nil {
					p = &pendingCall{}
					pending[idx] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				p.name += tc.Function.Name
				if s := rawString(tc.Function.Arguments); s != "" {
					p.args.WriteString(s)
				} else if a := tc.Function.Arguments; len(a) > 0 && a[0] == '{' {
					p.args.Reset()
					p.args.Write(a)
				}
			}
		}
	})
	show(split.flush())
	if err == nil && ranOutThinking(finish, thought, reply.Len(), len(pending)) {
		e.emit(Event{Type: EvNote, Text: ThinkingBudgetNote})
	}
	return reply.String(), assemble(pending), reply.Len() > 0, err
}

// ThinkingBudgetNote is shown when a reply's token budget ran out while the
// model was still thinking: the answer is empty, not lost (gateway phase G4).
const ThinkingBudgetNote = "The token budget ran out while the model was thinking, so there is no answer. " +
	"Raise the budget or lower the reasoning effort."

// ranOutThinking reports a reply that ended on its budget with thinking but
// no answer text and no tool call.
func ranOutThinking(finish string, thought bool, answer, calls int) bool {
	return finish == "length" && thought && answer == 0 && calls == 0
}

// readTools never change anything.
var readTools = map[string]bool{"read_file": true, "list_dir": true, "search": true}

// verdict is what happens to a tool call before it runs.
type verdict struct {
	action string // "allow" | "ask" | "refuse"
	why    string // the refusal the model reads, or the card's reason
	forced bool   // the policy's "always ask"
}

// judge applies the mode, the path rules and the command policy.
func (e *engine) judge(name string, a args) verdict {
	if readTools[name] {
		if e.o.AskOutside {
			p := resolve(orStr(a.str("path"), "."), e.o.Cwd)
			// A protected path is refused by the tool itself: no card.
			if e.tools.refused(p) == "" && !e.readable(p) {
				return verdict{action: "ask", why: "outside the working folder " + e.o.Cwd}
			}
		}
		return verdict{action: "allow"}
	}
	if !changeTools[name] {
		return verdict{action: "allow"} // the toolbox says there is no such tool
	}
	if e.o.Mode == ModePlan {
		return verdict{action: "refuse", why: name + " is not available in plan mode (read-only): propose the change instead"}
	}
	if why := e.tools.precheck(name, a); why != "" {
		return verdict{action: "refuse", why: why}
	}
	ask := true
	switch e.o.Mode {
	case ModeBypass:
		ask = false
	case ModeAcceptEdits:
		ask = !editTools[name] // precheck keeps edits inside the working directory
	case ModeAuto:
		ask = name != "run_command" || !SafeCommand(a.str("command"), e.o.Cwd, e.readable)
	}
	v := verdict{action: "allow"}
	if name == "run_command" {
		pv := e.o.Policy.Check(a.str("command"))
		switch pv.Action {
		case PolicyDeny:
			return verdict{action: "refuse", why: "Refused by the command policy: this command " + pv.Reason +
				". It is never run and cannot be approved; do not try to get around it, find another way or " +
				"ask the user to run it themselves."}
		case PolicyAsk:
			ask = true
			v.forced = true
			v.why = "policy: this command " + truncRunes(sanitize(pv.Reason), 200) + " (asked even in auto mode)"
		case PolicyAllow:
			ask = false
		}
	}
	if ask {
		v.action = "ask"
	}
	return v
}

// readable reports whether a path may be read without asking: not
// protected, and inside the working directory (or anywhere, unless reads
// outside it ask), or covered by a session rule.
func (e *engine) readable(p string) bool {
	if e.tools.refused(p) != "" {
		return false
	}
	if !e.o.AskOutside || under(p, e.o.Cwd) || under(realPath(p), e.tools.realCwd) {
		return true
	}
	if rc, ok := e.o.Asker.(RuleChecker); ok {
		if d, ok := rc.Covered(AskRequest{Tool: "Read", Display: "read_file", Input: map[string]any{"file_path": p}, Native: true}); ok {
			return d.Allow
		}
	}
	return false
}

// call runs one tool call (asking first when needed) and returns what the
// model is told.
func (e *engine) call(ctx context.Context, c toolCall) string {
	e.calls++
	name := c.Function.Name
	var a args
	if err := json.Unmarshal([]byte(c.Function.Arguments), &a); err != nil || a == nil {
		e.emit(Event{Type: EvTool, ID: c.ID, Name: name, Canon: name, Input: map[string]any{}})
		why := "not an object"
		if err != nil {
			why = err.Error()
		}
		out := fmt.Sprintf("the arguments were not valid JSON (%s): %s", why, truncRunes(c.Function.Arguments, 200))
		e.emit(Event{Type: EvToolDone, ID: c.ID, Output: out, IsError: true})
		return out
	}
	cname, cinput := Canon(name, a)
	e.emit(Event{Type: EvTool, ID: c.ID, Name: name, Canon: cname, Input: cinput, Args: a})
	if isSpawnTool(name) {
		return e.callSpawn(ctx, c.ID, name, a)
	}
	v := e.judge(name, a)
	switch v.action {
	case "refuse":
		e.emit(Event{Type: EvToolDone, ID: c.ID, Output: v.why, IsError: true})
		return v.why
	case "ask":
		req := AskRequest{ID: randomHex(16), Tool: cname, Display: name, Input: cinput, Suggestions: []map[string]any{},
			Reason: v.why, ToolUseID: c.ID, Native: true, Forced: v.forced, Args: a}
		if d := e.ask(ctx, req); !d.Allow {
			out := e.o.Refusal(d.Note)
			if ctx.Err() != nil {
				out = "stopped by the user"
			}
			e.emit(Event{Type: EvToolDone, ID: c.ID, Output: out, IsError: true})
			return out
		}
	}
	out, bad := e.tools.run(ctx, name, a)
	e.emit(Event{Type: EvToolDone, ID: c.ID, Output: out, IsError: bad})
	return out
}

// ask answers a card: a covering session rule first (unless the policy
// forces the question), else the Asker after an ask event.
func (e *engine) ask(ctx context.Context, req AskRequest) Decision {
	if e.o.Asker == nil {
		return Decision{Note: "nobody can answer permission requests here"}
	}
	if rc, ok := e.o.Asker.(RuleChecker); ok && !req.Forced {
		if d, ok := rc.Covered(req); ok {
			return d
		}
	}
	if ctx.Err() != nil {
		return Decision{Note: "stopped"}
	}
	r := req
	e.emit(Event{Type: EvAsk, Req: &r})
	return e.o.Asker.Ask(ctx, req)
}

// spawnApproval lists the cerberus_* tools that need approval in ask mode,
// like run_command: starting and stopping sessions.
var spawnApproval = map[string]bool{ToolSpawn: true, ToolStop: true}

// callSpawn runs a cerberus_* tool, asking first for spawn and stop.
func (e *engine) callSpawn(ctx context.Context, id, name string, a args) string {
	done := func(out string, bad bool) string {
		e.emit(Event{Type: EvToolDone, ID: id, Output: out, IsError: bad})
		return out
	}
	if e.o.Spawn == nil {
		return done(fmt.Sprintf("there is no tool %q: the tools are %s", name, toolNames()), true)
	}
	if spawnApproval[name] && e.o.Mode != ModeBypass {
		req := AskRequest{ID: randomHex(16), Tool: name, Display: name, Input: map[string]any(a),
			Suggestions: []map[string]any{}, ToolUseID: id, Native: true, Args: a}
		if d := e.ask(ctx, req); !d.Allow {
			return done(e.o.Refusal(d.Note), true)
		}
	}
	out, err := e.o.Spawn.Call(ctx, name, a)
	if err != nil {
		return done(clip(err.Error(), e.o.ResultClip), true)
	}
	return done(clip(out, e.o.ResultClip), false)
}

// Canon maps a native tool call to the name and input shape Claude Code
// uses (Bash {command}, Write {file_path, content}, Edit {file_path,
// old_string, new_string}, Read {file_path}, LS {path}, Grep {pattern,
// glob, path}), so permission cards, chips and session rules share one
// code path across backends.
func Canon(name string, a map[string]any) (string, map[string]any) {
	s := func(k string) string { return args(a).str(k) }
	path := s("path")
	switch name {
	case "run_command":
		out := map[string]any{"command": s("command")}
		if d := s("description"); d != "" {
			out["description"] = d
		}
		return "Bash", out
	case "write_file":
		return "Write", map[string]any{"file_path": path, "content": s("content")}
	case "edit_file":
		out := map[string]any{"file_path": path, "old_string": s("old_string"), "new_string": s("new_string")}
		if args(a).bool("replace_all") {
			out["replace_all"] = true
		}
		return "Edit", out
	case "read_file":
		return "Read", map[string]any{"file_path": path}
	case "list_dir":
		return "LS", map[string]any{"path": orStr(path, ".")}
	case "search":
		out := map[string]any{}
		for _, k := range []string{"pattern", "glob", "path"} {
			if v := s(k); v != "" {
				out[k] = v
			}
		}
		return "Grep", out
	}
	out := make(map[string]any, len(a))
	for k, v := range a {
		out[k] = v
	}
	return name, out
}

// Auto mode's read-only commands (Atrium's SAFE_COMMANDS): one of these, as
// one plain command. Left out on purpose: anything that can write through an
// option (sort -o, uniq IN OUT, tree -o, rg --pre runs a program, sed -i, tee).
var (
	safeCommands = set("ls", "pwd", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "stat", "file",
		"du", "df", "which", "whoami", "id", "uname", "date", "echo", "diff", "cmp", "cut",
		"realpath", "readlink", "basename", "dirname", "nproc", "uptime", "free", "lsblk", "true",
		"find", "git")
	safeGit    = set("status", "log", "diff", "show", "rev-parse", "ls-files", "blame", "describe", "shortlog")
	unsafeFind = set("-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls")
	shellSigns = ";&|`\n<>$*?[]{}"
)

// SafeCommand is auto mode's judgement (Atrium's safe_command): true only
// for one plain command on the read-only list (git only with a reading
// subcommand and no --output, find without -exec / -delete / -fprint, date
// without -s), nothing joined, redirected, expanded or globbed, and every
// path it names (and cwd) readable. Anything else is a question, never a
// refusal. readable may be nil (nothing outside is readable).
func SafeCommand(command, cwd string, readable func(path string) bool) bool {
	if readable == nil {
		readable = func(string) bool { return false }
	}
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, shellSigns) {
		return false
	}
	words, ok := shlexSplit(command)
	if !ok || len(words) == 0 || !safeCommands[words[0]] {
		return false
	}
	switch words[0] {
	case "git":
		var rest []string
		for _, w := range words[1:] {
			if !strings.HasPrefix(w, "-") {
				rest = append(rest, w)
			}
		}
		if len(rest) == 0 || !safeGit[rest[0]] {
			return false
		}
		for _, w := range words {
			for _, bad := range []string{"--output", "-o", "--ext-diff", "--textconv", "-c", "--exec"} {
				if strings.HasPrefix(w, bad) {
					return false
				}
			}
		}
	case "find":
		for _, w := range words {
			if unsafeFind[w] {
				return false
			}
		}
	case "date":
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "-s") || strings.HasPrefix(w, "--set") {
				return false
			}
		}
	}
	if !readable(cwd) {
		return false
	}
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "-") && !strings.Contains(w, "=") {
			continue
		}
		if strings.HasPrefix(w, "-") {
			_, w, _ = strings.Cut(w, "=")
		}
		_, err := os.Lstat(filepath.Join(cwd, w))
		if strings.Contains(w, "/") || strings.HasPrefix(w, "~") || w == "." || w == ".." || err == nil {
			if !readable(resolve(w, cwd)) {
				return false
			}
		}
	}
	return true
}

// shlexSplit splits like Python's shlex.split for the plain commands
// SafeCommand accepts: whitespace, single and double quotes, backslashes.
func shlexSplit(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	in, quote, esc := false, rune(0), false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc, in = false, true
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || esc {
		return nil, false
	}
	if in {
		words = append(words, cur.String())
	}
	return words, true
}

// PathGuard answers "may a tool touch this path at all?" the way a chat
// run's toolbox does (deny_paths, Cerberus' config folder via
// DefaultDenyPaths, and the protected data folders), for backends that run
// their own tools (the claude CLI in chats).
type PathGuard struct{ t *toolbox }

// NewPathGuard builds the guard for a run in cwd. protectData hides the
// data folder's records (sessions, worktrees, chats); protect adds more
// folders. As in Run, cwd itself stays usable only when it lies inside a
// protected folder.
func NewPathGuard(cwd string, deny, protect []string, protectData bool) *PathGuard {
	var a privateArea
	if protectData {
		a = dataArea(cwd)
	}
	for _, p := range protect {
		if filepath.IsAbs(p) {
			a.roots = append(a.roots, filepath.Clean(p))
		}
	}
	a.own = ownInside(cwd, a.roots)
	t := newToolbox(cwd, deny, 0)
	t.private = a.resolved()
	return &PathGuard{t: t}
}

// Refused says why p (relative to the guard's cwd, ~ expanded) may not be
// touched ("" = allowed).
func (g *PathGuard) Refused(p string) string {
	if g == nil || strings.TrimSpace(p) == "" {
		return ""
	}
	return g.t.refused(resolve(p, g.t.cwd))
}

// Folders are the protected folders that apply in cwd (absolute), for a
// backend's own deny list. A folder the run works inside is left out (a
// deny list has no "except cwd"); Refused still guards the rest of it.
func (g *PathGuard) Folders() []string {
	if g == nil {
		return nil
	}
	var out []string
	for _, r := range g.t.private.roots {
		inside := false
		for _, o := range g.t.private.own {
			inside = inside || under(o, r)
		}
		if !inside && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}
