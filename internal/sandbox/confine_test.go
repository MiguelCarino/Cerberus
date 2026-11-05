package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func TestConfine(t *testing.T) {
	home := t.TempDir()
	statFn = func(string) (os.FileInfo, error) { return os.Stat(home) }
	defer func() { statFn = os.Stat }()
	cfg := config.SandboxConfig{ReadOnlyHome: false, ExtraRW: []string{"/global/rw"}, ExtraRO: []string{"/global/ro"}}
	p := config.AgentProfile{SandboxRW: []string{"/profile/rw"}}
	s := FromConfig(cfg, p, home).Confine(p)
	if !s.Confined || !s.ReadOnlyHome || !slices.Equal(s.ExtraRW, []string{"/profile/rw"}) || !slices.Equal(s.ExtraRO, []string{"/global/ro"}) {
		t.Fatalf("spec %+v", s)
	}
	args := Args("bwrap", s)
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && (args[i+1] == home || args[i+1] == "/global/rw") {
			t.Fatalf("confined spec binds %s writable: %v", args[i+1], args)
		}
	}
	// Confined wins over a writable home even if ReadOnlyHome was cleared.
	s.ReadOnlyHome = false
	for i, a := range Args("bwrap", s) {
		if a == "--bind" && Args("bwrap", s)[i+1] == home {
			t.Fatal("home writable in a confined spec")
		}
	}
}

// A confined spec hides the whole home (other repos, ~/.config/gh, ~/.aws)
// and binds back only the extras, the kept paths and the workspace.
func TestConfinedHidesHome(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"Github/OtherRepo", ".config/gh", "ws", "go/bin", "bin"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	agentBin := filepath.Join(home, "bin", "agent")
	if err := os.WriteFile(agentBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.SandboxConfig{Network: true, ExtraRO: []string{"~/go/bin"}}
	s := FromConfig(cfg, config.AgentProfile{}, home).Confine(config.AgentProfile{})
	s.Workspace = filepath.Join(home, "ws")
	argv := Wrap("bwrap", s, []string{agentBin, "run"})
	idx := func(seq ...string) int {
		for i := 0; i+len(seq) <= len(argv); i++ {
			if slices.Equal(argv[i:i+len(seq)], seq) {
				return i
			}
		}
		return -1
	}
	hide := idx("--tmpfs", home)
	if hide < 0 {
		t.Fatalf("home not hidden: %v", argv)
	}
	for _, seq := range [][]string{
		{"--ro-bind", filepath.Join(home, "go/bin"), filepath.Join(home, "go/bin")},
		{"--ro-bind", agentBin, agentBin},
		{"--bind", s.Workspace, s.Workspace},
	} {
		if i := idx(seq...); i < hide {
			t.Fatalf("%v missing or before the home tmpfs: %v", seq, argv)
		}
	}
	// Not confined: no home tmpfs.
	s.Confined = false
	if i := slices.Index(Wrap("bwrap", s, []string{agentBin}), home); i >= 0 && Wrap("bwrap", s, []string{agentBin})[i-1] == "--tmpfs" {
		t.Fatal("unconfined spec hides the home")
	}
}

// With a real bwrap: a confined command cannot read the home, can read its
// workspace and the kept paths.
func TestConfinedHidesHomeBwrap(t *testing.T) {
	bin, err := Available(context.Background())
	if err != nil {
		t.Skip(err)
	}
	// Outside /tmp, which every sandbox replaces with a tmpfs anyway.
	home, err := os.MkdirTemp("/var/tmp", "cerberus-confine-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	for _, d := range []string{".config/gh", "ws"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(home, ".config/gh/hosts.yml")
	_ = os.WriteFile(secret, []byte("token"), 0o600)
	ws := filepath.Join(home, "ws")
	_ = os.WriteFile(filepath.Join(ws, "mine"), []byte("ok"), 0o600)
	s := Spec{Home: home, Workspace: ws, Network: true, Confined: true, ReadOnlyHome: true}
	argv := Wrap(bin, s, []string{"/bin/sh", "-c", "cat " + filepath.Join(ws, "mine") + " && ! cat " + secret + " 2>/dev/null"})
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil || string(out) != "ok" {
		t.Fatalf("confined run: %v %q (argv %v)", err, out, argv)
	}
	// The same command unconfined reads the file (the check is real).
	s.Confined = false
	argv = Wrap(bin, s, []string{"/bin/sh", "-c", "! cat " + secret})
	if err := exec.Command(argv[0], argv[1:]...).Run(); err == nil {
		t.Fatal("unconfined sandbox could not read the home either: the test checks nothing")
	}
}
