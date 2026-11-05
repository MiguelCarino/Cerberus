package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/packaging"
)

func TestVersionAndUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, nil, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "cerberus 0.0.0 (dev) ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	if code := run([]string{"nope"}, nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("unknown: %d %q", code, errb.String())
	}
	if code := run(nil, nil, &out, &errb); code != 2 {
		t.Fatalf("no args: %d", code)
	}
}

func TestLocalURL(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7340": "http://127.0.0.1:7340",
		"0.0.0.0:7340":   "http://127.0.0.1:7340",
		":7340":          "http://127.0.0.1:7340",
		"[::]:7340":      "http://127.0.0.1:7340",
		"[::1]:7340":     "http://[::1]:7340",
		"100.64.0.2:80":  "http://100.64.0.2:80",
	} {
		if got := localURL(in); got != want {
			t.Errorf("localURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPairingURLs(t *testing.T) {
	sc := config.ServerConfig{Listen: "127.0.0.1:7340", ExtraListen: []string{"0.0.0.0:7341", "bad"},
		Token: "a b", PublicURL: "http://desk.ts.net:7340/"}
	got := pairingURLs(sc, []string{"192.168.1.5", "fd7a::1"})
	want := []string{
		"http://desk.ts.net:7340/?token=a+b",
		"http://127.0.0.1:7340/?token=a+b",
		"http://127.0.0.1:7341/?token=a+b",
		"http://192.168.1.5:7341/?token=a+b",
		"http://[fd7a::1]:7341/?token=a+b",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pairingURLs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// With a certificate the extra addresses serve only HTTPS.
	sc = config.ServerConfig{Listen: "127.0.0.1:7340", ExtraListen: []string{"100.64.0.10:7340"},
		Token: "t", TLSCert: "c.pem", TLSKey: "k.pem"}
	got = pairingURLs(sc, nil)
	want = []string{"http://127.0.0.1:7340/?token=t", "https://100.64.0.10:7340/?token=t"}
	if !slices.Equal(got, want) {
		t.Fatalf("TLS pairingURLs = %v, want %v", got, want)
	}
}

// writeConfig writes a config pointing at srv and returns its path.
func writeConfig(t *testing.T, listen string) string {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Listen = listen
	cfg.Server.Token = "tok"
	p := filepath.Join(t.TempDir(), "config.json")
	cfg.SetPath(p)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatusAndTaskCommands(t *testing.T) {
	var gotSpec session.TaskSpec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"missing or invalid token"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/status":
			io.WriteString(w, `{"version":"1.2","host":"desk","uptime":3725,"gpu":{"GPUs":[{"Index":0,"Name":"RTX","PowerW":40,"PowerLimitW":300,"MemUsedMiB":100,"MemTotalMiB":24000}]},
				"models":[{"ID":"qwen","Backend":"llamacpp","Loaded":true},{"ID":"x/y"}],"sessions":{"running":1,"waiting":2},
				"guard":{"mem_available_mib":9000,"max_sessions":3,"running":3},"agents":[],"repos":[]}`)
		case "POST /api/sessions":
			_ = json.NewDecoder(r.Body).Decode(&gotSpec)
			io.WriteString(w, `[{"ID":"s1","State":"queued","Title":"fix"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfgPath := writeConfig(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errb bytes.Buffer
	if code := run([]string{"status", "-config", cfgPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("status: %d %s", code, errb.String())
	}
	for _, want := range []string{"Cerberus 1.2 on desk, up 1h2m5s", "GPU 0  RTX: 40 W of 300 W", "2 known, 1 loaded: qwen [llamacpp]",
		"running 1, waiting 2", "3 of 3 running, memory available 9000 MiB"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}

	repo := t.TempDir()
	out.Reset()
	code := run([]string{"task", "-config", cfgPath, "--repo", repo, "--agent", "shell", "--count", "2", "fix", "the", "bug"}, nil, &out, &errb)
	if code != 0 {
		t.Fatalf("task: %d %s", code, errb.String())
	}
	if gotSpec.Task != "fix the bug" || gotSpec.Agent != "shell" || gotSpec.Count != 2 || !filepath.IsAbs(gotSpec.Repo) {
		t.Fatalf("spec = %+v", gotSpec)
	}
	if !strings.Contains(out.String(), "s1") || !strings.Contains(out.String(), "/#/s/s1") {
		t.Fatalf("task output %q", out.String())
	}
	if code := run([]string{"task", "-config", cfgPath}, nil, &out, &errb); code != 2 {
		t.Fatalf("empty task: %d", code)
	}
}

func TestResolveWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	plain := t.TempDir()
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }

	cases := []struct {
		name    string
		cwd     string
		spec    session.TaskSpec
		scratch bool
		want    session.TaskSpec
	}{
		{"inside repo, no flag", sub, session.TaskSpec{}, false, session.TaskSpec{Workspace: "repo", Repo: real(repo)}},
		{"outside repo, no flag", plain, session.TaskSpec{}, false, session.TaskSpec{Workspace: "scratch"}},
		{"scratch flag in repo", sub, session.TaskSpec{}, true, session.TaskSpec{Workspace: "scratch"}},
		{"dir", sub, session.TaskSpec{Dir: "."}, false, session.TaskSpec{Dir: sub}},
		{"repo subfolder", plain, session.TaskSpec{Repo: sub}, false, session.TaskSpec{Workspace: "repo", Repo: real(repo)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Chdir(c.cwd)
			spec := c.spec
			if err := resolveWorkspace(&spec, c.scratch); err != nil {
				t.Fatal(err)
			}
			if spec.Repo != "" {
				spec.Repo = real(spec.Repo)
			}
			if spec.Workspace != c.want.Workspace || spec.Repo != c.want.Repo || spec.Dir != c.want.Dir {
				t.Fatalf("got %+v want %+v", spec, c.want)
			}
		})
	}
	spec := session.TaskSpec{Dir: plain}
	if err := resolveWorkspace(&spec, true); err == nil {
		t.Fatal("--dir with --scratch accepted")
	}
}

func TestClientErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"repo is not configured"}`)
	}))
	defer srv.Close()
	cfgPath := writeConfig(t, strings.TrimPrefix(srv.URL, "http://"))
	var out, errb bytes.Buffer
	if code := run([]string{"task", "-config", cfgPath, "--repo", t.TempDir(), "x"}, nil, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "repo is not configured") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "build", "cerberus")
	_ = os.MkdirAll(filepath.Dir(exe), 0o755)
	if err := os.WriteFile(exe, []byte("binary-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	active := true
	in := &installer{goos: "linux", exe: exe, home: filepath.Join(dir, "home"), configDir: filepath.Join(dir, "home", ".config"),
		out: io.Discard, systemctl: func(args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			if args[0] == "is-active" && !active {
				return os.ErrNotExist
			}
			return nil
		}}
	var audited []string
	in.audit = func(event, detail string, targets ...string) {
		audited = append(audited, event+" "+detail+" "+strings.Join(targets, ","))
	}
	if err := in.install(); err != nil {
		t.Fatal(err)
	}
	if len(audited) != 1 || !strings.HasPrefix(audited[0], "service.install installed") || !strings.Contains(audited[0], "cerberus.service") {
		t.Fatalf("audit = %q", audited)
	}
	dst := filepath.Join(dir, "home", ".local", "bin", "cerberus")
	if raw, _ := os.ReadFile(dst); string(raw) != "binary-v2" {
		t.Fatalf("binary = %q", raw)
	}
	if st, _ := os.Stat(dst); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", st.Mode())
	}
	unit, _ := os.ReadFile(filepath.Join(dir, "home", ".config", "systemd", "user", "cerberus.service"))
	if string(unit) != packaging.Unit {
		t.Fatal("unit not written")
	}
	want := []string{"daemon-reload", "is-active --quiet cerberus.service", "enable --now cerberus.service", "restart cerberus.service"}
	if !slices.Equal(calls, want) {
		t.Fatalf("systemctl calls = %q", calls)
	}

	// Running the installed binary again copies nothing and does not restart.
	calls = nil
	in.exe = dst
	if err := in.install(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(calls, "restart cerberus.service") {
		t.Fatalf("restarted without a new binary: %q", calls)
	}

	in.goos = "darwin"
	if err := in.install(); err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("darwin: %v", err)
	}
}
