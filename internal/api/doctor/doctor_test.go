package doctor

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/users"
)

var errMissing = errors.New("missing")

// fixture is a healthy machine: every check ok. Cases break one thing.
type fixture struct {
	t        *testing.T
	cfg      *config.Config
	dir      string
	path     map[string]bool // binaries on PATH
	sys      map[string]bool // fake /sys and /run files that exist
	readable bool
	env      map[string]string
	gitOut   string
	free     uint64
	reach    map[string]bool
	bwrap    error
	rotated  time.Time
	now      time.Time
	users    *users.Store
	linger   string
}

func newFixture(t *testing.T) *fixture {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Server.Token = "tok"
	cfg.Server.LocalNoAuth = false
	cfg.Sandbox.Default = "bwrap"
	cfg.SetPath(filepath.Join(dir, "config", "config.json"))
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, cfg: cfg, dir: dir,
		path: map[string]bool{"llama-server": true, "claude": true, "opencode": true, "aider": true, "bash": true,
			"git": true, "systemd-run": true, "ffmpeg": true, "lms": true},
		sys:      map[string]bool{"/sys/class/powercap/intel-rapl:0/energy_uj": true, "/run/user/1000/bus": true},
		readable: true,
		env:      map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "USER": "carino"},
		gitOut:   "git version 2.47.1\n",
		free:     200 << 30,
		reach:    map[string]bool{"http://127.0.0.1:11434/api/version": true},
		now:      time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
	f.rotated = f.now.Add(-30 * 24 * time.Hour)
	unit := filepath.Join(dir, "userconfig", "systemd", "user", "cerberus.service")
	_ = os.MkdirAll(filepath.Dir(unit), 0o700)
	_ = os.WriteFile(unit, []byte("[Unit]\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "data"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "data", "audit.jsonl"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "config", "devices.json"), []byte(`{"devices":[]}`), 0o600)
	// An admin account and lingering on (Phase 4a).
	t.Cleanup(users.SetCostForTest(1000))
	st, err := users.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("ana", "", users.RoleAdmin, "a long password"); err != nil {
		t.Fatal(err)
	}
	f.users = st
	f.linger = filepath.Join(dir, "linger")
	_ = os.MkdirAll(f.linger, 0o755)
	_ = os.WriteFile(filepath.Join(f.linger, "carino"), nil, 0o644)
	return f
}

func (f *fixture) envFor() Env {
	return Env{
		Cfg: f.cfg, ConfigPath: f.cfg.Path(), DataDir: filepath.Join(f.dir, "data"),
		UserConfig: filepath.Join(f.dir, "userconfig"), Home: filepath.Join(f.dir, "home"), GOOS: "linux",
		LookPath: func(name string) (string, error) {
			if f.path[filepath.Base(name)] {
				return "/usr/bin/" + name, nil
			}
			return "", errMissing
		},
		Output: func(_ context.Context, name string, args ...string) (string, error) {
			if name == "git" && f.gitOut != "" {
				return f.gitOut, nil
			}
			return "", errMissing
		},
		DiskFree:  func(string) (uint64, error) { return f.free, nil },
		Reachable: func(_ context.Context, u string) error { return boolErr(f.reach[u]) },
		Bwrap: func(context.Context) (string, error) {
			if f.bwrap != nil {
				return "", f.bwrap
			}
			return "/usr/bin/bwrap", nil
		},
		Stat: func(p string) (fs.FileInfo, error) {
			if strings.HasPrefix(p, "/sys/") || strings.HasPrefix(p, "/run/") {
				if f.sys[p] {
					return os.Stat(f.dir) // any existing file info
				}
				return nil, fs.ErrNotExist
			}
			return os.Stat(p)
		},
		Readable:     func(string) bool { return f.readable },
		Getenv:       func(k string) string { return f.env[k] },
		Resolve:      func(string) ([]net.IP, error) { return nil, errMissing },
		Now:          func() time.Time { return f.now },
		TokenRotated: func() time.Time { return f.rotated },
		Users:        f.users,
		LingerDir:    f.linger,
	}
}

func boolErr(ok bool) error {
	if ok {
		return nil
	}
	return errMissing
}

func (f *fixture) run() map[string]Check {
	rep := Run(context.Background(), f.envFor())
	out := map[string]Check{}
	for _, c := range rep.Checks {
		out[c.ID] = c
	}
	return out
}

func TestHealthyFixtureAllOK(t *testing.T) {
	f := newFixture(t)
	rep := Run(context.Background(), f.envFor())
	if len(rep.Checks) != 24 {
		t.Fatalf("%d checks", len(rep.Checks))
	}
	for _, c := range rep.Checks {
		if c.Status != OK {
			t.Errorf("%s: %s %s", c.ID, c.Status, c.Detail)
		}
		if c.Name == "" || c.Detail == "" {
			t.Errorf("%s: empty name or detail", c.ID)
		}
	}
	if rep.Worst() != OK || rep.Counts[OK] != 24 {
		t.Fatalf("counts %v", rep.Counts)
	}
}

// Each case breaks one fact and expects that check's status.
func TestChecksAgainstFixtureStates(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(f *fixture)
		id     string
		want   Status
		text   string
	}{
		{"config bad listen", func(f *fixture) { f.cfg.Server.Listen = "nonsense" }, "config", Fail, "host:port"},
		{"config half TLS", func(f *fixture) { f.cfg.Server.TLSCert = "/x.pem" }, "config", Fail, "tls_key"},
		{"config unknown sandbox", func(f *fixture) { f.cfg.Sandbox.Default = "jail" }, "config", Fail, "treated as bwrap"},
		{"config bad approve", func(f *fixture) { f.cfg.Agent.Approve = "yolo" }, "config", Fail, "approve"},
		{"config missing default agent", func(f *fixture) { f.cfg.DefaultAgent = "ghost" }, "config", Fail, "ghost"},
		{"loose config mode", func(f *fixture) { _ = os.Chmod(f.cfg.Path(), 0o644) }, "modes", Fail, "config.json (0644)"},
		{"loose devices mode", func(f *fixture) { _ = os.Chmod(filepath.Join(f.dir, "config", "devices.json"), 0o640) }, "modes", Fail, "devices.json"},
		{"loose audit mode", func(f *fixture) { _ = os.Chmod(filepath.Join(f.dir, "data", "audit.jsonl"), 0o604) }, "modes", Fail, "audit.jsonl"},
		{"public listen", func(f *fixture) { f.cfg.Server.ExtraListen = []string{"8.8.8.8:7340"} }, "listen", Fail, "public"},
		{"public allowed", func(f *fixture) {
			f.cfg.Server.ExtraListen = []string{"8.8.8.8:7340"}
			f.cfg.Server.AllowPublic = true
		}, "listen", Warn, "PUBLIC"},
		{"wildcard listen", func(f *fixture) { f.cfg.Server.Listen = "0.0.0.0:7340" }, "listen", Warn, "EVERY interface"},
		{"plain http on LAN", func(f *fixture) { f.cfg.Server.ExtraListen = []string{"192.168.1.5:7340"} }, "tls", Warn, "plain HTTP"},
		{"tls file missing", func(f *fixture) {
			f.cfg.Server.ExtraListen = []string{"192.168.1.5:7340"}
			f.cfg.Server.Listen = "192.168.1.5:7341"
			f.cfg.Server.TLSCert, f.cfg.Server.TLSKey = "/nope/cert.pem", "/nope/key.pem"
		}, "tls", Fail, "missing"},
		{"old token", func(f *fixture) { f.rotated = f.now.Add(-400 * 24 * time.Hour) }, "token_age", Warn, "400 days"},
		{"slice turned off", func(f *fixture) { f.cfg.Guard.SystemdSlice = "" }, "systemd", Warn, "systemd_slice is empty"},
		{"engines all off", func(f *fixture) {
			f.cfg.Engines.LlamaCpp.Enabled, f.cfg.Engines.LMStudio.Enabled, f.cfg.Engines.Ollama.Enabled = false, false, false
			f.cfg.Providers = nil
		}, "engines", Warn, "turned off"},
		{"old token_created", func(f *fixture) {
			f.rotated = time.Time{}
			f.cfg.Server.TokenCreated = f.now.Add(-200 * 24 * time.Hour)
		}, "token_age", Warn, "created 200 days"},
		{"local no auth", func(f *fixture) { f.cfg.Server.LocalNoAuth = true; f.users = nil }, "local_no_auth", Warn, "without a key"},
		{"bwrap missing, fallback", func(f *fixture) { f.bwrap = errors.New("bwrap is not installed") }, "sandbox", Fail, "fall back"},
		{"bwrap missing, strict", func(f *fixture) {
			f.bwrap = errors.New("bwrap is not installed")
			f.cfg.Sandbox.Strict = true
		}, "sandbox", Fail, "refuse"},
		{"bwrap unused", func(f *fixture) { f.cfg.Sandbox.Default = "none" }, "sandbox", Warn, "no session uses it"},
		{"bwrap missing, unused", func(f *fixture) {
			f.cfg.Sandbox.Default = "none"
			f.bwrap = errors.New("bwrap is not installed")
		}, "sandbox", Warn, "unsandboxed"},
		{"rapl root only", func(f *fixture) { f.readable = false }, "rapl", Warn, "root-only"},
		{"no engines", func(f *fixture) {
			delete(f.path, "llama-server")
			delete(f.path, "lms")
			f.reach = nil
		}, "engines", Warn, "no local engine"},
		{"default agent missing", func(f *fixture) {
			f.cfg.DefaultAgent = "claude"
			delete(f.path, "claude")
		}, "agents", Fail, "default agent"},
		{"no systemd-run", func(f *fixture) { delete(f.path, "systemd-run") }, "systemd", Warn, "systemd-run"},
		{"no user bus", func(f *fixture) { delete(f.sys, "/run/user/1000/bus") }, "systemd", Warn, "D-Bus"},
		{"low disk", func(f *fixture) { f.free = 3 << 30 }, "disk", Warn, "GiB free"},
		{"no disk", func(f *fixture) { f.free = 100 << 20 }, "disk", Fail, "GiB free"},
		{"keep forever", func(f *fixture) { f.cfg.Sessions.RetentionDays = 0 }, "retention", Warn, "forever"},
		{"no git", func(f *fixture) { delete(f.path, "git") }, "git", Fail, "not installed"},
		{"old git", func(f *fixture) { f.gitOut = "git version 2.11.0" }, "git", Fail, "2.17"},
		{"no ffmpeg", func(f *fixture) { delete(f.path, "ffmpeg") }, "voice", Warn, "ffmpeg"},
		{"no unit", func(f *fixture) {
			_ = os.Remove(filepath.Join(f.dir, "userconfig", "systemd", "user", "cerberus.service"))
		}, "service", Warn, "no systemd user unit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.break_(f)
			c := f.run()[tc.id]
			if c.Status != tc.want || !strings.Contains(c.Detail, tc.text) {
				t.Fatalf("%s = %s %q, want %s containing %q", tc.id, c.Status, c.Detail, tc.want, tc.text)
			}
			if c.Status != OK && c.Fix == "" {
				t.Fatalf("%s: no fix", tc.id)
			}
		})
	}
}

func TestConfigLoadFailure(t *testing.T) {
	rep := Run(context.Background(), Env{CfgErr: errors.New("config x: invalid character"), ConfigPath: "/x/config.json"})
	if len(rep.Checks) != 1 || rep.Checks[0].Status != Fail || rep.Worst() != Fail {
		t.Fatalf("%+v", rep)
	}
}

func TestOtherOSSkipsLinuxChecks(t *testing.T) {
	f := newFixture(t)
	e := f.envFor()
	e.GOOS = "windows"
	rep := Run(context.Background(), e)
	for _, c := range rep.Checks {
		if (c.ID == "rapl" || c.ID == "systemd" || c.ID == "service" || c.ID == "modes") && c.Status != OK {
			t.Errorf("%s on windows: %s", c.ID, c.Status)
		}
	}
}

func TestCheckListen(t *testing.T) {
	resolve := func(h string) ([]net.IP, error) {
		switch h {
		case "desk.tailnet.ts.net":
			return []net.IP{net.ParseIP("100.100.1.2")}, nil
		case "example.com":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return nil, errMissing
	}
	cases := []struct {
		addr    string
		allow   bool
		wantErr bool
		warn    bool
	}{
		{"127.0.0.1:7340", false, false, false},
		{"[::1]:7340", false, false, false},
		{"localhost:7340", false, false, false},
		{"192.168.1.10:7340", false, false, false},
		{"10.0.0.1:7340", false, false, false},
		{"172.16.5.4:7340", false, false, false},
		{"100.64.0.1:7340", false, false, false},      // CGNAT / Tailscale
		{"100.127.255.254:7340", false, false, false}, // CGNAT top
		{"100.128.0.1:7340", false, true, false},      // just outside CGNAT
		{"[fd7a:115c:a1e0::1]:7340", false, false, false},
		{"[fe80::1]:7340", false, false, false},
		{"169.254.1.1:7340", false, false, false},
		{"desk.tailnet.ts.net:7340", false, false, false},
		{"8.8.8.8:7340", false, true, false},
		{"[2001:4860:4860::8888]:7340", false, true, false},
		{"example.com:7340", false, true, false},
		{"8.8.8.8:7340", true, false, true},
		{"0.0.0.0:7340", false, false, true},
		{"[::]:7340", false, false, true},
		{":7340", false, false, true},
		{"no-port", false, true, false},
	}
	for _, tc := range cases {
		warns, err := CheckListen([]string{tc.addr}, tc.allow, resolve)
		if (err != nil) != tc.wantErr || (len(warns) > 0) != tc.warn {
			t.Errorf("%s allow=%v: err=%v warns=%v", tc.addr, tc.allow, err, warns)
		}
	}
}

func TestLastRotation(t *testing.T) {
	l, err := audit.Open(t.TempDir(), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !LastRotation(l).IsZero() || !LastRotation(nil).IsZero() {
		t.Fatal("empty log has a rotation")
	}
	t1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_ = l.Log(audit.Record{Time: t1, Actor: "cli", Route: RotationEvent})
	_ = l.Log(audit.Record{Time: t2, Actor: "owner:admin", Method: "POST", Route: RotationRoute, Status: 500})
	if got := LastRotation(l); !got.Equal(t1) {
		t.Fatalf("got %v (a failed rotation must not count)", got)
	}
	_ = l.Log(audit.Record{Time: t2, Actor: "owner:admin", Method: "POST", Route: RotationRoute, Status: 200})
	if got := LastRotation(l); !got.Equal(t2) {
		t.Fatalf("got %v", got)
	}
}

// TestTokenCreatedBeatsOlderRotation: a fresh Server.TokenCreated wins over
// an older audited rotation (e.g. a rotation made while the log was moved).
func TestTokenCreatedBeatsOlderRotation(t *testing.T) {
	f := newFixture(t)
	f.rotated = f.now.Add(-400 * 24 * time.Hour)
	f.cfg.Server.TokenCreated = f.now.Add(-10 * 24 * time.Hour)
	c := f.run()["token_age"]
	if c.Status != OK || !strings.Contains(c.Detail, "created 10 days") {
		t.Fatalf("token_age = %+v", c)
	}
}

// TestTokenAgeSkipsAuditScan: with Server.TokenCreated set, the doctor does
// not call TokenRotated (which decodes the audit log).
func TestTokenAgeSkipsAuditScan(t *testing.T) {
	f := newFixture(t)
	f.cfg.Server.TokenCreated = f.now.Add(-10 * 24 * time.Hour)
	e := f.envFor()
	called := false
	e.TokenRotated = func() time.Time { called = true; return time.Time{} }
	c := tokenAgeCheck(e)
	if called {
		t.Fatal("TokenRotated read although TokenCreated is set")
	}
	if !strings.Contains(c.Detail, "created 10 days") {
		t.Fatalf("token_age = %+v", c)
	}
}

// TestBoundSafe: the address actually bound is judged, closing the gap of a
// listen name that did not resolve when CheckListen ran.
func TestBoundSafe(t *testing.T) {
	for _, c := range []struct {
		ip     string
		allow  bool
		refuse bool
	}{
		{"127.0.0.1", false, false},
		{"192.168.1.5", false, false},
		{"100.64.0.9", false, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"203.0.113.7", false, true},
		{"2001:db8::1", false, true},
		{"203.0.113.7", true, false},
	} {
		err := BoundSafe(&net.TCPAddr{IP: net.ParseIP(c.ip), Port: 7340}, c.allow)
		if (err != nil) != c.refuse {
			t.Errorf("BoundSafe(%s, allow=%v) = %v", c.ip, c.allow, err)
		}
	}
	w, err := CheckListen([]string{"box.example.net:7340"}, false, func(string) ([]net.IP, error) { return nil, errMissing })
	if err != nil || len(w) != 1 || !strings.Contains(w[0], "checked again") {
		t.Fatalf("unresolvable name: %v %v", w, err)
	}
}

func TestUsersChecks(t *testing.T) {
	// No accounts: users warns.
	f := newFixture(t)
	f.users = nil
	if c := f.run()["users"]; c.Status != Warn || !strings.Contains(c.Fix, "cerberus user add") {
		t.Fatalf("no users: %+v", c)
	}
	// Accounts and a plain-HTTP LAN address: users_tls fails.
	f = newFixture(t)
	f.cfg.Server.ExtraListen = []string{"192.168.1.5:7340"}
	if c := f.run()["users_tls"]; c.Status != Fail {
		t.Fatalf("users_tls: %+v", c)
	}
	// Lingering off.
	f = newFixture(t)
	_ = os.Remove(filepath.Join(f.linger, "carino"))
	if c := f.run()["linger"]; c.Status != Warn || !strings.Contains(c.Fix, "enable-linger") {
		t.Fatalf("linger: %+v", c)
	}
	// A user account and no bwrap.
	f = newFixture(t)
	if _, err := f.users.Add("bob", "", users.RoleUser, "a long password"); err != nil {
		t.Fatal(err)
	}
	f.bwrap = errMissing
	if c := f.run()["users_sandbox"]; c.Status != Fail {
		t.Fatalf("users_sandbox: %+v", c)
	}
	// local_no_auth warns with users.
	f = newFixture(t)
	f.cfg.Server.LocalNoAuth = true
	if c := f.run()["local_no_auth"]; c.Status != Warn || !strings.Contains(c.Detail, "user accounts") {
		t.Fatalf("local_no_auth: %+v", c)
	}
	// ... and fails once a role-user account exists (budgets can be bypassed).
	if _, err := f.users.Add("bob", "", users.RoleUser, "a long password"); err != nil {
		t.Fatal(err)
	}
	if c := f.run()["local_no_auth"]; c.Status != Fail || !strings.Contains(c.Detail, "role-user") {
		t.Fatalf("local_no_auth with a user: %+v", c)
	}
	// modes covers users.json and logins.json.
	f = newFixture(t)
	_ = os.Chmod(f.users.Path(), 0o644)
	if c := f.run()["modes"]; c.Status != Fail || !strings.Contains(c.Detail, "users.json") {
		t.Fatalf("modes: %+v", c)
	}
}

func TestContainerChecks(t *testing.T) {
	// The image's shape: 0.0.0.0 inside, no unit, no slice, no linger, accounts.
	f := newFixture(t)
	f.env[containerEnv] = "1"
	f.cfg.Server.Listen = "0.0.0.0:7340"
	f.cfg.Guard.SystemdSlice = ""
	_ = os.RemoveAll(filepath.Join(f.dir, "userconfig"))
	_ = os.Remove(filepath.Join(f.linger, "carino"))
	got := f.run()
	for _, id := range []string{"listen", "systemd", "service", "linger"} {
		if c := got[id]; c.Status != OK || !strings.Contains(c.Detail, "container") {
			t.Errorf("%s in a container: %+v", id, c)
		}
	}
	for _, id := range []string{"tls", "users_tls"} {
		if c := got[id]; c.Status != Warn || !strings.Contains(c.Detail, "container") || !strings.Contains(c.Fix, "127.0.0.1") {
			t.Errorf("%s in a container: %+v", id, c)
		}
	}
	// A public address is still refused, container or not.
	f.cfg.Server.Listen = "8.8.8.8:7340"
	if c := f.run()["listen"]; c.Status != Fail {
		t.Errorf("public listen in a container: %+v", c)
	}
	// Outside a container the same config still warns and fails as before.
	f = newFixture(t)
	f.cfg.Server.Listen = "0.0.0.0:7340"
	f.cfg.Guard.SystemdSlice = ""
	got = f.run()
	if got["listen"].Status != Warn || got["systemd"].Status != Warn || got["users_tls"].Status != Fail {
		t.Errorf("outside a container: listen %v systemd %v users_tls %v", got["listen"].Status, got["systemd"].Status, got["users_tls"].Status)
	}
}

// TestPreviewPortsCheck: off, ok, overlapping the listen port, a bad
// range, a public host and no free port.
func TestPreviewPortsCheck(t *testing.T) {
	f := newFixture(t)
	if c := f.run()["preview_ports"]; c.Status != OK || !strings.Contains(c.Detail, "off") {
		t.Fatalf("off: %+v", c)
	}
	f.cfg.Server.PreviewPorts = "7350-7353"
	env := f.envFor()
	env.Bindable = func(addr string) bool { return !strings.HasSuffix(addr, ":7350") }
	c := Run(context.Background(), env)
	var got Check
	for _, x := range c.Checks {
		if x.ID == "preview_ports" {
			got = x
		}
	}
	if got.Status != OK || !strings.Contains(got.Detail, "3 of 4 free") {
		t.Fatalf("ok: %+v", got)
	}
	env.Bindable = func(string) bool { return false }
	if got := previewPortsCheck(Defaults(env)); got.Status != Warn {
		t.Fatalf("none free: %+v", got)
	}
	f.cfg.Server.PreviewPorts = "7330-7349"
	if got := previewPortsCheck(Defaults(f.envFor())); got.Status != Fail || !strings.Contains(got.Detail, "7340") {
		t.Fatalf("overlap: %+v", got)
	}
	f.cfg.Server.PreviewPorts = "80-90"
	if got := previewPortsCheck(Defaults(f.envFor())); got.Status != Fail {
		t.Fatalf("bad range: %+v", got)
	}
	f.cfg.Server.PreviewPorts = "7350-7351"
	f.cfg.Server.ExtraListen = []string{"8.8.8.8:7340"}
	env = f.envFor()
	env.Bindable = func(string) bool { return true }
	if got := previewPortsCheck(Defaults(env)); got.Status != Fail {
		t.Fatalf("public: %+v", got)
	}
}

func TestRecordsCheck(t *testing.T) {
	cases := []struct {
		on     bool
		text   string
		listen []string
		want   Status
	}{
		{false, "all", []string{"0.0.0.0:7340"}, OK},
		{true, "none", []string{"0.0.0.0:7340"}, OK},
		{true, "all", []string{"127.0.0.1:7340"}, OK},
		{true, "user", []string{"127.0.0.1:7340", "100.64.0.1:7340"}, Warn},
		{true, "all", []string{":7340"}, Warn},
	}
	for _, c := range cases {
		cfg := config.Defaults()
		cfg.Gateway.Records.Enabled, cfg.Gateway.Records.Text = c.on, c.text
		cfg.Server.Listen, cfg.Server.ExtraListen = c.listen[0], c.listen[1:]
		if got := recordsCheck(Env{Cfg: cfg}); got.Status != c.want {
			t.Errorf("%+v: %s (%s)", c, got.Status, got.Detail)
		}
	}
}

func TestAgentGuardCheck(t *testing.T) {
	for _, c := range []struct {
		mode, model string
		want        Status
	}{
		{"", "", OK}, {"off", "", OK}, {"hold", "", Fail}, {"watch", "qwen3guard", OK}, {"shout", "x", Fail},
	} {
		cfg := config.Defaults()
		cfg.Gateway.Guard.Mode, cfg.Gateway.Guard.Model = c.mode, c.model
		if got := agentGuardCheck(Env{Cfg: cfg}); got.Status != c.want {
			t.Errorf("%+v: %s (%s)", c, got.Status, got.Detail)
		}
	}
}
