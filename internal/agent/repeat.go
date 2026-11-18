package agent

// The stuck-loop guard (SCOPE "Flow quality (2026-11-16)"). A small model
// can fall into a loop: it rewrites the same file with the same few
// versions, or reads the same thing again and again, until the run's
// token budget is gone. The guard notices a call that repeats an earlier
// one with nothing changed in between, tells the model once, and ends the
// run when it keeps going.

import (
	"encoding/json"
	"errors"
	"strings"
)

// Guard bounds.
const (
	repeatWindow = 8  // the calls a repeat is looked for in
	repeatStop   = 4  // repeats that end the run (the first one gets the note)
	repeatClear  = 10 // calls in a row without a repeat that clear the count
)

// ErrRepeating ends a run whose model kept making the same call.
var ErrRepeating = errors.New("stopped: the model kept repeating the same action")

// repeatNote is added to the result of the first repeated call, so the
// model reads it in its next request.
const repeatNote = "(Cerberus: you already made this exact call and nothing has changed since. " +
	"You are repeating yourself. Do something different, or stop and give your final answer now.)"

// repeatGuard watches one run's tool calls.
type repeatGuard struct {
	recent  []seenCall // the last repeatWindow calls, oldest first
	epoch   int        // bumped by every call that may have changed the workspace
	strikes int        // repeats since the last clear
	clean   int        // calls in a row without a repeat
}

// seenCall is one watched call: its signature and the epoch after it ran.
type seenCall struct {
	sig   string
	epoch int
}

// callSig is a call's signature: the tool and its arguments as compact
// JSON (keys sorted), the path resolved against cwd and run_command's
// description left out (models reword it). ok is false for arguments
// that are not an object.
func callSig(name, raw, cwd string) (string, args, bool) {
	var a args
	if err := json.Unmarshal([]byte(raw), &a); err != nil || a == nil {
		return "", nil, false
	}
	norm := make(map[string]any, len(a))
	for k, v := range a {
		norm[k] = v
	}
	if p := a.str("path"); p != "" {
		norm["path"] = resolve(p, cwd)
	}
	if name == "run_command" {
		delete(norm, "description")
		norm["command"] = strings.TrimSpace(a.str("command"))
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return "", nil, false
	}
	return name + " " + string(b), a, true
}

// observe records one call that ran (out is what the model is told, bad
// whether it failed) and answers the output to send (with the note on the
// first repeat) and whether the run must stop. changes says whether a
// successful call of this tool may have changed the workspace.
//
// elided says old tool results are being shortened in the requests (fit.go):
// the stub asks the model to run a call again when it needs the output, so
// a repeated read or command is not counted then (writes still are).
func (g *repeatGuard) observe(sig, name, out string, bad, changes, elided bool) (string, bool) {
	repeat := false
	if editTools[name] {
		// Writing what was written a few calls ago (A, B, A) or what the
		// file already holds is going round in circles.
		repeat = strings.HasPrefix(out, unchangedPrefix)
		for _, c := range g.recent {
			repeat = repeat || c.sig == sig
		}
	} else if !elided {
		// The same read or command with nothing changed since it last ran.
		for _, c := range g.recent {
			repeat = repeat || (c.sig == sig && c.epoch == g.epoch)
		}
	}
	if changes && !bad && !strings.HasPrefix(out, unchangedPrefix) {
		g.epoch++
	}
	g.recent = append(g.recent, seenCall{sig: sig, epoch: g.epoch})
	if len(g.recent) > repeatWindow {
		g.recent = g.recent[len(g.recent)-repeatWindow:]
	}
	if !repeat {
		g.clean++
		if g.clean >= repeatClear {
			g.strikes, g.clean = 0, 0
		}
		return out, false
	}
	g.clean = 0
	g.strikes++
	if g.strikes == 1 {
		return strings.TrimRight(out, "\n") + "\n\n" + repeatNote, false
	}
	return out, g.strikes >= repeatStop
}

// mayChange says whether a successful call may have changed the
// workspace: an edit, or a command that is not read-only.
func (e *engine) mayChange(name string, a args) bool {
	switch {
	case editTools[name]:
		return true
	case name == "run_command":
		return !SafeCommand(a.str("command"), e.o.Cwd, e.readable)
	}
	return false
}

// watch runs a finished call through the guard (cerberus_* tools are
// left out: waiting on a child repeats by design). It answers the output
// to give the model and whether the run must stop.
func (e *engine) watch(c toolCall, out string, bad bool) (string, bool) {
	name := c.Function.Name
	if isSpawnTool(name) {
		return out, false
	}
	sig, a, ok := callSig(name, c.Function.Arguments, e.o.Cwd)
	if !ok {
		return out, false
	}
	before := e.repeats.strikes
	out, stop := e.repeats.observe(sig, name, out, bad, e.mayChange(name, a), e.elided > 0 || e.squeezed)
	switch {
	case stop:
		e.emit(Event{Type: EvNote, Text: "the model kept repeating the same action (" + name + "): the run is stopped"})
	case e.repeats.strikes == 1 && before == 0:
		e.emit(Event{Type: EvNote, Text: "the model repeated the same action (" + name + "): it is told to change course or finish"})
	}
	return out, stop
}
