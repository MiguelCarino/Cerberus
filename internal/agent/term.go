package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
)

// term writes the human-readable transcript. It tracks whether the cursor is
// at the start of a line so cards and notes never glue onto streamed text.
type term struct {
	w           io.Writer
	color       bool
	atLineStart bool
	thinking    bool
	thinkStart  time.Time
}

func newTerm(w io.Writer, color bool) *term { return &term{w: w, color: color, atLineStart: true} }

func (t *term) paint(code, s string) string {
	if !t.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (t *term) write(s string) {
	if s == "" {
		return
	}
	io.WriteString(t.w, s)
	t.atLineStart = strings.HasSuffix(s, "\n")
}

// endLine moves to a fresh line if the cursor is mid-line.
func (t *term) endLine() {
	t.thinkEnd()
	if !t.atLineStart {
		t.write("\n")
	}
}

func (t *term) line(s string) {
	t.endLine()
	t.write(s + "\n")
}

// text streams reply words. Model text never reaches the terminal with
// control characters (but newlines and tabs): an escape sequence in it
// could restyle the transcript or forge a Cerberus marker.
func (t *term) text(s string) {
	t.thinkEnd()
	t.write(cleanText(s))
}

// cleanText drops C0 and C1 control characters but \n and \t.
func cleanText(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool {
	return r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0))
}

// thinkBegin shows the folded reasoning indicator once per stretch.
func (t *term) thinkBegin() {
	if t.thinking {
		return
	}
	t.endLine()
	t.thinking, t.thinkStart = true, time.Now()
	t.write(t.paint(ansiDim, "· thinking…"))
}

// thinkEnd replaces the indicator with how long the model thought.
func (t *term) thinkEnd() {
	if !t.thinking {
		return
	}
	t.thinking = false
	done := fmt.Sprintf("· thought for %.1f s", time.Since(t.thinkStart).Seconds())
	if t.color {
		t.write("\r\x1b[2K" + t.paint(ansiDim, done) + "\n")
	} else {
		t.write("\n")
	}
}

// The lines below print text that may come from the model or a server
// (notes, errors, reasons), so it is cleaned first.
func (t *term) dimLine(s string)   { t.line(t.paint(ansiDim, cleanText(s))) }
func (t *term) errorLine(s string) { t.line(t.paint(ansiRed, "✗ "+cleanText(s))) }
func (t *term) summary(s string)   { t.line(t.paint(ansiGreen, "✓ ") + cleanText(s)) }

// card is the one-line header of a tool call.
func (t *term) card(name, summary string) {
	t.line(t.paint(ansiCyan, "▸ "+sanitize(name)) + t.paint(ansiDim, ": "+sanitize(summary)))
}

// result prints a tool's output, clipped to a few lines.
func (t *term) result(out string, bad bool) {
	const maxLines, maxWidth = 6, 200
	lines := splitLines(strings.TrimRight(out, "\n"))
	code := ansiDim
	if bad {
		code = ansiRed
	}
	for i, l := range lines {
		if i == maxLines {
			t.line(t.paint(ansiDim, fmt.Sprintf("  … %d more lines", len(lines)-maxLines)))
			break
		}
		t.line(t.paint(code, "  "+truncRunes(sanitize(l), maxWidth)))
	}
}

// osc emits a Cerberus marker: ESC ] 777 ; cerberus ; kind [; text] BEL.
func (t *term) osc(kind, text string) {
	s := "\x1b]777;cerberus;" + kind
	if text != "" {
		s += ";" + strings.ReplaceAll(sanitize(text), ";", ",")
	}
	io.WriteString(t.w, s+"\x07")
}

// sanitize drops control characters that would garble the terminal (C1
// too: some terminals take U+009B as a CSI).
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// approval prints what a change tool wants to do, then the question.
func (t *term) approval(name string, a args, hint string) {
	t.endLine()
	t.write(t.paint(ansiBold, "Approve "+name+"?") + "\n")
	switch name {
	case "run_command":
		if d := a.str("description"); d != "" {
			t.line(t.paint(ansiDim, "  "+sanitize(d)))
		}
		for _, l := range headLines(a.str("command"), 20) {
			t.line("  $ " + l)
		}
	case "write_file":
		t.line("  " + sanitize(a.str("path")))
		for _, l := range headLines(a.str("content"), 12) {
			t.line(t.paint(ansiGreen, "  + "+l))
		}
	case ToolSpawn:
		if n := a.str("agent"); n != "" {
			t.line(t.paint(ansiDim, "  agent "+sanitize(n)))
		}
		for _, l := range headLines(a.str("task"), 12) {
			t.line("  " + l)
		}
	case ToolStop:
		t.line("  session " + sanitize(a.str("id")))
	case "edit_file":
		t.line("  " + sanitize(a.str("path")))
		for _, l := range headLines(a.str("old_string"), 12) {
			t.line(t.paint(ansiRed, "  - "+l))
		}
		for _, l := range headLines(a.str("new_string"), 12) {
			t.line(t.paint(ansiGreen, "  + "+l))
		}
	}
	if hint != "" {
		t.line(t.paint(ansiDim, hint))
	}
	t.write(fmt.Sprintf("[y] yes  [a] always %s (this run)  [n] no  (or type a reason) > ", name))
}

func headLines(s string, n int) []string {
	lines := splitLines(s)
	if len(lines) > n {
		lines = append(lines[:n:n], fmt.Sprintf("… %d more lines", len(lines)-n))
	}
	for i, l := range lines {
		lines[i] = truncRunes(sanitize(l), 200)
	}
	return lines
}

// lineReader reads approval answers from stdin (a PTY, so lines may end in
// "\r", "\n" or "\r\n"). One goroutine reads, and only when asked: typed-ahead
// input stays in the terminal buffer and idle waits block, never poll.
type lineReader struct {
	in      *bufio.Reader
	req     chan struct{}
	res     chan lineResult
	started bool
	err     error
	lastCR  bool
}

type lineResult struct {
	line string
	err  error
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{in: bufio.NewReader(r), req: make(chan struct{}), res: make(chan lineResult, 1)}
}

// next returns the next line, or an error once stdin ended or ctx is done.
func (l *lineReader) next(ctx context.Context) (string, error) {
	if l.err != nil {
		return "", l.err
	}
	if !l.started {
		l.started = true
		go l.serve()
	}
	select {
	case l.req <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case r := <-l.res:
		l.err = r.err
		return r.line, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (l *lineReader) serve() {
	for range l.req {
		line, err := l.readLine()
		l.res <- lineResult{line, err}
		if err != nil {
			return
		}
	}
}

func (l *lineReader) readLine() (string, error) {
	var b []byte
	for {
		c, err := l.in.ReadByte()
		if err != nil {
			if len(b) > 0 {
				return string(b), nil
			}
			return "", err
		}
		if c == '\n' && l.lastCR && len(b) == 0 {
			l.lastCR = false
			continue
		}
		l.lastCR = false
		if c == '\r' || c == '\n' {
			l.lastCR = c == '\r'
			return string(b), nil
		}
		b = append(b, c)
	}
}
