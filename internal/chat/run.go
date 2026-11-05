package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/permits"
)

// Limits on what a message may carry. Images are scaled in the browser
// (≤ 1568 px, JPEG over 3.5 MB) before they come here.
const (
	maxImages     = 8
	maxImageBytes = 12 << 20 // one data: URL
	maxTextBytes  = 256 << 10
)

// SendOptions is one message (POST /api/chats/{id}/messages).
type SendOptions struct {
	Text   string   `json:"text"`
	Images []string `json:"images"` // data:image/…;base64,… URLs
	Model  string   `json:"model"`  // optional: applies from this message on
	Mode   string   `json:"mode"`   // optional: applies from this message on
}

// SendResult says where the message went: ID is the chat that runs it
// (a new one when the model switched backend), Queued when it waits.
type SendResult struct {
	ID       string `json:"id"`
	Queued   bool   `json:"queued"`
	Position int    `json:"position,omitempty"` // 1 = next
	Switched bool   `json:"switched,omitempty"`
}

// Send runs a message now or queues it while the chat is busy.
func (m *Manager) Send(id string, o SendOptions) (SendResult, error) {
	cfg := m.cfg()
	if !cfg.Chat.Enabled {
		return SendResult{}, ErrDisabled
	}
	q, err := m.checkMessage(o)
	if err != nil {
		return SendResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return SendResult{}, ErrClosing
	}
	l, err := m.lookup(id)
	if err != nil {
		return SendResult{}, err
	}
	res := SendResult{ID: id}
	backend := orStr(l.chat.Backend, BackendNative)
	if q.Model != "" && BackendOf(q.Model) != backend && (len(l.chat.Messages) > 0 || l.busy) {
		// A claude session cannot go on in the native engine, nor the
		// other way round: a new chat in the same folder.
		nl, err := m.forkLocked(l, q)
		if err != nil {
			return SendResult{}, err
		}
		m.emitLocked(l, "switched", map[string]any{"to": nl.chat.ID, "model": q.Model})
		l, res = nl, SendResult{ID: nl.chat.ID, Switched: true}
	}
	if l.busy {
		// Also while a stopped run is still ending: Stop emptied the
		// queue, so whatever is queued now came after it and runs next.
		l.queue = append(l.queue, q)
		res.Queued, res.Position = true, len(l.queue)
		m.emitLocked(l, "queued", map[string]any{"text": q.Text, "images": len(q.Images), "position": len(l.queue)})
		return res, nil
	}
	if err := m.startLocked(l, q); err != nil {
		return SendResult{}, err
	}
	return res, nil
}

// checkMessage validates a message and its options.
func (m *Manager) checkMessage(o SendOptions) (queued, error) {
	q := queued{Text: strings.TrimSpace(o.Text), Model: strings.TrimSpace(o.Model)}
	if q.Text == "" && len(o.Images) == 0 {
		return q, ErrEmpty
	}
	if len(q.Text) > maxTextBytes {
		return q, fmt.Errorf("the message is too long (%d KB, at most %d KB)", len(q.Text)>>10, maxTextBytes>>10)
	}
	if len(o.Images) > maxImages {
		return q, fmt.Errorf("at most %d images per message", maxImages)
	}
	for _, img := range o.Images {
		if !strings.HasPrefix(img, "data:image/") || !strings.Contains(img[:min(len(img), 64)], ";base64,") {
			return q, fmt.Errorf("an image must be a data:image/…;base64 URL")
		}
		if len(img) > maxImageBytes {
			return q, fmt.Errorf("an image is too big (%d MB): scale it down first", len(img)>>20)
		}
		q.Images = append(q.Images, img)
	}
	if o.Mode != "" {
		mode, err := parseMode(o.Mode)
		if err != nil {
			return q, err
		}
		q.Mode = mode
	}
	if q.Model != "" && BackendOf(q.Model) == BackendClaude && !m.ClaudeAvailable() {
		return q, ErrNoClaude
	}
	return q, nil
}

// forkLocked starts a new chat in l's folder for a model on the other backend.
func (m *Manager) forkLocked(l *live, q queued) (*live, error) {
	now := m.now()
	c := &Chat{ID: newID(now), Cwd: l.chat.Cwd, Model: q.Model, Created: unix(now), Updated: unix(now),
		Messages: []agent.Message{}, Backend: BackendOf(q.Model), Mode: orStr(q.Mode, l.chat.Mode), Owner: l.chat.Owner}
	if err := m.store.save(c); err != nil {
		return nil, err
	}
	nl := newLive(c)
	m.addLocked(nl)
	return nl, nil
}

// startLocked begins a run for q. Call with m.mu held and l idle.
func (m *Manager) startLocked(l *live, q queued) error {
	c := l.chat
	if q.Model != "" {
		c.Model, c.Backend = q.Model, BackendOf(q.Model)
	}
	if q.Mode != "" {
		c.Mode = q.Mode
	}
	if c.Mode == "" {
		c.Mode = orStr(m.cfg().Chat.DefaultMode, "ask")
	}
	backend := orStr(c.Backend, BackendNative)
	claude := ""
	if backend == BackendClaude && !m.access(c.Owner).Admin {
		return ErrBackend
	}
	if backend == BackendClaude {
		if claude = m.claudePath(); claude == "" {
			return ErrNoClaude
		}
	}
	now := m.now()
	c.Messages = append(c.Messages, agent.Message{Role: "user", Content: q.Text, Images: q.Images})
	if c.Title == "" {
		c.Title = titleOf(c.Messages)
	}
	c.Updated = unix(now)
	m.saveLocked(l)
	ctx, cancel := context.WithCancel(context.Background())
	l.busy, l.cancel, l.started, l.state, l.stopped = true, cancel, now, "thinking", false
	l.runStart = l.next
	m.emitLocked(l, "user", map[string]any{"text": q.Text, "images": len(q.Images), "model": c.Model, "mode": c.Mode})
	m.emitLocked(l, "state", map[string]any{"busy": true, "queue": len(l.queue)})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		if backend == BackendClaude {
			m.runClaude(ctx, l, claude, q)
		} else {
			m.runNative(ctx, l)
		}
	}()
	return nil
}

func (m *Manager) saveLocked(l *live) {
	if err := m.store.save(l.chat); err != nil {
		m.o.Log.Warn("chat: save failed", "chat", l.chat.ID, "err", err)
	}
}

// gateway is the base URL and key for native runs.
func (m *Manager) gateway(cfg *config.Config) (string, string) {
	if m.o.Gateway != nil {
		return m.o.Gateway()
	}
	listen := orStr(cfg.Server.Listen, "127.0.0.1:7340")
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	scheme := "http"
	return scheme + "://" + listen + "/v1", cfg.TokenSnapshot()
}

// runNative runs the newest user message through agent.Run.
func (m *Manager) runNative(ctx context.Context, l *live) {
	cfg := m.cfg()
	m.mu.Lock()
	c := l.chat
	msgs := append([]agent.Message(nil), c.Messages...)
	model, cwd, owner := c.Model, c.Cwd, c.Owner
	mode, _ := agent.ParseMode(c.Mode)
	m.mu.Unlock()
	acc := m.access(owner)
	if acc.Cwd != "" {
		_ = os.MkdirAll(acc.Cwd, 0o700)
		cwd = acc.Cwd
	}
	if (model == "" || model == "default") && m.o.ResolveModel != nil {
		// Pin the alias to the model that will answer: the chat keeps it
		// even after that model is unloaded and "default" turns ambiguous.
		if id, err := m.o.ResolveModel(ctx, "default"); err == nil && id != "" {
			model = id
			m.mu.Lock()
			if l.chat.Model == "" || l.chat.Model == "default" {
				l.chat.Model = id
				m.saveLocked(l)
			}
			m.mu.Unlock()
		}
	}
	start := len(msgs)
	base, key := m.gateway(cfg)
	timeout := cfg.Chat.CommandTimeoutS
	if timeout <= 0 {
		timeout = 120
	}
	o := agent.RunOptions{
		// Session labels the chat's gateway requests with its chat id
		// (X-Cerberus-Session), so usage and captured examples are
		// attributed to the chat (the Flow graph's answer edge). The
		// gateway uses it for attribution only; chat ids never name a
		// session for limits, energy or the leaderboard.
		Base: base, Key: key, Model: model, Session: l.chat.ID, Client: "cerberus-chat", Cwd: cwd, Messages: msgs, Mode: mode,
		MaxSteps: orInt(cfg.Chat.MaxSteps, 12), DenyPaths: agent.DefaultDenyPaths(cfg), ProtectData: true,
		Protect: []string{m.store.dir}, CommandTimeout: time.Duration(min(timeout, 600)) * time.Second, ResultClip: orInt(cfg.Chat.ResultClip, 12000),
		AskOutside: true, System: func(tools bool) string { return systemPrompt(cwd, mode, tools, m.now()) },
		Owner: owner, NoTools: acc.NoTools, Jail: acc.Jail, Wrap: acc.Wrap,
		Refusal: denyMessage, Policy: agent.NewPolicy(cfg.Agent.Policy), Asker: &asker{m: m, l: l},
		OnSave: func(_ string, all []agent.Message) {
			m.mu.Lock()
			defer m.mu.Unlock()
			l.chat.Messages = all
			l.chat.Updated = unix(m.now())
			m.saveLocked(l)
		},
	}
	res := agent.Run(ctx, o, func(ev agent.Event) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if ev.Type == agent.EvDone {
			m.finishNativeLocked(l, ev, cfg, model, start)
			return
		}
		m.emitAgentLocked(l, ev)
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	msgsOut := res.Messages
	if (res.Err != nil || res.Done.Stopped) && len(msgsOut) == start {
		// Nothing came back for it: the message is not kept (an image a
		// model refuses would be refused again with every next one).
		msgsOut = msgsOut[:start-1]
	}
	l.chat.Messages = msgsOut
	m.endRunLocked(l, res.Done)
}

// finishNativeLocked records usage and cost before the done event goes out.
func (m *Manager) finishNativeLocked(l *live, ev agent.Event, cfg *config.Config, model string, start int) {
	d := ev.Done
	if d != nil {
		if used := d.Usage.Prompt + d.Usage.Completion; used > 0 {
			l.chat.ContextUsed = used
		}
		if p, ok := priceOf(cfg, model); ok {
			d.CostUSD = (float64(d.Total.Prompt)*p.In + float64(d.Total.Completion)*p.Out) / 1e6
			l.chat.CostUSD += d.CostUSD
		}
	}
	m.emitAgentLocked(l, ev)
}

// priceOf finds a model's price: its own entry, else "<provider>/*".
func priceOf(cfg *config.Config, model string) (config.Price, bool) {
	if p, ok := cfg.Prices[model]; ok {
		return p, true
	}
	if prov, _, ok := strings.Cut(model, "/"); ok {
		p, ok := cfg.Prices[prov+"/*"]
		return p, ok
	}
	return config.Price{}, false
}

// endRunLocked closes a run: state, save, status, notification, and the
// next queued message.
func (m *Manager) endRunLocked(l *live, d agent.Done) {
	c := l.chat
	l.busy, l.cancel = false, nil
	l.elapsed = m.now().Sub(l.started).Seconds()
	m.denyAsksLocked(l, "end") // a backend that ended with cards open
	l.chans = nil              // every waiter of this run has returned
	switch {
	case d.Stopped || l.stopped:
		l.state = "stopped"
	case d.OK:
		l.state = "done"
	default:
		l.state = "error"
	}
	c.Updated = unix(m.now())
	m.saveLocked(l)
	m.emitLocked(l, "state", map[string]any{"busy": false, "state": l.state, "queue": len(l.queue)})
	m.emitLocked(l, "status", m.statusLocked(l))
	switch l.state {
	case "done":
		m.notifyLocked(l, "LLM chat finished", orStr(lastReply(c.Messages), c.Title), "normal")
	case "error":
		m.notifyLocked(l, "LLM chat failed", orStr(d.Error, c.Title), "normal")
	}
	// The queue holds only messages sent after any Stop (Stop empties it),
	// so it runs on even after a stopped run. One that cannot start is
	// reported and the next one tried.
	for len(l.queue) > 0 && !m.closing {
		next := l.queue[0]
		l.queue = l.queue[1:]
		err := m.startLocked(l, next)
		if err == nil {
			break
		}
		m.emitLocked(l, "error", map[string]any{"text": err.Error()})
		m.emitLocked(l, "state", map[string]any{"busy": false, "state": l.state, "queue": len(l.queue)})
	}
}

// denyAsksLocked denies every open card (by: stop, end).
func (m *Manager) denyAsksLocked(l *live, by string) {
	for _, a := range l.asks {
		a.ch <- answer{d: agent.Decision{Note: "stopped"}}
		m.emitLocked(l, "answered", map[string]any{"req": a.req.ID, "decision": DecideDeny, "by": by})
	}
	l.asks = nil
}

// lastReply is the first line of the last assistant text.
func lastReply(msgs []agent.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			line, _, _ := strings.Cut(strings.TrimSpace(msgs[i].Content), "\n")
			return line
		}
	}
	return ""
}

// asker answers a native run's cards from the chat: session rules first,
// else the card waits for Answer or Stop.
type asker struct {
	m *Manager
	l *live
}

func (a *asker) Covered(req agent.AskRequest) (agent.Decision, bool) {
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	if _, ok := a.m.coveringRuleLocked(a.l, req); ok {
		return agent.Decision{Allow: true}, true
	}
	return agent.Decision{}, false
}

func (a *asker) Ask(ctx context.Context, req agent.AskRequest) agent.Decision {
	return a.m.wait(ctx, a.l, req).d
}

// coveringRuleLocked finds a rule that answers req: the owner's saved
// rules first, then the chat's session rules. A request the policy forces
// is never covered.
func (m *Manager) coveringRuleLocked(l *live, req agent.AskRequest) (Rule, bool) {
	if req.Forced {
		return Rule{}, false
	}
	if sv, ok := m.o.Permits.Match(l.chat.Owner, req.Tool, req.Input, l.chat.Cwd); ok {
		r := sv.Rule()
		r.Text = savedPrefix + r.Text
		return r, true
	}
	for _, r := range l.rules {
		if ruleMatches(r, req.Tool, req.Input, l.chat.Cwd) {
			return r, true
		}
	}
	return Rule{}, false
}

// savedPrefix marks a covering rule that is a saved one.
const savedPrefix = "your saved permission "

// coverText names a covering rule for a note.
func coverText(r Rule) string {
	if strings.HasPrefix(r.Text, savedPrefix) {
		return r.Text
	}
	return "the session rule " + r.Text
}

// offerLocked fills req.Always: the rules "Always allow" may save for it
// (none when the policy forces the question or nothing is saved here).
func (m *Manager) offerLocked(l *live, req *agent.AskRequest) {
	req.Always = nil
	if req.Forced || m.o.Permits == nil {
		return
	}
	req.Always = permits.Suggest(req.Tool, req.Input, l.chat.Cwd)
}

// wait blocks until the card for req (registered by its ask event) is
// answered or ctx ends.
func (m *Manager) wait(ctx context.Context, l *live, req agent.AskRequest) answer {
	m.mu.Lock()
	var p *pendingAsk
	for _, a := range l.asks {
		if a.req.ID == req.ID {
			p = a
		}
	}
	_, announced := l.chans[req.ID]
	ch := l.chanFor(req.ID)
	if p == nil && !announced { // not announced: register it now
		m.offerLocked(l, &req)
		p = &pendingAsk{req: req, ch: ch}
		l.asks = append(l.asks, p)
		m.pokeAsksLocked()
	}
	if p != nil { // still open (else it was answered before this wait began)
		m.notifyLocked(l, "LLM chat is waiting for you", req.Display+": "+cardSummary(req), "critical")
	}
	m.mu.Unlock()
	done := func() {
		m.mu.Lock()
		delete(l.chans, req.ID)
		m.mu.Unlock()
	}
	select {
	case ans := <-ch:
		done()
		return ans
	case <-ctx.Done():
		m.mu.Lock()
		if m.dropAskLocked(l, req.ID) != nil {
			m.pokeAsksLocked()
		}
		delete(l.chans, req.ID)
		m.mu.Unlock()
		return answer{d: agent.Decision{Note: "stopped"}}
	}
}

func (m *Manager) dropAskLocked(l *live, id string) *pendingAsk {
	for i, a := range l.asks {
		if a.req.ID == id {
			l.asks = append(l.asks[:i:i], l.asks[i+1:]...)
			return a
		}
	}
	return nil
}

// cardSummary is a card's one-line headline.
func cardSummary(req agent.AskRequest) string {
	if cmd := str(req.Input["command"]); cmd != "" {
		line, _, _ := strings.Cut(cmd, "\n")
		return truncRunes(line, 120)
	}
	if p := toolPath(req.Input); p != "" {
		return p
	}
	if u := str(req.Input["url"]); u != "" {
		return u
	}
	return req.Tool
}

// Decisions for Answer.
const (
	DecideOnce    = "once"
	DecideSession = "session"
	DecideDeny    = "deny"
	DecideAlways  = "always"
)

// ErrForced refuses "Always allow" on a request the command policy always
// asks for.
var ErrForced = errors.New("the command policy always asks for this one: it cannot be allowed for good")

// Answer answers an open card: once, session (adds a session rule, which
// also answers every other open card it covers) or deny with a note.
func (m *Manager) Answer(id, reqID, decision, note string) (*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	var p *pendingAsk
	for _, a := range l.asks {
		if a.req.ID == reqID {
			p = a
		}
	}
	if p == nil {
		return nil, ErrNoRequest
	}
	ans := answer{}
	var rule *Rule
	switch decision {
	case DecideOnce:
		ans.d = agent.Decision{Allow: true}
	case DecideSession:
		r, updated, ok := sessionRule(p.req, l.chat.Cwd)
		ans.d, ans.updated = agent.Decision{Allow: true, Session: true}, updated
		if !ok { // nothing narrow enough to remember: this once only
			decision, ans.d, ans.updated = DecideOnce, agent.Decision{Allow: true}, nil
		} else if !p.req.Forced {
			rule = &r
			if !containsRule(l.rules, r) {
				l.rules = append(l.rules, r)
			}
		}
	case DecideDeny:
		ans.d = agent.Decision{Note: strings.TrimSpace(note)}
	default:
		return nil, fmt.Errorf("decision must be once, session, always or deny, not %q", decision)
	}
	m.dropAskLocked(l, reqID)
	p.ch <- ans
	ev := map[string]any{"req": reqID, "decision": decision, "by": "user"}
	if note != "" && decision == DecideDeny {
		ev["note"] = note
	}
	if rule != nil {
		ev["rule"] = rule
	}
	m.emitLocked(l, "answered", ev)
	if rule != nil {
		m.emitLocked(l, "rules", map[string]any{"rules": l.rules})
		m.answerCoveredLocked(l, rule)
	}
	if len(l.asks) == 0 && l.busy {
		l.state = "thinking"
	}
	return rule, nil
}

// answerCoveredLocked allows every open card a new rule covers.
func (m *Manager) answerCoveredLocked(l *live, rule *Rule) {
	for _, a := range append([]*pendingAsk(nil), l.asks...) {
		if _, ok := m.coveringRuleLocked(l, a.req); ok {
			m.dropAskLocked(l, a.req.ID)
			a.ch <- answer{d: agent.Decision{Allow: true}}
			m.emitLocked(l, "answered", map[string]any{"req": a.req.ID, "decision": DecideOnce, "by": "rule", "rule": rule})
		}
	}
}

// AnswerAlways answers an open card "Always allow": it saves option (an
// index into permits.Suggest for that request, worked out again here: a
// rule never comes from the client) for the chat's owner, allows the card,
// and allows every other open card the rule covers.
func (m *Manager) AnswerAlways(id, reqID string, option int) (*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	var p *pendingAsk
	for _, a := range l.asks {
		if a.req.ID == reqID {
			p = a
		}
	}
	if p == nil {
		return nil, ErrNoRequest
	}
	if p.req.Forced {
		return nil, ErrForced
	}
	opts := permits.Suggest(p.req.Tool, p.req.Input, l.chat.Cwd)
	if option < 0 || option >= len(opts) {
		return nil, fmt.Errorf("option must be one of the %d offered for this request", len(opts))
	}
	sv, _, err := m.o.Permits.Add(l.chat.Owner, opts[option], permits.SourceChat)
	if err != nil {
		return nil, err
	}
	rule := sv.Rule()
	m.dropAskLocked(l, reqID)
	p.ch <- answer{d: agent.Decision{Allow: true}}
	m.emitLocked(l, "answered", map[string]any{"req": reqID, "decision": DecideAlways, "by": "user", "rule": rule, "saved": sv.ID})
	m.answerCoveredLocked(l, &rule)
	if len(l.asks) == 0 && l.busy {
		l.state = "thinking"
	}
	return &rule, nil
}

func containsRule(rules []Rule, r Rule) bool {
	for _, x := range rules {
		if x.Tool == r.Tool && x.Content == r.Content {
			return true
		}
	}
	return false
}

// Stop cancels a chat's run: the stream is cut, the running command's
// process group ended, open cards denied and the queue dropped. The chat
// itself stays.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.lookup(id)
	if err != nil {
		return err
	}
	m.stopLocked(l)
	return nil
}

func (m *Manager) stopLocked(l *live) {
	if n := len(l.queue); n > 0 {
		l.queue = nil
		m.emitLocked(l, "dropped", map[string]any{"count": n})
	}
	if !l.busy {
		return
	}
	l.stopped = true
	m.denyAsksLocked(l, "stop")
	if l.cancel != nil {
		l.cancel()
	}
}

// ---- prompts

// denyMessage is what the model reads for a denied card (as Atrium).
func denyMessage(note string) string {
	if note = strings.TrimSpace(note); note != "" && note != "stopped" {
		return "The user denied this in the chat window: " + note
	}
	return "The user denied this in the chat window."
}

// systemPrompt is the chat's system prompt (Atrium's, for the web).
func systemPrompt(cwd string, mode agent.Mode, tools bool, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a helpful assistant in Cerberus' LLM chat, a web page the user may open from any "+
		"device; tools run on the user's computer. Today is %s. The working directory is %s. Answer in the "+
		"user's language; Markdown is rendered.", now.Format("Monday 2006-01-02 15:04"), cwd)
	if tools {
		b.WriteString(" You have tools to read files, list folders, search, run shell commands and write or edit " +
			"files; paths are relative to the working directory. Use them when the task needs them, not " +
			"otherwise. The user may deny a tool call: then respect that and do not retry it.")
		if mode == agent.ModePlan {
			b.WriteString(" PLAN MODE: only reading tools are available. Investigate, then propose a plan; change nothing.")
		}
	}
	return b.String()
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
