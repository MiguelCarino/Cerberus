package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/inbox"
)

// flowDaemon is the real API over a real flow engine whose dependencies
// are the real FlowHost with a fake gateway (it answers every chat
// completion with "re: " and the prompt) and a temp inbox. No model runs.
type flowDaemon struct {
	cfgPath string
	eng     *flows.Engine
}

func newFlowDaemon(t *testing.T) *flowDaemon {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	cfg := config.Defaults()
	cfg.Server.Token = "tok"
	cfg.Notify.Disabled = true
	ib, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ib.Close() })
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []flows.Message `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		last := ""
		if n := len(body.Messages); n > 0 {
			last = body.Messages[n-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "m",
			"choices": []any{map[string]any{"message": map[string]string{"content": "re: " + last}}},
			"usage":   map[string]int{"prompt_tokens": 3, "completion_tokens": 2}})
	})
	feed := api.NewFlowFeed()
	host := api.NewFlowHost(api.FlowHostDeps{Config: func() *config.Config { return cfg }, Gateway: gw, Inbox: ib, Feed: feed,
		MemAvail: func() int { return -1 }})
	eng, err := flows.Open(host.Options(filepath.Join(t.TempDir(), "flows")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	srv := httptest.NewServer(api.New(api.Deps{Cfg: cfg, Log: slog.New(slog.DiscardHandler), Version: "test",
		Inbox: ib, Flows: eng, FlowFeed: feed}))
	t.Cleanup(srv.Close)
	return &flowDaemon{cfgPath: writeConfig(t, strings.TrimPrefix(srv.URL, "http://")), eng: eng}
}

func (d *flowDaemon) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append(append([]string{"flow"}, args...), "-config", d.cfgPath), nil, &out, &errb)
	return code, out.String(), errb.String()
}

func (d *flowDaemon) create(t *testing.T, js string) flows.Flow {
	t.Helper()
	var f flows.Flow
	if err := json.Unmarshal([]byte(js), &f); err != nil {
		t.Fatal(err)
	}
	out, _, err := d.eng.Create("test", f)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFlowCommands(t *testing.T) {
	d := newFlowDaemon(t)

	if code, out, errs := d.run(t, "list"); code != 0 || !strings.Contains(out, "No flows yet") {
		t.Fatalf("empty list: %d %q %q", code, out, errs)
	}
	plain := d.create(t, `{"name":"Echo","description":"Says it back","trigger":{"kind":"schedule","schedule":"daily 08:00"},
	  "enabled":true,"input_hint":"Words","steps":[{"id":"say","kind":"prompt","model":"default","template":"Say: {input}"}]}`)
	gate := d.create(t, `{"name":"Gate","trigger":{"kind":"manual"},"steps":[
	  {"id":"a","kind":"prompt","model":"default","template":"{input}"},
	  {"id":"ok","kind":"approve","message":"Ship {input}?"},
	  {"id":"done","kind":"output","to":"inbox","template":"shipped"}]}`)

	code, out, errs := d.run(t, "list")
	if code != 0 || !strings.Contains(out, plain.ID) || !strings.Contains(out, "Daily at 08:00") || !strings.Contains(out, gate.ID) ||
		!strings.Contains(out, "never run") {
		t.Fatalf("list: %d %q %q", code, out, errs)
	}
	code, out, _ = d.run(t, "show", plain.ID)
	if code != 0 || !strings.Contains(out, "Echo (version 1)") || !strings.Contains(out, "1. say") ||
		!strings.Contains(out, "schedule on") || !strings.Contains(out, "#/flows/"+plain.ID) {
		t.Fatalf("show: %d %q", code, out)
	}
	if code, _, errs = d.run(t, "show", "fl-nope"); code != 1 || !strings.Contains(errs, "no such flow") {
		t.Fatalf("show unknown: %d %q", code, errs)
	}

	// run --wait follows the event stream to the end and prints the result.
	code, out, errs = d.run(t, "run", plain.ID, "--input", "hello", "--wait")
	if code != 0 || !strings.Contains(out, "Started fr-") || !strings.Contains(out, " passed") || !strings.Contains(out, "re: Say: hello") {
		t.Fatalf("run --wait: %d %q %q", code, out, errs)
	}
	// A flow ending in a file output names the file it wrote.
	file := d.create(t, `{"name":"Saver","trigger":{"kind":"manual"},"steps":[
	  {"id":"save","kind":"output","to":"file","file":"note.md","template":"{input}"}]}`)
	if code, out, errs := d.run(t, "run", file.ID, "--input", "kept", "--wait"); code != 0 ||
		!strings.Contains(out, "Wrote files/note.md in the run folder (4 bytes)") {
		t.Fatalf("run --wait file: %d %q %q", code, out, errs)
	}
	runs := d.eng.Runs(plain.ID, 0)
	if len(runs) != 1 || runs[0].Actor != audit.ActorCLI || runs[0].Input != "hello" {
		t.Fatalf("runs %+v", runs)
	}
	// Without --wait it only starts.
	if code, out, _ = d.run(t, "run", plain.ID); code != 0 || !strings.Contains(out, "#/flows/runs/fr-") {
		t.Fatalf("run: %d %q", code, out)
	}

	// An approval: --wait stops there with exit 0; approve lets it finish.
	code, out, errs = d.run(t, "run", gate.ID, "--input", "v2", "--wait")
	if code != 0 || !strings.Contains(out, "Waiting for approval: Ship v2?") {
		t.Fatalf("run gate: %d %q %q", code, out, errs)
	}
	id := d.eng.Runs(gate.ID, 1)[0].ID
	if code, out, errs = d.run(t, "approve", id, "--note", "fine"); code != 0 || !strings.Contains(out, "approved") {
		t.Fatalf("approve: %d %q %q", code, out, errs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if r, err := d.eng.Wait(ctx, id); err != nil || r.State != flows.StatePassed || r.Approval == nil || r.Approval.By != audit.ActorCLI {
		t.Fatalf("after approve: %+v %v", r, err)
	}
	// Approving again is a conflict.
	if code, _, errs = d.run(t, "approve", id); code != 1 || errs == "" {
		t.Fatalf("approve twice: %d %q", code, errs)
	}

	// Reject.
	if code, _, _ = d.run(t, "run", gate.ID, "--wait"); code != 0 {
		t.Fatalf("second gate run: %d", code)
	}
	id = d.eng.Runs(gate.ID, 1)[0].ID
	if code, out, _ = d.run(t, "approve", id, "--reject"); code != 0 || !strings.Contains(out, "rejected") {
		t.Fatalf("reject: %d %q", code, out)
	}

	// A third one, canceled while waiting.
	if code, _, _ = d.run(t, "run", gate.ID, "--wait"); code != 0 {
		t.Fatalf("third gate run: %d", code)
	}
	id = d.eng.Runs(gate.ID, 1)[0].ID
	if code, out, _ = d.run(t, "cancel", id); code != 0 || !strings.Contains(out, "canceled") {
		t.Fatalf("cancel: %d %q", code, out)
	}
	if code, _, errs = d.run(t, "cancel", id); code != 1 || !strings.Contains(errs, "state") {
		t.Fatalf("cancel twice: %d %q", code, errs)
	}

	code, out, _ = d.run(t, "runs", gate.ID)
	for _, want := range []string{"passed", "rejected", "canceled", "Gate"} {
		if !strings.Contains(out, want) {
			t.Errorf("runs output lacks %q:\n%s", want, out)
		}
	}
	if code != 0 || strings.Contains(out, "Echo") {
		t.Fatalf("runs of one flow: %d\n%s", code, out)
	}
	if code, out, _ = d.run(t, "runs", "--limit", "1"); code != 0 || strings.Count(out, "\n") != 1 {
		t.Fatalf("runs --limit 1: %d %q", code, out)
	}
	code, out, _ = d.run(t, "list")
	if code != 0 || !strings.Contains(out, "last canceled") && !strings.Contains(out, "last passed") {
		t.Fatalf("list after runs: %q", out)
	}
}

func TestFlowCommandUsage(t *testing.T) {
	d := newFlowDaemon(t)
	for _, args := range [][]string{{"bogus"}, {"show"}, {"run"}, {"cancel"}, {"approve"}, {"runs", "a", "b"}, {"list", "x"}} {
		if code, _, _ := d.run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	// Help: the overview and each subcommand.
	for _, args := range [][]string{{"flow", "-h"}, {"help", "flow"}, {"flows", "--help"}} {
		var out bytes.Buffer
		if code := run(args, nil, &out, &out); code != 0 || !strings.Contains(out.String(), "Usage: cerberus flow list | show ID") {
			t.Errorf("%v: %d\n%s", args, code, out.String())
		}
	}
	for _, sub := range []string{"list", "show", "run", "runs", "cancel", "approve"} {
		for _, args := range [][]string{{"flow", sub, "-h"}, {"help", "flow", sub}} {
			var out bytes.Buffer
			if code := run(args, nil, &out, &out); code != 0 || !strings.Contains(out.String(), "Usage: cerberus flow "+sub) {
				t.Errorf("%v: %d\n%s", args, code, out.String())
			}
		}
	}
	var overview bytes.Buffer
	run([]string{"help"}, nil, &overview, &overview)
	if !strings.Contains(overview.String(), "\n  flow list") || !strings.Contains(overview.String(), "\n  flow approve RUN") {
		t.Errorf("overview lacks the flow commands:\n%s", overview.String())
	}
	// No daemon: a clear error, exit 1.
	dead := writeConfig(t, "127.0.0.1:1")
	var out, errb bytes.Buffer
	if code := run([]string{"flow", "list", "-config", dead}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "cannot reach cerberus") {
		t.Fatalf("no daemon: %d %q", code, errb.String())
	}
}

// TestFlowRunOutputIsCleaned: model output, approval text and reasons
// reach the terminal with their control characters made harmless, so a
// reply cannot write the clipboard (OSC 52) or move the cursor.
func TestFlowRunOutputIsCleaned(t *testing.T) {
	d := newFlowDaemon(t)
	echo := d.create(t, `{"name":"Echo","trigger":{"kind":"manual"},"steps":[{"id":"say","kind":"prompt","model":"default","template":"{input}"}]}`)
	gate := d.create(t, `{"name":"Gate","trigger":{"kind":"manual"},"steps":[
	  {"id":"a","kind":"prompt","model":"default","template":"{input}"},
	  {"id":"ok","kind":"approve","message":"Ship {steps.a.output}?"}]}`)
	evil := "\x1b]52;c;ZWNobyBoaQ==\x07\x1b[2K\x1b[1Apassed\u009b31m\nsecond\tline"
	code, out, errs := d.run(t, "run", echo.ID, "--input", evil, "--wait")
	if code != 0 || !strings.Contains(out, "second\tline") {
		t.Fatalf("run --wait: %d %q %q", code, out, errs)
	}
	code2, out2, errs2 := d.run(t, "run", gate.ID, "--input", evil, "--wait")
	if code2 != 0 || !strings.Contains(out2, "Waiting for approval") {
		t.Fatalf("gate: %d %q %q", code2, out2, errs2)
	}
	code3, out3, _ := d.run(t, "runs")
	if code3 != 0 {
		t.Fatalf("runs: %d", code3)
	}
	for _, s := range []string{out, out2, out3} {
		if strings.ContainsAny(s, "\x1b\x07\u009b") {
			t.Fatalf("control characters reached the terminal: %q", s)
		}
	}
}
