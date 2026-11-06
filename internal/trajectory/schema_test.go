package trajectory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A small JSON Schema (2020-12) validator for the keywords the ATIF schema
// in testdata uses. A keyword it does not know fails the test, so the
// schema cannot grow past what is checked.

type schemaValidator struct {
	root map[string]any
	errs []string
}

var annotationKeys = map[string]bool{"$schema": true, "$id": true, "$defs": true, "title": true, "description": true}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/atif-v1.8.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func decodeDoc(t *testing.T, raw []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// validateSchema returns every problem of doc against schema (nil = valid).
func validateSchema(schema map[string]any, doc any) []string {
	v := &schemaValidator{root: schema}
	v.check(schema, doc, "$")
	return v.errs
}

func (v *schemaValidator) fail(path, f string, a ...any) {
	v.errs = append(v.errs, path+": "+fmt.Sprintf(f, a...))
}

func (v *schemaValidator) resolve(ref string) map[string]any {
	if ref == "#" {
		return v.root
	}
	name, ok := strings.CutPrefix(ref, "#/$defs/")
	if !ok {
		panic("unsupported $ref " + ref)
	}
	defs, _ := v.root["$defs"].(map[string]any)
	d, ok := defs[name].(map[string]any)
	if !ok {
		panic("unknown $ref " + ref)
	}
	return d
}

func typeOf(x any) string {
	switch n := x.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if _, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", x)
}

func typeOK(want string, x any) bool {
	got := typeOf(x)
	return got == want || want == "number" && got == "integer"
}

func (v *schemaValidator) check(s map[string]any, x any, path string) {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kw := s[k]
		switch k {
		case "$ref":
			v.check(v.resolve(kw.(string)), x, path)
		case "type":
			ok := false
			switch tv := kw.(type) {
			case string:
				ok = typeOK(tv, x)
			case []any:
				for _, t := range tv {
					ok = ok || typeOK(t.(string), x)
				}
			}
			if !ok {
				v.fail(path, "type %s, want %v", typeOf(x), kw)
			}
		case "enum":
			found := false
			for _, e := range kw.([]any) {
				found = found || reflect.DeepEqual(e, x)
			}
			if !found {
				v.fail(path, "%v is not one of %v", x, kw)
			}
		case "const":
			if !reflect.DeepEqual(kw, x) {
				v.fail(path, "%v, want %v", x, kw)
			}
		case "minimum":
			if n, ok := x.(json.Number); ok {
				f, _ := n.Float64()
				if f < kw.(float64) {
					v.fail(path, "%v is below %v", f, kw)
				}
			}
		case "minItems":
			if a, ok := x.([]any); ok && float64(len(a)) < kw.(float64) {
				v.fail(path, "%d items, want at least %v", len(a), kw)
			}
		case "format":
			if str, ok := x.(string); ok && kw == "date-time" {
				if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
					v.fail(path, "not a date-time: %q", str)
				}
			}
		case "required":
			if o, ok := x.(map[string]any); ok {
				for _, r := range kw.([]any) {
					if _, has := o[r.(string)]; !has {
						v.fail(path, "missing %q", r)
					}
				}
			}
		case "properties":
			if o, ok := x.(map[string]any); ok {
				props := kw.(map[string]any)
				for name, sub := range props {
					if val, has := o[name]; has {
						v.check(sub.(map[string]any), val, path+"."+name)
					}
				}
			}
		case "additionalProperties":
			o, ok := x.(map[string]any)
			if !ok {
				continue
			}
			props, _ := s["properties"].(map[string]any)
			for name := range o {
				if _, known := props[name]; known {
					continue
				}
				switch ap := kw.(type) {
				case bool:
					if !ap {
						v.fail(path, "unknown field %q", name)
					}
				case map[string]any:
					v.check(ap, o[name], path+"."+name)
				}
			}
		case "items":
			if a, ok := x.([]any); ok {
				for i, e := range a {
					v.check(kw.(map[string]any), e, fmt.Sprintf("%s[%d]", path, i))
				}
			}
		case "anyOf":
			var all []string
			ok := false
			for _, alt := range kw.([]any) {
				sub := &schemaValidator{root: v.root}
				sub.check(alt.(map[string]any), x, path)
				if len(sub.errs) == 0 {
					ok = true
					break
				}
				all = append(all, strings.Join(sub.errs, "; "))
			}
			if !ok {
				v.fail(path, "matches no alternative (%s)", strings.Join(all, " | "))
			}
		default:
			if !annotationKeys[k] {
				panic("schema keyword not supported by the test validator: " + k)
			}
		}
	}
}

// semanticProblems are the ATIF rules the reference models check in code
// (validators), not in the schema.
func semanticProblems(tr map[string]any) []string {
	var out []string
	steps, _ := tr["steps"].([]any)
	for i, s := range steps {
		st, _ := s.(map[string]any)
		if id, _ := st["step_id"].(json.Number); id.String() != strconv.Itoa(i+1) {
			out = append(out, fmt.Sprintf("steps[%d].step_id is %v, want %d", i, st["step_id"], i+1))
		}
		if st["source"] != "agent" {
			for _, f := range []string{"model_name", "reasoning_effort", "reasoning_content", "tool_calls", "metrics"} {
				if val, ok := st[f]; ok && val != nil {
					out = append(out, fmt.Sprintf("steps[%d].%s on a %v step", i, f, st["source"]))
				}
			}
		}
		if n, ok := st["llm_call_count"].(json.Number); ok && n.String() == "0" && st["source"] == "agent" {
			for _, f := range []string{"metrics", "reasoning_content"} {
				if val, ok := st[f]; ok && val != nil {
					out = append(out, fmt.Sprintf("steps[%d].%s with llm_call_count 0", i, f))
				}
			}
		}
		if ts, ok := st["timestamp"].(string); ok {
			if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
				out = append(out, fmt.Sprintf("steps[%d].timestamp %q", i, ts))
			}
		}
		ids := map[string]bool{}
		calls, _ := st["tool_calls"].([]any)
		for _, c := range calls {
			cm, _ := c.(map[string]any)
			id, _ := cm["tool_call_id"].(string)
			ids[id] = true
		}
		if obs, ok := st["observation"].(map[string]any); ok {
			res, _ := obs["results"].([]any)
			for _, r := range res {
				rm, _ := r.(map[string]any)
				if sc, ok := rm["source_call_id"].(string); ok && !ids[sc] {
					out = append(out, fmt.Sprintf("steps[%d] observation names call %q it did not make", i, sc))
				}
			}
		}
	}
	if subs, ok := tr["subagent_trajectories"].([]any); ok {
		seen := map[string]bool{}
		for i, s := range subs {
			sm, _ := s.(map[string]any)
			id, _ := sm["trajectory_id"].(string)
			if id == "" || seen[id] {
				out = append(out, fmt.Sprintf("subagent_trajectories[%d].trajectory_id missing or repeated", i))
			}
			seen[id] = true
			out = append(out, semanticProblems(sm)...)
		}
	}
	return out
}

// atifProblems is everything wrong with raw as an ATIF document.
func atifProblems(t *testing.T, raw []byte) []string {
	t.Helper()
	doc := decodeDoc(t, raw)
	ps := validateSchema(loadSchema(t), doc)
	if m, ok := doc.(map[string]any); ok {
		ps = append(ps, semanticProblems(m)...)
	}
	return ps
}

// The validator accepts the RFC's own example and rejects broken copies of
// it, so a pass on an export means something.
func TestSchemaValidatorOnTheSpecExample(t *testing.T) {
	raw, err := os.ReadFile("testdata/rfc-example.json")
	if err != nil {
		t.Fatal(err)
	}
	if ps := atifProblems(t, raw); len(ps) != 0 {
		t.Fatalf("the RFC example fails: %v", ps)
	}
	for name, mutate := range map[string]func(m map[string]any){
		"no agent":        func(m map[string]any) { delete(m, "agent") },
		"unknown field":   func(m map[string]any) { m["bogus"] = true },
		"empty steps":     func(m map[string]any) { m["steps"] = []any{} },
		"bad version":     func(m map[string]any) { m["schema_version"] = "ATIF-v9" },
		"bad source":      func(m map[string]any) { step(m, 0)["source"] = "robot" },
		"no message":      func(m map[string]any) { delete(step(m, 0), "message") },
		"string tokens":   func(m map[string]any) { step(m, 1)["metrics"].(map[string]any)["prompt_tokens"] = "5" },
		"args not object": func(m map[string]any) { call(m, 1, 0)["arguments"] = "x" },
		"step id gap":     func(m map[string]any) { step(m, 2)["step_id"] = json.Number("7") },
		"user tool calls": func(m map[string]any) { step(m, 0)["tool_calls"] = []any{} },
		"bad call ref": func(m map[string]any) {
			step(m, 1)["observation"].(map[string]any)["results"].([]any)[0].(map[string]any)["source_call_id"] = "nope"
		},
		"bad timestamp": func(m map[string]any) { step(m, 0)["timestamp"] = "yesterday" },
		"bad content part": func(m map[string]any) {
			step(m, 0)["message"] = []any{map[string]any{"type": "image", "text": "x"}}
		},
	} {
		doc := decodeDoc(t, raw).(map[string]any)
		mutate(doc)
		b, _ := json.Marshal(doc)
		if ps := atifProblems(t, b); len(ps) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func step(m map[string]any, i int) map[string]any { return m["steps"].([]any)[i].(map[string]any) }

func call(m map[string]any, i, k int) map[string]any {
	return step(m, i)["tool_calls"].([]any)[k].(map[string]any)
}
