package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func TestEvalsCheck(t *testing.T) {
	data := t.TempDir()
	write := func(id, body string) {
		_ = os.MkdirAll(filepath.Join(data, "evals"), 0o700)
		if err := os.WriteFile(filepath.Join(data, "evals", id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listed := func(ids ...string) func(context.Context) []string {
		return func(context.Context) []string { return ids }
	}
	run := func(mut func(c *config.Config), models func(context.Context) []string) Check {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		cfg := config.Defaults()
		if mut != nil {
			mut(cfg)
		}
		return evalsCheck(context.Background(), Env{Cfg: cfg, DataDir: data, Models: models})
	}
	if c := run(func(c *config.Config) { c.Evals.Enabled = false }, nil); c.Status != OK || c.Detail != "off" {
		t.Fatalf("off: %+v", c)
	}
	// No evals and no judge: nothing to run yet, fine.
	if c := run(nil, nil); c.Status != OK {
		t.Fatalf("empty: %+v", c)
	}
	write("ev-a", `{"id":"ev-a","name":"A","setups":[{"label":"q","model":"qwen"}]}`)
	if c := run(nil, nil); c.Status != Warn || !strings.Contains(c.Detail, "no judge model") {
		t.Fatalf("no judge: %+v", c)
	}
	if c := run(func(c *config.Config) { c.Evals.JudgeModel = "ghost" }, listed("qwen")); c.Status != Warn || !strings.Contains(c.Detail, "ghost") {
		t.Fatalf("unknown judge: %+v", c)
	}
	// Without an engine listing a model only engines know is not flagged.
	if c := run(func(c *config.Config) { c.Evals.JudgeModel = "judge" }, nil); c.Status != OK ||
		!strings.Contains(c.Detail, "not checked without an engine listing: judge") {
		t.Fatalf("unlisted: %+v", c)
	}
	if c := run(func(c *config.Config) { c.Evals.JudgeModel = "qwen" }, listed("qwen")); c.Status != Warn || !strings.Contains(c.Detail, "judges itself in ev-a (q)") {
		t.Fatalf("self judge: %+v", c)
	}
	if c := run(func(c *config.Config) { c.Engines.DefaultModel = "judge" }, listed("qwen", "judge")); c.Status != OK || !strings.Contains(c.Detail, "judge judge, 1 eval") {
		t.Fatalf("fine: %+v", c)
	}
}
