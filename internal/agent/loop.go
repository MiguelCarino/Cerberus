package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	"carino.systems/cerberus/internal/permits"
)

const lastStepNudge = "(The step limit is reached: answer now with what you have, without tools.)"

// runner is the terminal client of Run: it turns events into the
// human-readable transcript, asks approvals on the PTY and keeps
// conversation.json and the inbox ledger.
type runner struct {
	o       options
	term    *term
	answers *lineReader
	docs    []contextDoc
	always  map[string]bool // tools approved with "a"
	permits *permits.AgentReader

	messages    []message
	saveErr     bool              // a failed save was reported (once is enough)
	inboxLedger map[string]string // inbox-seen.json to write after the first successful request
}

func newRunner(o options, t *term, stdin io.Reader) *runner {
	return &runner{
		o:        o,
		term:     t,
		answers:  newLineReader(stdin),
		docs:     loadContext(o.Cwd, o.ContextFiles, o.ContextMax),
		messages: initialMessages(o),
		always:   map[string]bool{},
		permits:  &permits.AgentReader{Path: o.Permits},
	}
}

func initialMessages(o options) []message {
	if o.Resumed != nil {
		return resumeMessages(o.Resumed, o.Task)
	}
	return []message{{Role: "user", Content: o.Task}}
}

// mode is the engine mode for --approve.
func (r *runner) mode() Mode {
	if r.o.Approve == "auto" {
		return ModeBypass
	}
	return ModeAsk
}

// runOptions builds the engine's options for this task.
func (r *runner) runOptions() RunOptions {
	return RunOptions{
		Base: r.o.Base, Model: r.o.Model, Key: r.o.Key, Session: r.o.Session,
		Cwd: r.o.Cwd, Origin: r.o.Origin, Messages: r.messages, Mode: r.mode(), MaxSteps: r.o.MaxSteps,
		DenyPaths: r.o.DenyPaths, CommandTimeout: r.o.CommandTimeout,
		System: func(tools bool) string {
			s := systemPrompt(r.o.Cwd, r.o.Approve, tools, r.docs)
			if tools && r.o.Spawn != nil {
				s += spawnNote
			}
			return s
		},
		Spawn: r.o.Spawn, Policy: r.o.Policy, Asker: r, OnSave: r.save,
		OnReply: func() {
			if r.inboxLedger != nil {
				commitInbox(r.o.InboxSeen, r.inboxLedger)
				r.inboxLedger = nil
			}
		},
		private: r.o.Private, reapCommands: true,
	}
}

// save writes the conversation to o.SaveTo, if set. A failure is shown once
// and never stops the task: the work matters more than its record.
func (r *runner) save(system string, msgs []message) {
	if r.o.SaveTo == "" {
		return
	}
	all := make([]message, 0, len(msgs)+1)
	all = append(all, message{Role: "system", Content: system})
	all = append(all, msgs...)
	err := saveConversation(r.o.SaveTo, conversation{Model: r.o.Model, Cwd: r.o.Cwd, Messages: all})
	if err != nil && !r.saveErr {
		r.saveErr = true
		r.term.dimLine("· the conversation could not be saved: " + err.Error())
	}
}

// handle prints one engine event.
func (r *runner) handle(ev Event) {
	t := r.term
	switch ev.Type {
	case EvText:
		t.text(ev.Text)
	case EvReasoning:
		t.thinkBegin()
	case EvTool:
		if ev.Args == nil {
			t.card(ev.Name, "(bad arguments)")
		} else {
			t.card(ev.Name, cardSummary(ev.Name, args(ev.Args)))
		}
	case EvToolDone:
		t.result(ev.Output, ev.IsError)
	case EvNoTools:
		t.dimLine("· the model takes no tools (" + truncRunes(ev.Why, 160) + "): plain chat")
	case EvNote:
		t.dimLine("· " + ev.Text)
	case EvStart, EvDone:
		t.endLine()
	}
}

// Covered answers a card for a tool approved with "a", then one a saved
// rule of the session's owner covers, never one the policy forces.
func (r *runner) Covered(req AskRequest) (Decision, bool) {
	if r.always[req.Display] {
		return Decision{Allow: true}, true
	}
	if req.Forced {
		return Decision{}, false
	}
	if rule, ok := r.permits.Match(req.Tool, req.Input, r.o.Cwd); ok {
		if req.Tool != "Read" { // reads are checked often (paths in commands): no line each
			r.term.dimLine("· allowed by your saved permission " + rule.Text)
		}
		r.term.osc("permit", rule.ID)
		return Decision{Allow: true}, true
	}
	return Decision{}, false
}

// Ask shows the request, marks the session waiting and reads the answer:
// y = run it, a = run it and stop asking for this tool, n = refuse, anything
// else = refuse with that text as the reason.
func (r *runner) Ask(ctx context.Context, req AskRequest) Decision {
	if req.Reason != "" {
		r.term.dimLine("  " + req.Reason)
	}
	name := req.Display
	hint := ""
	if r.saveOptions(req) != "" {
		hint = "  To allow this for good, use Always ▾ → Every time in the Sessions list or the bell."
	}
	r.term.approval(name, args(req.Args), hint)
	// "ask" names what the approval is for, so the session list and the
	// bell can show and answer it without the terminal.
	r.term.osc("ask", truncRunes(name+": "+cardSummary(name, args(req.Args)), 300))
	// "askrules" lists what "Every time" may save from the session list or
	// the bell (base64url JSON: the marker text may hold no ';'). The
	// daemon takes it only right after an "ask" and validates a save again.
	if opts := r.saveOptions(req); opts != "" {
		r.term.osc("askrules", opts)
	}
	r.term.osc("waiting", "approval")
	defer r.term.osc("resumed", "")
	for {
		line, err := r.answers.next(ctx)
		if err != nil {
			r.term.endLine()
			if ctx.Err() != nil {
				return Decision{Note: "stopped"}
			}
			return Decision{Note: "no answer could be read (input closed)"}
		}
		r.term.atLineStart = true // the terminal echoed the user's Enter
		answer := strings.TrimSpace(line)
		switch strings.ToLower(answer) {
		case "":
			continue // a stray Enter is not an answer
		case "y", "yes":
			return Decision{Allow: true}
		case "a", "always":
			r.always[name] = true
			return Decision{Allow: true, Session: true}
		case "n", "no":
			return Decision{}
		}
		return Decision{Note: answer}
	}
}

// saveOptions is the "askrules" marker text for req ("" when none).
func (r *runner) saveOptions(req AskRequest) string {
	if req.Forced || r.o.Permits == "" {
		return ""
	}
	opts := permits.Suggest(req.Tool, req.Input, r.o.Cwd)
	if len(opts) == 0 {
		return ""
	}
	raw, err := json.Marshal(opts)
	if err != nil || len(raw) > 6000 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// addInbox tells the model about inbox files that are new since the last
// run: a note on this run's user message (or a new one) and, unless the
// gateway says the model takes no images, the new images as image parts.
func (r *runner) addInbox(ctx context.Context) {
	files, ledger := peekInbox(r.o.Cwd, r.o.Inbox, r.o.InboxSeen, r.o.DenyPaths)
	if len(files) == 0 {
		return
	}
	var images []string
	if slices.ContainsFunc(files, func(f inboxFile) bool { return imageTypes[strings.ToLower(path.Ext(f.Rel))] != "" }) &&
		newClient(r.o.Base, r.o.Key, r.o.Session).modelVision(ctx, r.o.Model) != visionNo {
		images = loadImages(r.o.Cwd, r.o.Inbox, files)
	}
	note := inboxNote(r.o.Inbox, files, len(images))
	r.term.dimLine("· " + note)
	// The ledger is written once a request carrying the note succeeded: a
	// run that fails first announces the files again next time.
	r.inboxLedger = ledger
	// A fresh run, or a resume with new text (or the continue nudge), ends
	// in this run's own user message: the note joins it. Otherwise the
	// note is a user message of its own.
	if n := len(r.messages); n > len(r.o.Resumed) && r.messages[n-1].Role == "user" {
		last := &r.messages[n-1]
		last.Content = strings.TrimSpace(last.Content + "\n\n" + note)
		last.Images = append(last.Images, images...)
		return
	}
	r.messages = append(r.messages, message{Role: "user", Content: note, Images: images})
}

// spawnNote tells the model about the cerberus_* tools.
const spawnNote = "\n\n# Child sessions\n\nWith cerberus_spawn you can start other agents as child sessions that work " +
	"in parallel (cerberus_list, cerberus_read, cerberus_send, cerberus_wait, cerberus_stop steer them). Use it only " +
	"for independent pieces of work worth an agent of their own; give each child a complete task, wait for it and " +
	"check its result. Limits on how many and how deep apply."

// pendingCall is a tool call being assembled from deltas.
type pendingCall struct {
	id, name string
	args     strings.Builder
}

// assemble orders calls by index, drops nameless ones and fills missing ids.
func assemble(pending map[int]*pendingCall) []toolCall {
	keys := make([]int, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var calls []toolCall
	for n, k := range keys {
		p := pending[k]
		if p.name == "" {
			continue
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", randomHex(6), n)
		}
		a := p.args.String()
		if strings.TrimSpace(a) == "" {
			a = "{}"
		}
		calls = append(calls, toolCall{ID: id, Type: "function", Function: functionCall{Name: p.name, Arguments: a}})
	}
	return calls
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func refusal(note string) string {
	if note = strings.TrimSpace(note); note != "" {
		return "The user refused this: " + note
	}
	return "The user refused this."
}

// cardSummary is the short text after "▸ tool:".
func cardSummary(name string, a args) string {
	one := func(s string) string { return truncRunes(sanitize(strings.SplitN(s, "\n", 2)[0]), 120) }
	switch name {
	case "run_command":
		return one(a.str("command"))
	case "read_file":
		s := a.str("path")
		if off, ok := a.int("offset"); ok {
			s += fmt.Sprintf(" from line %d", off)
		}
		return one(s)
	case "list_dir":
		return one(orStr(a.str("path"), "."))
	case "search":
		var parts []string
		if p := a.str("pattern"); p != "" {
			parts = append(parts, "/"+p+"/")
		}
		if g := a.str("glob"); g != "" {
			parts = append(parts, g)
		}
		return one(strings.Join(parts, " ") + " in " + orStr(a.str("path"), "."))
	case "write_file":
		return one(fmt.Sprintf("%s (%d lines)", a.str("path"), len(splitLines(a.str("content")))))
	case "edit_file":
		return one(a.str("path"))
	case ToolSpawn:
		return one(a.str("task"))
	case ToolRead, ToolSend, ToolWait, ToolStop:
		return one(a.str("id"))
	}
	return ""
}

// summary is the final line: steps, tool calls, time and tokens.
func summary(d Done) string {
	s := fmt.Sprintf("done · %d step%s · %d tool call%s · %.1f s", d.Steps, plural(d.Steps), d.Calls,
		plural(d.Calls), d.Seconds)
	if d.Total.Prompt+d.Total.Completion > 0 {
		s += fmt.Sprintf(" · %d prompt + %d completion tokens", d.Total.Prompt, d.Total.Completion)
	}
	if !d.Tools {
		s += " · no tools"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
