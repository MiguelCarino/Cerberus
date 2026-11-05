package chat

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"carino.systems/cerberus/internal/agent"
)

// Atrium import: the desktop chat's own sessions in
// $XDG_STATE_HOME/hypr/llm-chat/sessions/<id>.json are listed and opened
// read-only; "continue here" copies one into Cerberus as a new chat.

// AtriumSession is one importable Atrium session.
type AtriumSession struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"` // "atrium"
	Title    string  `json:"title"`
	Cwd      string  `json:"cwd"`
	Model    string  `json:"model"`
	Updated  float64 `json:"updated"`
	Messages int     `json:"messages"`
	Imported string  `json:"imported,omitempty"` // the Cerberus chat it was continued as
}

// atriumDir is where Atrium keeps its sessions ("" = import off).
func (m *Manager) atriumDir() string {
	if !m.cfg().Chat.AtriumImport {
		return ""
	}
	if m.o.AtriumDir != "" {
		return m.o.AtriumDir
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		if m.o.Home == "" {
			return ""
		}
		state = filepath.Join(m.o.Home, ".local", "state")
	}
	return filepath.Join(state, "hypr", "llm-chat", "sessions")
}

// AtriumSessions lists Atrium's sessions, newest first.
func (m *Manager) AtriumSessions() []AtriumSession {
	out := []AtriumSession{}
	dir := m.atriumDir()
	if dir == "" {
		return out
	}
	imported := map[string]string{}
	for _, c := range listDir(m.store.dir) {
		if id, ok := strings.CutPrefix(c.Source, "atrium:"); ok {
			imported[id] = c.ID
		}
	}
	for _, c := range listDir(dir) {
		out = append(out, AtriumSession{ID: c.ID, Kind: "atrium", Title: orStr(c.Title, "(no words)"), Cwd: c.Cwd,
			Model: c.Model, Updated: c.Updated, Messages: len(c.Messages), Imported: imported[c.ID]})
	}
	return out
}

// AtriumGet opens an Atrium session read-only.
func (m *Manager) AtriumGet(id string) (*Chat, error) {
	dir := m.atriumDir()
	if dir == "" || !IsID(id) {
		return nil, ErrNotFound
	}
	c, err := loadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, err
	}
	c.ID = id
	return c, nil
}

// ContinueAtrium copies an Atrium session into Cerberus (once: a second
// call returns the same copy). Atrium's file is never written.
func (m *Manager) ContinueAtrium(id string) (*Chat, error) {
	src, err := m.AtriumGet(id)
	if err != nil {
		return nil, err
	}
	m.contMu.Lock()
	defer m.contMu.Unlock()
	if c := m.bySource("atrium:" + id); c != nil {
		return c, nil
	}
	cwd, err := m.checkCwd(src.Cwd)
	if err != nil {
		cwd = m.o.Home
	}
	now := m.now()
	model := src.Model
	if model == "" || BackendOf(model) == BackendClaude {
		model = m.defaultModel(m.cfg())
	}
	c := &Chat{ID: newID(now), Title: src.Title, Cwd: cwd, Model: model, Created: orFloat(src.Created, unix(now)),
		Updated: unix(now), Messages: cleanMessages(src.Messages), Backend: BackendNative,
		Mode: orStr(m.cfg().Chat.DefaultMode, "ask"), Source: "atrium:" + id}
	if c.Title == "" {
		c.Title = titleOf(c.Messages)
	}
	if err := m.store.save(c); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.addLocked(newLive(c))
	m.mu.Unlock()
	return clone(c), nil
}

// cleanMessages keeps the roles a request may carry and closes tool calls
// that never got a result.
func cleanMessages(in []agent.Message) []agent.Message {
	out := []agent.Message{}
	for i := 0; i < len(in); i++ {
		m := in[i]
		switch m.Role {
		case "user", "assistant", "tool":
		default:
			continue
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			continue
		}
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for i+1 < len(in) && in[i+1].Role == "tool" {
			i++
			answered[in[i].ToolCallID] = true
			out = append(out, in[i])
		}
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				out = append(out, agent.Message{Role: "tool", ToolCallID: tc.ID, Content: "(not run: the session ended before this call ran)"})
			}
		}
	}
	return out
}

func orFloat(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

// ---- search

// Hit is one search result.
type Hit struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"` // "cerberus"
	Title   string  `json:"title"`
	Cwd     string  `json:"cwd"`
	Updated float64 `json:"updated"`
	Snippet string  `json:"snippet"` // the matching words in context ("" when the title matched)
	Role    string  `json:"role,omitempty"`
	Owner   string  `json:"owner,omitempty"`
}

// Search finds chats whose title or message text holds every word of q
// (case-insensitive), newest first, at most 50.
func (m *Manager) Search(q string) []Hit { return m.SearchWhere(q, nil) }

// SearchWhere is Search over the chats whose owner keep accepts (nil: all).
func (m *Manager) SearchWhere(q string, keep func(owner string) bool) []Hit {
	words := strings.Fields(strings.ToLower(q))
	out := []Hit{}
	if len(words) == 0 {
		return out
	}
	for _, c := range listDir(m.store.dir) {
		if keep != nil && !keep(c.Owner) {
			continue
		}
		h := Hit{ID: c.ID, Kind: "cerberus", Title: orStr(c.Title, "(no words)"), Cwd: c.Cwd, Updated: c.Updated, Owner: c.Owner}
		var all strings.Builder
		all.WriteString(strings.ToLower(c.Title))
		for _, msg := range c.Messages {
			if msg.Role == "user" || msg.Role == "assistant" {
				all.WriteString("\n" + strings.ToLower(msg.Content))
			}
		}
		if !containsAll(all.String(), words) {
			continue
		}
		if !containsAll(strings.ToLower(c.Title), words) {
			for _, msg := range c.Messages {
				if (msg.Role == "user" || msg.Role == "assistant") && strings.Contains(strings.ToLower(msg.Content), words[0]) {
					h.Snippet, h.Role = snippet(msg.Content, words[0]), msg.Role
					break
				}
			}
		}
		out = append(out, h)
		if len(out) >= 50 {
			break
		}
	}
	return out
}

func containsAll(s string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}

// snippet is about 120 characters of text around the first match of word.
func snippet(text, word string) string {
	low := strings.ToLower(text)
	at := strings.Index(low, word)
	if at < 0 || len(low) != len(text) { // lowercasing changed the length: keep it simple
		return truncRunes(strings.Join(strings.Fields(text), " "), 120)
	}
	from, to := max(0, at-50), min(len(text), at+len(word)+70)
	for from > 0 && !utf8.RuneStart(text[from]) {
		from--
	}
	for to < len(text) && !utf8.RuneStart(text[to]) {
		to++
	}
	s := strings.Join(strings.Fields(text[from:to]), " ")
	if from > 0 {
		s = "…" + s
	}
	if to < len(text) {
		s += "…"
	}
	return s
}
