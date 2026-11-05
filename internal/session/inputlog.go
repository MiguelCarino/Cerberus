package session

import (
	"sync"

	"carino.systems/cerberus/internal/platform"
)

// inputLineMax bounds a line collected by InputLog before Enter (the feed
// clips it to 80 characters after masking anyway).
const inputLineMax = 4 * inputClip

// hiddenLine is what the feed shows for a line typed while the terminal
// had echo off (a password or token prompt): never its text.
const hiddenLine = "(hidden input)"

// InputLog turns terminal input into feed texts without logging every
// keystroke: printable input collects per session and becomes one text
// ("line ⏎") when Enter arrives; control keys are named ("Ctrl-C", "Esc",
// "↑"…). A line any part of which was typed while the terminal had echo off
// is recorded as "(hidden input)", never its text. The zero value is ready.
type InputLog struct {
	mu     sync.Mutex
	lines  map[string]string
	hidden map[string]bool
}

// Observe returns the feed texts that data completes for session id;
// hidden reports that the terminal had echo off when data arrived.
func (l *InputLog) Observe(id string, data []byte, hidden bool) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lines == nil {
		l.lines, l.hidden = map[string]string{}, map[string]bool{}
	}
	out, line, hid := scanInput(string(data), l.lines[id], l.hidden[id] || hidden)
	if line == "" {
		delete(l.lines, id)
		delete(l.hidden, id)
	} else {
		l.lines[id], l.hidden[id] = line, hid
	}
	return out
}

// inputTexts is Observe for one self-contained piece of input (bulk input
// and quick keys): text left without Enter is returned as it is.
func inputTexts(data string, hidden bool) []string {
	out, line, hid := scanInput(data, "", hidden)
	if line != "" {
		out = append(out, shownLine(line, hid))
	}
	return out
}

// shownLine is line as the feed may show it.
func shownLine(line string, hidden bool) string {
	if hidden {
		return hiddenLine
	}
	return line
}

// scanInput consumes data on top of the pending line and returns the
// completed feed texts, the new pending line and whether it is hidden.
func scanInput(str, line string, hidden bool) ([]string, string, bool) {
	var out []string
	flush := func(text string) {
		out = append(out, text)
		line, hidden = "", false
	}
	key := func(name string) string {
		if line == "" {
			return name
		}
		return shownLine(line, hidden) + " " + name
	}
	for i := 0; i < len(str); i++ {
		c := str[i]
		switch {
		case c == '\r' || c == '\n':
			flush(key("⏎"))
			if c == '\r' && i+1 < len(str) && str[i+1] == '\n' {
				i++
			}
		case c == 0x1b:
			// An escape sequence (arrow keys) or a lone Esc.
			if i+2 < len(str) && (str[i+1] == '[' || str[i+1] == 'O') {
				name := map[byte]string{'A': "↑", 'B': "↓", 'C': "→", 'D': "←"}[str[i+2]]
				i += 2
				if name != "" {
					flush(key(name))
				}
				continue
			}
			flush(key("Esc"))
		case c == 0x7f || c == 0x08:
			if r := []rune(line); len(r) > 0 {
				line = string(r[:len(r)-1])
			}
		case c == '\t':
			flush(key("Tab"))
		case c < 0x20:
			flush(key("Ctrl-" + string(rune('A'+c-1))))
		default:
			if len(line) < inputLineMax {
				line += str[i : i+1]
			}
		}
	}
	return out, line, hidden
}

// InputHidden reports whether input typed into session id now should be
// kept out of the feed: its terminal has echo off (a password prompt), or
// echo cannot be read. A session with no run reports true.
func (m *Manager) InputHidden(id string) bool {
	m.mu.Lock()
	var r *run
	if s, ok := m.sessions[id]; ok {
		r = s.run
	}
	m.mu.Unlock()
	if r == nil {
		return true
	}
	on, known := platform.PTYEcho(r.pty)
	return !known || !on
}
