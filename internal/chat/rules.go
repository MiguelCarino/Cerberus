package chat

import (
	"os"
	"path/filepath"
	"strings"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/permits"
)

// Session rules ("Allow for this session"), ported from Atrium's
// llm-chat.py session_rule / rule_matches. The rule language and its
// matching live in internal/permits (saved rules use them too); see there.

// Rule is one session rule.
type Rule = permits.Rule

// fileTools take a path.
var fileTools = permits.FileTools

// sessionRule is the rule "Allow for this session" adds for req, and the
// updatedPermissions the claude CLI gets for it. The CLI's own suggestion
// wins (its destination changed to "session": nothing is ever written to a
// settings file from here), else the narrowest: a file tool in the file's
// folder, a Bash command exactly, another tool by name. A relative path
// is the chat's (cwd): a file tool never becomes a tool-wide rule, and
// when no folder can be told, ok is false and the answer counts once.
func sessionRule(req agent.AskRequest, cwd string) (r Rule, updated []map[string]any, ok bool) {
	tool, data := req.Tool, req.Input
	for _, s := range req.Suggestions {
		if s["type"] == "addRules" && s["behavior"] == "allow" {
			rules, _ := s["rules"].([]any)
			for _, r := range rules {
				rm, ok := r.(map[string]any)
				if !ok || rm["toolName"] != tool {
					continue
				}
				content, _ := rm["ruleContent"].(string)
				wire := map[string]any{"toolName": tool}
				if content != "" {
					wire["ruleContent"] = content
				}
				return newRule(tool, content), []map[string]any{{"type": "addRules", "rules": []any{wire},
					"behavior": "allow", "destination": "session"}}, true
			}
			break
		}
	}
	if fileTools[tool] {
		for _, s := range req.Suggestions {
			if s["type"] != "addDirectories" {
				continue
			}
			dirs, _ := s["directories"].([]any)
			for _, d := range dirs {
				if ds, ok := d.(string); ok && ds != "" {
					return newRule(tool, "dir:"+ds), []map[string]any{{"type": "addDirectories",
						"directories": []any{ds}, "destination": "session"}}, true
				}
			}
		}
	}
	if fileTools[tool] {
		folder := ""
		if p := toolPath(data); p != "" {
			if filepath.IsAbs(expandHome(p)) || cwd != "" { // else relative to nothing known
				target := absPath(p, cwd)
				folder = filepath.Dir(target)
				if st, err := os.Stat(target); tool == "Glob" || tool == "Grep" || (err == nil && st.IsDir()) {
					folder = target
				}
			}
		} else if tool == "Glob" || tool == "Grep" || tool == "LS" {
			folder = cwd // no path: the working directory
		}
		if folder == "" || !filepath.IsAbs(folder) {
			return Rule{}, nil, false
		}
		return newRule(tool, "dir:"+folder), []map[string]any{{"type": "addRules", "rules": []any{
			map[string]any{"toolName": tool, "ruleContent": "/" + filepath.ToSlash(folder) + "/**"}},
			"behavior": "allow", "destination": "session"}}, true
	}
	if cmd := strings.TrimSpace(str(data["command"])); tool == "Bash" && cmd != "" {
		return newRule(tool, cmd), []map[string]any{{"type": "addRules", "rules": []any{
			map[string]any{"toolName": tool, "ruleContent": cmd}}, "behavior": "allow", "destination": "session"}}, true
	}
	return newRule(tool, ""), []map[string]any{{"type": "addRules", "rules": []any{map[string]any{"toolName": tool}},
		"behavior": "allow", "destination": "session"}}, true
}

func newRule(tool, content string) Rule { return permits.NewRule(tool, content) }

// ruleMatches reports whether rule covers a call of tool with data.
func ruleMatches(rule Rule, tool string, data map[string]any, cwd string) bool {
	return permits.Matches(rule, tool, data, cwd)
}

func toolPath(data map[string]any) string { return permits.ToolPath(data) }

func str(v any) string { return permits.Str(v) }

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func expandHome(p string) string { return permits.ExpandHome(p) }

func absPath(p, cwd string) string { return permits.AbsPath(p, cwd) }

func under(p, folder string) bool { return permits.Under(p, folder) }
