package session

import (
	"slices"
	"strings"
	"time"
)

const (
	// redactMask replaces every secret value in session output.
	redactMask = "••••••"
	// minRedactLen: shorter values would mask ordinary words.
	minRedactLen = 6
	// redactRefresh is how often a live pump picks up secrets added or
	// changed since its run started (checked on read, never on a timer).
	redactRefresh = 5 * time.Second
	// maxRedactSkip bounds the escape sequences and control bytes a match may
	// step over inside one value, and so how much a candidate can hold back.
	maxRedactSkip = 512
)

var redactMaskB = []byte(redactMask)

// redactor replaces secret values in a byte stream that may split them
// across reads. Only the tail of a read that could be the start of a value
// is held back, so ordinary output, prompts included, is never delayed.
//
// A value still matches when escape sequences (colours, cursor moves) or
// control bytes such as CR and LF land between its bytes: grep --color, a
// TUI that highlights part of a token, or one that wraps a long key at the
// terminal width. The skipped sequences are written out after the mask so
// the terminal state stays right. The PTY turns every LF into CRLF, which
// the same rule covers for multi-line values (PEM keys, JSON credentials).
//
// It is used by one goroutine (the pump, or the output's transcript pass).
type redactor struct {
	values  [][]byte  // longest first
	first   [256]bool // first bytes of values
	carry   []byte    // a possible value start held back from the last read
	join    []byte    // scratch: carry + input
	out     []byte    // scratch: result of the last feed
	skipped []byte    // scratch: sequences stepped over inside the current match
	fetched time.Time // when values were last set from the source
}

// set replaces the values (see expandSecrets for which count).
func (r *redactor) set(values []string) {
	r.values = r.values[:0]
	r.first = [256]bool{}
	for _, v := range expandSecrets(values) {
		r.values = append(r.values, []byte(v))
		r.first[v[0]] = true
	}
}

// expandSecrets returns the strings to mask for values, deduplicated and
// longest first (so a value that contains another is masked whole). Values
// under minRedactLen are dropped. A multi-line value also contributes each
// of its lines, trimmed, so a key that a pager or TUI reflows, or that is
// printed only in part, is still hidden line by line.
func expandSecrets(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	add := func(v string) {
		if len(v) >= minRedactLen && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range values {
		add(v)
		if strings.ContainsAny(v, "\r\n") {
			for _, line := range strings.FieldsFunc(v, func(c rune) bool { return c == '\n' || c == '\r' }) {
				add(strings.TrimSpace(line))
			}
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// feed returns in with every value masked, minus a held-back tail that
// could still become a value. The result is valid until the next call.
func (r *redactor) feed(in []byte) []byte {
	data := in
	if len(r.carry) > 0 {
		r.join = append(append(r.join[:0], r.carry...), in...)
		r.carry = r.carry[:0]
		data = r.join
	}
	if len(r.values) == 0 {
		return data
	}
	r.out = r.scan(r.out[:0], data, true)
	return r.out
}

// flush returns the held-back tail (masked where it holds a whole value).
func (r *redactor) flush() []byte {
	if len(r.carry) == 0 {
		return nil
	}
	c := append([]byte(nil), r.carry...)
	r.carry = r.carry[:0]
	return r.scan(nil, c, false)
}

// mask returns data with every value masked, holding nothing back (complete
// text: marker text, transcript lines). It appends to dst.
func (r *redactor) mask(dst, data []byte) []byte {
	if len(r.values) == 0 {
		return append(dst, data...)
	}
	return r.scan(dst, data, false)
}

// scan appends data to out with values masked. With hold, a tail that may
// still complete a value is kept in r.carry instead of being returned.
func (r *redactor) scan(out, data []byte, hold bool) []byte {
	for i := 0; i < len(data); {
		if !r.first[data[i]] {
			// Copy the run of bytes that cannot start a value in one go.
			j := i + 1
			for j < len(data) && !r.first[data[j]] {
				j++
			}
			out = append(out, data[i:j]...)
			i = j
			continue
		}
		rest := data[i:]
		end, partial := 0, false
		// The loop stops at the first full match, so r.skipped then holds
		// that match's sequences.
		for _, v := range r.values {
			e, p := r.match(rest, v)
			if p {
				// A longer value may still complete in the next read; it wins
				// over a shorter value matching here.
				if hold {
					partial = true
					break
				}
				continue
			}
			if e > 0 {
				end = e
				break
			}
		}
		switch {
		case partial:
			r.carry = append(r.carry[:0], rest...)
			return out
		case end > 0:
			out = append(out, redactMaskB...)
			out = append(out, r.skipped...)
			i += end
		default:
			out = append(out, data[i])
			i++
		}
	}
	return out
}

// match reports whether data starts with v, allowing escape sequences and
// control bytes between v's bytes (after the first). It returns the length
// of data the match covers, or partial when data ends while it could still
// match. The sequences stepped over are left in r.skipped.
func (r *redactor) match(data, v []byte) (end int, partial bool) {
	r.skipped = r.skipped[:0]
	j, skip := 0, 0
	for k := 0; k < len(v); {
		if j == len(data) {
			return 0, true
		}
		if data[j] == v[k] {
			j++
			k++
			continue
		}
		if k == 0 {
			return 0, false
		}
		n, complete := skipLen(data[j:])
		if !complete {
			return 0, skip+len(data)-j <= maxRedactSkip
		}
		if n == 0 || skip+n > maxRedactSkip {
			return 0, false
		}
		r.skipped = append(r.skipped, data[j:j+n]...)
		j += n
		skip += n
	}
	return j, false
}

// skipLen is the length of the escape sequence or control byte at the start
// of d that a match may step over, 0 when d starts with text (or with a
// backspace or tab, which change the text), and complete=false when d ends
// inside a sequence.
func skipLen(d []byte) (n int, complete bool) {
	b := d[0]
	switch {
	case b == 0x1b:
		if len(d) < 2 {
			return 0, false
		}
		switch c := d[1]; {
		case c == '[': // CSI: parameters and intermediates, then a final byte
			for i := 2; i < len(d); i++ {
				switch {
				case d[i] >= 0x40 && d[i] <= 0x7e:
					return i + 1, true
				case d[i] < 0x20 || d[i] > 0x3f:
					return 0, true
				}
			}
			return 0, false
		case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_': // string, until BEL or ST
			for i := 2; i < len(d); i++ {
				if d[i] == 0x07 {
					return i + 1, true
				}
				if d[i] == 0x1b {
					if i+1 == len(d) {
						return 0, false
					}
					if d[i+1] == '\\' {
						return i + 2, true
					}
					return 0, true
				}
			}
			return 0, false
		case c >= 0x20 && c <= 0x2f: // intermediates, then a final byte
			for i := 2; i < len(d); i++ {
				if d[i] >= 0x30 && d[i] <= 0x7e {
					return i + 1, true
				}
				if d[i] < 0x20 || d[i] > 0x2f {
					return 0, true
				}
			}
			return 0, false
		case c >= 0x30 && c <= 0x7e:
			return 2, true
		}
		return 0, true
	case b == '\b' || b == '\t':
		return 0, true
	case b < 0x20 || b == 0x7f:
		return 1, true
	}
	return 0, true
}

// redactString masks values in s (marker text, check output, errors).
func redactString(s string, values []string) string {
	var r redactor
	r.set(values)
	if len(r.values) == 0 {
		return s
	}
	return string(r.mask(nil, []byte(s)))
}
