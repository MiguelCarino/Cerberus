package fsafe

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNameTable(t *testing.T) {
	long := strings.Repeat("ä", 120) + ".txt" // 240 bytes + ext
	cases := []struct {
		in, want string
		err      bool
	}{
		{"report.pdf", "report.pdf", false},
		{"e\u0301te\u0301.txt", "été.txt", false}, // NFD from macOS → NFC
		{"man\u0303ana.md", "mañana.md", false},
		{"S\u030Ckoda.png", "Škoda.png", false},
		{"../../etc/passwd", "passwd", false},
		{`C:\Users\me\b.txt`, "b.txt", false},
		{"a/b/../c.txt", "c.txt", false},
		{".env", "env", false},
		{"...hidden", "hidden", false},
		{"..", "", true},
		{".", "", true},
		{"", "", true},
		{"   ", "", true},
		{"/", "", true},
		{"a\x00b.txt", "ab.txt", false},
		{"bell\a\x1b[31m.txt", "bell[31m.txt", false},
		{"evil\u202Etxt.exe", "eviltxt.exe", false},
		{"what?<x>|y*.txt", "what__x__y_.txt", false},
		{"trailing. . ", "trailing", false},
		{"CON", "_CON", false},
		{"com1.txt", "_com1.txt", false},
		{"bad\xffutf8.txt", "bad_utf8.txt", false},
		{"photo 1 (2).jpg", "photo 1 (2).jpg", false},
	}
	for _, c := range cases {
		got, err := Name(c.in)
		if c.err {
			if !errors.Is(err, ErrBadName) {
				t.Errorf("Name(%q) = %q, %v; want ErrBadName", c.in, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Name(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	got, err := Name(long)
	if err != nil || len(got) > MaxNameBytes || !strings.HasSuffix(got, ".txt") || !strings.HasPrefix(got, "ää") {
		t.Fatalf("long name: %q (%d bytes) %v", got, len(got), err)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("cut in the middle of a character: %q", got)
		}
	}
}

func TestNFCLeavesComposedAndUnknown(t *testing.T) {
	for _, s := range []string{"plain", "ñandú", "日本語", "x\u0301"} {
		if got := NFC(s); got != s {
			t.Errorf("NFC(%q) = %q", s, got)
		}
	}
	if got := NFC("Zaz\u0307o\u0301\u0142c\u0301"); got != "Zażółć" {
		t.Errorf("Polish: %q", got)
	}
}

func TestUnique(t *testing.T) {
	taken := map[string]bool{"a.txt": true, "a (2).txt": true, "x.tar.gz": true, "noext": true, "p (4).png": true}
	exists := func(n string) bool { return taken[n] }
	for in, want := range map[string]string{
		"a.txt":     "a (3).txt",
		"b.txt":     "b.txt",
		"x.tar.gz":  "x (2).tar.gz",
		"noext":     "noext (2)",
		"a (2).txt": "a (3).txt",
		"p (4).png": "p (5).png",
	} {
		if got, err := Unique(in, exists); err != nil || got != want {
			t.Errorf("Unique(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	long := strings.Repeat("n", MaxNameBytes-4) + ".txt"
	got, _ := Unique(long, func(n string) bool { return n == long })
	if len(got) > MaxNameBytes || !strings.HasSuffix(got, " (2).txt") {
		t.Fatalf("long collision: %q (%d)", got, len(got))
	}
}

func TestSecretishAndMode(t *testing.T) {
	for _, n := range []string{"server.pem", "tls.KEY", "id_ed25519", ".env", "env", ".env.local", "prod.env", "credentials.json", "vault.kdbx"} {
		if !Secretish(n) {
			t.Errorf("%q should be secret-ish", n)
		}
	}
	for _, n := range []string{"photo.png", "keynote.txt", "environment.md", "readme"} {
		if Secretish(n) {
			t.Errorf("%q should not be secret-ish", n)
		}
	}
	if m := FileMode("env", ".env", false, true); m != 0o600 {
		t.Fatalf("env mode %o", m)
	}
	if m := FileMode("run.sh", "", false, true); m != 0o644 {
		t.Fatalf("exec bit set without AllowExec: %o", m)
	}
	if m := FileMode("run.sh", "", true, true); m != 0o755 {
		t.Fatalf("AllowExec mode %o", m)
	}
}

func TestCleanRel(t *testing.T) {
	ok := map[string]string{"": "", ".": "", "inbox": "inbox", "inbox/": "inbox", "a//b/./c": "a/b/c"}
	for in, want := range ok {
		if got, err := CleanRel(in); err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"..", "../x", "a/../../x", "a/..", "/etc", "C:/x", `a\b`, "a\x00", ".git", "x/.git/config", ".GIT/hooks"} {
		if _, err := CleanRel(bad); !errors.Is(err, ErrBadPath) {
			t.Errorf("CleanRel(%q) = %v; want ErrBadPath", bad, err)
		}
	}
}

func TestValidStaging(t *testing.T) {
	if !ValidStaging("0123456789abcdef0123") || ValidStaging("../../etc") || ValidStaging("ABCDEF0123456789") || ValidStaging("abc") {
		t.Fatal("staging id validation")
	}
}

func TestMoveIntoCollisionsAndModes(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "inbox")
	write := func(dir, name, body string, mode os.FileMode) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(dst, "a.txt", "old", 0o644)
	write(src, "a.txt", "new", 0o644)
	write(src, "tool.sh", "#!/bin/sh\n", 0o755)
	write(src, "id_rsa", "k", 0o644)
	moved, err := MoveInto(src, dst, false)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int64{}
	for _, m := range moved {
		names[m.Name] = m.Size
	}
	if _, ok := names["a (2).txt"]; !ok || len(moved) != 3 {
		t.Fatalf("moved %+v", moved)
	}
	if raw, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(raw) != "old" {
		t.Fatal("an existing file was replaced")
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging folder left behind")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dst, "tool.sh"))
		if st.Mode().Perm()&0o111 != 0 {
			t.Fatalf("exec bit kept without AllowExec: %v", st.Mode())
		}
		st, _ = os.Stat(filepath.Join(dst, "id_rsa"))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("secret-ish mode %v", st.Mode())
		}
	}
	// A symlink in place of the inbox is refused.
	ws := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, "inbox")); err == nil {
		src2 := t.TempDir()
		write(src2, "x.txt", "x", 0o644)
		if _, err := MoveInto(src2, filepath.Join(ws, "inbox"), false); err == nil {
			t.Fatal("moved into a symlinked inbox")
		}
	}
}

func TestCopyIntoKeepsSource(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if m, err := CopyInto(src, filepath.Join(t.TempDir(), "inbox"), false); err != nil || len(m) != 1 {
			t.Fatalf("copy %d: %v %v", i, m, err)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Fatal("source removed by CopyInto")
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{12: "12 B", 34000: "34 KB", 2_500_000: "2.5 MB"} {
		if got := HumanSize(n); got != want {
			t.Errorf("HumanSize(%d) = %q", n, got)
		}
	}
}

func TestDeliverSkipsPartsAndKeepsPrivateModes(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "inbox")
	// keep_names off renamed id_rsa; the Store wrote it 0600.
	if err := os.WriteFile(filepath.Join(src, "upload-20261101-120000-ab12"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, TempPrefix+"0011.part"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := MoveInto(src, dst, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0].Name != "upload-20261101-120000-ab12" {
		t.Fatalf("moved %+v", moved)
	}
	st, err := os.Stat(filepath.Join(dst, moved[0].Name))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v: a private file became readable", st.Mode(), err)
	}
}

// TestHeldFolders: files waiting under DATA (staging, attach) are 0600 in
// 0700 folders; which of them stay private in the workspace travels in the
// PrivateList, through every hop.
func TestHeldFolders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	base := t.TempDir()
	staging := filepath.Join(base, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	// Written before the folder was held: its 0600 mode marks it private.
	if err := os.WriteFile(filepath.Join(staging, "legacy-key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InitHeld(staging); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"env", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(staging, n), []byte(n), HeldMode(false, false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := MarkPrivate(staging, "env"); err != nil { // uploaded as ".env"
		t.Fatal(err)
	}
	if err := InitHeld(staging); err != nil { // idempotent: the list is kept
		t.Fatal(err)
	}
	attach := filepath.Join(base, "sess", "attach")
	if _, err := MoveIntoHeld(staging, attach, false); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(attach); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("held dir mode %v %v", st.Mode(), err)
	}
	for _, n := range []string{"env", "notes.txt", "legacy-key", PrivateList} {
		if st, err := os.Stat(filepath.Join(attach, n)); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("held %s: %v %v", n, st, err)
		}
	}
	inbox := filepath.Join(base, "ws", "inbox")
	moved, err := MoveInto(attach, inbox, false)
	if err != nil || len(moved) != 3 {
		t.Fatalf("deliver: %v %v", moved, err)
	}
	want := map[string]os.FileMode{"env": 0o600, "legacy-key": 0o600, "notes.txt": 0o644}
	for n, m := range want {
		if st, err := os.Stat(filepath.Join(inbox, n)); err != nil || st.Mode().Perm() != m {
			t.Fatalf("workspace %s: %v %v, want %o", n, st, err, m)
		}
	}
	if _, err := os.Stat(filepath.Join(inbox, PrivateList)); !os.IsNotExist(err) {
		t.Fatal("the private list was delivered into the workspace")
	}
	if HeldMode(true, true) != 0o700 || HeldMode(false, true) != 0o600 {
		t.Fatal("HeldMode exec bit")
	}
}
