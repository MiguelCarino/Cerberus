package users

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fast keeps hashing cheap in tests that are not about the cost.
func fast(t *testing.T) {
	t.Helper()
	t.Cleanup(SetCostForTest(1000))
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func open(t *testing.T) (*Store, *clock, string) {
	t.Helper()
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)}
	s, err := OpenWith(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, dir
}

func TestHashFormatAndRoundTrip(t *testing.T) {
	h, err := HashPassword(context.Background(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != "600000" {
		t.Fatalf("hash %q: want pbkdf2-sha256$600000$salt$key", h)
	}
	iter, salt, key, err := parseHash(h)
	if err != nil || iter != DefaultIterations || len(salt) != 16 || len(key) != 32 {
		t.Fatalf("parse: %d %d %d %v", iter, len(salt), len(key), err)
	}
	ok, rehash, err := CheckPasswordHash(context.Background(), h, "correct horse battery")
	if !ok || rehash || err != nil {
		t.Fatalf("check: %v %v %v", ok, rehash, err)
	}
	if ok, _, _ := CheckPasswordHash(context.Background(), h, "correct horse batterY"); ok {
		t.Fatal("a wrong password matched")
	}
	h2, _ := HashPassword(context.Background(), "correct horse battery")
	if h2 == h {
		t.Fatal("two hashes of one password are equal: no per-hash salt")
	}
}

func TestRehashOlderCount(t *testing.T) {
	fast(t)
	s, _, _ := open(t)
	restore := SetCostForTest(500)
	u, err := s.Add("ana", "Ana", RoleAdmin, "a long password")
	restore()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.Password, "$500$") {
		t.Fatalf("stored %q", u.Password)
	}
	if _, err := s.Verify("ana", "a long password"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(u.ID)
	if !strings.Contains(got.Password, "$1000$") {
		t.Fatalf("not rehashed: %q", got.Password)
	}
}

func TestUnknownNameRunsDummy(t *testing.T) {
	fast(t)
	s, _, _ := open(t)
	if _, err := s.Add("ana", "", RoleAdmin, "a long password"); err != nil {
		t.Fatal(err)
	}
	before := DummyChecks()
	_, err := s.Verify("nobody", "whatever password")
	if !errors.Is(err, ErrBadLogin) {
		t.Fatalf("err %v", err)
	}
	if DummyChecks() != before+1 {
		t.Fatal("an unknown name did not run the dummy hash")
	}
	_, err2 := s.Verify("ana", "wrong password!")
	if err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("unknown name and wrong password differ: %v / %v", err, err2)
	}
	_, _ = s.SetDisabled(mustByName(t, s, "ana").ID, false)
}

func mustByName(t *testing.T, s *Store, n string) User {
	t.Helper()
	u, ok := s.ByName(n)
	if !ok {
		t.Fatalf("no user %s", n)
	}
	return u
}

func TestPasswordRules(t *testing.T) {
	for _, c := range []struct {
		name, pw string
		ok       bool
	}{
		{"ana", "short", false},
		{"ana", "0123456789", true},
		{"ana", strings.Repeat("x", 256), true},
		{"ana", strings.Repeat("x", 257), false},
		{"longername", "LongerName", false},
		{"ana", "ana is my name", true},
	} {
		err := CheckPassword(c.name, c.pw)
		if (err == nil) != c.ok {
			t.Errorf("%q/%q: %v", c.name, c.pw, err)
		}
		if err != nil && !IsPasswordError(err) {
			t.Errorf("not a PasswordError: %v", err)
		}
	}
}

func TestNameRules(t *testing.T) {
	for n, ok := range map[string]bool{"ana": true, "a": true, "9lives": true, "a.b_c-d": true,
		"-ana": false, ".ana": false, "": false, "Ana Maria": false, strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false, "ñandu": false} {
		if ValidName(NormName(n)) != ok {
			t.Errorf("%q: want %v", n, ok)
		}
	}
	fast(t)
	s, _, _ := open(t)
	if _, err := s.Add("Ana", "", RoleAdmin, "a long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("ANA", "", RoleUser, "a long password"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("case-insensitive uniqueness: %v", err)
	}
	if _, err := s.Add("bad name", "", RoleUser, "a long password"); !errors.Is(err, ErrName) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := s.Add("bob", "", "root", "a long password"); !errors.Is(err, ErrRole) {
		t.Fatalf("bad role: %v", err)
	}
}

func TestFirstUserAdminAndLastAdmin(t *testing.T) {
	fast(t)
	s, _, _ := open(t)
	if s.HasUsers() || s.Owner("") != "" {
		t.Fatal("empty store")
	}
	if _, err := s.Add("bob", "", RoleUser, "a long password"); !errors.Is(err, ErrFirstAdmin) {
		t.Fatalf("first user as user: %v", err)
	}
	ana, err := s.Add("ana", "", RoleAdmin, "a long password")
	if err != nil {
		t.Fatal(err)
	}
	if s.Owner("") != ana.ID || s.LegacyOwner() != ana.ID {
		t.Fatal("legacy owner is not the first admin")
	}
	bob, _ := s.Add("bob", "", RoleUser, "a long password")
	if _, err := s.SetRole(ana.ID, RoleUser); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if _, err := s.SetDisabled(ana.ID, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable last admin: %v", err)
	}
	if _, err := s.Remove(ana.ID, bob.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("remove last admin: %v", err)
	}
	if _, err := s.SetRole(bob.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDisabled(ana.ID, true); err != nil {
		t.Fatalf("disable with another admin: %v", err)
	}
	if _, err := s.Verify("ana", "a long password"); !errors.Is(err, ErrBadLogin) {
		t.Fatal("a disabled user signed in")
	}
	if _, err := s.SetRole(bob.ID, RoleViewer); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("bob is now the last enabled admin: %v", err)
	}
}

func TestMovedOwnerChains(t *testing.T) {
	fast(t)
	s, _, _ := open(t)
	ana, _ := s.Add("ana", "", RoleAdmin, "a long password")
	bob, _ := s.Add("bob", "", RoleAdmin, "a long password")
	cy, _ := s.Add("cy", "", RoleUser, "a long password")
	if s.Owner(cy.ID) != cy.ID {
		t.Fatal("own id")
	}
	if _, err := s.Remove(cy.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if s.Owner(cy.ID) != bob.ID {
		t.Fatal("cy's data did not move to bob")
	}
	if _, err := s.Remove(ana.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if s.Owner("") != bob.ID || s.Owner(ana.ID) != bob.ID {
		t.Fatalf("legacy owner chain: %q", s.Owner(""))
	}
	got := s.MovedFrom(bob.ID)
	for _, want := range []string{bob.ID, ana.ID, cy.ID, ""} {
		if !slices.Contains(got, want) {
			t.Fatalf("MovedFrom %v lacks %q", got, want)
		}
	}
	// A loop (a hand-edited file) does not hang.
	s.mu.Lock()
	s.f.Moved["x"] = "y"
	s.f.Moved["y"] = "x"
	s.mu.Unlock()
	_ = s.Owner("x")
}

func TestReloadAndModes(t *testing.T) {
	fast(t)
	s, _, dir := open(t)
	if _, err := s.Add("ana", "", RoleAdmin, "a long password"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("users.json mode %v %v", st.Mode(), err)
	}
	// Another process (the CLI) adds a user.
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Add("bob", "", RoleUser, "a long password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ByName("bob"); !ok {
		t.Fatal("the outside change was not seen")
	}
	if _, _, err := s.Logins.Create(mustByName(t, s, "ana").ID, "127.0.0.1", "test"); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(filepath.Join(dir, LoginsFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("logins.json mode %v %v", st.Mode(), err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	var f File
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Users) != 2 {
		t.Fatalf("file %s", raw)
	}
}

func TestLogins(t *testing.T) {
	fast(t)
	s, c, _ := open(t)
	ana, _ := s.Add("ana", "", RoleAdmin, "a long password")
	s.Logins.SetLimits(2*time.Hour, 24*time.Hour)
	tok, lg, err := s.Logins.Create(ana.ID, "10.0.0.2", strings.Repeat("M", 200))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, LoginPrefix) || len(tok) != 4+64 || len([]rune(lg.Agent)) != 80 {
		t.Fatalf("token %q agent %d", tok, len(lg.Agent))
	}
	raw, _ := os.ReadFile(s.Logins.Path())
	if strings.Contains(string(raw), tok) {
		t.Fatal("the token is stored in clear")
	}
	if got, ok := s.Logins.Verify(tok); !ok || got.ID != lg.ID {
		t.Fatal("verify")
	}
	if _, ok := s.Logins.Verify(tok + "x"); ok {
		t.Fatal("a wrong token verified")
	}
	// Idle expiry.
	c.add(3 * time.Hour)
	if _, ok := s.Logins.Verify(tok); ok {
		t.Fatal("an idle login verified")
	}
	// Absolute expiry even when used.
	tok2, lg2, _ := s.Logins.Create(ana.ID, "", "")
	for range 30 {
		c.add(time.Hour)
		s.Logins.Touch(lg2.ID, "")
	}
	if _, ok := s.Logins.Verify(tok2); ok {
		t.Fatal("a login older than max age verified")
	}
	// Prune on write, revoke per user.
	t3, l3, _ := s.Logins.Create(ana.ID, "", "")
	_, l4, _ := s.Logins.Create(ana.ID, "", "")
	if n := len(s.Logins.List(ana.ID)); n != 2 {
		t.Fatalf("live logins %d, want 2 (expired ones pruned)", n)
	}
	if n := s.Logins.RevokeUser(ana.ID, l3.ID); n != 1 {
		t.Fatalf("revoked %d", n)
	}
	if _, ok := s.Logins.Verify(t3); !ok {
		t.Fatal("the kept login was revoked")
	}
	if s.Logins.Revoke(l4.ID) {
		t.Fatal("revoked twice")
	}
	if !s.Logins.Revoke(l3.ID) || len(s.Logins.List(ana.ID)) != 0 {
		t.Fatal("revoke")
	}
}

func TestLockout(t *testing.T) {
	c := &clock{t: time.Now()}
	l := NewLockout(5, 15*time.Minute, c.now)
	for i := range 4 {
		if locked, _ := l.Fail("Ghost"); locked {
			t.Fatalf("locked after %d", i+1)
		}
	}
	locked, retry := l.Fail("ghost")
	if !locked || retry != 15*time.Minute {
		t.Fatalf("5th failure: %v %v", locked, retry)
	}
	if ok, _ := l.Locked("GHOST"); !ok {
		t.Fatal("not locked (case)")
	}
	c.add(16 * time.Minute)
	if ok, _ := l.Locked("ghost"); ok {
		t.Fatal("still locked")
	}
	// Failures outside the window do not count.
	for range 4 {
		l.Fail("ana")
		c.add(5 * time.Minute)
	}
	if locked, _ := l.Fail("ana"); locked {
		t.Fatal("failures outside the window counted")
	}
	l.Fail("bob")
	l.Clear("bob")
	for range 4 {
		l.Fail("bob")
	}
	if ok, _ := l.Locked("bob"); ok {
		t.Fatal("clear did not reset")
	}
}

func TestAPIKeys(t *testing.T) {
	fast(t)
	s, _, _ := open(t)
	ana, _ := s.Add("ana", "", RoleAdmin, "a long password")
	bob, _ := s.Add("bob", "", RoleUser, "a long password")
	k, err := s.NewAPIKey(bob.ID)
	if err != nil || !strings.HasPrefix(k, APIKeyPrefix) {
		t.Fatal(k, err)
	}
	if u, ok := s.VerifyAPIKey(k); !ok || u.ID != bob.ID || u.APIKeyHint != k[len(k)-4:] {
		t.Fatal("verify key")
	}
	k2, _ := s.NewAPIKey(bob.ID)
	if _, ok := s.VerifyAPIKey(k); ok {
		t.Fatal("the replaced key still works")
	}
	_, _ = s.SetDisabled(bob.ID, true)
	if _, ok := s.VerifyAPIKey(k2); ok {
		t.Fatal("a disabled user's key works")
	}
	_, _ = s.SetDisabled(bob.ID, false)
	if err := s.RevokeAPIKey(bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.VerifyAPIKey(k2); ok {
		t.Fatal("a revoked key works")
	}
	_ = ana
}

func TestCapsTable(t *testing.T) {
	want := map[string][]string{
		RoleAdmin:   AllCaps,
		RoleUser:    {"chat", "chat.tools", "sessions", "sessions.write", "flows", "models.read", "usage", "gateway", "voice", "devices"},
		RoleViewer:  {"sessions.watch_all", "monitor", "doctor.counts", "devices"},
		RoleMonitor: {"monitor", "doctor.counts", "devices"},
		RoleChat:    {"chat", "usage", "voice", "devices"},
	}
	for role, caps := range want {
		got := Caps(role)
		if !slices.Equal(got, caps) {
			t.Errorf("%s: %v, want %v", role, got, caps)
		}
		for _, c := range AllCaps {
			if Can(role, c) != slices.Contains(caps, c) {
				t.Errorf("Can(%s, %s)", role, c)
			}
		}
	}
	if len(AllCaps) != 25 {
		t.Fatalf("%d caps", len(AllCaps))
	}
	if Home(RoleViewer) != "#/sessions" || Home(RoleMonitor) != "#/monitor" || Home(RoleChat) != "#/chat" || Home(RoleUser) != "#/chat" {
		t.Fatal("homes")
	}
	if Can("nobody", CapChat) || len(Caps("nobody")) != 0 {
		t.Fatal("unknown role has caps")
	}
}

func TestSharesAgentsBudget(t *testing.T) {
	fast(t)
	s, _, _ := open(t)
	_, _ = s.Add("ana", "", RoleAdmin, "a long password")
	bob, _ := s.Add("bob", "Bob B.", RoleUser, "a long password")
	if !bob.AllowsAgent("cerberus") || bob.AllowsAgent("claude") {
		t.Fatal("default agent rule")
	}
	if _, err := s.SetRepos(bob.ID, []RepoShare{{Path: "rel"}}); err == nil {
		t.Fatal("relative repo accepted")
	}
	u, err := s.SetRepos(bob.ID, []RepoShare{{Path: "/r/a/"}, {Path: "/r/a", Accept: true}, {Path: "/r/b", Accept: true}})
	if err != nil || len(u.Repos) != 2 {
		t.Fatal(u.Repos, err)
	}
	if sh, ok := u.Share("/r/b/"); !ok || !sh.Accept {
		t.Fatal("share lookup")
	}
	u, _ = s.SetAgents(bob.ID, []string{"claude", "claude", " "})
	if !u.AllowsAgent("claude") || u.AllowsAgent("cerberus") {
		t.Fatal("agents", u.Agents)
	}
	if _, err := s.SetBudget(bob.ID, Budget{MonthlyUSD: -1}); err == nil {
		t.Fatal("negative budget")
	}
	u, _ = s.SetBudget(bob.ID, Budget{MonthlyUSD: 5, MonthlyTokens: 1000})
	if u.Budget.MonthlyUSD != 5 || u.Label() != "Bob B." {
		t.Fatal("budget/label")
	}
	if _, err := s.SetDisplay(bob.ID, strings.Repeat("é", 61)); err == nil {
		t.Fatal("long display")
	}
	u, _ = s.SetPassword(bob.ID, "another long one", true)
	if !u.MustChange {
		t.Fatal("must change")
	}
	if _, err := s.SetPassword(bob.ID, "bob", false); !IsPasswordError(err) {
		t.Fatal("rule on reset")
	}
	if d, err := s.EnsureFilesDir(bob.ID); err != nil || !strings.HasSuffix(d, filepath.Join("users", bob.ID, "files")) {
		t.Fatal(d, err)
	}
}

func TestHashSemaphoreHonoursContext(t *testing.T) {
	// Fill the queue, then a waiting hash gives up with its context.
	for range maxHashing {
		hashSem <- struct{}{}
	}
	defer func() {
		for range maxHashing {
			<-hashSem
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := HashPassword(ctx, "a long password"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}
