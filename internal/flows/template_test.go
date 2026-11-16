package flows

import (
	"strings"
	"testing"
)

func TestPlaceholders(t *testing.T) {
	got := Placeholders(`Hi {input}, {steps.a-1.output} {input} {"json": 1} {} { x } {item.title}{date}`)
	want := []string{"input", "steps.a-1.output", "item.title", "date"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Placeholders = %v, want %v", got, want)
	}
}

func TestRender(t *testing.T) {
	env := renderEnv{
		input: "IN", flowName: "F", date: "2026-11-02", time: "08:00",
		outputs: map[string]any{"a": "text", "b": []any{map[string]any{"title": "x"}}, "c": 3.0},
		known:   map[string]bool{"a": true, "b": true, "c": true, "skipped": true},
		item:    map[string]string{"title": "T", "task": "K", "kind": "html"},
	}
	got, err := render(`{flow.name}:{input} {steps.a.output} {steps.b.output} {steps.c.output} [{steps.skipped.output}] {item.title}/{item.task}/{item.kind} {date} {time} {"keep": true}`, env)
	if err != nil {
		t.Fatal(err)
	}
	want := "F:IN text [\n  {\n    \"title\": \"x\"\n  }\n] 3 [] T/K/html 2026-11-02 08:00 {\"keep\": true}"
	if got != want {
		t.Fatalf("render =\n%s\nwant\n%s", got, want)
	}
	if _, err := render("{nope}", env); err == nil {
		t.Error("unknown placeholder rendered")
	}
	if _, err := render("{steps.zzz.output}", env); err == nil {
		t.Error("unknown step rendered")
	}
	env.item = nil
	if _, err := render("{item.title}", env); err == nil {
		t.Error("item outside fan-out rendered")
	}
}

func TestCheckPlaceholder(t *testing.T) {
	earlier := func(id string) bool { return id == "a" }
	for name, ok := range map[string]bool{
		"input": true, "date": true, "time": true, "flow.name": true, "steps.a.output": true,
		"steps.b.output": false, "steps.a.out": false, "item.title": false, "bogus": false,
	} {
		if got := checkPlaceholder(name, earlier, false) == ""; got != ok {
			t.Errorf("%s: ok=%v want %v", name, got, ok)
		}
	}
	if checkPlaceholder("item.kind", earlier, true) != "" {
		t.Error("item.kind refused inside fan-out")
	}
}

// Outputs reach a model as written: fan-out texts as titled sections with
// literal markup and real newlines, JSON without HTML escaping and
// indented, and the raw form on request.
func TestRenderForModels(t *testing.T) {
	html := "<html>\n  <body>Tom & Jerry</body>\n</html>"
	env := renderEnv{
		outputs: map[string]any{
			"parts": []any{html, "body { color: red; }\n"},
			"data":  map[string]any{"title": "<b>x</b>", "n": 2.0},
			"short": []any{"a", "b"},
			"reply": "Sure: {\"ok\": true, \"why\": [\"fast\", \"<cheap>\"]} done",
			"judge": map[string]any{"passed": false, "failed": []any{"tests", "style"}, "report": map[string]any{"x": 1.0}},
		},
		known: map[string]bool{"parts": true, "data": true, "short": true, "reply": true, "judge": true, "skip": true},
		items: map[string][]ItemRecord{"parts": {{Index: 0, Title: "Home page", Kind: "html"}, {Index: 1, Title: "Styles", Kind: ""}}},
	}
	got, err := render("{steps.parts.output}", env)
	if err != nil {
		t.Fatal(err)
	}
	want := "### 1. Home page (html)\n" + html + "\n\n### 2. Styles\nbody { color: red; }"
	if got != want {
		t.Fatalf("parts =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "\\u00") || strings.Contains(got, `\n`) {
		t.Fatal("parts are escaped")
	}
	got, _ = render("{steps.data.output}", env)
	if got != "{\n  \"n\": 2,\n  \"title\": \"<b>x</b>\"\n}" {
		t.Fatalf("data = %q", got)
	}
	got, _ = render("{steps.data.output.json}|{steps.parts.output.json}", env)
	if got != `{"n":2,"title":"<b>x</b>"}|["<html>\n  <body>Tom & Jerry</body>\n</html>","body { color: red; }\n"]` {
		t.Fatalf("raw = %q", got)
	}
	// without item records, one-line texts are a list
	if got, _ = render("{steps.short.output}", env); got != "- a\n- b" {
		t.Fatalf("short = %q", got)
	}
	// field paths: a judge's verdict and failures, a list element, a field
	// of the JSON inside a reply text, a missing field, a skipped step
	got, err = render("{steps.judge.output.passed}/{steps.judge.output.failed}/{steps.parts.output.1}/"+
		"{steps.reply.output.why.1}/{steps.reply.output.ok}/[{steps.judge.output.nope}]/[{steps.skip.output.x}]/[{steps.skip.output.json}]", env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "false/- tests\n- style/body { color: red; }\n/<cheap>/true/[]/[]/[]" {
		t.Fatalf("paths = %q", got)
	}
	if _, err := render("{steps.zzz.output.x}", env); err == nil {
		t.Fatal("a path into an unknown step rendered")
	}
	// min_ok: a failed item is null, and the rest still render as sections
	// with matching titles (flows-agent-4).
	env.outputs["partial"] = []any{html, nil}
	env.known["partial"] = true
	env.items["partial"] = env.items["parts"]
	got, _ = render("{steps.partial.output}", env)
	if want := "### 1. Home page (html)\n" + html + "\n\n### 2. Styles\n" + failedItemText; got != want {
		t.Fatalf("partial =\n%s\nwant\n%s", got, want)
	}
	env.outputs["none"] = []any{nil, nil}
	env.known["none"] = true
	if got, _ = render("{steps.none.output}", env); !strings.HasPrefix(got, "[") {
		t.Fatalf("all failed = %q, want JSON", got)
	}
}

func TestStepRef(t *testing.T) {
	for name, want := range map[string][2]string{
		"steps.a.output":              {"a", ""},
		"steps.a-1.output.json":       {"a-1", "json"},
		"steps.j_2.output.failed.0":   {"j_2", "failed.0"},
		"steps.a.output.x.y-z.under_": {"a", "x.y-z.under_"},
	} {
		id, path, ok := stepRef(name)
		if !ok || id != want[0] || path != want[1] {
			t.Errorf("stepRef(%q) = %q %q %v", name, id, path, ok)
		}
	}
	for _, name := range []string{"steps.a.out", "steps.a.output.", "steps.a.output..x", "steps..output", "steps.a", "step.a.output", "steps.a.outputs.x"} {
		if _, _, ok := stepRef(name); ok {
			t.Errorf("stepRef(%q) accepted", name)
		}
	}
	earlier := func(id string) bool { return id == "a" }
	if checkPlaceholder("steps.a.output.passed", earlier, false) != "" || checkPlaceholder("steps.a.output.json", earlier, false) != "" {
		t.Error("field path refused")
	}
	if checkPlaceholder("steps.b.output.passed", earlier, false) == "" || checkPlaceholder("steps.a.output.", earlier, false) == "" {
		t.Error("bad field path accepted")
	}
}
