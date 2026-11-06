package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
)

// isolate points HOME and the XDG folders at a temp dir, so nothing here
// can touch the real config, data or keyring. It returns the data folder.
func isolate(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdgconfig"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdgdata"))
	return filepath.Join(tmp, "xdgdata", "cerberus")
}

// deadAddr is a loopback address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func cliRecords(t *testing.T, data string) []audit.Record {
	t.Helper()
	l, err := audit.Open(data, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	recs, err := l.Query(time.Time{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// daemon serves the real API over httptest with an audit log and device
// store, using cfgPath as its config (shared with the CLI under test).
func daemon(t *testing.T, data string) (cfgPath string, ds *devices.Store, al *audit.Logger) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Token = "tok"
	cfg.RepoRoots = nil
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	cfg.SetPath(cfgPath)
	var err error
	if al, err = audit.Open(data, audit.Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	if ds, err = devices.Open(dir); err != nil {
		t.Fatal(err)
	}
	h := api.New(api.Deps{Cfg: cfg, Audit: al, Devices: ds, Log: slog.New(slog.DiscardHandler)})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.Server.Listen = strings.TrimPrefix(srv.URL, "http://")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return cfgPath, ds, al
}

func TestPairOffline(t *testing.T) {
	data := isolate(t)
	cfgPath := writeConfig(t, deadAddr(t))
	var out, errb bytes.Buffer
	if code := run([]string{"pair", "-config", cfgPath, "--name", "phone", "--url", "http://desk.lan:7340"}, nil, &out, &errb); code != 0 {
		t.Fatalf("pair: %d %s", code, errb.String())
	}
	o := out.String()
	if !strings.Contains(o, "http://desk.lan:7340/?token=cdv-") || !strings.Contains(o, "█") || !strings.Contains(o, `Paired "phone"`) {
		t.Fatalf("output:\n%s", o)
	}
	ds, err := devices.Open(filepath.Dir(cfgPath))
	if err != nil {
		t.Fatal(err)
	}
	list := ds.List()
	if len(list) != 1 || list[0].Name != "phone" {
		t.Fatalf("devices %+v", list)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(ds.Path()); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	// The token printed is the one stored (hashed).
	i := strings.Index(o, "token=")
	tok := strings.Fields(o[i+len("token="):])[0]
	if _, ok := ds.Verify(tok); !ok {
		t.Fatal("printed token does not verify")
	}
	recs := cliRecords(t, data)
	if len(recs) != 1 || recs[0].Actor != "cli" || recs[0].Route != "device.pair" || recs[0].Targets[0] != list[0].ID {
		t.Fatalf("audit %+v", recs)
	}

	out.Reset()
	if code := run([]string{"devices", "-config", cfgPath}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "phone") {
		t.Fatalf("devices: %d %s %s", code, out.String(), errb.String())
	}
	if code := run([]string{"devices", "rm", list[0].ID, "-config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("rm: %d %s", code, errb.String())
	}
	if _, ok := ds.Verify(tok); ok {
		t.Fatal("revoked device still verifies")
	}
	if recs := cliRecords(t, data); recs[0].Route != "device.revoke" {
		t.Fatalf("audit %+v", recs[0])
	}
	if code := run([]string{"devices", "explode", "-config", cfgPath}, nil, &out, &errb); code != 2 {
		t.Fatalf("bad subcommand: %d", code)
	}
}

func TestPairAndRevokeThroughDaemon(t *testing.T) {
	data := isolate(t)
	cfgPath, ds, _ := daemon(t, data)
	var out, errb bytes.Buffer
	if code := run([]string{"pair", "-config", cfgPath, "--name", "tablet"}, nil, &out, &errb); code != 0 {
		t.Fatalf("pair: %d %s", code, errb.String())
	}
	list := ds.List()
	if len(list) != 1 || list[0].Name != "tablet" {
		t.Fatalf("daemon devices %+v", list)
	}
	recs := cliRecords(t, data)
	if len(recs) != 1 || recs[0].Actor != "cli" || recs[0].Route != "POST /api/devices" || recs[0].Status != 200 {
		t.Fatalf("daemon audit %+v", recs)
	}
	if code := run([]string{"devices", "rm", "tablet", "-config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("rm by name: %d %s", code, errb.String())
	}
	if len(ds.List()) != 0 {
		t.Fatal("not revoked")
	}
}

func TestTokenRotateOffline(t *testing.T) {
	data := isolate(t)
	cfgPath := writeConfig(t, deadAddr(t))
	var out, errb bytes.Buffer
	if code := run([]string{"token", "rotate", "-config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("rotate: %d %s", code, errb.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || cfg.Server.Token == "tok" || len(cfg.Server.Token) != 64 {
		t.Fatalf("token not rotated: %v %q", err, cfg.Server.Token)
	}
	recs := cliRecords(t, data)
	if len(recs) != 1 || recs[0].Route != doctor.RotationEvent || recs[0].Actor != "cli" {
		t.Fatalf("audit %+v", recs)
	}
	if code := run([]string{"token", "-config", cfgPath}, nil, &out, &errb); code != 2 {
		t.Fatalf("no subcommand: %d", code)
	}
}

func TestTokenRotateThroughDaemon(t *testing.T) {
	data := isolate(t)
	cfgPath, _, _ := daemon(t, data)
	before, _ := config.Load(cfgPath)
	var out, errb bytes.Buffer
	if code := run([]string{"token", "rotate", "-config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("rotate: %d %s", code, errb.String())
	}
	after, _ := config.Load(cfgPath)
	if after.Server.Token == before.Server.Token {
		t.Fatal("the daemon did not save a new token")
	}
	// The old token is dead at the daemon; the new one works.
	for tok, want := range map[string]int{before.Server.Token: 401, after.Server.Token: 200} {
		req, _ := http.NewRequest("GET", "http://"+after.Server.Listen+"/api/devices", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("token %s…: %d, want %d", tok[:6], resp.StatusCode, want)
		}
	}
	if !doctor.LastRotation(mustOpen(t, data)).After(time.Now().Add(-time.Minute)) {
		t.Fatal("rotation not audited")
	}
}

func mustOpen(t *testing.T, data string) *audit.Logger {
	t.Helper()
	l, err := audit.Open(data, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestAuditCommand(t *testing.T) {
	data := isolate(t)
	var out, errb bytes.Buffer
	if code := run([]string{"audit"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "No audit records yet") {
		t.Fatalf("empty: %d %q", code, out.String())
	}
	l := mustOpen(t, data)
	_ = l.Log(audit.Record{Time: time.Now().Add(-48 * time.Hour), Actor: "cli", Route: "token.rotate"})
	_ = l.Log(audit.Record{Actor: "owner:phone", Remote: "192.0.2.4", Method: "POST", Route: "POST /api/sessions/{id}/{action}",
		Targets: []string{"s1", "stop"}, Status: 200})
	_ = l.Log(audit.Record{Actor: "cli", Route: "device.pair", Detail: "paired x\x1b[31m"})
	out.Reset()
	if code := run([]string{"audit"}, nil, &out, &errb); code != 0 {
		t.Fatalf("audit: %d %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "device.pair") ||
		!strings.Contains(lines[1], "owner:phone  192.0.2.4  POST /api/sessions/{id}/{action}  [s1 stop]  200") {
		t.Fatalf("default 24h:\n%s", out.String())
	}
	if strings.ContainsRune(out.String(), 0x1b) {
		t.Fatal("control characters reached the terminal")
	}
	out.Reset()
	run([]string{"audit", "--since", "", "--actor", "cli", "--json"}, nil, &out, &errb)
	dec := json.NewDecoder(&out)
	n := 0
	for {
		var r audit.Record
		if dec.Decode(&r) != nil {
			break
		}
		if r.Actor != "cli" {
			t.Fatalf("actor filter: %+v", r)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("%d cli records", n)
	}
	if code := run([]string{"audit", "--since", "whenever"}, nil, &out, &errb); code != 2 {
		t.Fatalf("bad since: %d", code)
	}
}

func TestDoctorCommand(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope", "config.json")
	var out, errb bytes.Buffer
	if code := run([]string{"doctor", "-config", missing}, nil, &out, &errb); code != 1 || !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("missing config: %d\n%s", code, out.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("doctor created a config")
	}

	cfgPath := writeConfig(t, "127.0.0.1:7381")
	old := doctorDeps
	defer func() { doctorDeps = old }()
	gitOut := "git version 2.47.0"
	doctorDeps = func(e doctor.Env) doctor.Env {
		e.GOOS = "linux"
		e.UserConfig = t.TempDir()
		e.LookPath = func(n string) (string, error) { return "/usr/bin/" + n, nil }
		e.Output = func(context.Context, string, ...string) (string, error) { return gitOut, nil }
		e.Bwrap = func(context.Context) (string, error) { return "/usr/bin/bwrap", nil }
		e.Reachable = func(context.Context, string) error { return nil }
		e.DiskFree = func(string) (uint64, error) { return 50 << 30, nil }
		e.Readable = func(string) bool { return true }
		e.Getenv = func(string) string { return "" }
		return e
	}
	out.Reset()
	if code := run([]string{"doctor", "-config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("healthy-ish doctor: %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "24 checks:") || !strings.Contains(out.String(), "fix: ") {
		t.Fatalf("output:\n%s", out.String())
	}
	gitOut = "git version 2.9.5"
	out.Reset()
	if code := run([]string{"doctor", "--json", "-config", cfgPath}, nil, &out, &errb); code != 1 {
		t.Fatalf("old git must fail: %d", code)
	}
	var rep doctor.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.Worst() != doctor.Fail {
		t.Fatalf("json: %v %+v", err, rep)
	}
}

func TestBindSafety(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	if err := bindSafety(config.ServerConfig{Listen: "127.0.0.1:7381", ExtraListen: []string{"100.64.1.2:7381"}}, log); err != nil || logBuf.Len() != 0 {
		t.Fatalf("private: %v %s", err, logBuf.String())
	}
	if err := bindSafety(config.ServerConfig{Listen: "203.0.113.9:7381"}, log); err == nil {
		t.Fatal("public address accepted")
	}
	if err := bindSafety(config.ServerConfig{Listen: "203.0.113.9:7381", AllowPublic: true}, log); err != nil {
		t.Fatal(err)
	}
	logBuf.Reset()
	if err := bindSafety(config.ServerConfig{Listen: "0.0.0.0:7381"}, log); err != nil || !strings.Contains(logBuf.String(), "SECURITY WARNING") {
		t.Fatalf("wildcard: %v %s", err, logBuf.String())
	}
}

// serve refuses to start on a public address before binding anything.
func TestServeRefusesPublicBind(t *testing.T) {
	tmp := t.TempDir()
	isolate(t)
	cfg := config.Defaults()
	cfg.Server.Listen = "203.0.113.9:7381"
	cfg.Server.Token = "tok"
	p := filepath.Join(tmp, "config.json")
	cfg.SetPath(p)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	err := serve(context.Background(), func() {}, p, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil || !strings.Contains(err.Error(), "public address") {
		t.Fatalf("err = %v", err)
	}
}

func TestPickPairURL(t *testing.T) {
	urls := []string{"http://127.0.0.1:7340/?token=t", "http://192.168.1.5:7340/?token=t"}
	if got := pickPairURL(urls, "", "t"); got != urls[1] {
		t.Fatal(got)
	}
	if got := pickPairURL(urls[:1], "", "t"); got != urls[0] {
		t.Fatal(got)
	}
	if got := pickPairURL(urls, "https://desk.ts.net:7340/", "a b"); got != "https://desk.ts.net:7340/?token=a+b" {
		t.Fatal(got)
	}
}
