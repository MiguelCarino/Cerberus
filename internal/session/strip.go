package session

import "unicode/utf8"

// textStripper turns a terminal byte stream into plain text lines for the
// transcript. It removes escape sequences (CSI, OSC and the other string
// sequences, two- and three-byte escapes) and control characters, and treats
// a carriage return as "the next text replaces the line", so progress bars
// and spinners leave only their last state. It keeps state between feeds,
// so a sequence split across reads is still removed. Bytes from 0x80 up are
// text (UTF-8); 8-bit C1 controls are not recognised.
type textStripper struct {
	state  stripState
	seqLen int    // bytes in the current CSI or string sequence
	params []byte // the current CSI's parameter bytes
	line   []byte // the line being built
	cr     bool   // a carriage return is pending: the next text starts the line over
	out    []byte // scratch: completed lines of the last feed
}

type stripState uint8

const (
	stGround   stripState = iota
	stEsc                 // after ESC
	stEscInter            // ESC + intermediate bytes, waiting for the final byte
	stCSI                 // ESC [ …
	stString              // OSC, DCS, SOS, PM or APC body, until BEL or ESC \
)

const (
	maxLineBytes = 64 << 10 // a longer line without a newline is written out as is
	maxCSIBytes  = 256      // a runaway CSI is abandoned
	maxStrBytes  = 64 << 10 // a runaway string sequence is abandoned (its bytes stay dropped)
)

// feed consumes p and returns the lines it completed, each ending in '\n'.
// The result is valid until the next call.
func (t *textStripper) feed(p []byte) []byte {
	t.out = t.out[:0]
	for _, b := range p {
		t.step(b)
	}
	return t.out
}

// pending is the unfinished current line.
func (t *textStripper) pending() []byte { return t.line }

// flush completes the current line, if any, and returns it.
func (t *textStripper) flush() []byte {
	t.out = t.out[:0]
	if len(t.line) > 0 {
		t.emit()
	}
	t.cr = false
	return t.out
}

func (t *textStripper) step(b byte) {
	switch t.state {
	case stGround:
		t.ground(b)
	case stEsc:
		t.escape(b)
	case stEscInter:
		switch {
		case b >= 0x20 && b <= 0x2f:
		case b == 0x1b:
			t.state = stEsc
		default:
			t.state = stGround
		}
	case stCSI:
		t.seqLen++
		switch {
		case b >= 0x40 && b <= 0x7e:
			t.csiFinal(b)
			t.state = stGround
		case b >= 0x20 && b <= 0x3f:
			if len(t.params) < maxCSIBytes {
				t.params = append(t.params, b)
			}
			if t.seqLen > maxCSIBytes {
				t.state = stGround
			}
		case b == 0x1b:
			t.state = stEsc
		case b == 0x18 || b == 0x1a: // CAN, SUB abort the sequence
			t.state = stGround
		case b == '\n' || b == '\r' || b == '\b':
			t.ground(b) // C0 controls inside a CSI still act
		case b >= 0x80:
			t.state = stGround
			t.ground(b)
		}
	case stString:
		t.seqLen++
		switch {
		case b == 0x07:
			t.state = stGround
		case b == 0x1b:
			t.state = stEsc // ESC \ (ST) ends it; any other ESC starts a new sequence
		case b == 0x18 || b == 0x1a:
			t.state = stGround
		case t.seqLen > maxStrBytes:
			t.state = stGround
		}
	}
}

func (t *textStripper) escape(b byte) {
	switch {
	case b == '[':
		t.state, t.seqLen, t.params = stCSI, 0, t.params[:0]
	case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
		t.state, t.seqLen = stString, 0
	case b >= 0x20 && b <= 0x2f:
		t.state = stEscInter
	case b == 0x1b:
		// ESC ESC: the second one starts the sequence.
	default:
		t.state = stGround // a final byte (or ST's "\"), or garbage: dropped
	}
}

// csiFinal applies the few CSI sequences that matter for plain text:
// erase-line (2K) and cursor-to-column-1 (G) start the line over.
func (t *textStripper) csiFinal(final byte) {
	p := string(t.params)
	switch {
	case final == 'K' && p == "2":
		t.cr = true
	case final == 'G' && (p == "" || p == "1" || p == "0"):
		t.cr = true
	}
}

func (t *textStripper) ground(b byte) {
	switch {
	case b == 0x1b:
		t.state = stEsc
	case b == '\n':
		t.emit()
	case b == '\r':
		t.cr = true
	case b == '\b':
		if !t.cr && len(t.line) > 0 {
			_, n := utf8.DecodeLastRune(t.line)
			t.line = t.line[:len(t.line)-n]
		}
	case b == '\t' || b >= 0x20 && b != 0x7f:
		if t.cr {
			t.line = t.line[:0]
			t.cr = false
		}
		t.line = append(t.line, b)
		if len(t.line) >= maxLineBytes {
			t.emit()
		}
	}
	// Other C0 controls (BEL, NUL, …) and DEL are dropped.
}

// emit completes the current line ("\r\n" keeps the line: the carriage
// return had nothing after it to replace the text with).
func (t *textStripper) emit() {
	t.out = append(append(t.out, t.line...), '\n')
	t.line = t.line[:0]
	t.cr = false
}
