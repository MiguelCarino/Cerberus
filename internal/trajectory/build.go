package trajectory

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/records"
	"carino.systems/cerberus/internal/session"
)

// Input is what a trajectory is built from.
type Input struct {
	Session session.Info
	// Setup is the session's E3 snapshot (nil: none recorded).
	Setup *session.SetupView
	// Lines are the session's request records, any order.
	Lines []records.Line
	// TextLevel is the records text level of now (none, user, all).
	TextLevel string
	// More says lines were left out at the end (the line cap).
	More bool
	// Partial, when set, says why the start of the session is missing
	// (its first days were swept).
	Partial string
	// Cerberus is the daemon's version.
	Cerberus string
	Now      time.Time
}

// ErrNoLines is a session with no request records.
var ErrNoLines = errors.New("no request records for this session")

// seg is one "role: text" entry of a record's conversation text.
type seg struct{ role, text string }

var reRole = regexp.MustCompile(`^(user|assistant|tool|function):( |$)`)

const clippedMark = "…[clipped]"

// segments splits a record's Messages text ("role: text" lines; a text may
// span lines) into entries. A line that starts like a role always starts a
// new entry, so a message whose own text has such a line is split there:
// the text is only ever approximate.
func segments(s string) []seg {
	if s == "" {
		return nil
	}
	var out []seg
	for _, ln := range strings.Split(s, "\n") {
		if m := reRole.FindStringSubmatch(ln); m != nil {
			out = append(out, seg{role: m[1], text: ln[len(m[0]):]})
			continue
		}
		if len(out) == 0 {
			out = append(out, seg{role: "user"})
			out[0].text = ln
			continue
		}
		out[len(out)-1].text += "\n" + ln
	}
	return out
}

func sameSeg(a, b seg) bool {
	return a.role == b.role && strings.TrimSpace(a.text) == strings.TrimSpace(b.text)
}

// thread is one conversation the session's agent kept with the gateway (a
// main loop, a side call such as a title, a subagent...). A record
// continues the thread whose last conversation it extends.
type thread struct {
	id      int
	system  string
	sent    []seg // the conversation of the thread's last record (without its reply)
	reply   string
	msgs    int // the message count of the last record
	results int // its tool result count
	errs    int // its tool result error count
	agent   int // index in steps of the last agent step
	last    int // order of the last record (ties go to the newest thread)
}

// Build makes the ATIF trajectory of in.Session from its request records.
// It answers ErrNoLines when there are none (ATIF needs one step).
func Build(in Input) (Trajectory, error) {
	if len(in.Lines) == 0 {
		return Trajectory{}, ErrNoLines
	}
	lines := append([]records.Line(nil), in.Lines...)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.Before(lines[j].Time) })
	info := in.Session
	b := &builder{}

	anyText := false
	for _, l := range lines {
		if l.Text != nil {
			anyText = true
			break
		}
	}
	if !anyText && strings.TrimSpace(info.Task) != "" {
		// No conversation text was kept: the session's task stands in for
		// the first user message.
		b.add(Step{Source: SourceUser, Timestamp: stamp(lines[0].Time), Message: info.Task,
			Extra: map[string]any{"cerberus": map[string]any{"from": "session task"}}})
	}

	var threads []*thread
	var tot struct{ prompt, completion, cached, reasoning, flagged, held int }
	var durMs int64
	finishes := map[string]int{}
	models := map[string]int{}
	for i, l := range lines {
		tot.prompt += l.PromptTokens
		tot.completion += l.CompletionTokens
		tot.cached += l.CachedTokens
		tot.reasoning += l.ReasoningTokens
		durMs += l.DurationMs
		if l.Finish != "" {
			finishes[l.Finish]++
		}
		if l.Model != "" {
			models[l.Model]++
		}
		if l.Guard != nil {
			tot.flagged += l.Guard.Flagged
			tot.held += len(l.Guard.Held)
		}

		var text records.Text
		if l.Text != nil {
			text = *l.Text
		}
		hasText := l.Text != nil && (text.Messages != "" || text.System != "")
		clipped := strings.HasSuffix(text.Messages, clippedMark)
		segs := segments(text.Messages)
		if clipped && len(segs) > 0 {
			segs = segs[:len(segs)-1] // the cut entry is not trusted
		}

		// Find the thread this record continues.
		var t *thread
		var rest []seg
		if hasText {
			best := -1
			for _, c := range threads {
				if c.system != text.System || len(c.sent) > len(segs) || len(c.sent) < best {
					continue
				}
				ok := true
				for k := range c.sent {
					if !sameSeg(c.sent[k], segs[k]) {
						ok = false
						break
					}
				}
				if ok && (len(c.sent) > best || t == nil || c.last > t.last) {
					t, best = c, len(c.sent)
				}
			}
			if t != nil {
				rest = segs[len(t.sent):]
				// The thread's reply, as the client sent it back.
				if len(rest) > 0 && rest[0].role == "assistant" {
					rest = rest[1:]
				}
			}
		} else {
			for _, c := range threads {
				if c.msgs < l.Messages && (t == nil || c.msgs > t.msgs || c.msgs == t.msgs && c.last > t.last) {
					t = c
				}
			}
		}
		newThread := t == nil
		if newThread {
			t = &thread{id: len(threads) + 1, system: text.System, agent: -1}
			threads = append(threads, t)
			rest = segs
		}
		ts := stamp(l.Time)

		if newThread && text.System != "" {
			b.add(Step{Source: SourceSystem, Timestamp: ts, Message: text.System,
				Extra: map[string]any{"cerberus": map[string]any{"thread": t.id}}})
		}
		// Tool results that open the new part answer the thread's last calls.
		var toolTexts []string
		for len(rest) > 0 && rest[0].role == "tool" && !newThread {
			toolTexts = append(toolTexts, strings.TrimSpace(rest[0].text))
			rest = rest[1:]
		}
		newResults := l.Results
		newErrs := l.ResultErrors
		if !newThread && l.Results >= t.results {
			newResults = l.Results - t.results
			newErrs = l.ResultErrors - t.errs
		}
		if !newThread {
			b.observe(t, ts, toolTexts, newResults, newErrs, l.API == "messages")
		}
		if len(rest) > 0 {
			ex := map[string]any{"thread": t.id}
			if newThread && i > 0 {
				ex["conversation"] = "new"
			}
			b.add(Step{Source: SourceUser, Timestamp: ts, Message: render(rest), Extra: map[string]any{"cerberus": ex}})
		}

		// The model call itself.
		st := Step{Source: SourceAgent, Timestamp: ts, ModelName: l.Model, ReasoningEffort: l.Effort,
			Message: text.Reply, ReasoningContent: text.Reasoning, LLMCallCount: intp(1)}
		for k, c := range l.Calls {
			id := c.ID
			if id == "" {
				id = "call-" + strconv.Itoa(b.next()) + "-" + strconv.Itoa(k+1)
			}
			st.ToolCalls = append(st.ToolCalls, ToolCall{ToolCallID: id, FunctionName: orStr(c.Name, "unknown"),
				Arguments: map[string]any{}})
		}
		mx := map[string]any{"ttft_ms": l.TTFTMs, "duration_ms": l.DurationMs}
		if l.ReasoningTokens > 0 {
			mx["reasoning_tokens"] = l.ReasoningTokens
		}
		st.Metrics = &Metrics{PromptTokens: intp(l.PromptTokens), CompletionTokens: intp(l.CompletionTokens),
			CachedTokens: intp(l.CachedTokens), Extra: mx}
		cx := map[string]any{"thread": t.id, "api": l.API, "stream": l.Stream, "client": l.Client,
			"finish": l.Finish, "status": l.Status, "messages": l.Messages, "tools": l.Tools,
			"sampled": l.Sampled}
		for k, v := range map[string]string{"backend": l.Backend, "provider": l.Provider, "error": l.Error} {
			if v != "" {
				cx[k] = v
			}
		}
		for k, v := range map[string]int{"images": l.Images, "budget": l.Budget, "results": l.Results,
			"result_errors": l.ResultErrors, "matched": l.Matched} {
			if v != 0 {
				cx[k] = v
			}
		}
		if l.Thinking != nil {
			cx["thinking"] = *l.Thinking
		}
		if l.Schema {
			cx["schema"] = true
		}
		if clipped {
			cx["input_clipped"] = true
		}
		st.Extra = map[string]any{"cerberus": cx}
		if g := l.Guard; g != nil {
			ann := map[string]any{"kind": "agent_guard", "mode": g.Mode, "flagged": g.Flagged}
			if len(g.Scores) > 0 {
				ann["scores"] = g.Scores
			}
			if len(g.Held) > 0 {
				ann["held"] = g.Held
			}
			if g.TimedOut {
				ann["timed_out"] = true
			}
			st.Extra["annotations"] = []any{ann}
		}
		t.agent = b.add(st)

		t.sent, t.reply = segs, text.Reply
		t.msgs, t.results, t.errs, t.last = l.Messages, l.Results, l.ResultErrors, i
	}

	tr := Trajectory{SchemaVersion: SchemaVersion, SessionID: info.ID, Steps: b.steps}
	tr.Agent = agentOf(info, in.Setup, models)
	fx := map[string]any{"model_calls": len(lines), "duration_ms": durMs, "finishes": finishes}
	if tot.reasoning > 0 {
		fx["reasoning_tokens"] = tot.reasoning
	}
	if tot.flagged > 0 || tot.held > 0 {
		fx["guard_flagged"], fx["guard_held"] = tot.flagged, tot.held
	}
	tr.FinalMetrics = &FinalMetrics{TotalPromptTokens: intp(tot.prompt), TotalCompletionTokens: intp(tot.completion),
		TotalCachedTokens: intp(tot.cached), TotalSteps: intp(len(b.steps)), Extra: fx}
	tr.Notes = notes(in.TextLevel, anyText, in.Partial, in.More)

	sess := map[string]any{"id": info.ID, "agent": info.Agent, "model": info.Model, "workspace": info.Workspace,
		"state": info.State, "created": stamp(info.Created)}
	for k, v := range map[string]string{"title": info.Title, "task": info.Task, "repo": info.Repo,
		"base_commit": info.BaseCommit, "setup_hash": info.SetupHash} {
		if v != "" {
			sess[k] = v
		}
	}
	if !info.RunStarted.IsZero() {
		sess["run_started"] = stamp(info.RunStarted)
	}
	if !info.RunEnded.IsZero() {
		sess["run_ended"] = stamp(info.RunEnded)
	}
	rec := map[string]any{"text": orStr(in.TextLevel, records.TextNone), "lines": len(lines), "threads": len(threads)}
	if in.More {
		rec["more"] = true
	}
	if in.Partial != "" {
		rec["partial"] = in.Partial
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	tr.Extra = map[string]any{"cerberus": map[string]any{"version": in.Cerberus, "spec": SpecSource,
		"exported": stamp(now), "session": sess, "records": rec}}
	return tr, nil
}

// builder numbers steps as they are added.
type builder struct{ steps []Step }

func (b *builder) next() int { return len(b.steps) + 1 }

func (b *builder) add(s Step) int {
	s.StepID = len(b.steps) + 1
	b.steps = append(b.steps, s)
	return len(b.steps) - 1
}

// observe records the tool results a record sent back for t's last calls:
// on that agent step when it made calls, else on a system step. texts are
// the results' text (records text level all), n the count from the
// record's counters; errs is how many of them the client marked errors,
// known only for the Messages API (flagged).
func (b *builder) observe(t *thread, ts string, texts []string, n, errs int, flagged bool) {
	if len(texts) > 0 {
		n = len(texts)
	}
	if n <= 0 {
		return
	}
	var calls []ToolCall
	if t.agent >= 0 {
		calls = b.steps[t.agent].ToolCalls
	}
	res := make([]ObservationResult, n)
	for k := range res {
		if len(texts) == n {
			res[k].Content = texts[k]
		}
		if len(calls) == n {
			res[k].SourceCallID = calls[k].ToolCallID
		}
	}
	unattributed := 0
	if flagged && errs >= 0 {
		switch errs {
		case 0, n:
			for k := range res {
				res[k].Extra = map[string]any{"is_error": errs == n}
			}
		default:
			unattributed = errs
		}
	}
	if len(calls) == 0 || len(calls) != n {
		// Not a one-to-one answer to the last calls: a system step holds them.
		ex := map[string]any{"thread": t.id}
		if unattributed > 0 {
			ex["result_errors"] = unattributed
		}
		for k := range res {
			res[k].SourceCallID = ""
		}
		b.add(Step{Source: SourceSystem, Timestamp: ts, Message: "Tool results sent back",
			Observation: &Observation{Results: res}, Extra: map[string]any{"cerberus": ex}})
		return
	}
	st := &b.steps[t.agent]
	st.Observation = &Observation{Results: res}
	if unattributed > 0 {
		if cx, ok := st.Extra["cerberus"].(map[string]any); ok {
			cx["result_errors_new"] = unattributed
		}
	}
}

// render is the text of a user step: the user's words, or the entries with
// their roles when there are others.
func render(ss []seg) string {
	allUser := true
	for _, s := range ss {
		if s.role != "user" {
			allUser = false
			break
		}
	}
	parts := make([]string, len(ss))
	for i, s := range ss {
		if allUser {
			parts[i] = strings.TrimSpace(s.text)
		} else {
			parts[i] = s.role + ": " + strings.TrimSpace(s.text)
		}
	}
	if allUser {
		return strings.Join(parts, "\n\n")
	}
	return strings.Join(parts, "\n")
}

func agentOf(info session.Info, v *session.SetupView, models map[string]int) Agent {
	a := Agent{Name: orStr(info.Agent, "cerberus-session"), Version: "unknown"}
	if v != nil {
		if v.Setup.Agent.Name != "" {
			a.Name = v.Setup.Agent.Name
		}
		if v.Setup.Agent.Version != "" {
			a.Version = v.Setup.Agent.Version
		}
		a.ModelName = v.Setup.Model.ID
		a.Extra = map[string]any{"setup": v.Setup, "setup_hash": v.Hash, "setup_recorded_hash": v.Recorded,
			"setup_verified": v.Verified}
	}
	if a.ModelName == "" {
		best := 0
		for m, n := range models {
			if n > best || n == best && m < a.ModelName {
				a.ModelName, best = m, n
			}
		}
	}
	if a.ModelName == "" {
		a.ModelName = info.Model
	}
	return a
}

func notes(level string, anyText bool, partial string, more bool) string {
	var b strings.Builder
	b.WriteString("Exported by Cerberus from its gateway request records: one agent step per model call, in request order. ")
	switch {
	case !anyText:
		b.WriteString("No conversation text was recorded (gateway.records.text none), so messages are empty and the first user step is the session's task. ")
	case level == records.TextUser:
		b.WriteString("Text level user: user and assistant text only; system prompts, reasoning and tool result text are not recorded. ")
	default:
		b.WriteString("User steps and tool results are the new part of each request's conversation, so they are approximate when a conversation was edited or clipped. ")
	}
	b.WriteString("Tool call arguments are not recorded, so arguments is {}. Records are redacted with the vault's secrets when written. Guard verdicts are step annotations.")
	if partial != "" {
		b.WriteString(" " + partial)
	}
	if more {
		b.WriteString(" The record cap was reached: later model calls are left out.")
	}
	return b.String()
}

// stamp is an ISO 8601 UTC timestamp with milliseconds.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
