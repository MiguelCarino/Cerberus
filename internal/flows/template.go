package flows

import (
	"bytes"
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
//	{steps.<id>.output}    an earlier step's output, written for a model to
//	                       read: a string as it is; a fan-out's list of
//	                       texts as titled sections ("### 1. <title>
//	                       (<kind>)" then the text); anything else as
//	                       indented JSON without HTML escaping. "" when
//	                       that step was skipped by an if.
//	{steps.<id>.output.json}
//	                       the output as compact JSON (no HTML escaping)
//	{steps.<id>.output.<dotted.path>}
//	                       one field of the output (the JSON in a reply
//	                       text is parsed first; numbers index lists), in
//	                       the same form as a whole output; "" when the
//	                       field is missing
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

// stepRef parses a step output placeholder: "steps.<id>.output",
// "steps.<id>.output.json" or "steps.<id>.output.<dotted.path>". It
// answers the step id and the path ("" for the whole output, "json" for
// the raw JSON form); ok is false for any other name.
func stepRef(name string) (id, path string, ok bool) {
	rest, found := strings.CutPrefix(name, "steps.")
	if !found {
		return "", "", false
	}
	id, after, found := strings.Cut(rest, ".")
	if !found || id == "" {
		return "", "", false
	}
	if after == "output" {
		return id, "", true
	}
	path, found = strings.CutPrefix(after, "output.")
	if !found || path == "" {
		return "", "", false
	}
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			return "", "", false
		}
	}
	return id, path, true
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
	if id, _, ok := stepRef(name); ok {
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
	// items are the fan-out steps' item records (titles and kinds for
	// the sections of {steps.<id>.output}).
	items map[string][]ItemRecord
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
	if id, path, ok := stepRef(name); ok && env.known[id] {
		v := env.outputs[id]
		switch path {
		case "":
			return templateText(v, env.items[id]), nil
		case "json":
			if v == nil {
				return "", nil
			}
			return encodeJSON(v, false), nil
		}
		if v == nil {
			return "", nil
		}
		got, found := lookup(parsed(v), path)
		if !found {
			return "", nil
		}
		return templateText(got, nil), nil
	}
	return "", fmt.Errorf("{%s} is not a known placeholder", name)
}

// OutputText is a step record's output as {steps.<id>.output} renders
// it: what a model or a person reads (an MCP client's cerberus_flow_run).
func OutputText(rec StepRecord) string { return templateText(rec.Output, rec.Items) }

// outputText is an output as plain text: a string as is, nil as "",
// anything else compact JSON. The if tests and plan item fields use it, so
// it stays as it was; templates use templateText.
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

// templateText is an output written for a model to read. A string is
// itself. A list of strings (a fan-out's results) becomes sections, one
// per item, titled from items when they match ("### 2. Styles (css)",
// then the text); without item records a list of one-line strings is a
// "- " list. A null in that list (an item that failed under min_ok) keeps
// its place as failedItemText, so the titles still match. Anything else is
// indented JSON without HTML escaping, so markup and "&" reach the model
// as written.
func templateText(v any, items []ItemRecord) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		if strs, ok := allStrings(x); ok && len(strs) > 0 {
			return sections(strs, items)
		}
	}
	return encodeJSON(v, true)
}

// failedItemText stands in for a fan-out item that failed (null in the
// output under min_ok).
const failedItemText = "(this item failed)"

// allStrings is list as strings when every entry is a string or null (a
// failed item, as failedItemText) and at least one is a string.
func allStrings(list []any) ([]string, bool) {
	out := make([]string, len(list))
	some := false
	for i, x := range list {
		switch s := x.(type) {
		case string:
			out[i], some = s, true
		case nil:
			out[i] = failedItemText
		default:
			return nil, false
		}
	}
	return out, some
}

func sections(strs []string, items []ItemRecord) string {
	titled := len(items) == len(strs)
	if !titled {
		short := true
		for _, s := range strs {
			if strings.ContainsAny(strings.TrimSpace(s), "\n") {
				short = false
				break
			}
		}
		if short {
			lines := make([]string, len(strs))
			for i, s := range strs {
				lines[i] = "- " + strings.TrimSpace(s)
			}
			return strings.Join(lines, "\n")
		}
	}
	var b strings.Builder
	for i, s := range strs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "### %d.", i+1)
		if titled {
			if t := strings.TrimSpace(items[i].Title); t != "" {
				b.WriteString(" " + t)
			}
			if k := strings.TrimSpace(items[i].Kind); k != "" {
				b.WriteString(" (" + k + ")")
			}
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(s, "\n"))
	}
	return b.String()
}

// encodeJSON is v as JSON without HTML escaping, indented or compact.
func encodeJSON(v any, indent bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimRight(buf.String(), "\n")
}
