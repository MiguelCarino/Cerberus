package chat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"carino.systems/cerberus/internal/agent"
)

// Backends.
const (
	BackendNative = "native" // the gateway and the built-in agent's tool loop
	BackendClaude = "claude" // the claude CLI, one process per message
)

// Chat is one conversation in Atrium's native session shape
// ({id, title, cwd, model, created, updated, messages} with OpenAI
// messages), plus Cerberus' own keys, which Atrium ignores.
type Chat struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Cwd      string          `json:"cwd"`
	Model    string          `json:"model"`
	Created  float64         `json:"created"` // Unix seconds, as Python's time.time()
	Updated  float64         `json:"updated"`
	Messages []agent.Message `json:"messages"`

	Backend       string  `json:"backend,omitempty"`        // native (default) | claude
	Mode          string  `json:"mode,omitempty"`           // ask | auto | acceptEdits | plan
	ClaudeSession string  `json:"claude_session,omitempty"` // the claude CLI session to --resume
	Source        string  `json:"source,omitempty"`         // "atrium:<id>" or "claude:<id>" for a continued session
	CostUSD       float64 `json:"cost_usd,omitempty"`       // API spend so far
	ContextUsed   int     `json:"context_used,omitempty"`   // tokens in the context after the last reply
	Owner         string  `json:"owner,omitempty"`          // user id (Phase 4a; "" = from before users)
	OwnerName     string  `json:"owner_name,omitempty"`     // filled by the API on read, never stored meaningfully
}

// MarshalJSON writes the messages as stored: an image becomes the words
// "[image]" (a picture is never written to disk, as in Atrium).
func (c Chat) MarshalJSON() ([]byte, error) {
	type plain Chat
	msgs := make([]json.RawMessage, 0, len(c.Messages))
	for _, m := range c.Messages {
		raw, err := storedMessage(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, raw)
	}
	return json.Marshal(struct {
		plain
		Messages []json.RawMessage `json:"messages"`
	}{plain(c), msgs})
}

// storedMessage is m with each image as a text part "[image]" (images
// first, then the words, as Atrium sends them).
func storedMessage(m agent.Message) (json.RawMessage, error) {
	if len(m.Images) == 0 {
		return json.Marshal(m)
	}
	parts := make([]map[string]string, 0, len(m.Images)+1)
	for range m.Images {
		parts = append(parts, map[string]string{"type": "text", "text": "[image]"})
	}
	if m.Content != "" {
		parts = append(parts, map[string]string{"type": "text", "text": m.Content})
	}
	return json.Marshal(map[string]any{"role": m.Role, "content": parts})
}

var reID = regexp.MustCompile(`^chat-\d{8}-\d{6}-[0-9a-f]{6}$`)

// IsID reports whether s is a chat id (chat-YYYYmmdd-HHMMSS-xxxxxx).
func IsID(s string) bool { return reID.MatchString(s) }

// newID makes a fresh chat id.
func newID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return now.Format("chat-20060102-150405-") + hex.EncodeToString(b)
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// ErrNotFound is returned for an unknown chat or session.
var ErrNotFound = errors.New("no such chat")

// store reads and writes DATA/chats/<id>.json.
type store struct{ dir string }

func (s store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// save writes c atomically, mode 0600.
func (s store) save(c *Chat) error {
	if !IsID(c.ID) {
		return fmt.Errorf("bad chat id %q", c.ID)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomic(s.dir, s.path(c.ID), raw)
}

func writeAtomic(dir, path string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(raw)
	if werr == nil {
		werr = f.Chmod(0o600)
	}
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s store) load(id string) (*Chat, error) {
	if !IsID(id) {
		return nil, ErrNotFound
	}
	return loadFile(s.path(id))
}

// loadFile reads a session file in Atrium's shape.
func loadFile(path string) (*Chat, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var probe struct {
		Messages *json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Messages == nil || !strings.HasPrefix(strings.TrimSpace(string(*probe.Messages)), "[") {
		return nil, fmt.Errorf("%s is not a chat session", filepath.Base(path))
	}
	var c Chat
	if err := json.Unmarshal(raw, &c); err != nil {
		// One odd message must not hide the whole chat: keep the ones that parse.
		var loose struct {
			Chat
			Messages []json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(raw, &loose) != nil {
			return nil, fmt.Errorf("%s is not a chat session: %v", filepath.Base(path), err)
		}
		c = loose.Chat
		c.Messages = nil
		for _, m := range loose.Messages {
			var msg agent.Message
			if json.Unmarshal(m, &msg) == nil {
				c.Messages = append(c.Messages, msg)
			}
		}
	}
	if c.Backend == "" {
		c.Backend = BackendNative
	}
	return &c, nil
}

func (s store) remove(id string) error {
	if !IsID(id) {
		return ErrNotFound
	}
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// all loads every chat in dir, newest first. Unreadable files are skipped.
func listDir(dir string) []*Chat {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*Chat
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") || !IsID(strings.TrimSuffix(name, ".json")) {
			continue
		}
		c, err := loadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if c.ID == "" {
			c.ID = strings.TrimSuffix(name, ".json")
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

// titleOf is the first user message's words, one line, at most 200 characters.
func titleOf(msgs []agent.Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		words := strings.Join(strings.Fields(m.Content), " ")
		if words == "" && len(m.Images) > 0 {
			words = "[image]"
		}
		if words == "" {
			continue
		}
		return truncRunes(words, 200)
	}
	return ""
}

func truncRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
