package session

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// stripAll runs chunks through one stripper, flushing at the end.
func stripAll(chunks ...[]byte) string {
	var t textStripper
	var out bytes.Buffer
	for _, c := range chunks {
		out.Write(t.feed(c))
	}
	out.Write(t.flush())
	return out.String()
}

func TestStripper(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "hello\nworld\n", "hello\nworld\n"},
		{"colours", "a\x1b[31mred\x1b[0m \x1b[1;38;5;208mbold\x1b[m\n", "ared bold\n"},
		{"carriage return overwrite", "10%\r20%\r30%\ndone\n", "30%\ndone\n"},
		{"crlf keeps the line", "abc\r\ndef\r\n", "abc\ndef\n"},
		{"cr at end keeps text", "keep\r", "keep\n"},
		{"osc title bel", "\x1b]0;my title\x07text\n", "text\n"},
		{"osc hyperlink st", "\x1b]8;;http://x.example\x1b\\link\x1b]8;;\x1b\\\n", "link\n"},
		{"dcs", "a\x1bPq#0;2;0;0;0#0~~\x1b\\b\n", "ab\n"},
		{"charset and keypad", "\x1b(B\x1b=x\x1b>\n", "x\n"},
		{"backspace", "abd\bc\n", "abc\n"},
		{"backspace utf8", "añ\bb\n", "ab\n"},
		{"erase line and column 1", "old\x1b[2K\x1b[1Gnew\n", "new\n"},
		{"cursor moves dropped", "a\x1b[3;4Hb\x1b[?25lc\n", "abc\n"},
		{"controls dropped", "a\x07b\x00c\x7fd\n", "abcd\n"},
		{"tab kept", "a\tb\n", "a\tb\n"},
		{"utf8 text", "héllo ••\n", "héllo ••\n"},
		{"can aborts csi", "a\x1b[12\x18b\n", "ab\n"},
		{"esc esc", "a\x1b\x1b[31mb\n", "ab\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripAll([]byte(tt.in)); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripperSplitSequences(t *testing.T) {
	in := []byte("x\x1b[38;5;208mcol\x1b[0m\x1b]0;title\x07\r\n" +
		"10%\r\x1b[2K20%\r\x1b]8;;u\x1b\\30%\x1b]8;;\x1b\\\n" + "end")
	want := stripAll(in)
	if want != "xcol\n30%\nend\n" {
		t.Fatalf("whole: %q", want)
	}
	for i := 0; i <= len(in); i++ {
		for j := i; j <= len(in); j += 5 {
			if got := stripAll(in[:i], in[i:j], in[j:]); got != want {
				t.Fatalf("split %d/%d: %q, want %q", i, j, got, want)
			}
		}
	}
	var chunks [][]byte
	for _, b := range in {
		chunks = append(chunks, []byte{b})
	}
	if got := stripAll(chunks...); got != want {
		t.Fatalf("byte by byte: %q", got)
	}
}

func TestStripperLongLineAndRunawayString(t *testing.T) {
	long := strings.Repeat("y", maxLineBytes+10)
	got := stripAll([]byte(long + "\n"))
	if !strings.HasPrefix(got, strings.Repeat("y", maxLineBytes)+"\n") || len(got) != len(long)+2 {
		t.Fatalf("long line not split as expected: len %d", len(got))
	}
	runaway := "\x1b]0;" + strings.Repeat("z", maxStrBytes-1) + "ok\n" // "0;" counts too
	if got := stripAll([]byte(runaway)); got != "ok\n" {
		t.Fatalf("runaway OSC: %q", got)
	}
}

func TestOutputWritesTranscript(t *testing.T) {
	dir := t.TempDir()
	o := newOutput(dir+"/scrollback.log", 1<<20, 1024)
	o.Write([]byte("\x1b[32mgreen\x1b[0m line\r\npartial"))
	if got := string(o.transcript()); got != "green line\npartial\n" {
		t.Fatalf("live transcript %q", got)
	}
	o.release()
	if got := string(o.transcript()); got != "green line\npartial\n" {
		t.Fatalf("released transcript %q", got)
	}
}

const (
	secretA = "sk-live-0123456789abcdef"
	secretB = "tok-ABCDEF"
)

// redactAll runs chunks through one redactor, flushing at the end.
func redactAll(values []string, chunks ...[]byte) (string, int) {
	var r redactor
	r.set(values)
	var out bytes.Buffer
	maxCarry := 0
	for _, c := range chunks {
		out.Write(r.feed(c))
		maxCarry = max(maxCarry, len(r.carry))
	}
	out.Write(r.flush())
	return out.String(), maxCarry
}

func TestRedactorSplitAtEveryByte(t *testing.T) {
	values := []string{secretB, "short", secretA} // "short" is under the minimum length
	in := []byte("key=" + secretA + " and " + secretB + secretB + " short sk-live-not tok-ABC\n" + "end:" + secretA)
	want := "key=" + redactMask + " and " + redactMask + redactMask + " short sk-live-not tok-ABC\n" + "end:" + redactMask
	for i := 0; i <= len(in); i++ {
		for j := i; j <= len(in); j += 3 {
			got, carry := redactAll(values, in[:i], in[i:j], in[j:])
			if got != want {
				t.Fatalf("split %d/%d: %q", i, j, got)
			}
			if carry > len(secretA)-1 {
				t.Fatalf("split %d/%d: carry %d exceeds max value length - 1", i, j, carry)
			}
		}
	}
	var chunks [][]byte
	for _, b := range in {
		chunks = append(chunks, []byte{b})
	}
	if got, _ := redactAll(values, chunks...); got != want {
		t.Fatalf("byte by byte: %q", got)
	}
}

func TestRedactorHoldsOnlyPossibleStarts(t *testing.T) {
	var r redactor
	r.set([]string{secretA})
	if got := string(r.feed([]byte("user@host:~$ "))); got != "user@host:~$ " {
		t.Fatalf("a prompt must not be held back: %q", got)
	}
	if got := string(r.feed([]byte("echo sk-li"))); got != "echo " {
		t.Fatalf("a possible secret start must be held: %q", got)
	}
	if got := string(r.feed([]byte("ght"))); got != "sk-light" {
		t.Fatalf("held bytes must come out once they cannot be a secret: %q", got)
	}
	// A longer value still completing wins over a shorter one matching now.
	r.set([]string{"abcdef", "abcdefghij"})
	out := string(r.feed([]byte("x abcdef")))
	out += string(r.feed([]byte("ghij y")))
	if out != "x "+redactMask+" y" {
		t.Fatalf("overlapping values: %q", out)
	}
}

func TestRedactorWithoutValuesPassesThrough(t *testing.T) {
	var r redactor
	r.set(nil)
	if got := string(r.feed([]byte("anything"))); got != "anything" {
		t.Fatal(got)
	}
	if r.flush() != nil {
		t.Fatal("nothing should be held")
	}
}

// TestMarkersThenRedaction chains the two stages the way the pump does, at
// every split, including a marker inside a secret and a secret inside a
// marker's text.
func TestMarkersThenRedaction(t *testing.T) {
	values := []string{secretA}
	in := []byte("a " + secretA[:7] + "\x1b]777;cerberus;done\x07" + secretA[7:] + " b" +
		"\x1b]777;cerberus;waiting;paste " + secretA + "\x07" +
		"\x1b[31m" + secretA + "\x1b[0m end " + secretA[:4])
	wantOut := "a " + redactMask + " b" + "\x1b[31m" + redactMask + "\x1b[0m end " + secretA[:4]
	wantMarks := []marker{{"done", ""}, {"waiting", "paste " + redactMask}}
	run := func(chunks ...[]byte) (string, []marker) {
		var p markerParser
		var r redactor
		r.set(values)
		var out bytes.Buffer
		var marks []marker
		for _, c := range chunks {
			o, mk := p.feed(c)
			out.Write(r.feed(o))
			for _, m := range mk {
				m.text = redactString(m.text, values)
				marks = append(marks, m)
			}
		}
		out.Write(r.feed(p.flush()))
		out.Write(r.flush())
		return out.String(), marks
	}
	for i := 0; i <= len(in); i++ {
		for j := i; j <= len(in); j += 4 {
			out, marks := run(in[:i], in[i:j], in[j:])
			if out != wantOut || !reflect.DeepEqual(marks, wantMarks) {
				t.Fatalf("split %d/%d: %q %v", i, j, out, marks)
			}
		}
	}
}

func TestSnippets(t *testing.T) {
	text := []byte(strings.Repeat("a ", 100) + "needle" + strings.Repeat(" b", 100) + " needle end")
	got := snippets(text, []int{200, 200 + 6 + 200 + 1})
	if len(got) != 2 || !strings.Contains(got[0], "needle") || !strings.HasPrefix(got[0], "…") {
		t.Fatalf("snippets %q", got)
	}
	if len([]rune(got[0])) > 2*snippetRadius+2 {
		t.Fatalf("snippet too long: %d", len(got[0]))
	}
}

func TestRedactString(t *testing.T) {
	if got := redactString("x "+secretA+" y "+secretA, []string{secretA, "abc"}); got != "x "+redactMask+" y "+redactMask {
		t.Fatal(got)
	}
}

func TestGlobEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no glob escapes on windows")
	}
	dir := filepath.Join(t.TempDir(), "a[1]*?")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.js"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(globEscape(dir), "*.js")); len(m) != 1 {
		t.Fatalf("matches %v", m)
	}
}
