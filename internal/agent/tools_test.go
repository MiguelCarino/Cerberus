package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func newTestBox(t *testing.T) *toolbox {
	t.Helper()
	return newToolbox(t.TempDir(), []string{"**/.ssh/**", "**/secrets.json", "**/Carino-Systems-Business/**"}, 30*time.Second)
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEditFileExactOnce(t *testing.T) {
	tb := newTestBox(t)
	p := filepath.Join(tb.cwd, "f.txt")
	cases := []struct {
		name, start string
		a           args
		wantErr     string
		wantText    string
	}{
		{"once", "a b c", args{"path": "f.txt", "old_string": "b", "new_string": "B"}, "", "a B c"},
		{"missing", "a b c", args{"path": "f.txt", "old_string": "z", "new_string": "Z"}, "is not in", "a b c"},
		{"twice", "x x", args{"path": "f.txt", "old_string": "x", "new_string": "y"}, "2 times", "x x"},
		{"replace all", "x x", args{"path": "f.txt", "old_string": "x", "new_string": "y", "replace_all": true}, "", "y y"},
		{"empty old", "x", args{"path": "f.txt", "old_string": "", "new_string": "y"}, "old_string is empty", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			write(t, p, c.start)
			out, bad := tb.run(context.Background(), "edit_file", c.a)
			if c.wantErr != "" && (!bad || !strings.Contains(out, c.wantErr)) {
				t.Errorf("got %q (bad=%v), want error %q", out, bad, c.wantErr)
			}
			if c.wantErr == "" && bad {
				t.Errorf("unexpected error %q", out)
			}
			if got, _ := os.ReadFile(p); string(got) != c.wantText {
				t.Errorf("file = %q, want %q", got, c.wantText)
			}
		})
	}
	write(t, p, "y y")
	if out, _ := tb.run(context.Background(), "edit_file", args{"path": "f.txt", "old_string": "y", "new_string": "z", "replace_all": true}); out != "edited "+p+": 2 replacements" {
		t.Errorf("replace_all message %q", out)
	}
}

func TestEditKeepsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	tb := newTestBox(t)
	p := filepath.Join(tb.cwd, "run.sh")
	write(t, p, "echo a")
	os.Chmod(p, 0o755)
	if out, bad := tb.run(context.Background(), "edit_file", args{"path": "run.sh", "old_string": "a", "new_string": "b"}); bad {
		t.Fatal(out)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
}

func TestDenyGlobs(t *testing.T) {
	d := newDenyList([]string{"**/.ssh/**", "**/secrets.json", "/etc/shadow", "*.pem", "/srv/**/private/*.key"})
	cases := []struct {
		path string
		deny bool
	}{
		{"/home/u/.ssh/id_rsa", true},
		{"/home/u/.ssh", true},
		{"/home/u/.sshx/a", false},
		{"/x/config/secrets.json", true},
		{"/secrets.json", true},
		{"/x/secrets.json.bak", false},
		{"/etc/shadow", true},
		{"/etc/shadow2", false},
		{"/a/b/cert.pem", true},
		{"/srv/a/b/private/k.key", true},
		{"/srv/private/k.key", true},
		{"/srv/a/private/sub/k.key", false},
		{"/home/u/project/main.go", false},
	}
	for _, c := range cases {
		if got := d.match(c.path) != ""; got != c.deny {
			t.Errorf("%s: denied=%v, want %v", c.path, got, c.deny)
		}
	}
}

func TestPathRules(t *testing.T) {
	tb := newTestBox(t)
	ctx := context.Background()
	other := t.TempDir()
	write(t, filepath.Join(other, "readme.txt"), "outside\n")
	write(t, filepath.Join(other, ".ssh", "id"), "key\n")

	if out, bad := tb.run(ctx, "read_file", args{"path": filepath.Join(other, "readme.txt")}); bad || !strings.Contains(out, "outside") {
		t.Errorf("read outside cwd should work: %q", out)
	}
	if out, bad := tb.run(ctx, "read_file", args{"path": filepath.Join(other, ".ssh", "id")}); !bad || !strings.Contains(out, "deny_paths") {
		t.Errorf("denied read: %q", out)
	}
	if out, bad := tb.run(ctx, "write_file", args{"path": filepath.Join(other, "new.txt"), "content": "x"}); !bad || !strings.Contains(out, "outside the working directory") {
		t.Errorf("write outside: %q", out)
	}
	if out, bad := tb.run(ctx, "write_file", args{"path": "../escape.txt", "content": "x"}); !bad {
		t.Errorf("relative escape written: %q", out)
	}
	if out, bad := tb.run(ctx, "write_file", args{"path": "conf/secrets.json", "content": "{}"}); !bad || !strings.Contains(out, "deny_paths") {
		t.Errorf("denied write: %q", out)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(other, filepath.Join(tb.cwd, "link")); err != nil {
			t.Fatal(err)
		}
		if out, bad := tb.run(ctx, "write_file", args{"path": "link/sneaky.txt", "content": "x"}); !bad {
			t.Errorf("write through symlink escaped: %q", out)
		}
		if _, err := os.Stat(filepath.Join(other, "sneaky.txt")); err == nil {
			t.Error("sneaky.txt exists")
		}
	}
	if out, bad := tb.run(ctx, "write_file", args{"path": "sub/dir/ok.txt", "content": "a\nb"}); bad || !strings.Contains(out, "created") || !strings.Contains(out, "2 lines") {
		t.Errorf("normal write: %q", out)
	}
	if out, _ := tb.run(ctx, "write_file", args{"path": "sub/dir/ok.txt", "content": "c"}); !strings.HasPrefix(out, "replaced") {
		t.Errorf("rewrite: %q", out)
	}
}

func TestReadFile(t *testing.T) {
	tb := newTestBox(t)
	ctx := context.Background()
	write(t, filepath.Join(tb.cwd, "f.txt"), "one\ntwo\nthree\n")
	write(t, filepath.Join(tb.cwd, "bin"), "a\x00b")
	write(t, filepath.Join(tb.cwd, "empty"), "")
	cases := []struct {
		a    args
		want string
		bad  bool
	}{
		{args{"path": "f.txt"}, "     1\tone\n     2\ttwo\n     3\tthree", false},
		{args{"path": "f.txt", "offset": 2.0, "limit": "1"}, "     2\ttwo", false},
		{args{"path": "f.txt", "offset": 9.0}, "(empty file)", false},
		{args{"path": "bin"}, "binary file", true},
		{args{"path": "empty"}, "(empty file)", false},
		{args{"path": "."}, "use list_dir", true},
		{args{"path": "nope"}, "there is no file", true},
	}
	for _, c := range cases {
		out, bad := tb.run(ctx, "read_file", c.a)
		if bad != c.bad || !strings.Contains(out, c.want) {
			t.Errorf("%v: got %q bad=%v", c.a, out, bad)
		}
	}
}

func TestListAndSearch(t *testing.T) {
	tb := newTestBox(t)
	ctx := context.Background()
	write(t, filepath.Join(tb.cwd, "b.go"), "package b\nfunc Hello() {}\n")
	write(t, filepath.Join(tb.cwd, "A.md"), "hello docs\n")
	write(t, filepath.Join(tb.cwd, "sub", "c.go"), "package sub // Hello\n")
	write(t, filepath.Join(tb.cwd, "node_modules", "x.go"), "Hello\n")
	write(t, filepath.Join(tb.cwd, ".hidden", "y.go"), "Hello\n")
	write(t, filepath.Join(tb.cwd, "secrets.json"), "Hello\n")

	out, _ := tb.run(ctx, "list_dir", args{})
	if !strings.HasSuffix(out, ":\n.hidden/\nnode_modules/\nsub/\nA.md\nb.go\nsecrets.json") {
		t.Errorf("list_dir: %q", out)
	}
	out, _ = tb.run(ctx, "search", args{"pattern": "Hello"})
	want := "b.go:2: func Hello() {}\n" + filepath.Join("sub", "c.go") + ":1: package sub // Hello"
	if out != want {
		t.Errorf("search pattern:\n%s\nwant:\n%s", out, want)
	}
	if out, _ = tb.run(ctx, "search", args{"glob": "*.md"}); out != "A.md" {
		t.Errorf("search glob: %q", out)
	}
	if out, bad := tb.run(ctx, "search", args{"pattern": "("}); !bad || !strings.Contains(out, "regular expression") {
		t.Errorf("bad regexp: %q", out)
	}
	if out, bad := tb.run(ctx, "search", args{}); !bad {
		t.Errorf("empty search: %q", out)
	}
}

func TestRunCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	tb := newTestBox(t)
	ctx := context.Background()
	if out, bad := tb.run(ctx, "run_command", args{"command": "pwd; echo err >&2; printf '\\033[31mred\\033[0m'"}); bad ||
		!strings.Contains(out, tb.cwd) || !strings.Contains(out, "err") || !strings.Contains(out, "red") || strings.Contains(out, "\x1b") {
		t.Errorf("output %q bad=%v", out, bad)
	}
	if out, bad := tb.run(ctx, "run_command", args{"command": "echo nope; exit 3"}); !bad || out != "nope\n[exit code 3]" {
		t.Errorf("exit code: %q", out)
	}
	if out, _ := tb.run(ctx, "run_command", args{"command": "read x; echo got:$x"}); out != "got:" {
		t.Errorf("stdin should be closed: %q", out)
	}
	if out, _ := tb.run(ctx, "run_command", args{"command": "true"}); out != "(no output)" {
		t.Errorf("no output: %q", out)
	}
	start := time.Now()
	out, bad := tb.run(ctx, "run_command", args{"command": "sleep 30 & sleep 30", "timeout": 1.0})
	if !bad || !strings.Contains(out, "stopped after 1 s (timeout)") {
		t.Errorf("timeout: %q", out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("timeout took %s", d)
	}
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if out, _ := tb.run(cctx, "run_command", args{"command": "sleep 30"}); !strings.Contains(out, "stopped by the user") {
		t.Errorf("cancel: %q", out)
	}
}

func TestCapture(t *testing.T) {
	c := &capture{max: 4}
	c.Write([]byte("abcdefgh"))
	if c.String() != "abcdefgh" {
		t.Errorf("exact fit %q", c.String())
	}
	c.Write([]byte("ijk"))
	if got := c.String(); got != "abcd\n[... output cut ...]\nhijk" {
		t.Errorf("overflow %q", got)
	}
}

func TestClip(t *testing.T) {
	if clip("short", 10) != "short" {
		t.Error("short text clipped")
	}
	got := clip(strings.Repeat("a", 20)+strings.Repeat("z", 10), 9)
	if !strings.HasPrefix(got, "aaaaaa\n[... 21 characters cut ...]\n") || !strings.HasSuffix(got, "zzz") {
		t.Errorf("clip %q", got)
	}
}

func TestThinkSplitter(t *testing.T) {
	var s thinkSplitter
	var reply, think strings.Builder
	for _, piece := range []string{"Hi <", "thi", "nk>deep", " thought</thi", "nk> there <", "b>"} {
		r, th := s.feed(piece)
		reply.WriteString(r)
		think.WriteString(th)
	}
	r, th := s.flush()
	reply.WriteString(r)
	think.WriteString(th)
	if reply.String() != "Hi  there <b>" || think.String() != "deep thought" {
		t.Errorf("reply %q think %q", reply.String(), think.String())
	}
}

func TestLineReader(t *testing.T) {
	l := newLineReader(strings.NewReader("y\r\n\rabc\nlast"))
	ctx := context.Background()
	var got []string
	for {
		line, err := l.next(ctx)
		if err != nil {
			break
		}
		got = append(got, line)
	}
	want := []string{"y", "", "abc", "last"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines %q, want %q", got, want)
	}
}

func TestNoToolSupport(t *testing.T) {
	cases := []struct {
		e    ServerError
		want bool
	}{
		{ServerError{400, "tools are not supported"}, true},
		{ServerError{500, "template needs --jinja"}, true},
		{ServerError{0, "function calling unavailable"}, true},
		{ServerError{422, "unprocessable"}, true},
		{ServerError{400, "context length exceeded"}, false},
		{ServerError{401, "tools"}, false},
		{ServerError{429, "slow down"}, false},
		{ServerError{0, "connection refused"}, false},
		{ServerError{503, "busy"}, false},
		// the model is missing: tools are not the problem
		{ServerError{400, "No models loaded. Please load a model in the developer page or use the 'lms load' command."}, false},
		{ServerError{404, "unknown model: default (set engines.default_model, or pick a model)"}, false},
		{ServerError{404, "model 'llama3.2:3b' not found"}, false},
		{ServerError{400, "this model does not support tools"}, true},
	}
	for _, c := range cases {
		if got := noToolSupport(&c.e); got != c.want {
			t.Errorf("%+v: %v", c.e, got)
		}
	}
}

func TestSearchSkipsDeniedSymlinkTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	tb := newTestBox(t)
	secret := filepath.Join(t.TempDir(), ".ssh", "id_ed25519")
	write(t, secret, "PRIVATE KEY MATERIAL\n")
	write(t, filepath.Join(tb.cwd, "ok.txt"), "PRIVATE but harmless\n")
	if err := os.Symlink(secret, filepath.Join(tb.cwd, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	out, bad := tb.run(context.Background(), "search", args{"pattern": "PRIVATE"})
	if bad || strings.Contains(out, "KEY MATERIAL") || !strings.Contains(out, "ok.txt") {
		t.Fatalf("search through a denied symlink: %q (bad=%v)", out, bad)
	}
}

func TestDenyCoversOriginRepo(t *testing.T) {
	tb := newTestBox(t)
	tb.origin = "/home/u/Github/Carino-Systems-Business"
	write(t, filepath.Join(tb.cwd, "plan.md"), "secret plan\n")
	if out, bad := tb.run(context.Background(), "read_file", args{"path": "plan.md"}); !bad || !strings.Contains(out, "deny_paths") {
		t.Fatalf("read in a worktree of a denied repo: %q (bad=%v)", out, bad)
	}
	if out, _ := tb.run(context.Background(), "search", args{"pattern": "secret"}); strings.Contains(out, "secret plan") {
		t.Fatalf("search in a worktree of a denied repo: %q", out)
	}
	tb.origin = "/home/u/Github/Fine"
	if out, bad := tb.run(context.Background(), "read_file", args{"path": "plan.md"}); bad {
		t.Fatalf("allowed repo refused: %q", out)
	}
}

func TestCommandEnvDropsCredentials(t *testing.T) {
	got := commandEnv([]string{"PATH=/bin", "OPENAI_API_KEY=k", "CERBERUS_URL=u", "CERBERUS_SESSION=s", "HOME=/h"})
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h" {
		t.Fatalf("env = %v", got)
	}
}

func TestConfigDirAlwaysDenied(t *testing.T) {
	cfg := config.Defaults()
	cfg.SetPath("/home/u/.config/cerberus/config.json")
	cfg.Agent.DenyPaths = nil
	if DeniedBy(configDeny(cfg), "/home/u/.config/cerberus/config.json") == "" {
		t.Fatal("config.json readable by the agent")
	}
}
