package agent

import "strings"

// thinkSplitter separates reasoning written inline as "<think>…</think>" from
// the reply, holding back a tag cut in two between chunks.
type thinkSplitter struct {
	inside  bool
	pending string
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// feed returns the reply text and the reasoning text in s.
func (t *thinkSplitter) feed(s string) (reply, think string) {
	buf := t.pending + s
	t.pending = ""
	var rb, tb strings.Builder
	for buf != "" {
		tag := thinkOpen
		out := &rb
		if t.inside {
			tag, out = thinkClose, &tb
		}
		if at := strings.Index(buf, tag); at >= 0 {
			out.WriteString(buf[:at])
			buf = buf[at+len(tag):]
			t.inside = !t.inside
			continue
		}
		keep := 0
		for n := len(tag) - 1; n > 0; n-- {
			if strings.HasSuffix(buf, tag[:n]) {
				keep = n
				break
			}
		}
		out.WriteString(buf[:len(buf)-keep])
		t.pending = buf[len(buf)-keep:]
		break
	}
	return rb.String(), tb.String()
}

// flush returns what was held back at the end of the stream.
func (t *thinkSplitter) flush() (reply, think string) {
	rest := t.pending
	t.pending = ""
	if t.inside {
		return "", rest
	}
	return rest, ""
}
