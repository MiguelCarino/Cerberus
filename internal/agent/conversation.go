package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// conversationFile is the name written under $CERBERUS_SESSION_DIR.
const conversationFile = "conversation.json"

// continueText is the user turn added on a resume without new text when the
// model already answered: re-sending a conversation that ends in an answer
// would only repeat it.
const continueText = "Continue where you left off."

// conversation is conversation.json: the messages as last sent to the model
// (system prompt first), with the model and working directory they ran in.
type conversation struct {
	Model    string    `json:"model"`
	Cwd      string    `json:"cwd"`
	Messages []message `json:"messages"`
}

// conversationPath is $CERBERUS_SESSION_DIR/conversation.json, or "" when the
// variable is unset (a run outside a session keeps nothing).
func conversationPath() string {
	dir := os.Getenv("CERBERUS_SESSION_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, conversationFile)
}

// saveConversation writes c to path atomically (temp file in the same folder,
// then rename), so a reader or a later --resume never sees half a file. The
// file can hold file contents and command output, hence 0600.
func saveConversation(path string, c conversation) error {
	raw, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".conversation-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(raw, '\n'))
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// loadResume reads a conversation.json for --resume and returns its messages
// without the system prompt (it is rebuilt fresh for this run), with any tool
// call left unanswered by an interrupted run closed off, so the request is
// valid for the server.
func loadResume(path string) ([]message, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("resume file: %w", err)
	}
	var c struct {
		Messages *[]message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("resume file %s is not a conversation: %v", path, err)
	}
	if c.Messages == nil {
		return nil, fmt.Errorf("resume file %s has no messages", path)
	}
	var out []message
	for i, m := range *c.Messages {
		switch m.Role {
		case "system":
			continue
		case "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("resume file %s: message %d has role %q", path, i+1, m.Role)
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return nil, fmt.Errorf("resume file %s: tool message %d has no tool_call_id", path, i+1)
		}
		out = append(out, m)
	}
	return closeToolCalls(out), nil
}

// closeToolCalls adds a result for each tool call that never got one (the
// run stopped between the reply and the tool), as chat APIs reject a
// tool_calls message without its tool replies.
func closeToolCalls(msgs []message) []message {
	out := make([]message, 0, len(msgs))
	for i := 0; i < len(msgs); {
		m := msgs[i]
		out = append(out, m)
		i++
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for ; i < len(msgs) && msgs[i].Role == "tool"; i++ {
			answered[msgs[i].ToolCallID] = true
			out = append(out, msgs[i])
		}
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				out = append(out, message{Role: "tool", ToolCallID: c.ID,
					Content: "(not run: the previous session ended before this call ran)"})
			}
		}
	}
	return out
}

// resumeMessages is the conversation a resumed run starts from: the prior
// messages, then the new text as a user turn. Without new text the run picks
// up where it stopped, or, when the model had already answered, is told to
// continue.
func resumeMessages(prior []message, text string) []message {
	msgs := append([]message(nil), prior...)
	switch {
	case strings.TrimSpace(text) != "":
		msgs = append(msgs, message{Role: "user", Content: strings.TrimSpace(text)})
	case len(msgs) == 0 || msgs[len(msgs)-1].Role == "assistant":
		msgs = append(msgs, message{Role: "user", Content: continueText})
	}
	return msgs
}
