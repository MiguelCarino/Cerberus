package session

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// grep --color=always puts SGR and erase sequences inside the match; the
// value must still be masked in the raw stream at every split, with the
// sequences kept after the mask.
func TestRedactorMatchesAcrossEscapes(t *testing.T) {
	const val = "sk-abcdef123456"
	in := []byte("KEY=\x1b[01;31msk-\x1b[m\x1b[Kabcdef123456\r\nnext sk-\x1b[mzzz\n")
	want := "KEY=\x1b[01;31m" + redactMask + "\x1b[m\x1b[K\r\nnext sk-\x1b[mzzz\n"
	for i := 0; i <= len(in); i++ {
		for j := i; j <= len(in); j += 2 {
			if got, _ := redactAll([]string{val}, in[:i], in[i:j], in[j:]); got != want {
				t.Fatalf("split %d/%d: %q", i, j, got)
			}
		}
	}
	// A TUI wrapping a long key at the terminal width.
	if got, _ := redactAll([]string{val}, []byte("k: sk-abcdef\r\n123456 ok")); got != "k: "+redactMask+"\r\n ok" {
		t.Fatalf("wrapped: %q", got)
	}
	// A runaway sequence inside a candidate gives up rather than holding forever.
	long := append([]byte("sk-\x1b]0;"), bytes.Repeat([]byte("x"), 2*maxRedactSkip)...)
	if got, carry := redactAll([]string{val}, long); got != string(long) || carry > maxRedactSkip+len(val) {
		t.Fatalf("runaway: carry %d, %q", carry, got)
	}
}

// The PTY turns LF into CRLF, so a multi-line value (a PEM key) never
// arrives as stored; it must be masked anyway, and each of its lines too.
func TestRedactorMultiLineValues(t *testing.T) {
	pem := "-----BEGIN TEST KEY-----\nMIIEabcdefghijklmnop\nqrstuvwxyz0123456789\n-----END TEST KEY-----"
	in := []byte("$ cat key\r\n" + strings.ReplaceAll(pem, "\n", "\r\n") + "\r\n$ ")
	got, _ := redactAll([]string{pem}, in)
	if strings.Contains(got, "MIIE") || strings.Contains(got, "qrstuv") {
		t.Fatalf("multi-line value leaked: %q", got)
	}
	// Only one line printed (head -2) is masked by the per-line values.
	got, _ = redactAll([]string{pem}, []byte("x MIIEabcdefghijklmnop y"))
	if got != "x "+redactMask+" y" {
		t.Fatalf("single line: %q", got)
	}
	if s := redactString("out: "+pem, []string{pem}); strings.Contains(s, "MIIE") {
		t.Fatalf("redactString: %q", s)
	}
}

// Stripping can join a value that the raw stream kept apart (a backspace
// here); the transcript pass masks it before transcript.txt and search.
func TestTranscriptIsRedactedAfterStripping(t *testing.T) {
	const val = "sk-abcdef123456"
	o := newOutput(filepath.Join(t.TempDir(), "scrollback.log"), 1<<20, 4096)
	o.setSecrets([]string{val})
	o.Write([]byte("a sk-abcX\bdef123456 b\r\n"))
	o.Write([]byte("pending sk-abc\x1b[1mdef123456"))
	tx := string(o.transcript())
	if strings.Contains(tx, "def123456") {
		t.Fatalf("transcript leaks: %q", tx)
	}
	o.release()
	if tx := string(o.transcript()); strings.Contains(tx, "def123456") || !strings.Contains(tx, "a "+redactMask+" b\n") {
		t.Fatalf("released transcript: %q", tx)
	}
}

// Values injected at launch stay masked for the run's life, whatever the
// vault says now (rotated, deleted, or no vault at all).
func TestRunSecretsKeepPinnedValues(t *testing.T) {
	m := &Manager{}
	r := &run{pinned: []string{"sk-old-rotated-1"}}
	if got := m.runSecrets(r); len(got) != 1 || got[0] != "sk-old-rotated-1" {
		t.Fatalf("runSecrets %v", got)
	}
	if got := m.runSecrets(nil); len(got) != 0 {
		t.Fatalf("no run: %v", got)
	}
}
