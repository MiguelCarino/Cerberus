package permits

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Saved rules ("Always allow") outlive the chat or run that made them and
// apply in every folder, so they are held to tighter limits than session
// rules: Suggest offers only rules Validate accepts, and Validate refuses
// anything that would say yes to too much (docs/SCOPE.md "As built: saved
// permissions").

// riskyHeads are commands no "prefix:*" rule may name: they delete, move or
// overwrite, run other programs or code, reach the network, change the
// system or its processes, or raise privileges. An exact command of most of
// them may still be saved (it covers that one command only).
var riskyHeads = map[string]bool{}

func init() {
	for _, s := range strings.Fields(`
		rm rmdir mv cp ln install dd chmod chown chgrp chattr setfacl truncate shred unlink tee sed awk gawk mawk tar unzip
		sudo su doas pkexec sudoedit runuser
		kill pkill killall shutdown reboot poweroff halt init telinit
		fdisk sfdisk parted wipefs mount umount losetup swapoff swapon cryptsetup
		curl wget ssh scp sftp rsync nc ncat netcat socat telnet ftp
		bash sh zsh fish dash ksh csh tcsh busybox
		node deno perl ruby irb php lua luajit java Rscript R julia tclsh expect osascript powershell pwsh cmd
		eval exec env xargs find fd rg parallel watch nohup setsid timeout nice ionice time strace ltrace gdb
		chroot nsenter unshare bwrap flock command builtin source .
		crontab at batch systemctl service journalctl launchctl loginctl
		iptables ip6tables nft ufw firewall-cmd ip
		npx pnpx bunx uvx pipx dnf yum apt apt-get dpkg rpm pacman zypper brew flatpak snap gh
		open xdg-open`) {
		riskyHeads[s] = true
	}
}

// subTools take a subcommand: their prefix rules name it ("go test:*"),
// never the bare tool, and the listed subcommands are refused.
var subTools = map[string]map[string]bool{
	"git": words(`push reset clean checkout rebase restore switch rm mv filter-branch filter-repo gc prune reflog
		update-ref stash branch tag config remote submodule worktree merge pull am apply cherry-pick revert commit
		notes replace lfs grep send-email daemon credential`),
	"go":        words(`env clean`),
	"npm":       words(`publish unpublish deprecate owner access adduser login logout token exec x dlx create init`),
	"pnpm":      words(`publish unpublish deprecate owner access adduser login logout token exec x dlx create init`),
	"yarn":      words(`publish unpublish deprecate owner access adduser login logout token exec x dlx create init`),
	"bun":       words(`publish x exec create init`),
	"cargo":     words(`publish yank owner login logout install uninstall`),
	"pip":       words(`uninstall`),
	"pip3":      words(`uninstall`),
	"uv":        words(`publish tool`),
	"poetry":    words(`publish config`),
	"docker":    containerSubs,
	"podman":    containerSubs,
	"kubectl":   containerSubs,
	"terraform": words(`apply destroy import state taint untaint workspace force-unlock login logout refresh`),
	"helm":      words(`install uninstall upgrade rollback delete push repo registry plugin`),
	"dotnet":    words(`nuget tool`),
	"mvn":       words(`deploy release`),
	"gradle":    words(`publish`),
	"rustup":    words(`self toolchain uninstall`),
}

var containerSubs = words(`rm rmi kill stop restart system volume network prune exec run cp compose container image push
	login logout delete apply create replace patch edit scale drain cordon taint label annotate rollout set attach
	debug port-forward proxy auth certificate`)

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

var (
	plainHead = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]*$`)
	plainSub  = regexp.MustCompile(`^[a-z0-9][a-z0-9:._-]*$`)
)

// risky reports whether head (a command's first word) may not lead a
// prefix rule.
func risky(head string) bool {
	base := strings.TrimSuffix(filepath.Base(head), ".exe")
	if riskyHeads[base] || riskyHeads[head] {
		return true
	}
	for _, p := range []string{"mkfs", "python", "pypy", "perl", "ruby", "php", "node", "lua"} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// privileged heads may not even be saved as an exact command.
var privileged = words(`sudo su doas pkexec sudoedit runuser`)

// prefixOf is the "prefix:*" rule content for command, "" when none is
// safe: no shell joins, a plain head that is not risky, and for tools with
// subcommands a plain subcommand that is not refused.
func prefixOf(command string) string {
	if ShellJoin(command) {
		return ""
	}
	f := strings.Fields(command)
	if len(f) == 0 || !plainHead.MatchString(f[0]) || risky(f[0]) {
		return ""
	}
	if subs, ok := subTools[f[0]]; ok {
		if len(f) < 2 || !plainSub.MatchString(f[1]) || subs[f[1]] {
			return ""
		}
		return f[0] + " " + f[1] + ":*"
	}
	return f[0] + ":*"
}

// URLTools take a url.
var URLTools = map[string]bool{"WebFetch": true}

// Suggest lists the rules "Always allow" may save for a call of tool with
// input (relative paths are cwd's), narrowest first; every one passes
// Validate. A Bash command gets itself and, when safe, a prefix rule; a
// file tool its folder; a URL tool its host; another tool the tool.
func Suggest(tool string, input map[string]any, cwd string) []Rule {
	var out []Rule
	add := func(content string) {
		r := NewRule(tool, content)
		if Validate(r) != nil {
			return
		}
		for _, x := range out {
			if x.Content == r.Content {
				return
			}
		}
		out = append(out, r)
	}
	switch {
	case tool == "Bash":
		cmd := strings.TrimSpace(Str(input["command"]))
		if cmd == "" {
			return nil
		}
		add(cmd)
		if p := prefixOf(cmd); p != "" {
			add(p)
		}
	case FileTools[tool]:
		folder := ""
		if p := ToolPath(input); p != "" {
			if filepath.IsAbs(ExpandHome(p)) || cwd != "" {
				target := AbsPath(p, cwd)
				folder = filepath.Dir(target)
				if st, err := os.Stat(target); tool == "Glob" || tool == "Grep" || (err == nil && st.IsDir()) {
					folder = target
				}
			}
		} else if tool == "Glob" || tool == "Grep" || tool == "LS" {
			folder = cwd
		}
		if folder != "" && filepath.IsAbs(folder) {
			add("dir:" + folder)
		}
	case URLTools[tool] || Str(input["url"]) != "":
		if u, err := urlHost(Str(input["url"])); err == nil {
			add("domain:" + u)
		}
	default:
		add("")
	}
	return out
}

// Validate says why a rule may not be saved (nil when it may).
func Validate(r Rule) error {
	tool, content := strings.TrimSpace(r.Tool), r.Content
	switch {
	case tool == "" || tool != r.Tool:
		return errors.New("name the tool (Bash, Read, Edit, Write, WebFetch, …)")
	case len(tool) > 64 || strings.ContainsFunc(tool, badRune) || strings.ContainsAny(tool, " ()"):
		return errors.New("that is not a tool name")
	case len(content) > 1000:
		return errors.New("the rule is too long (at most 1000 characters)")
	case strings.ContainsFunc(content, func(c rune) bool { return badRune(c) && c != '\t' }):
		return errors.New("the rule may not hold control characters")
	}
	if tool == "Bash" {
		return validateBash(content)
	}
	if FileTools[tool] {
		return validateFile(tool, content)
	}
	if want, ok := strings.CutPrefix(content, "domain:"); ok {
		if want == "" || strings.ContainsAny(want, "/:@ *") || strings.HasPrefix(want, ".") || strings.HasSuffix(want, ".") {
			return errors.New("domain: needs a host name, such as domain:docs.python.org")
		}
	}
	return nil
}

func badRune(c rune) bool { return c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) }

func validateBash(content string) error {
	c := strings.TrimSpace(content)
	if c == "" {
		return errors.New("a Bash rule needs a command: running any command without asking is never saved")
	}
	if ShellJoin(c) {
		return errors.New("a saved command may not join, pipe or redirect (; & | < > ` $( or a line break)")
	}
	prefix, isPrefix := strings.CutSuffix(c, ":*")
	if !isPrefix {
		if f := strings.Fields(c); privileged[filepath.Base(f[0])] {
			return fmt.Errorf("commands run with %s are never saved", f[0])
		}
		return nil
	}
	f := strings.Fields(prefix)
	if len(f) == 0 {
		return errors.New("a prefix rule needs a command before :*")
	}
	if !plainHead.MatchString(f[0]) {
		return errors.New("a prefix rule must start with a plain command name")
	}
	if risky(f[0]) {
		return fmt.Errorf("%q can delete, overwrite, run other programs or reach the network: save exact commands of it, not a prefix", f[0])
	}
	if subs, ok := subTools[f[0]]; ok {
		if len(f) < 2 || !plainSub.MatchString(f[1]) {
			return fmt.Errorf("a prefix rule for %s must name its subcommand, such as %s status:*", f[0], f[0])
		}
		if subs[f[1]] {
			return fmt.Errorf("%s %s can change or lose work: save exact commands of it, not a prefix", f[0], f[1])
		}
	}
	return nil
}

// writeTools change files: never tool-wide.
var writeTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

func validateFile(tool, content string) error {
	if content == "" {
		if writeTools[tool] {
			return fmt.Errorf("%s needs a folder or a path (dir:~/notes): writing anywhere is never saved", tool)
		}
		return nil
	}
	p := content
	if d, ok := strings.CutPrefix(content, "dir:"); ok {
		p = d
	} else if s, ok := strings.CutPrefix(content, "//"); ok {
		p = "/" + s
	} else if !strings.HasPrefix(content, "~/") {
		return errors.New("a saved path must be absolute: dir:/abs/folder, dir:~/folder, //abs/** or ~/path")
	}
	full := ExpandHome(strings.TrimSuffix(p, "/**"))
	if !filepath.IsAbs(full) {
		return errors.New("a saved folder must be absolute (dir:/abs/folder or dir:~/folder)")
	}
	full = filepath.Clean(full)
	if full == string(filepath.Separator) || filepath.Dir(full) == full {
		return errors.New("the whole disk is too wide: name a folder")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" && full == filepath.Clean(h) {
		return errors.New("the whole home folder is too wide: name a folder in it")
	}
	return nil
}

func urlHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	h := strings.ToLower(u.Hostname())
	if h == "" {
		return "", errors.New("no host")
	}
	return h, nil
}
