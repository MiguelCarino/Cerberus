package railguard

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// This file reads railguard.json the way railguard.py does: Python's
// json.load (then read_settings' float(), bool() and str() on each value).
// encoding/json cannot stand in for it: Python's json takes NaN, Infinity
// and -Infinity, keeps 3 an int and 3.0 a float (str() tells them apart),
// and keeps a dict's keys in order (str() of a dict shows them). The shared
// fixture (testdata/railguard-settings-cases.json) and TestPyEdgeCases pin
// the two readers together.

// pyInt is a JSON integer as Python keeps it: exact, its decimal text.
type pyInt string

// pyDict is a JSON object as Python keeps it: keys in first-seen order,
// a repeated key's last value.
type pyDict struct {
	keys []string
	vals map[string]any
}

// maxDepth is past where Python's json.load gives up on nesting: its C
// scanner is bounded by the C stack, not sys.getrecursionlimit() (Python
// 3.15 on an 8 MiB stack reads about 52,000 nested objects or 58,000
// lists, and str() of them works). Deeper, json.load raises RecursionError,
// which read_settings does not catch (railguard stops); Cerberus takes the
// defaults. The parser and pyRepr recurse once per level, which Go's
// growable stacks take at this depth.
const maxDepth = 100_000

// pyIntMaxDigits is Python's int_max_str_digits default: a longer integer
// literal makes json.load raise ValueError, so the whole file is defaults.
const pyIntMaxDigits = 4300

// decodePy is json.loads on raw as Python reads a text file (UTF-8): ok is
// false wherever Python raises ValueError (bad UTF-8, a BOM, bad syntax,
// extra data). Values are nil, bool, string, pyInt, float64, []any and
// *pyDict.
func decodePy(raw []byte) (v any, ok bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	p := &pyParser{s: string(raw)}
	p.ws()
	v, ok = p.value(0)
	if !ok {
		return nil, false
	}
	p.ws()
	return v, p.i == len(p.s)
}

type pyParser struct {
	s string
	i int
}

func (p *pyParser) ws() {
	for p.i < len(p.s) && strings.IndexByte(" \t\n\r", p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *pyParser) lit(word string) bool {
	if strings.HasPrefix(p.s[p.i:], word) {
		p.i += len(word)
		return true
	}
	return false
}

func (p *pyParser) value(depth int) (any, bool) {
	if p.i >= len(p.s) || depth > maxDepth {
		return nil, false
	}
	switch c := p.s[p.i]; {
	case c == '"':
		return p.str()
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case p.lit("null"):
		return nil, true
	case p.lit("true"):
		return true, true
	case p.lit("false"):
		return false, true
	case p.lit("NaN"):
		return pyNaN(), true
	case p.lit("Infinity"):
		return pyInf(1), true
	case p.lit("-Infinity"):
		return pyInf(-1), true
	}
	return p.number()
}

func (p *pyParser) object(depth int) (any, bool) {
	p.i++ // {
	d := &pyDict{vals: map[string]any{}}
	p.ws()
	if p.lit("}") {
		return d, true
	}
	for {
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, false
		}
		k, ok := p.str()
		if !ok {
			return nil, false
		}
		p.ws()
		if !p.lit(":") {
			return nil, false
		}
		p.ws()
		v, ok := p.value(depth + 1)
		if !ok {
			return nil, false
		}
		ks := k.(string)
		if _, seen := d.vals[ks]; !seen {
			d.keys = append(d.keys, ks)
		}
		d.vals[ks] = v
		p.ws()
		if p.lit("}") {
			return d, true
		}
		if !p.lit(",") {
			return nil, false
		}
		p.ws()
	}
}

func (p *pyParser) array(depth int) (any, bool) {
	p.i++ // [
	out := []any{}
	p.ws()
	if p.lit("]") {
		return out, true
	}
	for {
		v, ok := p.value(depth + 1)
		if !ok {
			return nil, false
		}
		out = append(out, v)
		p.ws()
		if p.lit("]") {
			return out, true
		}
		if !p.lit(",") {
			return nil, false
		}
		p.ws()
	}
}

// str is Python's strict scanstring: no raw control characters, the JSON
// escapes only, \u pairs joined. A lone surrogate, which Python keeps in
// the str, is kept too, as its three WTF-8 bytes (wtf8Surrogate): it sorts
// where Python sorts it, is never blank, and pyStrRepr writes it as \udXXX.
func (p *pyParser) str() (any, bool) {
	p.i++ // "
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), true
		case c < 0x20:
			return nil, false
		case c != '\\':
			b.WriteByte(c)
			p.i++
			continue
		}
		if p.i+1 >= len(p.s) {
			return nil, false
		}
		e := p.s[p.i+1]
		p.i += 2
		if r, ok := map[byte]rune{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}[e]; ok {
			b.WriteRune(r)
			continue
		}
		if e != 'u' {
			return nil, false
		}
		r, ok := p.hex4()
		if !ok {
			return nil, false
		}
		if utf16.IsSurrogate(r) && r < 0xdc00 && strings.HasPrefix(p.s[p.i:], `\u`) {
			save := p.i
			p.i += 2
			if lo, ok := p.hex4(); ok && lo >= 0xdc00 && lo <= 0xdfff {
				r = utf16.DecodeRune(r, lo)
			} else {
				p.i = save
			}
		}
		if utf16.IsSurrogate(r) {
			b.WriteString(wtf8Surrogate(r))
		} else {
			b.WriteRune(r)
		}
	}
	return nil, false
}

// wtf8Surrogate is a lone surrogate as UTF-8 would encode it if it could
// (WTF-8): ED A0..BF xx. Go strings hold any bytes; decodePy only takes
// valid UTF-8, so these are the only ill-formed sequences a value has.
func wtf8Surrogate(r rune) string {
	return string([]byte{0xe0 | byte(r>>12), 0x80 | byte(r>>6)&0x3f, 0x80 | byte(r)&0x3f})
}

// wtf8SurrogateAt is the lone surrogate wtf8Surrogate wrote at s[i:], if any.
func wtf8SurrogateAt(s string, i int) (rune, bool) {
	if i+2 < len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2]&0xc0 == 0x80 {
		return rune(s[i]&0x0f)<<12 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f), true
	}
	return 0, false
}

func (p *pyParser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
	if err != nil || strings.ContainsAny(p.s[p.i:p.i+4], "+-_") {
		return 0, false
	}
	p.i += 4
	return rune(n), true
}

// jsonNumber is Python json's NUMBER_RE (ASCII digits, as its C scanner).
var jsonNumber = regexp.MustCompile(`^(-?(?:0|[1-9][0-9]*))(\.[0-9]+)?([eE][-+]?[0-9]+)?`)

func (p *pyParser) number() (any, bool) {
	m := jsonNumber.FindStringSubmatch(p.s[p.i:])
	if m == nil {
		return nil, false
	}
	p.i += len(m[0])
	if m[2] == "" && m[3] == "" {
		if len(strings.TrimPrefix(m[1], "-")) > pyIntMaxDigits {
			return nil, false
		}
		return pyInt(m[0]), true
	}
	f, err := strconv.ParseFloat(m[0], 64)
	if err != nil && !isRange(err) { // 1e400 is inf, as in Python
		return nil, false
	}
	return f, true
}

func pyNaN() float64         { return math.NaN() }
func pyInf(sign int) float64 { return math.Inf(sign) }

func isRange(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// --- Python's builtins on those values ---------------------------------

// truthy is Python's bool(v).
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case pyInt:
		return strings.Trim(string(x), "-0") != ""
	case float64:
		return x != 0 // NaN is true, as in Python
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *pyDict:
		return len(x.keys) > 0
	}
	return true
}

// pyFloat is Python's float(v): ints, floats, bools and numeric strings
// convert; None, lists and dicts raise TypeError (ok false). An int past
// float's range raises OverflowError in Python, which read_settings does
// not catch (railguard stops); here it is ±inf, so it clamps.
func pyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case pyInt:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil || isRange(err)
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		return pyFloatString(x)
	}
	return 0, false
}

// pyFloatText is what float(str) takes once spaces are stripped: digits
// with single underscores between them, an optional fraction and exponent,
// or inf / infinity / nan in any case, all with an optional sign.
var pyFloatText = regexp.MustCompile(`^[+-]?(?:(?:[0-9](?:_?[0-9])*(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?|(?i:inf|infinity|nan))$`)

// pyFloatString is Python's float(str). Python first turns every
// non-ASCII Unicode space into " " and every Unicode decimal digit into its
// ASCII digit (so "\u0661\u0669" is 19.0), and refuses any other non-ASCII
// character; then it strips the ASCII spaces (C's isspace: not U+001C to
// U+001F, which str.isspace counts) and parses. A sign before nan is
// allowed, as Python allows it ("-nan").
func pyFloatString(s string) (float64, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			b.WriteRune(r) // U+001C stays itself, and float() refuses it
		case pyIsSpace(r):
			b.WriteByte(' ')
		case unicode.IsDigit(r):
			b.WriteByte(byte('0' + digitValue(r)))
		default:
			return 0, false
		}
	}
	t := strings.Trim(b.String(), " \t\n\v\f\r")
	if !pyFloatText.MatchString(t) {
		return 0, false
	}
	if strings.EqualFold(strings.TrimLeft(t, "+-"), "nan") {
		return math.NaN(), true // strconv takes no sign before nan
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
	if err != nil && !isRange(err) {
		return 0, false
	}
	return f, true
}

// digitValue is a Unicode decimal digit's value: digits come in runs of
// ten from 0 (the mathematical ones in back-to-back runs).
func digitValue(r rune) int {
	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}
	return int(r-start) % 10
}

// pyIsSpace is Python's str.isspace for one character: Go's unicode.IsSpace
// plus the four ASCII separators U+001C to U+001F.
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyBlank is not str(x).strip(): all of it Python spaces.
func pyBlank(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return !pyIsSpace(r) }) < 0
}

// pyStr is Python's str(v).
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// pyRepr is Python's repr(v).
func pyRepr(v any) string {
	var b strings.Builder
	reprTo(&b, v)
	return b.String()
}

// reprTo writes repr(v) into one builder, so a deeply nested value costs
// its length, not its length times its depth.
func reprTo(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case pyInt:
		if strings.Trim(string(x), "-0") == "" {
			b.WriteString("0") // int("-0")
		} else {
			b.WriteString(string(x))
		}
	case float64:
		b.WriteString(pyFloatRepr(x))
	case string:
		b.WriteString(pyStrRepr(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			reprTo(b, e)
		}
		b.WriteByte(']')
	case *pyDict:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyStrRepr(k))
			b.WriteString(": ")
			reprTo(b, x.vals[k])
		}
		b.WriteByte('}')
	}
}

// pyFloatRepr is repr(float): the shortest digits that read back, in
// fixed notation for exponents -4 to 15 (with ".0" when whole), else
// scientific ("1e+16", "1e-05").
func pyFloatRepr(f float64) string {
	switch {
	case f != f:
		return "nan"
	case f > math.MaxFloat64:
		return "inf"
	case f < -math.MaxFloat64:
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyStrRepr is repr(str): single quotes unless the text has a ' and no ",
// backslash escapes for the quote, \\, \t, \n, \r, and \x, \u or \U for
// what Python counts unprintable.
func pyStrRepr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for i, r := range s {
		if lone, ok := wtf8SurrogateAt(s, i); ok {
			b.WriteString(`\u` + hexPad(lone, 4)) // unprintable, as in Python
			continue
		}
		if r == utf8.RuneError && i > 0 {
			if _, ok := wtf8SurrogateAt(s, i-1); ok {
				continue // the surrogate's 2nd byte
			}
			if _, ok := wtf8SurrogateAt(s, i-2); ok {
				continue // ... and its 3rd
			}
		}
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + hexPad(r, 2))
		case r < utf8.RuneSelf || unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			b.WriteString(`\x` + hexPad(r, 2))
		case r < 0x10000:
			b.WriteString(`\u` + hexPad(r, 4))
		default:
			b.WriteString(`\U` + hexPad(r, 8))
		}
	}
	b.WriteByte(q)
	return b.String()
}

func hexPad(r rune, n int) string {
	h := strconv.FormatInt(int64(r), 16)
	return strings.Repeat("0", n-len(h)) + h
}
