package sandbox

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func TestResolveOrder(t *testing.T) {
	cases := []struct {
		def, prof, repo, want string
	}{
		{"", "", "", ModeNone},
		{"none", "", "", ModeNone},
		{"bwrap", "", "", ModeBwrap},
		{"none", "bwrap", "", ModeBwrap},
		{"bwrap", "none", "", ModeNone},
		{"none", "none", "bwrap", ModeBwrap},
		{"bwrap", "bwrap", "none", ModeNone},
		{"none", "", "off", ModeNone},
		{"none", "bubblewrap", "", ModeBwrap}, // unknown words fail closed
	}
	for _, c := range cases {
		got := Resolve(config.SandboxConfig{Default: c.def}, config.AgentProfile{Sandbox: c.prof}, c.repo)
		if got != c.want {
			t.Errorf("Resolve(%q,%q,%q) = %q, want %q", c.def, c.prof, c.repo, got, c.want)
		}
	}
}

func TestExpand(t *testing.T) {
	for in, want := range map[string]string{
		"~": "/h", "~/.ssh": "/h/.ssh", "~/a/../b/": "/h/b", "/x//y": "/x/y", "rel/x": "", "~bob/x": "", "": "",
	} {
		if got := Expand(in, "/h"); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeStat makes every listed path exist (files end in ".json").
func fakeStat(t *testing.T, paths ...string) {
	t.Helper()
	old := statFn
	t.Cleanup(func() { statFn = old })
	statFn = func(p string) (os.FileInfo, error) {
		if !slices.Contains(paths, p) {
			return nil, fs.ErrNotExist
		}
		return fakeInfo{dir: !strings.HasSuffix(p, ".json")}, nil
	}
}

type fakeInfo struct {
	os.FileInfo
	dir bool
}

func (f fakeInfo) IsDir() bool { return f.dir }

func TestArgsTable(t *testing.T) {
	fakeStat(t, "/h", "/h/.ssh", "/h/.claude", "/h/.claude.json", "/h/ws", "/d/sessions/s1", "/d", "/opt/ro",
		"/d/sessions/s1/agent-config.json", "/run/user/1000")
	base := Spec{Home: "/h", Workspace: "/h/ws", SessionDir: "/d/sessions/s1", Network: true, ReadOnlyHome: true}
	head := []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	tail := []string{"--unshare-pid", "--die-with-parent", "--new-session", "--chdir", "/h/ws", "--"}
	binds := []string{"--bind", "/h/ws", "/h/ws", "--bind", "/d/sessions/s1", "/d/sessions/s1"}
	join := func(parts ...[]string) []string { return slices.Concat(parts...) }

	cases := []struct {
		name string
		mod  func(*Spec)
		want []string
	}{
		{"minimal", func(*Spec) {}, join(head, binds, tail)},
		{"no network", func(s *Spec) { s.Network = false },
			join(head, binds, []string{"--unshare-net"}, tail)},
		{"writable home", func(s *Spec) { s.ReadOnlyHome = false },
			join(head, []string{"--bind", "/h", "/h"}, binds, tail)},
		{"hide home, missing skipped, file masked", func(s *Spec) {
			s.HideHome = []string{"~/.ssh", "~/.gnupg", "~/.claude.json", "relative"}
		}, join(head, []string{"--tmpfs", "/h/.ssh", "--ro-bind", os.DevNull, "/h/.claude.json"}, binds, tail)},
		{"extras with ~ expansion", func(s *Spec) {
			s.ExtraRO = []string{"/opt/ro", "/missing"}
			s.ExtraRW = []string{"~/.claude", "~/.claude.json", "/"}
		}, join(head, []string{"--ro-bind", "/opt/ro", "/opt/ro", "--bind", "/h/.claude", "/h/.claude",
			"--bind", "/h/.claude.json", "/h/.claude.json"}, binds, tail)},
		{"hide data and runtime, then bind own dirs, then protect", func(s *Spec) {
			s.Hide = []string{"/d", "/run/user/1000", "/d"}
			s.Protect = []string{"/d/sessions/s1/agent-config.json"}
		}, join(head, []string{"--tmpfs", "/d", "--tmpfs", "/run/user/1000"}, binds,
			[]string{"--ro-bind", "/d/sessions/s1/agent-config.json", "/d/sessions/s1/agent-config.json"}, tail)},
	}
	for _, c := range cases {
		s := base
		c.mod(&s)
		got := Args("bwrap", s)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
	w := Wrap("bwrap", base, []string{"claude", "fix it"})
	if w[len(w)-3] != "--" || w[len(w)-1] != "fix it" {
		t.Errorf("Wrap tail %q", w[len(w)-3:])
	}
}

func TestFromConfig(t *testing.T) {
	cfg := config.Defaults().Sandbox
	cfg.ExtraRW = []string{"/srv"}
	s := FromConfig(cfg, config.AgentProfile{SandboxRW: []string{"~/.claude"}}, "/h")
	if !s.Network || !s.ReadOnlyHome || !slices.Equal(s.ExtraRW, []string{"/srv", "~/.claude"}) || !slices.Contains(s.HideHome, "~/.ssh") {
		t.Errorf("spec %+v", s)
	}
	for _, name := range []string{"claude", "claude-headless"} {
		p, _ := config.Defaults().AgentProfile(name)
		if !slices.Contains(p.SandboxRW, "~/.claude") || !slices.Contains(p.SandboxRW, "~/.claude.json") {
			t.Errorf("%s profile lacks ~/.claude binds: %v", name, p.SandboxRW)
		}
	}
}

// TestRealBwrap runs a real sandbox over temp folders (never the real
// home): hidden folders are empty, the home is read-only, the workspace and
// session folder are writable, and the network is gone when asked.
func TestRealBwrap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Probe(ctx, bin); err != nil {
		t.Skipf("bwrap unusable here: %v", err)
	}
	// Not under /tmp: the sandbox mounts an empty tmpfs there, which would
	// make every check below pass for the wrong reason.
	wd, _ := os.Getwd()
	root, err := os.MkdirTemp(wd, ".bwraptest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if strings.HasPrefix(root, "/tmp/") {
		t.Skip("the package folder is under /tmp")
	}
	home := filepath.Join(root, "home")
	ws := filepath.Join(home, "proj")
	sess := filepath.Join(root, "data", "sessions", "s1")
	for _, d := range []string{filepath.Join(home, ".ssh"), ws, sess, filepath.Join(home, ".claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	must := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(home, ".ssh", "id_ed25519"), "PRIVATE")
	must(filepath.Join(sess, "agent-config.json"), "{}")
	must(filepath.Join(root, "data", "other-secret"), "OTHER")
	s := Spec{Home: home, Workspace: ws, SessionDir: sess, Network: false, ReadOnlyHome: true,
		HideHome: []string{"~/.ssh"}, ExtraRW: []string{"~/.claude"}, Hide: []string{filepath.Join(root, "data")},
		Protect: []string{filepath.Join(sess, "agent-config.json")}}
	script := `
set -u
H=` + home + `
ls -A "$H/.ssh" | wc -l | tr -d ' ' | sed 's/^/ssh=/'
cat "` + filepath.Join(root, "data", "other-secret") + `" 2>/dev/null || echo data=hidden
echo w > "$H/escape" 2>/dev/null && echo home=writable || echo home=ro
echo w > "$PWD/ok" && echo ws=writable
echo w > "` + sess + `/c.json" && echo sess=writable
echo w > "$H/.claude/x" && echo claude=writable
echo w >> "` + filepath.Join(sess, "agent-config.json") + `" 2>/dev/null && echo protect=writable || echo protect=ro
echo w > /tmp/t && echo tmp=writable
[ "$(grep -c : /proc/net/dev)" -le 1 ] && echo net=no || echo net=yes
`
	cmd := exec.CommandContext(ctx, bin, append(Args(bin, s)[1:], "sh", "-c", script)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bwrap run: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"ssh=0", "data=hidden", "home=ro", "ws=writable", "sess=writable", "claude=writable",
		"protect=ro", "tmp=writable", "net=no"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "escape")); err == nil {
		t.Error("wrote outside the workspace")
	}
	if raw, _ := os.ReadFile(filepath.Join(sess, "agent-config.json")); string(raw) != "{}" {
		t.Error("protected file changed")
	}
	if _, err := os.Stat(filepath.Join(ws, "ok")); err != nil {
		t.Error("workspace write did not land on the host")
	}
}
