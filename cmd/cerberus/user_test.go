package main

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/users"
)

func runUser(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	var in *strings.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	}
	var code int
	if in != nil {
		code = run(args, in, &out, &errb)
	} else {
		code = run(args, nil, &out, &errb)
	}
	return code, out.String(), errb.String()
}

func TestUserCLIOffline(t *testing.T) {
	t.Cleanup(users.SetCostForTest(1000))
	data := isolate(t)
	cfgPath := writeConfig(t, deadAddr(t))

	// No accounts yet: --json is "[]", never "null" (the container
	// entrypoint tests it before creating the first admin).
	if code, out, errs := runUser(t, "", "user", "list", "--json", "-config", cfgPath); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty list --json: %d %q %s", code, out, errs)
	}

	// The first user must be an admin.
	if code, _, errs := runUser(t, "a long password\n", "user", "add", "bob", "--role", "user", "--password-stdin", "-config", cfgPath); code != 2 || !strings.Contains(errs, "first user must be an admin") {
		t.Fatalf("first user as user: %d %s", code, errs)
	}
	if code, out, errs := runUser(t, "a long password\n", "user", "add", "ana", "--role", "admin", "--display", "Ana", "--password-stdin", "-config", cfgPath); code != 0 || !strings.Contains(out, "Added ana (admin)") {
		t.Fatalf("add admin: %d %s %s", code, out, errs)
	}
	code, out, errs := runUser(t, "", "user", "add", "bob", "--role", "user", "--generate", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "Password (shown once") {
		t.Fatalf("add generated: %d %s %s", code, out, errs)
	}
	pw := strings.TrimSpace(out[strings.LastIndex(out, ": ")+2:])
	st, err := users.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	bob, ok := st.ByName("bob")
	if !ok || !bob.MustChange || len(pw) != 16 {
		t.Fatalf("bob %+v pw %q", bob, pw)
	}
	if _, err := st.Verify("bob", pw); err != nil {
		t.Fatal("the printed password does not verify")
	}
	// Without a terminal and without a flag: refused.
	if code, _, errs := runUser(t, "", "user", "add", "cy", "--role", "viewer", "-config", cfgPath); code != 2 || !strings.Contains(errs, "not a terminal") {
		t.Fatalf("no password source: %d %s", code, errs)
	}
	// Last admin guards.
	if code, _, errs := runUser(t, "", "user", "role", "ana", "user", "-config", cfgPath); code != 2 || !strings.Contains(errs, "last enabled admin") {
		t.Fatalf("demote last admin: %d %s", code, errs)
	}
	if code, _, _ := runUser(t, "", "user", "disable", "ana", "-config", cfgPath); code != 2 {
		t.Fatal("disabled the last admin")
	}
	if code, _, _ := runUser(t, "", "user", "rm", "ana", "--yes", "-config", cfgPath); code != 2 {
		t.Fatal("removed the last admin")
	}
	// passwd revokes sign-ins and devices.
	_, _, _ = st.Logins.Create(bob.ID, "", "")
	ds, _ := devices.Open(filepath.Dir(cfgPath))
	if _, _, err := ds.PairScoped("bob-phone", "", bob.ID); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runUser(t, "another long password\n", "user", "passwd", "bob", "--password-stdin", "-config", cfgPath); code != 0 || !strings.Contains(out, "Ended 1 sign-in(s) and revoked 1 device(s)") {
		t.Fatalf("passwd: %d %s %s", code, out, errs)
	}
	if _, err := st.Verify("bob", "another long password"); err != nil {
		t.Fatal(err)
	}
	if len(ds.List()) != 0 {
		t.Fatal("device kept")
	}
	// role, disable, enable, budget, share.
	for _, args := range [][]string{
		{"user", "role", "bob", "viewer"},
		{"user", "disable", "bob"},
		{"user", "enable", "bob"},
		{"user", "budget", "bob", "--usd", "5"},
		{"user", "share", "bob", "/srv/repo", "--accept"},
	} {
		if code, out, errs := runUser(t, "", append(args, "-config", cfgPath)...); code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, errs)
		}
	}
	bob, _ = st.ByName("bob")
	if bob.Role != users.RoleViewer || bob.Disabled || bob.Budget.MonthlyUSD != 5 || len(bob.Repos) != 1 || !bob.Repos[0].Accept {
		t.Fatalf("bob %+v", bob)
	}
	if code, _, _ := runUser(t, "", "user", "share", "bob", "--unshare", "/srv/repo", "-config", cfgPath); code != 0 {
		t.Fatal("unshare")
	}
	// list.
	code, out, _ = runUser(t, "", "user", "list", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "ana (Ana)") || !strings.Contains(out, "viewer") || !strings.Contains(out, "US$5.00/month") {
		t.Fatalf("list: %d\n%s", code, out)
	}
	// rm --to: a second admin takes the data.
	if code, _, errs := runUser(t, "yet another password\n", "user", "add", "dee", "--role", "admin", "--password-stdin", "-config", cfgPath); code != 0 {
		t.Fatal(errs)
	}
	if code, _, errs := runUser(t, "", "user", "rm", "bob", "-config", cfgPath); code != 2 || !strings.Contains(errs, "--yes") {
		t.Fatalf("rm without --yes off a terminal: %d %s", code, errs)
	}
	if code, out, errs := runUser(t, "", "user", "rm", "bob", "--to", "dee", "--yes", "-config", cfgPath); code != 0 || !strings.Contains(out, "belongs to dee") {
		t.Fatalf("rm: %d %s %s", code, out, errs)
	}
	dee, _ := st.ByName("dee")
	if st.Owner(bob.ID) != dee.ID {
		t.Fatal("bob's data did not move to dee")
	}
	// Audit: every change recorded as cli; audit --user filters.
	var routes []string
	for _, r := range cliRecords(t, data) {
		if r.Actor == audit.ActorCLI {
			routes = append(routes, r.Route)
		}
	}
	for _, want := range []string{"user.add", "user.password", "user.role", "user.disable", "user.enable", "user.budget", "user.share", "user.remove"} {
		found := false
		for _, r := range routes {
			found = found || r == want
		}
		if !found {
			t.Errorf("no %s record in %v", want, routes)
		}
	}
	l, _ := audit.Open(data, audit.Options{})
	_ = l.Log(audit.Record{Actor: audit.User("ana"), User: "ana", Route: "POST /api/chats", Status: 201})
	l.Close()
	code, out, _ = runUser(t, "", "audit", "--user", "ana", "--since", "")
	if code != 0 || !strings.Contains(out, "user:ana") || strings.Contains(out, "user.add") {
		t.Fatalf("audit --user: %d\n%s", code, out)
	}
}

// userDaemon serves the real API with a users store over httptest.
func userDaemon(t *testing.T, data string) (string, *users.Store, *audit.Logger) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Token = "tok"
	cfg.RepoRoots = nil
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg.SetPath(cfgPath)
	al, err := audit.Open(data, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	ds, err := devices.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := users.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(api.Deps{Cfg: cfg, Audit: al, Devices: ds, Users: st, DataDir: data, Log: slog.New(slog.DiscardHandler)})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.Server.Listen = strings.TrimPrefix(srv.URL, "http://")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return cfgPath, st, al
}

func TestUserCLIThroughDaemon(t *testing.T) {
	t.Cleanup(users.SetCostForTest(1000))
	data := isolate(t)
	cfgPath, st, _ := userDaemon(t, data)
	if code, _, errs := runUser(t, "a long password\n", "user", "add", "bob", "--role", "chat", "--password-stdin", "-config", cfgPath); code != 2 || !strings.Contains(errs, "first user must be an admin") {
		t.Fatalf("first user: %d %s", code, errs)
	}
	if code, _, errs := runUser(t, "a long password\n", "user", "add", "ana", "--role", "admin", "--password-stdin", "-config", cfgPath); code != 0 {
		t.Fatalf("add: %d %s", code, errs)
	}
	if code, _, errs := runUser(t, "", "user", "add", "bob", "--role", "chat", "--generate", "-config", cfgPath); code != 0 {
		t.Fatalf("add bob: %d %s", code, errs)
	}
	if !st.HasUsers() {
		t.Fatal("the daemon's store has no users")
	}
	if code, _, errs := runUser(t, "", "user", "role", "ana", "viewer", "-config", cfgPath); code != 2 || !strings.Contains(errs, "last enabled admin") {
		t.Fatalf("demote through the daemon: %d %s", code, errs)
	}
	if code, _, errs := runUser(t, "", "user", "disable", "bob", "-config", cfgPath); code != 0 {
		t.Fatalf("disable: %d %s", code, errs)
	}
	if b, _ := st.ByName("bob"); !b.Disabled {
		t.Fatal("not disabled")
	}
	if code, out, errs := runUser(t, "", "user", "rm", "bob", "--yes", "-config", cfgPath); code != 0 || !strings.Contains(out, "Removed bob") {
		t.Fatalf("rm: %d %s %s", code, out, errs)
	}
	// The daemon audited the changes as cli.
	time.Sleep(10 * time.Millisecond)
	var got []string
	for _, r := range cliRecords(t, data) {
		if r.Actor == audit.ActorCLI {
			got = append(got, r.Route)
		}
	}
	joined := strings.Join(got, ",")
	for _, want := range []string{"POST /api/users", "PATCH /api/users/{id}", "DELETE /api/users/{id}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %s by cli in %v", want, got)
		}
	}
}
