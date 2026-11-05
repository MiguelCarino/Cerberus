package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// scriptAsker answers cards from a list and records them; rules answer
// covered requests by tool name.
type scriptAsker struct {
	mu      sync.Mutex
	answers []Decision
	asked   []AskRequest
	rules   map[string]bool
}

func (s *scriptAsker) Ask(_ context.Context, req AskRequest) Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, req)
	if len(s.answers) == 0 {
		return Decision{}
	}
	d := s.answers[0]
	s.answers = s.answers[1:]
	return d
}

func (s *scriptAsker) Covered(req AskRequest) (Decision, bool) {
	if s.rules[req.Tool] {
		return Decision{Allow: true}, true
	}
	return Decision{}, false
}

func engineOptions(t *testing.T, base string, mode Mode, a Asker) RunOptions {
	t.Helper()
	return RunOptions{Base: base, Model: "m", Cwd: t.TempDir(), Mode: mode, MaxSteps: 10,
		Messages: []Message{{Role: "user", Content: "go"}}, DenyPaths: config.Defaults().Agent.DenyPaths,
		CommandTimeout: 30 * time.Second, Asker: a, Refusal: func(n string) string { return "denied:" + n },
		Policy: NewPolicy(config.Defaults().Agent.Policy)}
}

func collect(evs *[]Event) func(Event) {
	var mu sync.Mutex
	return func(e Event) {
		mu.Lock()
		*evs = append(*evs, e)
		mu.Unlock()
	}
}

func wire(t *testing.T, evs []Event) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range evs {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestRunEventsWireShape(t *testing.T) {
	_, base := newFake(t,
		sse(append([]string{`{"choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}`, content("I'll look.")},
			toolChunks([2]string{"list_dir", `{"path":"."}`}, [2]string{"read_file", `{bad`})...)...),
		sse(content("<think>x</think>Done.")),
	)
	var evs []Event
	o := engineOptions(t, base, ModeAsk, nil)
	var saves int
	o.OnSave = func(string, []Message) { saves++ }
	res := Run(context.Background(), o, collect(&evs))
	if res.Err != nil || !res.Done.OK || res.Done.Steps != 2 || res.Done.Calls != 2 {
		t.Fatalf("result %+v", res)
	}
	w := wire(t, evs)
	var kinds []string
	for _, m := range w {
		kinds = append(kinds, m["type"].(string))
	}
	want := "start reasoning text usage tool tool_done tool tool_done start reasoning text done"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("events %s", strings.Join(kinds, " "))
	}
	if w[0]["step"] != 0.0 || w[3]["prompt"] != 10.0 || w[3]["completion"] != 5.0 {
		t.Errorf("start/usage %v %v", w[0], w[3])
	}
	tool := w[4]
	if tool["id"] != "call_0" || tool["name"] != "list_dir" || tool["canon"] != "LS" || tool["input"].(map[string]any)["path"] != "." {
		t.Errorf("tool %v", tool)
	}
	if w[5]["is_error"] != false || w[7]["is_error"] != true || !strings.Contains(w[7]["output"].(string), "not valid JSON") {
		t.Errorf("tool_done %v %v", w[5], w[7])
	}
	done := w[len(w)-1]
	for _, k := range []string{"ok", "stopped", "error", "steps", "seconds", "usage", "tools"} {
		if _, ok := done[k]; !ok {
			t.Errorf("done lacks %s: %v", k, done)
		}
	}
	if len(res.Messages) != 5 || res.Messages[4].Content != "Done." || saves < 3 {
		t.Errorf("messages %d saves %d", len(res.Messages), saves)
	}
}

func TestRunModes(t *testing.T) {
	cmd := func(c string) [2]string { return [2]string{"run_command", `{"command":` + jsonStr(c) + `}`} }
	t.Run("auto", func(t *testing.T) {
		f, base := newFake(t, sse(toolChunks(cmd("ls"), cmd("git status"), cmd("touch x"), cmd("sudo ls"))...), sse(content("ok")))
		a := &scriptAsker{answers: []Decision{{Note: "no"}, {Note: "no"}}}
		res := Run(context.Background(), engineOptions(t, base, ModeAuto, a), nil)
		if res.Err != nil || len(a.asked) != 2 || a.asked[0].Input["command"] != "touch x" || a.asked[1].Input["command"] != "sudo ls" || !a.asked[1].Forced {
			t.Fatalf("asked %+v", a.asked)
		}
		if tm := toolMessages(f.requests()[1]); tm[2] != "denied:no" {
			t.Errorf("tool messages %q", tm)
		}
	})
	t.Run("acceptEdits", func(t *testing.T) {
		_, base := newFake(t, sse(toolChunks([2]string{"write_file", `{"path":"a.txt","content":"a"}`}, cmd("echo hi"))...), sse(content("ok")))
		a := &scriptAsker{answers: []Decision{{Allow: true}}}
		o := engineOptions(t, base, ModeAcceptEdits, a)
		Run(context.Background(), o, nil)
		if len(a.asked) != 1 || a.asked[0].Tool != "Bash" {
			t.Fatalf("asked %+v", a.asked)
		}
		if b, _ := os.ReadFile(filepath.Join(o.Cwd, "a.txt")); string(b) != "a" {
			t.Errorf("edit not made: %q", b)
		}
	})
	t.Run("plan", func(t *testing.T) {
		f, base := newFake(t, sse(toolChunks([2]string{"write_file", `{"path":"a.txt","content":"a"}`})...), sse(content("plan")))
		a := &scriptAsker{}
		o := engineOptions(t, base, ModePlan, a)
		o.System = func(tools bool) string { return "sys" }
		Run(context.Background(), o, nil)
		if len(a.asked) != 0 {
			t.Errorf("asked in plan mode: %+v", a.asked)
		}
		if names := toolNamesIn(f.requests()[0]); strings.Join(names, ",") != "read_file,list_dir,search" {
			t.Errorf("plan tools %v", names)
		}
		if tm := toolMessages(f.requests()[1]); !strings.Contains(tm[0], "plan mode") {
			t.Errorf("tool messages %q", tm)
		}
	})
	t.Run("bypass keeps the policy", func(t *testing.T) {
		f, base := newFake(t, sse(toolChunks(cmd("rm -rf /"), cmd("git push origin main"), cmd("echo fine"))...), sse(content("ok")))
		a := &scriptAsker{rules: map[string]bool{"Bash": true}, answers: []Decision{{Note: "no push"}}}
		Run(context.Background(), engineOptions(t, base, ModeBypass, a), nil)
		// rm -rf / is denied without a card; git push is asked although a
		// rule covers Bash (the policy forces it); echo runs.
		if len(a.asked) != 1 || a.asked[0].Input["command"] != "git push origin main" || !a.asked[0].Forced {
			t.Fatalf("asked %+v", a.asked)
		}
		tm := toolMessages(f.requests()[1])
		if !strings.Contains(tm[0], "Refused by the command policy") || tm[1] != "denied:no push" || tm[2] != "fine" {
			t.Errorf("tool messages %q", tm)
		}
	})
	t.Run("reads outside ask", func(t *testing.T) {
		outside := t.TempDir()
		os.WriteFile(filepath.Join(outside, "f.txt"), []byte("secret-ish"), 0o644)
		_, base := newFake(t, sse(toolChunks([2]string{"read_file", `{"path":` + jsonStr(filepath.Join(outside, "f.txt")) + `}`},
			[2]string{"list_dir", `{"path":"."}`})...), sse(content("ok")))
		a := &scriptAsker{answers: []Decision{{}}}
		o := engineOptions(t, base, ModeAsk, a)
		o.AskOutside = true
		var evs []Event
		Run(context.Background(), o, collect(&evs))
		if len(a.asked) != 1 || a.asked[0].Tool != "Read" || !strings.Contains(a.asked[0].Reason, "outside") {
			t.Fatalf("asked %+v", a.asked)
		}
		var asks int
		for _, e := range evs {
			if e.Type == EvAsk {
				asks++
			}
		}
		if asks != 1 {
			t.Errorf("%d ask events", asks)
		}
	})
	t.Run("no asker denies", func(t *testing.T) {
		f, base := newFake(t, sse(toolChunks(cmd("touch y"))...), sse(content("ok")))
		Run(context.Background(), engineOptions(t, base, ModeAsk, nil), nil)
		if tm := toolMessages(f.requests()[1]); !strings.HasPrefix(tm[0], "denied:") {
			t.Errorf("tool messages %q", tm)
		}
	})
}

func TestRunNoToolsAndLimits(t *testing.T) {
	f, base := newFake(t, sse(content("plain")))
	temp := 0.3
	o := engineOptions(t, base, ModePlan, nil)
	o.NoTools, o.MaxSteps, o.MaxTokens, o.Temperature, o.Client = true, 0, 400, &temp, "cerberus-quick"
	res := Run(context.Background(), o, nil)
	req := f.requests()[0]
	if res.Err != nil || req["tools"] != nil || req["max_tokens"] != 400.0 || req["temperature"] != 0.3 || res.Done.Tools {
		t.Fatalf("request %v result %+v", req, res)
	}
	if got := f.headers[0].Get("X-Cerberus-Client"); got != "cerberus-quick" {
		t.Errorf("client header %q", got)
	}
	for _, m := range req["messages"].([]any) {
		if strings.Contains(m.(map[string]any)["content"].(string), "step limit") {
			t.Error("a tool-less run got the step-limit nudge")
		}
	}

	big := strings.Repeat("x", 5000)
	f2, base2 := newFake(t, sse(toolChunks([2]string{"run_command", `{"command":"printf ` + big + `"}`})...), sse(content("ok")))
	o2 := engineOptions(t, base2, ModeBypass, nil)
	o2.ResultClip = 1000
	Run(context.Background(), o2, nil)
	if tm := toolMessages(f2.requests()[1]); len(tm[0]) > 1100 || !strings.Contains(tm[0], "characters cut") {
		t.Errorf("result not clipped: %d chars", len(tm[0]))
	}
}

func TestRunStopKeepsPartialReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, base := newFake(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + content("half a ") + "\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second) // cut by the client
	})
	var evs []Event
	add := collect(&evs)
	res := Run(ctx, engineOptions(t, base, ModeAsk, nil), func(e Event) {
		add(e)
		if e.Type == EvText {
			cancel()
		}
	})
	if !res.Done.Stopped || res.Done.OK || res.Err == nil {
		t.Fatalf("result %+v", res.Done)
	}
	if last := res.Messages[len(res.Messages)-1]; last.Role != "assistant" || last.Content != "half a " {
		t.Errorf("messages %+v", res.Messages)
	}
	if evs[len(evs)-1].Type != EvDone {
		t.Errorf("last event %s", evs[len(evs)-1].Type)
	}
}

func TestSafeCommand(t *testing.T) {
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("x"), 0o644)
	inside := func(p string) bool { return under(p, cwd) }
	cases := map[string]bool{
		"ls":                       true,
		"ls -la":                   true,
		"cat notes.txt":            true,
		"cat 'notes.txt'":          true,
		"git status":               true,
		"git log --oneline -5":     true,
		"git -C . diff":            false, // as Atrium: the first non-flag word must be the subcommand
		"git push":                 false,
		"git diff --output=x":      false,
		"git -c core.pager=x log":  false,
		"find . -name x":           true,
		"find . -delete":           false,
		"date":                     true,
		"date -s 2020-01-01":       false,
		"ls; rm x":                 false,
		"cat notes.txt | head":     false,
		"echo $HOME":               false,
		"ls *.txt":                 false,
		"cat /etc/passwd":          false, // outside the folder
		"cat ../x":                 false,
		"rm notes.txt":             false,
		"sort -o out notes.txt":    false,
		"cat \"unterminated":       false,
		"":                         false,
		"tee x":                    false,
		"head --lines=3 notes.txt": true,
	}
	for c, want := range cases {
		if got := SafeCommand(c, cwd, inside); got != want {
			t.Errorf("SafeCommand(%q) = %v, want %v", c, got, want)
		}
	}
	if SafeCommand("ls", cwd, nil) {
		t.Error("nil readable must refuse (the folder itself is not readable)")
	}
}

func TestCanonAndModes(t *testing.T) {
	n, in := Canon("edit_file", map[string]any{"path": "a", "old_string": "x", "new_string": "y", "replace_all": true})
	if n != "Edit" || in["file_path"] != "a" || in["replace_all"] != true {
		t.Errorf("edit %s %v", n, in)
	}
	if n, in := Canon("search", map[string]any{"pattern": "p"}); n != "Grep" || len(in) != 1 {
		t.Errorf("search %s %v", n, in)
	}
	if n, in := Canon("run_command", map[string]any{"command": "ls", "timeout": 5.0}); n != "Bash" || len(in) != 1 {
		t.Errorf("bash %s %v", n, in)
	}
	if n, _ := Canon("cerberus_spawn", map[string]any{"task": "t"}); n != "cerberus_spawn" {
		t.Errorf("other %s", n)
	}
	for s, want := range map[string]Mode{"": ModeAsk, "default": ModeAsk, "AUTO": ModeAuto, "accept-edits": ModeAcceptEdits, "plan": ModePlan} {
		if got, ok := ParseMode(s); !ok || got != want {
			t.Errorf("ParseMode(%q) = %v %v", s, got, ok)
		}
	}
	if _, ok := ParseMode("yolo"); ok {
		t.Error("unknown mode parsed")
	}
	raw, _ := json.Marshal(Event{Type: EvAsk, Req: &AskRequest{ID: "r", Tool: "Bash", Args: map[string]any{"secret": 1}}})
	if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), `"req":{"id":"r"`) {
		t.Errorf("ask wire %s", raw)
	}
}
