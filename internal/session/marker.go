package session

import (
	"bytes"
	"strings"
)

// Agents signal state with OSC 777: ESC ] 777 ; cerberus ; <kind> [; text] BEL
// (ST = ESC \ is accepted too). The parser removes these from the stream.
const (
	markerPrefix = "\x1b]777;cerberus;"
	maxMarkerLen = 1024 // longer "markers" are passed through as plain output
)

var markerPrefixB = []byte(markerPrefix)

// marker is one parsed OSC 777 cerberus sequence.
type marker struct {
	kind string // waiting | resumed | done
	text string
}

// markerParser strips markers from a byte stream that may split them across
// reads. It is used by one goroutine (the pump).
type markerParser struct {
	carry []byte // an incomplete marker held back from the previous read
	join  []byte // scratch: carry + new input
	out   []byte // scratch: output of the last feed
}

// feed returns in without markers plus the markers found. The returned slice
// is only valid until the next call.
func (p *markerParser) feed(in []byte) ([]byte, []marker) {
	data := in
	if len(p.carry) > 0 {
		p.join = append(append(p.join[:0], p.carry...), in...)
		p.carry = p.carry[:0]
		data = p.join
	} else if bytes.IndexByte(in, 0x1b) < 0 {
		return in, nil
	}
	out := p.out[:0]
	var marks []marker
	i := 0
	for i < len(data) {
		j := bytes.IndexByte(data[i:], 0x1b)
		if j < 0 {
			out = append(out, data[i:]...)
			break
		}
		j += i
		out = append(out, data[i:j]...)
		mk, n, state := scanMarker(data[j:])
		switch state {
		case scanMatch:
			marks = append(marks, mk)
			i = j + n
		case scanPartial:
			p.carry = append(p.carry[:0], data[j:]...)
			i = len(data)
		default:
			out = append(out, 0x1b)
			i = j + 1
		}
	}
	p.out = out
	return out, marks
}

// flush returns any held-back bytes (at end of stream they are plain output).
func (p *markerParser) flush() []byte {
	c := p.carry
	p.carry = nil
	return c
}

type scanState int

const (
	scanNone    scanState = iota // not a marker
	scanPartial                  // could become a marker with more input
	scanMatch                    // a complete marker of n bytes
)

// scanMarker looks at b, which starts with ESC.
func scanMarker(b []byte) (marker, int, scanState) {
	if len(b) < len(markerPrefixB) {
		if bytes.HasPrefix(markerPrefixB, b) {
			return marker{}, 0, scanPartial
		}
		return marker{}, 0, scanNone
	}
	if !bytes.HasPrefix(b, markerPrefixB) {
		return marker{}, 0, scanNone
	}
	body := b[len(markerPrefixB):]
	end, termLen := findTerminator(body)
	if end < 0 {
		if len(b) <= maxMarkerLen {
			return marker{}, 0, scanPartial
		}
		return marker{}, 0, scanNone
	}
	if len(markerPrefixB)+end > maxMarkerLen {
		return marker{}, 0, scanNone
	}
	return parseMarker(string(body[:end])), len(markerPrefixB) + end + termLen, scanMatch
}

// findTerminator returns the index of BEL or ESC \ in b and its length, or -1.
func findTerminator(b []byte) (int, int) {
	for k := 0; k < len(b); k++ {
		switch b[k] {
		case 0x07:
			return k, 1
		case 0x1b:
			if k+1 < len(b) && b[k+1] == '\\' {
				return k, 2
			}
		}
	}
	return -1, 0
}

func parseMarker(body string) marker {
	kind, text, _ := strings.Cut(body, ";")
	return marker{kind: kind, text: text}
}
