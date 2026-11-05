package devices

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPairVerifyRevoke(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s, err := OpenWith(dir, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	tok, d, err := s.Pair("my phone!")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, TokenPrefix) || len(tok) != len(TokenPrefix)+64 {
		t.Fatalf("token %q", tok)
	}
	if d.Name != "my-phone" || d.ID == "" {
		t.Fatalf("device %+v", d)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(raw), tok) || !strings.Contains(string(raw), Hash(tok)) {
		t.Fatal("the file must hold the hash, never the token")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, FileName)); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	if got, ok := s.Verify(tok); !ok || got.ID != d.ID {
		t.Fatalf("verify = %+v %v", got, ok)
	}
	for _, bad := range []string{"", tok + "x", "cdv-00", strings.TrimPrefix(tok, TokenPrefix)} {
		if _, ok := s.Verify(bad); ok {
			t.Fatalf("verify(%q) passed", bad)
		}
	}
	// Names stay unique ("admin" is reserved for the admin token).
	_, d2, _ := s.Pair("my phone")
	_, d3, _ := s.Pair("admin")
	_, d4, _ := s.Pair("")
	if d2.Name != "my-phone-2" || d3.Name != "admin-2" || d4.Name != "device" {
		t.Fatalf("names %q %q %q", d2.Name, d3.Name, d4.Name)
	}
	// Touch updates memory now and the file at most once a minute.
	s.Touch(d.ID, "192.0.2.7")
	clock = clock.Add(10 * time.Second)
	s.Touch(d.ID, "192.0.2.8")
	if l := s.List(); l[0].LastAddr != "192.0.2.8" {
		t.Fatalf("list %+v", l[0])
	}
	s2, _ := Open(dir)
	if l := s2.List(); l[0].LastAddr != "192.0.2.7" {
		t.Fatalf("persisted %+v", l[0])
	}
	if _, err := s.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(tok); ok {
		t.Fatal("revoked token still valid")
	}
	if _, err := s.Revoke(d.ID); err != ErrNotFound {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := s.Revoke("admin-2"); err != nil { // by name
		t.Fatal(err)
	}
	if n := len(s.List()); n != 2 {
		t.Fatalf("%d devices left", n)
	}
}

// A device paired or revoked by another process (the CLI) is seen at once.
func TestReloadsChangesFromAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	daemon, _ := Open(dir)
	cli, _ := Open(dir)
	tok, d, err := cli.Pair("tablet")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := daemon.Verify(tok); !ok {
		t.Fatal("daemon did not see the new device")
	}
	if _, err := cli.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := daemon.Verify(tok); ok {
		t.Fatal("daemon still accepts a device revoked by the CLI")
	}
}

func TestNilStoreAndBrokenFile(t *testing.T) {
	var s *Store
	if _, ok := s.Verify("cdv-x"); ok || len(s.List()) != 0 {
		t.Fatal("nil store")
	}
	s.Touch("x", "y")
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Fatal("broken file accepted")
	}
	if _, err := Open(""); err == nil {
		t.Fatal("empty dir accepted")
	}
}

// TestRevokeSaveFailureKeepsDevice: when devices.json cannot be written,
// Revoke fails and the device stays listed with a working token.
func TestRevokeSaveFailureKeepsDevice(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok, d, err := s.Pair("phone")
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the temp file goes makes the save fail.
	if err := os.Mkdir(filepath.Join(dir, FileName+".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revoke(d.ID); err == nil {
		t.Fatal("revoke should fail when the file cannot be saved")
	}
	if _, ok := s.Verify(tok); !ok {
		t.Fatal("device token stopped working although the revocation was not saved")
	}
	if l := s.List(); len(l) != 1 || l[0].ID != d.ID {
		t.Fatalf("device list after failed revoke: %+v", l)
	}
	_ = os.Remove(filepath.Join(dir, FileName+".tmp"))
	if _, err := s.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(tok); ok {
		t.Fatal("revoked token still works")
	}
}

// TestCleanNameKeepsUnicode: accented and non-Latin names survive.
func TestCleanNameKeepsUnicode(t *testing.T) {
	for in, want := range map[string]string{
		"teléfono":       "teléfono",
		"Miguel's phone": "Miguel's-phone",
		"電話":             "電話",
		"телефон 2":      "телефон-2",
		"a/b<c>":         "abc",
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopedClientTokens(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok, d, err := s.PairScoped("", ScopeMCP, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, ClientPrefix) || d.Scope != ScopeMCP || d.Name != "mcp-client" {
		t.Fatalf("client %q %+v", tok, d)
	}
	if got, ok := s.Verify(tok); !ok || got.Scope != ScopeMCP {
		t.Fatalf("verify client: %+v %v", got, ok)
	}
	// The other prefix never opens it, so a stored scope cannot be widened.
	if _, ok := s.Verify(TokenPrefix + strings.TrimPrefix(tok, ClientPrefix)); ok {
		t.Error("a client token verified with the device prefix")
	}
	full, _, err := s.Pair("phone")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(ClientPrefix + strings.TrimPrefix(full, TokenPrefix)); ok {
		t.Error("a device token verified with the client prefix")
	}
	if _, _, err := s.PairScoped("x", "owner", ""); err == nil {
		t.Error("an unknown scope was paired")
	}
	// A scope this build does not know (a newer daemon wrote it) opens nothing.
	raw, _ := os.ReadFile(s.Path())
	edited := strings.Replace(string(raw), `"scope": "mcp"`, `"scope": "future"`, 1)
	if err := os.WriteFile(s.Path(), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(tok); ok {
		t.Error("a token of an unknown scope verified")
	}
	if list := s2.List(); len(list) != 2 {
		t.Errorf("list %+v", list)
	}
}

func TestDeviceUsers(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokA, a, _ := s.PairScoped("phone", "", "u-a")
	_, _, _ = s.PairScoped("mcp", ScopeMCP, "u-a")
	_, b, _ := s.PairScoped("tablet", "", "u-b")
	_, legacy, _ := s.PairScoped("old", "", "")
	if d, ok := s.Verify(tokA); !ok || d.User != "u-a" || a.User != "u-a" {
		t.Fatalf("verify %+v", d)
	}
	// "" resolves to u-a (the legacy owner).
	gone, err := s.RevokeUser("u-a", func(id string) string {
		if id == "" {
			return "u-a"
		}
		return id
	})
	if err != nil || len(gone) != 3 {
		t.Fatalf("revoked %+v %v", gone, err)
	}
	left := s.List()
	if len(left) != 1 || left[0].ID != b.ID {
		t.Fatalf("left %+v (legacy %s)", left, legacy.ID)
	}
	if _, ok := s.Verify(tokA); ok {
		t.Fatal("a revoked token verifies")
	}
}
