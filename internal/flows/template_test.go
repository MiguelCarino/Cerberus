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
	want := `F:IN text [{"title":"x"}] 3 [] T/K/html 2026-11-02 08:00 {"keep": true}`
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
