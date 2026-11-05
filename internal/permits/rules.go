// Package permits holds permission rules: the rule language the LLM chat
// shares with Claude Code ("Allow for this session"), and the saved rules
// ("Always allow") a person keeps across chats and agent sessions
// (docs/SCOPE.md "As built: saved permissions"). It uses the standard
// library only, so the agent, the chat and the daemon all import it.
package permits

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// A rule names a canonical tool (Bash, Read, Write, Edit, LS, Grep,
// WebFetch, …) and a content in Claude Code's own rule language:
//
//	""             any call of the tool
//	"npm test"     that exact Bash command
//	"npm run:*"    a Bash command starting with the prefix (never a joined one)
//	"dir:/abs"     a file tool inside the folder
//	"domain:host"  a URL tool on the host or below it
//	"//abs/**", "/rel/**", "~/x", "rel"   a path pattern
//
// Rules are narrow on purpose: a doubt asks.

// Rule is one permission rule.
type Rule struct {
	Tool    string `json:"tool"`
	Content string `json:"content,omitempty"`
	Text    string `json:"text"` // for people: "Bash(npm test)", "Read in ~/notes", "Edit (any)"
}

// FileTools take a path.
var FileTools = map[string]bool{"Read": true, "Edit": true, "Write": true, "MultiEdit": true,
	"NotebookEdit": true, "Glob": true, "Grep": true, "LS": true}

// shellJoin: a command holding any of these is more than one command (or
// writes somewhere), so a "prefix:*" rule never covers it.
var shellJoin = regexp.MustCompile("[;&|`\n\r<>]|\\$\\(")

// ShellJoin reports whether command joins, pipes, redirects or substitutes.
func ShellJoin(command string) bool { return shellJoin.MatchString(command) }

// NewRule is the rule for tool and content, with its text for people.
func NewRule(tool, content string) Rule {
	r := Rule{Tool: tool, Content: content}
	switch {
	case content == "":
		r.Text = tool + " (any)"
	case strings.HasPrefix(content, "dir:"):
		r.Text = tool + " in " + ShortPath(content[4:])
	default:
		r.Text = tool + "(" + TruncRunes(strings.Join(strings.Fields(content), " "), 60) + ")"
	}
	return r
}

// Matches reports whether rule covers a call of tool with data. Relative
// paths are cwd's.
func Matches(rule Rule, tool string, data map[string]any, cwd string) bool {
	if rule.Tool != tool {
		return false
	}
	content := rule.Content
	if content == "" {
		return true
	}
	if tool == "Bash" {
		command := strings.TrimSpace(Str(data["command"]))
		if prefix, ok := strings.CutSuffix(content, ":*"); ok {
			prefix = strings.TrimSpace(prefix)
			if prefix == "" || ShellJoin(command) {
				return false
			}
			return command == prefix || strings.HasPrefix(command, prefix+" ")
		}
		return command == strings.TrimSpace(content)
	}
	if want, ok := strings.CutPrefix(content, "domain:"); ok {
		u, err := url.Parse(Str(data["url"]))
		if err != nil {
			return false
		}
		host := strings.ToLower(u.Hostname())
		want = strings.ToLower(want)
		return host != "" && (host == want || strings.HasSuffix(host, "."+want))
	}
	p := ToolPath(data)
	if p == "" {
		return false
	}
	target := AbsPath(p, cwd)
	if dir, ok := strings.CutPrefix(content, "dir:"); ok {
		return Under(target, AbsPath(dir, cwd))
	}
	var pattern string
	switch {
	case strings.HasPrefix(content, "//"):
		pattern = content[1:]
	case strings.HasPrefix(content, "/"):
		pattern = filepath.Join(orStr(cwd, "/"), content[1:])
	default:
		pattern = AbsPath(content, cwd)
	}
	if base, ok := strings.CutSuffix(pattern, "/**"); ok {
		return Under(target, filepath.Clean(base))
	}
	ok, _ := path.Match(filepath.ToSlash(filepath.Clean(pattern)), filepath.ToSlash(target))
	return ok
}

// ToolPath is the path a file tool's input names ("" when none).
func ToolPath(data map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if s := Str(data[k]); s != "" {
			return s
		}
	}
	return ""
}

// Str is v when it is a string, else "".
func Str(v any) string {
	s, _ := v.(string)
	return s
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ExpandHome turns a leading ~ into the home folder.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

// AbsPath is p made absolute against cwd ("/" when cwd is empty) and cleaned.
func AbsPath(p, cwd string) string {
	p = ExpandHome(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(orStr(cwd, "/"), p)
	}
	return filepath.Clean(p)
}

// Under reports whether p is folder or inside it.
func Under(p, folder string) bool {
	if p == folder {
		return true
	}
	rel, err := filepath.Rel(folder, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ShortPath writes a path under the home folder as ~/….
func ShortPath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && Under(p, h) {
		if rel, err := filepath.Rel(h, p); err == nil {
			if rel == "." {
				return "~"
			}
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// TruncRunes cuts s to at most n runes.
func TruncRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
