package flows

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Templates are plain text with placeholders in braces. A placeholder is
// "{" + a name of letters, digits, ".", "-" or "_" (starting with a letter)
// + "}"; any other brace is plain text, so JSON examples in a prompt stay
// as they are. Known names:
//
//	{input}                the run's input
//	{steps.<id>.output}    an earlier step's output (JSON-encoded when it is
//	                       not a string; "" when that step was skipped by an if)
//	{item.title} {item.task} {item.kind}
//	                       the current item, inside a fan_out template only
//	{date} {time}          local date (2006-01-02) and time (15:04) at render
//	{flow.name}            the flow's name
//
// No code runs. An unknown name is a validation problem at save time and
// an error at render time; it never renders as empty.

// placeholder is one {name} found in a template.
type placeholder struct {
	start, end int // byte offsets of "{" and one past "}"
	name       string
}

func isNameByte(c byte, first bool) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
		return true
	}
	if first {
		return false
	}
	return c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_'
}

// placeholders lists the {name} placeholders of t in order.
func placeholders(t string) []placeholder {
	var out []placeholder
	for i := 0; i < len(t); i++ {
		if t[i] != '{' {
			continue
		}
		j := i + 1
		for j < len(t) && isNameByte(t[j], j == i+1) {
			j++
		}
		if j > i+1 && j < len(t) && t[j] == '}' {
			out = append(out, placeholder{start: i, end: j + 1, name: t[i+1 : j]})
			i = j
		}
	}
	return out
}

// Placeholders lists the distinct placeholder names used in t, in order
// of first use (for the designer).
func Placeholders(t string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range placeholders(t) {
		if !seen[p.name] {
			seen[p.name] = true
			out = append(out, p.name)
		}
	}
	return out
}

// stepRef is the step id of a "steps.<id>.output" name, or "".
func stepRef(name string) string {
	rest, ok := strings.CutPrefix(name, "steps.")
	if !ok {
		return ""
	}
	id, ok := strings.CutSuffix(rest, ".output")
	if !ok || id == "" {
		return ""
	}
	return id
}

// checkPlaceholder says what is wrong with a placeholder name in a step's
// template, or "". earlier says whether a step id comes before the step;
// inItem whether {item.*} is allowed.
func checkPlaceholder(name string, earlier func(string) bool, inItem bool) string {
	switch name {
	case "input", "date", "time", "flow.name":
		return ""
	case "item.title", "item.task", "item.kind":
		if inItem {
			return ""
		}
		return fmt.Sprintf("{%s} is only known inside a fan-out template", name)
	}
	if id := stepRef(name); id != "" {
		if earlier(id) {
			return ""
		}
		return fmt.Sprintf("{%s}: %q is not an earlier step", name, id)
	}
	return fmt.Sprintf("{%s} is not a known placeholder", name)
}

// renderEnv holds what a template can see.
type renderEnv struct {
	input    string
	flowName string
	date     string
	time     string
	outputs  map[string]any  // step id → output (generic JSON form)
	known    map[string]bool // step ids of the flow
	item     map[string]string
}

// render fills t from env. An unknown placeholder is an error.
func render(t string, env renderEnv) (string, error) {
	ps := placeholders(t)
	if len(ps) == 0 {
		return t, nil
	}
	var b strings.Builder
	last := 0
	for _, p := range ps {
		b.WriteString(t[last:p.start])
		last = p.end
		v, err := env.value(p.name)
		if err != nil {
			return "", err
		}
		b.WriteString(v)
	}
	b.WriteString(t[last:])
	return b.String(), nil
}

func (env renderEnv) value(name string) (string, error) {
	switch name {
	case "input":
		return env.input, nil
	case "date":
		return env.date, nil
	case "time":
		return env.time, nil
	case "flow.name":
		return env.flowName, nil
	case "item.title", "item.task", "item.kind":
		if env.item == nil {
			return "", fmt.Errorf("{%s} is only known inside a fan-out template", name)
		}
		return env.item[strings.TrimPrefix(name, "item.")], nil
	}
	if id := stepRef(name); id != "" && env.known[id] {
		return outputText(env.outputs[id]), nil
	}
	return "", fmt.Errorf("{%s} is not a known placeholder", name)
}

// outputText is an output as template text: a string as is, nil as "",
// anything else JSON-encoded.
func outputText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
