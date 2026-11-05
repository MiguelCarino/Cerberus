package agent

// The run_command policy.
//
// THIS IS A GUARD RAIL, NOT A SANDBOX. It reads the command the way a
// careful person would (quotes, wrappers, nested shells, pipes into a
// shell) to catch the obviously destructive or irreversible commands a model
// may produce by mistake, and to make sure some commands are always shown
// to a person first. It cannot see what a script, a Makefile, an alias, a
// function, a Python one-liner or a downloaded binary does once it runs,
// and a determined adversary can always write a command it does not
// recognise (`find / -delete`, `python -c 'import shutil; …'`, writing a
// script and running it later). Confinement is the job of the session
// sandbox (internal/sandbox); the policy only lowers the odds of an
// accident and makes some actions visible.
//
// Rules (config Agent.Policy, prefix patterns):
//   - Deny wins over everything and is never askable.
//   - AskAlways is asked even in auto mode, and when "always" was answered.
//   - Allow runs without asking, only when every command in the line is
//     allowed and nothing is suspicious.
//   - Otherwise the approve mode decides.
//
// Built in, whatever the config says: a pipe into a shell (curl … | sh) is
// denied; command substitution ($(…), backticks, <(…)), brace expansion,
// a command name from a variable or glob, and lines that cannot be parsed
// are asked.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"carino.systems/cerberus/internal/config"
)

// PolicyAction is what the policy says about a command.
type PolicyAction int

const (
	PolicyDefault PolicyAction = iota // no rule: the approve mode decides
	PolicyAllow                       // run without asking
	PolicyAsk                         // ask, even in auto mode
	PolicyDeny                        // never run, never ask
)

func (a PolicyAction) String() string {
	return [...]string{"default", "allow", "ask", "deny"}[a]
}

// PolicyVerdict is the policy's answer with the rule that decided it.
type PolicyVerdict struct {
	Action PolicyAction
	Rule   string // the pattern or built-in rule
	Reason string // one line for the person and the model
}

// Policy is a parsed Agent.Policy. A nil *Policy applies the built-in rules
// only.
type Policy struct {
	allow, deny, ask []pattern
	home             string
}

// NewPolicy parses c.
func NewPolicy(c config.PolicyConfig) *Policy {
	p := &Policy{}
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		p.home = filepath.Clean(h)
	}
	for _, s := range c.Allow {
		if pt, ok := parsePattern(s); ok {
			p.allow = append(p.allow, pt)
		}
	}
	for _, s := range c.Deny {
		if pt, ok := parsePattern(s); ok {
			p.deny = append(p.deny, pt)
		}
	}
	for _, s := range c.AskAlways {
		if pt, ok := parsePattern(s); ok {
			p.ask = append(p.ask, pt)
		}
	}
	return p
}

// maxDepth bounds nested shells (bash -c "bash -c …").
const maxDepth = 6

// Check returns the verdict for a run_command command line.
func (p *Policy) Check(command string) PolicyVerdict {
	if p == nil {
		p = &Policy{}
	}
	var r result
	p.check(command, 0, &r)
	switch {
	case r.deny != nil:
		return *r.deny
	case r.ask != nil:
		return *r.ask
	case r.leaves > 0 && r.unallowed == 0:
		return r.allowV
	}
	return PolicyVerdict{Action: PolicyDefault}
}

// result accumulates over every command of a line, nested ones included.
type result struct {
	deny, ask *PolicyVerdict
	allowV    PolicyVerdict
	leaves    int // commands that would run
	unallowed int // of those, not matched by an Allow pattern
}

func (r *result) denyWith(rule, reason string) {
	if r.deny == nil {
		r.deny = &PolicyVerdict{Action: PolicyDeny, Rule: rule, Reason: reason}
	}
}

func (r *result) askWith(rule, reason string) {
	if r.ask == nil {
		r.ask = &PolicyVerdict{Action: PolicyAsk, Rule: rule, Reason: reason}
	}
}

func (p *Policy) check(command string, depth int, r *result) {
	if depth > maxDepth {
		r.askWith("nesting", "shells nested too deeply to check")
		return
	}
	compact := compactSpace(command)
	for _, pt := range p.deny {
		if pt.raw != "" && strings.Contains(compact, pt.raw) {
			r.denyWith(pt.text, "matches the deny rule "+quote(pt.text))
		}
	}
	for _, pt := range p.ask {
		if pt.raw != "" && strings.Contains(compact, pt.raw) {
			r.askWith(pt.text, "matches the ask-always rule "+quote(pt.text))
		}
	}
	if isForkBomb(compact) {
		r.denyWith("fork bomb", "looks like a fork bomb")
	}
	lx := lex(command)
	if lx.err != "" {
		r.askWith("parse", "could not be parsed ("+lx.err+")")
	}
	for _, s := range lx.suspicious {
		r.askWith("suspicious", s)
	}
	for _, n := range lx.nested {
		p.check(n, depth+1, r)
	}
	for _, seg := range lx.segs {
		p.segment(seg, depth, r)
	}
}

// reFuncDef finds "name(){" in whitespace-free text.
var reFuncDef = regexp.MustCompile(`([A-Za-z_:.][A-Za-z0-9_:.-]*)\(\)\{`)

// isForkBomb catches the classic f(){ f|f& };f shape (any name) in
// whitespace-free text.
func isForkBomb(compact string) bool {
	for _, m := range reFuncDef.FindAllStringSubmatchIndex(compact, -1) {
		name := compact[m[2]:m[3]]
		body := compact[m[1]:]
		if end := strings.IndexByte(body, '}'); end >= 0 {
			body = body[:end]
		}
		if strings.Contains(body, name+"|"+name) && strings.Contains(body, "&") {
			return true
		}
	}
	return false
}

// segment evaluates one simple command and the commands it wraps.
func (p *Policy) segment(seg *segment, depth int, r *result) {
	ws := stripPrefix(seg.words)
	if len(ws) == 0 {
		return
	}
	for _, w := range ws {
		if w.brace {
			r.askWith("suspicious", "brace expansion can build any command: "+quote(w.text))
			break
		}
	}
	p.command(ws, seg.pipedIn, seg.heredocs, depth, r)
}

// command evaluates one command line's words (keywords and assignments
// already stripped), unwrapping wrappers such as sudo, env and xargs.
func (p *Policy) command(view []word, piped bool, heredocs []string, depth int, r *result) {
	privileged := false
	for range 12 {
		if len(view) == 0 {
			return
		}
		head := view[0]
		globbed := head.glob && head.text != "[" && head.text != "[["
		if head.dynamic || globbed || head.braceRange {
			r.askWith("suspicious", "the command name comes from a variable, glob or substitution: "+quote(head.text))
			r.leaves++
			r.unallowed++
			return
		}
		name := canonical(head.text)
		p.match(view, r)
		if isShell(name) {
			p.shell(view, piped, heredocs, depth, r)
			return
		}
		if name == "source" || name == "." {
			p.source(view, piped, heredocs, depth, r)
			return
		}
		next, payload, ok := unwrap(name, view)
		if payload != "" {
			p.check(payload, depth+1, r)
		}
		if ok && name == "find" {
			// find … -exec CMD … ; : the exec'd command is a command of
			// its own, and find itself stays a leaf.
			if len(next) > 0 {
				p.command(next, false, nil, depth+1, r)
			}
			break
		}
		if !ok {
			break
		}
		if payload != "" && len(next) == 0 {
			return // its payload was checked as commands of its own
		}
		if sudoLike[name] || name == "systemd-run" || name == "flock" || name == "watch" {
			privileged = true
		}
		if name == "xargs" {
			piped = false // xargs reads the pipe; the command it runs does not
		}
		view = next
	}
	if len(view) == 0 {
		return
	}
	name := canonical(view[0].text)
	if isInterpreter(name) && piped && readsStdin(name, view[1:]) {
		r.askWith("pipe", "pipes data into "+name+", which runs it as a program")
	}
	r.leaves++
	if privileged {
		r.unallowed++
		return
	}
	for _, pt := range p.allow {
		if pt.raw == "" {
			if m, _ := pt.match(view, p.home); m {
				if r.allowV.Rule == "" {
					r.allowV = PolicyVerdict{Action: PolicyAllow, Rule: pt.text, Reason: "matches the allow rule " + quote(pt.text)}
				}
				return
			}
		}
	}
	r.unallowed++
}

// match checks one view of a command against the deny and ask patterns.
func (p *Policy) match(view []word, r *result) {
	for _, pt := range p.deny {
		if pt.raw != "" {
			continue
		}
		m, maybe := pt.match(view, p.home)
		if m {
			r.denyWith(pt.text, "matches the deny rule "+quote(pt.text))
		} else if maybe {
			r.askWith(pt.text, "may expand to the denied command "+quote(pt.text))
		}
	}
	for _, pt := range p.ask {
		if pt.raw != "" {
			continue
		}
		if m, maybe := pt.match(view, p.home); m || maybe {
			r.askWith(pt.text, "matches the ask-always rule "+quote(pt.text))
		}
	}
}

// shell handles bash/sh/zsh…: a -c payload is checked as commands; a shell
// reading its program from a pipe is denied; a here-doc or here-string fed
// to it is checked as commands.
func (p *Policy) shell(view []word, piped bool, heredocs []string, depth int, r *result) {
	args := view[1:]
	stdin := true
	for i := 0; i < len(args); i++ {
		a := args[i].text
		if a == "--" {
			if i+1 < len(args) && args[i+1].text != "-" {
				stdin = false
			}
			break
		}
		if a == "-" {
			break
		}
		if len(a) > 1 && (a[0] == '-' || a[0] == '+') {
			if a == "-o" || a == "+o" || a == "-O" || a == "+O" || a == "--rcfile" || a == "--init-file" {
				i++
				continue
			}
			if !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'c') {
				// The payload is the first operand after the options.
				for j := i + 1; j < len(args); j++ {
					t := args[j].text
					if t == "--" {
						continue
					}
					if len(t) > 1 && (t[0] == '-' || t[0] == '+') {
						continue
					}
					p.check(t, depth+1, r)
					return
				}
				r.askWith("parse", "a shell -c without a command")
				return
			}
			if !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 's') {
				break // -s: the program comes from stdin, operands are its arguments
			}
			continue
		}
		if a != "/dev/stdin" {
			stdin = false // a script file
		}
		break
	}
	r.leaves++
	r.unallowed++
	if !stdin {
		return
	}
	if piped {
		r.denyWith("pipe into a shell", "pipes into a shell ("+canonical(view[0].text)+"), which runs whatever arrives")
	}
	for _, body := range heredocs {
		p.check(body, depth+1, r)
	}
}

// stdinPaths are the names a shell can read its own stdin through.
var stdinPaths = set("-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0")

// source handles the source and . builtins, which run a file's text in the
// current shell: reading stdin while piped is the same as piping into a
// shell (`curl x | source /dev/stdin`), a here-doc fed to it is checked as
// commands, and a file named by a variable or substitution (including a
// process substitution such as <(curl x)) is asked about.
func (p *Policy) source(view []word, piped bool, heredocs []string, depth int, r *result) {
	r.leaves++
	r.unallowed++
	if len(view) < 2 {
		return
	}
	arg := view[1]
	if arg.dynamic || arg.glob || arg.brace || arg.braceRange {
		r.askWith("suspicious", canonical(view[0].text)+" runs a file named by a variable, glob or substitution: "+quote(arg.text))
		return
	}
	if !stdinPaths[normPath(arg.text, "")] {
		return
	}
	if piped {
		r.denyWith("pipe into a shell", "pipes into "+canonical(view[0].text)+" "+arg.text+", which runs whatever arrives")
	}
	for _, body := range heredocs {
		p.check(body, depth+1, r)
	}
}

// ---- patterns ----

type pattern struct {
	text    string
	raw     string   // compacted text for raw patterns (":(){"), else ""
	cmd     string   // canonical command name
	subs    []string // positional words after the command
	flags   []string // normalised flag tokens ("-r", "-f", "--user")
	assigns []string // key=value words, matched anywhere ("of=/dev/" as a prefix)
}

func parsePattern(s string) (pattern, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return pattern{}, false
	}
	pt := pattern{text: s}
	if strings.ContainsAny(s, "(){}|;&<>$`") {
		pt.raw = compactSpace(s)
		return pt, true
	}
	fs := strings.Fields(s)
	pt.cmd = canonical(fs[0])
	for _, f := range fs[1:] {
		switch {
		case isFlag(f):
			pt.flags = append(pt.flags, flagTokens(pt.cmd, f)...)
		case strings.Contains(f, "="):
			pt.assigns = append(pt.assigns, f)
		default:
			pt.subs = append(pt.subs, f)
		}
	}
	return pt, true
}

// match reports whether view matches the pattern; maybe is true when the
// command and flags match but a positional word could not be compared
// because an argument is dynamic (a variable, glob or substitution).
func (pt pattern) match(view []word, home string) (match, maybe bool) {
	if len(view) == 0 {
		return false, false
	}
	name := canonical(view[0].text)
	if name != pt.cmd && !(isAlnum(pt.cmd) && strings.HasPrefix(name, pt.cmd+".")) {
		return false, false
	}
	pa := parseArgs(name, view[1:])
	for _, f := range pt.flags {
		if !pa.flags[f] {
			return false, false
		}
	}
	for _, a := range pt.assigns {
		if !slices.ContainsFunc(pa.all, func(w word) bool { return assignMatch(w.text, a, home) }) {
			return false, false
		}
	}
	if len(pt.subs) == 0 {
		return true, false
	}
	pos := pa.pos
	start := 0
	if subcommandTools[name] {
		if len(pos) == 0 || !wordEq(pos[0], pt.subs[0], home) {
			return false, len(pos) > 0 && pos[0].dynamic
		}
		start = 1
		pos = pos[1:]
	}
	i := 0
	for _, sub := range pt.subs[start:] {
		for i < len(pos) && !wordEq(pos[i], sub, home) {
			i++
		}
		if i == len(pos) {
			dyn := slices.ContainsFunc(pa.pos, func(w word) bool { return w.dynamic || w.glob })
			return false, dyn
		}
		i++
	}
	return true, false
}

// assignMatch compares a key=value word with a key=value pattern word. A
// path value is normalised on both sides (so of=//dev/sda and of=/./dev/sda
// match of=/dev/), and a pattern value ending in "/" matches as a path
// prefix.
func assignMatch(w, pat, home string) bool {
	if w == pat {
		return true
	}
	wk, wv, ok1 := strings.Cut(w, "=")
	pk, pv, ok2 := strings.Cut(pat, "=")
	if !ok1 || !ok2 || wk != pk {
		return false
	}
	nw, np := normPath(wv, home), normPath(pv, home)
	if nw == np {
		return true
	}
	if strings.HasSuffix(pv, "/") {
		prefix := strings.TrimSuffix(np, "/") + "/"
		return strings.HasPrefix(nw+"/", prefix) || strings.HasPrefix(nw, prefix)
	}
	return false
}

type parsedArgs struct {
	pos   []word
	all   []word
	flags map[string]bool
}

// valued lists options that take the next word as their value, per
// command (only before the subcommand for subcommand tools).
var valued = map[string]map[string]bool{
	"git":       set("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path", "--config-env"),
	"systemctl": set("-H", "-M", "-p", "-t", "-n", "-o", "--host", "--machine", "--property", "--type", "--state", "--lines", "--output", "--signal", "-s"),
	"npm":       set("--prefix", "-w", "--workspace"),
	"pip":       set("-r", "-c", "-e", "-t", "--target", "--index-url", "-i"),
	"dd":        set(),
}

// flagAlias maps equivalent options to one token, per command.
var flagAlias = map[string]map[string]string{
	"rm":  {"-R": "-r", "--recursive": "-r", "--force": "-f"},
	"git": {"--force": "-f", "--force-with-lease": "-f", "--force-if-includes": "-f", "-B": "-b", "--orphan": "-b", "-C": "-c", "--create": "-c", "--force-create": "-c"},
	"cp":  {"-R": "-r", "--recursive": "-r", "--force": "-f"},
}

func parseArgs(name string, args []word) parsedArgs {
	pa := parsedArgs{all: args, flags: map[string]bool{}}
	sub := name == "git" // git's options before the subcommand are global, not the subcommand's
	seenSub := false
	vals := valued[name]
	opts := true
	for i := 0; i < len(args); i++ {
		t := args[i].text
		if opts && t == "--" {
			opts = false
			continue
		}
		if opts && isFlag(t) {
			if sub && !seenSub {
				if vals[t] {
					i++
				}
				continue // global options do not count as the subcommand's flags
			}
			if !sub && vals[t] {
				i++
			}
			for _, f := range flagTokens(name, t) {
				pa.flags[f] = true
			}
			continue
		}
		if name == "git" && seenSub && strings.HasPrefix(t, "+") && len(pa.pos) > 0 && pa.pos[0].text == "push" {
			pa.flags["-f"] = true // git push origin +main is a force push
		}
		pa.pos = append(pa.pos, args[i])
		seenSub = true
	}
	return pa
}

// flagTokens splits "-rf" into "-r","-f", drops "=value" from long options
// and applies the command's aliases.
func flagTokens(name, f string) []string {
	alias := flagAlias[name]
	norm := func(t string) string {
		if a, ok := alias[t]; ok {
			return a
		}
		return t
	}
	if strings.HasPrefix(f, "--") {
		k, _, _ := strings.Cut(f, "=")
		return []string{norm(k)}
	}
	var out []string
	for _, c := range f[1:] {
		out = append(out, norm("-"+string(c)))
	}
	return out
}

func isFlag(t string) bool {
	if len(t) < 2 || t[0] != '-' {
		return false
	}
	if t[1] >= '0' && t[1] <= '9' {
		return false // a negative number (nice -10 is handled as a wrapper)
	}
	return true
}

// wordEq compares an argument with a pattern word: equal after path
// normalisation, or a unit-style suffix ("cerberus" matches
// "cerberus.service" and "cerberus@x").
func wordEq(w word, pw string, home string) bool {
	a, b := normPath(w.text, home), normPath(pw, home)
	if a == b {
		return true
	}
	return isAlnum(pw) && (strings.HasPrefix(w.text, pw+".") || strings.HasPrefix(w.text, pw+"@"))
}

// normPath makes "/", "//", "/./", "/*", "/tmp/.." all "/", and "~/",
// "$HOME", "${HOME}/*" and the home folder itself all "~".
func normPath(s, home string) string {
	for _, v := range []string{"${HOME}", "$HOME"} {
		if s == v || strings.HasPrefix(s, v+"/") {
			s = "~" + s[len(v):]
		}
	}
	if home != "" && (s == home || strings.HasPrefix(s, home+"/")) {
		s = "~" + s[len(home):]
	}
	if !strings.HasPrefix(s, "/") && s != "~" && !strings.HasPrefix(s, "~/") {
		return s
	}
	tilde := strings.HasPrefix(s, "~")
	rest := strings.TrimPrefix(s, "~")
	for {
		t := strings.TrimSuffix(strings.TrimSuffix(rest, "/*"), "/.")
		if t == rest {
			break
		}
		rest = t
	}
	if rest == "*" {
		rest = ""
	}
	rest = filepath.ToSlash(filepath.Clean("/" + rest))
	if tilde {
		if rest == "/" {
			return "~"
		}
		return "~" + rest
	}
	return rest
}

// ---- command names, wrappers, shells ----

var (
	subcommandTools = set("git", "npm", "pnpm", "yarn", "pip", "systemctl", "docker", "podman", "cargo", "go", "dnf",
		"yum", "apt", "apt-get", "brew", "kubectl", "gh", "uv", "poetry", "flatpak", "snap", "rpm-ostree", "zypper", "pacman")
	shells       = set("bash", "sh", "zsh", "dash", "ksh", "mksh", "ash", "fish", "yash", "tcsh", "csh")
	interpreters = set("python", "perl", "ruby", "node", "php", "lua", "deno", "bun", "osascript", "pwsh")
	sudoLike     = set("sudo", "doas", "pkexec", "run0")
	reVersioned  = regexp.MustCompile(`^(python|pip|perl|ruby|node|php|lua)[0-9.]*$`)
)

// canonical is the command's base name with versions dropped (pip3.12 →
// pip, /usr/bin/python3 → python) and privilege tools folded into sudo.
func canonical(t string) string {
	t = strings.TrimSpace(t)
	if strings.Contains(t, "/") {
		t = filepath.Base(filepath.ToSlash(t))
	}
	if m := reVersioned.FindStringSubmatch(t); m != nil {
		t = m[1]
	}
	if sudoLike[t] {
		return "sudo"
	}
	return t
}

func isShell(name string) bool       { return shells[name] }
func isInterpreter(name string) bool { return interpreters[name] }

// readsStdin reports whether an interpreter takes its program from stdin
// (no script, no -c/-e/-m).
func readsStdin(name string, args []word) bool {
	for _, a := range args {
		t := a.text
		switch {
		case t == "-":
			return true
		case t == "-c" || t == "-e" || t == "-m" || t == "-E" || t == "--eval" || t == "-r" || t == "-p":
			return false
		case isFlag(t):
			continue
		}
		return false
	}
	return true
}

// wrapper describes a command that runs another: the options that take a
// value, options after which nothing is run, and how many operands come
// before the wrapped command.
type wrapper struct {
	valued   map[string]bool
	noExec   map[string]bool
	operands int
	payload  map[string]bool // options whose value is a shell command
	joinRest bool            // the rest of the line is one shell command (eval, watch)
}

var wrappers = map[string]wrapper{
	"env":         {valued: set("-u", "--unset", "-C", "--chdir", "-S", "--split-string"), payload: set("-S", "--split-string")},
	"sudo":        {valued: set("-u", "-g", "-h", "-p", "-C", "-D", "-r", "-t", "-U", "-T", "--user", "--group", "--host", "--prompt", "--chdir", "--role", "--type", "--other-user", "--command-timeout"), noExec: set("-l", "--list", "-v", "--validate", "-k", "-K", "-e", "--edit", "-V")},
	"nohup":       {},
	"time":        {valued: set("-f", "--format", "-o", "--output")},
	"nice":        {valued: set("-n", "--adjustment")},
	"ionice":      {valued: set("-c", "-n", "--class", "--classdata"), noExec: set("-p", "--pid", "-P", "--pgid", "-u", "--uid")},
	"chrt":        {valued: set(), operands: 1, noExec: set("-p", "--pid", "-m", "--max")},
	"taskset":     {valued: set(), operands: 1, noExec: set("-p", "--pid")},
	"setsid":      {},
	"stdbuf":      {valued: set("-i", "-o", "-e", "--input", "--output", "--error")},
	"timeout":     {valued: set("-s", "-k", "--signal", "--kill-after"), operands: 1},
	"xargs":       {valued: set("-I", "-n", "-P", "-d", "-L", "-s", "-E", "-a", "--max-args", "--max-procs", "--delimiter", "--max-lines", "--max-chars", "--eof", "--arg-file", "--replace")},
	"exec":        {valued: set("-a")},
	"command":     {noExec: set("-v", "-V")},
	"builtin":     {},
	"busybox":     {},
	"unbuffer":    {},
	"strace":      {valued: set("-o", "-e", "-p", "-s", "-u", "-E", "-P", "-I", "-b", "-a", "-X", "-S", "-O")},
	"ltrace":      {valued: set("-o", "-e", "-p", "-s", "-u", "-E", "-a", "-n")},
	"flock":       {valued: set("-w", "-E", "--timeout", "--conflict-exit-code", "-c", "--command"), operands: 1, payload: set("-c", "--command")},
	"systemd-run": {valued: set("-p", "-E", "-u", "-M", "-H", "--property", "--setenv", "--unit", "--slice", "--description", "--uid", "--gid", "--nice", "--working-directory", "--on-calendar", "--on-active", "--machine", "--host")},
	"sudoedit":    {noExec: set("")},
	"eval":        {joinRest: true},
	"watch":       {valued: set("-n", "--interval", "-d", "--differences", "-q", "--equexit"), joinRest: true},
	"find":        {},
}

// unwrap returns the command a wrapper runs (next), a shell payload it
// runs (from env -S, flock -c, eval, watch), and whether name is a wrapper
// that runs something at all.
func unwrap(name string, view []word) (next []word, payload string, ok bool) {
	if name == "find" {
		return findExec(view), "", true
	}
	if name == "python" && len(view) > 2 && view[1].text == "-m" && canonical(view[2].text) == "pip" {
		out := append([]word{{text: "pip"}}, view[3:]...)
		return out, "", true
	}
	w, isW := wrappers[name]
	if !isW {
		return nil, "", false
	}
	args := view[1:]
	if name == "flock" {
		// flock [options] FILE -c COMMAND: -c may come after the file.
		for j, a := range args {
			k, v, has := strings.Cut(a.text, "=")
			if k == "-c" || k == "--command" {
				if has {
					return nil, v, true
				}
				if j+1 < len(args) {
					return nil, args[j+1].text, true
				}
			}
		}
	}
	if w.joinRest {
		i := 0
		for i < len(args) && isFlag(args[i].text) {
			if w.valued[args[i].text] {
				i++
			}
			i++
		}
		if i >= len(args) {
			return nil, "", true
		}
		var parts []string
		for _, a := range args[i:] {
			parts = append(parts, a.text)
		}
		return nil, strings.Join(parts, " "), true
	}
	i := 0
	for i < len(args) {
		t := args[i].text
		if t == "--" {
			i++
			break
		}
		if name == "env" && isAssignment(t) {
			i++
			continue
		}
		if name == "nice" && len(t) > 1 && t[0] == '-' && t[1] >= '0' && t[1] <= '9' {
			i++
			continue
		}
		if !isFlag(t) {
			break
		}
		key, val, hasVal := strings.Cut(t, "=")
		if w.noExec[key] {
			return nil, "", false
		}
		if w.payload[key] {
			if hasVal {
				payload = val
			} else if i+1 < len(args) {
				payload = args[i+1].text
			}
			rest := []string{payload}
			skip := i + 1
			if !hasVal {
				skip = i + 2
			}
			if name == "env" { // env -S STRING ARGS…: STRING is split and ARGS appended
				for _, a := range args[min(skip, len(args)):] {
					rest = append(rest, a.text)
				}
				return nil, strings.Join(rest, " "), true
			}
			return nil, payload, true
		}
		if w.valued[t] {
			i++ // the option's value is the next word
		}
		i++
	}
	if name == "env" {
		for i < len(args) && isAssignment(args[i].text) {
			i++
		}
	}
	i += w.operands
	if i >= len(args) {
		return nil, "", true // nothing wrapped (sudo -s, nohup alone…)
	}
	return args[i:], "", true
}

// findExec returns the command of find's first -exec/-execdir/-ok/-okdir.
func findExec(view []word) []word {
	for i, w := range view {
		switch w.text {
		case "-exec", "-execdir", "-ok", "-okdir":
			var out []word
			for _, a := range view[i+1:] {
				if a.text == ";" || a.text == "+" {
					break
				}
				out = append(out, a)
			}
			return out
		}
	}
	return nil
}

var reAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?\+?=`)

func isAssignment(t string) bool { return reAssign.MatchString(t) }

// reserved are shell words that may start a segment and are not commands.
var reserved = set("!", "{", "}", "then", "else", "elif", "do", "if", "while", "until", "fi", "done", "esac", "in", "time", "coproc", "[[", "]]")

// stripPrefix drops leading keywords and variable assignments. A
// segment that starts a for/case/select/function header runs nothing
// itself.
func stripPrefix(ws []word) []word {
	for len(ws) > 0 {
		t := ws[0].text
		switch {
		case ws[0].quoted:
			return ws
		case t == "for" || t == "case" || t == "select" || t == "function":
			return nil
		case reserved[t]:
			ws = ws[1:]
			if t == "time" && len(ws) > 0 && ws[0].text == "-p" {
				ws = ws[1:]
			}
		case isAssignment(t):
			ws = ws[1:]
		default:
			return ws
		}
	}
	return ws
}

// ---- lexer ----

// word is one shell word after quote removal.
type word struct {
	text       string
	quoted     bool // some part was quoted or escaped
	dynamic    bool // holds a variable, arithmetic or substitution: its value is unknown
	glob       bool // unquoted * ? [
	brace      bool // unquoted brace expansion with a comma
	braceRange bool // unquoted {a..b}
}

// segment is one simple command: the words between operators.
type segment struct {
	words    []word
	pipedIn  bool     // its stdin is a pipe from the previous command
	heredocs []string // here-document and here-string bodies fed to it
}

type lexed struct {
	segs       []*segment
	nested     []string // command substitutions, checked as commands of their own
	suspicious []string
	err        string
}

type heredocReq struct {
	seg    *segment
	delim  string
	strip  bool // <<-
	quoted bool // the delimiter was quoted: no expansion in the body
}

type lexer struct {
	s     []rune
	i     int
	out   lexed
	cur   *segment
	w     word
	b     strings.Builder
	in    bool // a word is being built
	redir string
	docs  []heredocReq
}

func lex(src string) lexed {
	l := &lexer{s: []rune(src)}
	l.cur = &segment{}
	l.run()
	l.endWord()
	l.endSeg(false)
	if len(l.docs) > 0 && l.out.err == "" {
		l.out.err = "unterminated here-document"
	}
	return l.out
}

func (l *lexer) suspicious(why string) {
	if !slices.Contains(l.out.suspicious, why) {
		l.out.suspicious = append(l.out.suspicious, why)
	}
}

func (l *lexer) peek(k int) rune {
	if l.i+k < len(l.s) {
		return l.s[l.i+k]
	}
	return 0
}

func (l *lexer) add(r rune) {
	l.in = true
	l.b.WriteRune(r)
}

func (l *lexer) endWord() {
	if !l.in {
		return
	}
	l.w.text = l.b.String()
	if strings.Contains(l.w.text, "{") && l.w.braceRange && !l.w.brace {
		l.w.braceRange = strings.Contains(l.w.text, "..")
	}
	w := l.w
	l.b.Reset()
	l.w = word{}
	l.in = false
	switch l.redir {
	case "":
		l.cur.words = append(l.cur.words, w)
	case "<<", "<<-":
		l.docs = append(l.docs, heredocReq{seg: l.cur, delim: w.text, strip: l.redir == "<<-", quoted: w.quoted})
	case "<<<":
		l.cur.heredocs = append(l.cur.heredocs, w.text)
	}
	l.redir = ""
}

// endSeg closes the current segment; pipe says the next one reads from it.
func (l *lexer) endSeg(pipe bool) {
	if l.redir != "" && l.out.err == "" {
		l.out.err = "a redirection without a target"
		l.redir = ""
	}
	empty := len(l.cur.words) == 0 && len(l.cur.heredocs) == 0
	if !empty {
		l.out.segs = append(l.out.segs, l.cur)
	}
	// "curl x |" + newline + "sh", or "| (sh)": the pipe carries over an
	// empty segment.
	l.cur = &segment{pipedIn: pipe || (empty && l.cur.pipedIn)}
}

func (l *lexer) run() {
	for l.i < len(l.s) && l.out.err == "" {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.endWord()
			l.i++
		case c == '\n':
			l.endWord()
			l.endSeg(false)
			l.i++
			l.readHeredocs()
		case c == '#' && !l.in:
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case c == ';':
			l.endWord()
			l.endSeg(false)
			l.i++
		case c == '&':
			l.endWord()
			switch l.peek(1) {
			case '&':
				l.endSeg(false)
				l.i += 2
			case '>':
				l.i++
				l.redirect()
			default:
				l.endSeg(false)
				l.i++
			}
		case c == '|':
			l.endWord()
			switch l.peek(1) {
			case '|':
				l.endSeg(false)
				l.i += 2
			case '&':
				l.endSeg(true)
				l.i += 2
			default:
				l.endSeg(true)
				l.i++
			}
		case c == '(' || c == ')':
			if c == '(' && l.in {
				// f() or a=(…): an array or a function header.
				l.endWord()
			}
			l.endWord()
			l.endSeg(false)
			l.i++
		case c == '<' || c == '>':
			if l.in && !l.w.quoted && isDigits(l.b.String()) {
				l.b.Reset() // a file descriptor number
				l.in = false
			}
			l.endWord()
			l.redirect()
		case c == '\'':
			l.single()
		case c == '"':
			l.double()
		case c == '\\':
			l.w.quoted = true
			if l.i+1 < len(l.s) {
				if l.s[l.i+1] != '\n' {
					l.add(l.s[l.i+1])
				}
				l.i += 2
			} else {
				l.i++
			}
		case c == '$':
			l.dollar(false)
		case c == '`':
			l.backtick()
		default:
			switch c {
			case '*', '?', '[':
				l.w.glob = true
			case '{':
				if l.braceAhead() {
					l.w.brace = true
				} else if l.rangeAhead() {
					l.w.braceRange = true
				}
			}
			l.add(c)
			l.i++
		}
	}
}

// braceAhead reports whether an unquoted "{" starts a {a,b} expansion.
func (l *lexer) braceAhead() bool {
	depth := 0
	for j := l.i; j < len(l.s); j++ {
		switch l.s[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return false
			}
		case ',':
			if depth == 1 {
				return true
			}
		case ' ', '\t', '\n', ';', '|', '&', '\'', '"':
			return false
		}
	}
	return false
}

func (l *lexer) rangeAhead() bool {
	for j := l.i; j < len(l.s); j++ {
		switch l.s[j] {
		case '}':
			return strings.Contains(string(l.s[l.i:j]), "..")
		case ' ', '\t', '\n', ';', '|', '&':
			return false
		}
	}
	return false
}

// redirect reads a redirection operator at l.i; the next word is its target.
func (l *lexer) redirect() {
	c := l.s[l.i]
	op := string(c)
	l.i++
	if c == '>' || c == '<' {
		if n := l.peek(0); n == '(' {
			l.i++
			end := l.matchParen(l.i)
			if end < 0 {
				l.out.err = "unterminated process substitution"
				return
			}
			l.out.nested = append(l.out.nested, string(l.s[l.i:end]))
			l.suspicious("process substitution " + op + "(…) runs a command whose output is used")
			l.i = end + 1
			l.in = true // stands in for a /dev/fd path
			l.w.dynamic = true
			return
		}
	}
	for l.i < len(l.s) && strings.ContainsRune("<>&|-", l.s[l.i]) && len(op) < 3 {
		if l.s[l.i] == '-' && op != "<<" {
			break
		}
		op += string(l.s[l.i])
		l.i++
	}
	if op == "&" {
		op = "&>"
	}
	// ">&2", "<&-": a descriptor target ("2", "-") is read as a word.
	l.redir = op
	switch op {
	case "<<", "<<-", "<<<":
	default:
		l.redir = "target"
	}
}

func (l *lexer) single() {
	l.w.quoted = true
	l.in = true
	l.i++
	for l.i < len(l.s) && l.s[l.i] != '\'' {
		l.b.WriteRune(l.s[l.i])
		l.i++
	}
	if l.i >= len(l.s) {
		l.out.err = "unterminated single quote"
		return
	}
	l.i++
}

func (l *lexer) double() {
	l.w.quoted = true
	l.in = true
	l.i++
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch c {
		case '"':
			l.i++
			return
		case '\\':
			if n := l.peek(1); n == '$' || n == '`' || n == '"' || n == '\\' {
				l.b.WriteRune(n)
				l.i += 2
				continue
			} else if n == '\n' {
				l.i += 2
				continue
			}
			l.b.WriteRune(c)
			l.i++
		case '$':
			l.dollar(true)
			if l.out.err != "" {
				return
			}
		case '`':
			l.backtick()
			if l.out.err != "" {
				return
			}
		default:
			l.b.WriteRune(c)
			l.i++
		}
	}
	l.out.err = "unterminated double quote"
}

// dollar handles $… at l.i: substitutions, arithmetic, ${…}, $'…', $name.
func (l *lexer) dollar(inDouble bool) {
	l.in = true
	n := l.peek(1)
	switch {
	case n == '(' && l.peek(2) == '(':
		end := l.matchParen(l.i + 2)
		if end < 0 || l.peek(end-l.i+1) != ')' {
			l.out.err = "unterminated arithmetic"
			return
		}
		inner := string(l.s[l.i+3 : end])
		if strings.Contains(inner, "$(") || strings.Contains(inner, "`") {
			l.suspicious("command substitution inside arithmetic")
		}
		l.w.dynamic = true
		l.b.WriteString("$((" + inner + "))")
		l.i = end + 2
	case n == '(':
		end := l.matchParen(l.i + 2)
		if end < 0 {
			l.out.err = "unterminated $("
			return
		}
		inner := string(l.s[l.i+2 : end])
		l.out.nested = append(l.out.nested, inner)
		l.suspicious("command substitution $(…) runs a command whose output becomes part of this one")
		l.w.dynamic = true
		l.b.WriteString("$(" + inner + ")")
		l.i = end + 1
	case n == '{':
		end := -1
		depth := 0
		for j := l.i + 1; j < len(l.s); j++ {
			if l.s[j] == '{' {
				depth++
			} else if l.s[j] == '}' {
				depth--
				if depth == 0 {
					end = j
					break
				}
			}
		}
		if end < 0 {
			l.out.err = "unterminated ${"
			return
		}
		inner := string(l.s[l.i+2 : end])
		if strings.Contains(inner, "$(") || strings.Contains(inner, "`") {
			l.out.nested = append(l.out.nested, inner)
			l.suspicious("command substitution inside ${…}")
		}
		l.w.dynamic = true
		l.b.WriteString("${" + inner + "}")
		l.i = end + 1
	case n == '\'' && !inDouble:
		l.w.quoted = true
		l.i += 2
		l.ansiC()
	case n == '"' && !inDouble:
		l.i++ // $"…" is a translated string: read it as "…"
		l.double()
	case n == '_' || unicode.IsLetter(n):
		j := l.i + 1
		for j < len(l.s) && (l.s[j] == '_' || unicode.IsLetter(l.s[j]) || unicode.IsDigit(l.s[j])) {
			j++
		}
		l.w.dynamic = true
		l.b.WriteString(string(l.s[l.i:j]))
		l.i = j
	case n != 0 && strings.ContainsRune("@*#?$!-0123456789", n):
		l.w.dynamic = true
		l.b.WriteString(string(l.s[l.i : l.i+2]))
		l.i += 2
	default:
		l.b.WriteRune('$')
		l.i++
	}
}

// ansiC reads the body of $'…' (l.i just past the quote), decoding escapes.
func (l *lexer) ansiC() {
	for l.i < len(l.s) {
		c := l.s[l.i]
		if c == '\'' {
			l.i++
			return
		}
		if c != '\\' || l.i+1 >= len(l.s) {
			l.b.WriteRune(c)
			l.i++
			continue
		}
		e := l.s[l.i+1]
		l.i += 2
		switch e {
		case 'n':
			l.b.WriteRune('\n')
		case 't':
			l.b.WriteRune('\t')
		case 'r':
			l.b.WriteRune('\r')
		case 'a', 'b', 'e', 'E', 'f', 'v':
			l.b.WriteRune(' ')
		case 'x', 'u', 'U':
			max := map[rune]int{'x': 2, 'u': 4, 'U': 8}[e]
			v, k := 0, 0
			for k < max && l.i < len(l.s) && isHex(l.s[l.i]) {
				v = v*16 + hexVal(l.s[l.i])
				l.i++
				k++
			}
			if k == 0 {
				l.b.WriteRune('\\')
				l.b.WriteRune(e)
			} else {
				l.b.WriteRune(rune(v))
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v, k := int(e-'0'), 1
			for k < 3 && l.i < len(l.s) && l.s[l.i] >= '0' && l.s[l.i] <= '7' {
				v = v*8 + int(l.s[l.i]-'0')
				l.i++
				k++
			}
			l.b.WriteRune(rune(v))
		case 'c':
			if l.i < len(l.s) {
				l.i++
			}
			l.w.dynamic = true
		default:
			l.b.WriteRune(e) // \\ \' \" \?
		}
	}
	l.out.err = "unterminated $'"
}

func (l *lexer) backtick() {
	l.in = true
	j := l.i + 1
	var inner strings.Builder
	for j < len(l.s) && l.s[j] != '`' {
		if l.s[j] == '\\' && j+1 < len(l.s) && strings.ContainsRune("`$\\", l.s[j+1]) {
			inner.WriteRune(l.s[j+1])
			j += 2
			continue
		}
		inner.WriteRune(l.s[j])
		j++
	}
	if j >= len(l.s) {
		l.out.err = "unterminated backtick"
		return
	}
	l.out.nested = append(l.out.nested, inner.String())
	l.suspicious("backticks `…` run a command whose output becomes part of this one")
	l.w.dynamic = true
	l.b.WriteString("`" + inner.String() + "`")
	l.i = j + 1
}

// matchParen returns the index of the ")" closing a "(" just before from,
// skipping quoted text, or -1.
func (l *lexer) matchParen(from int) int {
	depth := 1
	for j := from; j < len(l.s); j++ {
		switch l.s[j] {
		case '\\':
			j++
		case '\'':
			k := j + 1
			for k < len(l.s) && l.s[k] != '\'' {
				k++
			}
			j = k
		case '"':
			k := j + 1
			for k < len(l.s) && l.s[k] != '"' {
				if l.s[k] == '\\' {
					k++
				}
				k++
			}
			j = k
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// readHeredocs consumes the bodies of pending here-documents after a newline.
func (l *lexer) readHeredocs() {
	for len(l.docs) > 0 {
		d := l.docs[0]
		var body strings.Builder
		found := false
		for l.i < len(l.s) {
			j := l.i
			for j < len(l.s) && l.s[j] != '\n' {
				j++
			}
			line := string(l.s[l.i:j])
			l.i = min(j+1, len(l.s))
			check := line
			if d.strip {
				check = strings.TrimLeft(line, "\t")
			}
			if strings.TrimRight(check, "\r") == d.delim {
				found = true
				break
			}
			body.WriteString(line + "\n")
		}
		if !found {
			return // reported as unterminated by lex
		}
		text := body.String()
		if !d.quoted && (strings.Contains(text, "$(") || strings.Contains(text, "`")) {
			l.out.nested = append(l.out.nested, text)
			l.suspicious("command substitution inside a here-document")
		}
		d.seg.heredocs = append(d.seg.heredocs, text)
		l.docs = l.docs[1:]
	}
}

// ---- small helpers ----

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func compactSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isAlnum(s string) bool {
	for _, c := range s {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '-' && c != '_' {
			return false
		}
	}
	return s != ""
}

func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func hexVal(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	}
	return int(r-'A') + 10
}

func quote(s string) string { return "\"" + s + "\"" }
