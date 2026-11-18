package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toolTurn is a reply that makes one tool call.
func toolTurn(name, arguments string) func(http.ResponseWriter, map[string]any) {
	return sse(toolChunks([2]string{name, arguments})...)
}

func writeArgs(content string) string {
	return fmt.Sprintf(`{"path":"index.html","content":%s}`, jsonStr(content))
}

func notes(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type == EvNote {
			out = append(out, e.Text)
		}
	}
	return out
}

// lastTool is the content of the last tool message of a request.
func lastTool(t *testing.T, req map[string]any) string {
	t.Helper()
	tm := toolMessages(req)
	if len(tm) == 0 {
		t.Fatal("no tool messages")
	}
	return tm[len(tm)-1]
}

func TestRepeatGuardStopsIdenticalWrites(t *testing.T) {
	var replies []func(http.ResponseWriter, map[string]any)
	for range 8 {
		replies = append(replies, toolTurn("write_file", writeArgs("<p>same</p>")))
	}
	f, base := newFake(t, replies...)
	var evs []Event
	res := Run(context.Background(), engineOptions(t, base, ModeBypass, nil), collect(&evs))
	if !errors.Is(res.Err, ErrRepeating) || res.Done.OK || !strings.Contains(res.Done.Error, "kept repeating the same action") {
		t.Fatalf("result %+v err %v", res.Done, res.Err)
	}
	reqs := f.requests()
	// The first write creates; writes 2 to 5 repeat; the fifth ends the run.
	if len(reqs) != 5 {
		t.Fatalf("%d requests, want 5", len(reqs))
	}
	second := lastTool(t, reqs[2])
	if !strings.HasPrefix(second, unchangedPrefix) || !strings.Contains(second, "You are repeating yourself") {
		t.Errorf("second write's result: %q", second)
	}
	if third := lastTool(t, reqs[3]); strings.Contains(third, "You are repeating yourself") {
		t.Errorf("the note is given once: %q", third)
	}
	ns := notes(evs)
	if len(ns) != 2 || !strings.Contains(ns[0], "told to change course") || !strings.Contains(ns[1], "the run is stopped") {
		t.Errorf("notes %q", ns)
	}
	// Every call has its result: the conversation is valid for a resume.
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "tool" || len(res.Messages) != 1+5*2 {
		t.Errorf("messages %d, last %+v", len(res.Messages), last)
	}
}

func TestRepeatGuardStopsOscillation(t *testing.T) {
	// The owner's run: the same file rewritten with two versions in turn.
	var replies []func(http.ResponseWriter, map[string]any)
	for i := range 10 {
		replies = append(replies, toolTurn("write_file", writeArgs(fmt.Sprintf("<p>v%d</p>", i%2))))
	}
	f, base := newFake(t, replies...)
	res := Run(context.Background(), engineOptions(t, base, ModeBypass, nil), nil)
	if !errors.Is(res.Err, ErrRepeating) {
		t.Fatalf("err %v", res.Err)
	}
	// A, B are new; A, B, A, B repeat (the third call gets the note).
	reqs := f.requests()
	if len(reqs) != 6 {
		t.Fatalf("%d requests, want 6", len(reqs))
	}
	if third := lastTool(t, reqs[3]); !strings.HasPrefix(third, "replaced") || !strings.Contains(third, "repeating yourself") {
		t.Errorf("third result %q", third)
	}
}

func TestRepeatGuardStopsRereads(t *testing.T) {
	var replies []func(http.ResponseWriter, map[string]any)
	for range 8 {
		replies = append(replies, toolTurn("list_dir", `{}`))
	}
	f, base := newFake(t, replies...)
	res := Run(context.Background(), engineOptions(t, base, ModeBypass, nil), nil)
	if !errors.Is(res.Err, ErrRepeating) || len(f.requests()) != 5 {
		t.Fatalf("err %v after %d requests", res.Err, len(f.requests()))
	}
}

func TestRepeatGuardLetsWorkThrough(t *testing.T) {
	// Reading and checking again after each change is not a repeat, and
	// neither is a re-run command after an edit.
	read := `{"path":"index.html"}`
	cmd := `{"command":"cat index.html","description":"show it"}`
	f, base := newFake(t,
		toolTurn("write_file", writeArgs("<p>one</p>")),
		toolTurn("read_file", read),
		toolTurn("run_command", cmd),
		toolTurn("write_file", writeArgs("<p>two</p>")),
		toolTurn("read_file", read),
		toolTurn("run_command", `{"command":"cat index.html","description":"show it again"}`),
		toolTurn("edit_file", `{"path":"index.html","old_string":"two","new_string":"three"}`),
		toolTurn("read_file", read),
		toolTurn("run_command", cmd),
		sse(content("Done.")),
	)
	var evs []Event
	res := Run(context.Background(), engineOptions(t, base, ModeBypass, nil), collect(&evs))
	if res.Err != nil || !res.Done.OK {
		t.Fatalf("result %+v err %v", res.Done, res.Err)
	}
	if ns := notes(evs); len(ns) != 0 {
		t.Errorf("notes %q", ns)
	}
	for i, r := range f.requests() {
		for _, m := range toolMessages(r) {
			if strings.Contains(m, "repeating yourself") {
				t.Fatalf("request %d has the note: %q", i, m)
			}
		}
	}
}

func TestRepeatGuardSkipsSpawnTools(t *testing.T) {
	g := repeatGuard{}
	sig, _, _ := callSig("read_file", `{"path":"a"}`, "/w")
	for i := range repeatStop + 2 {
		out, stop := g.observe(sig, "read_file", "x", false, false, false)
		if stop {
			if i != repeatStop {
				t.Fatalf("stopped at call %d", i+1)
			}
			break
		}
		if i == 1 && !strings.Contains(out, repeatNote) {
			t.Errorf("no note on the first repeat: %q", out)
		}
	}
	e := &engine{o: RunOptions{Cwd: "/w"}, emit: func(Event) {}}
	for range 10 {
		c := toolCall{Function: functionCall{Name: ToolWait, Arguments: `{"session":"s"}`}}
		if _, stop := e.watch(c, "running", false); stop {
			t.Fatal("waiting on a child is not a repeat")
		}
	}
}

func TestRepeatGuardClears(t *testing.T) {
	g := repeatGuard{}
	same, _, _ := callSig("list_dir", `{}`, "/w")
	g.observe(same, "list_dir", "x", false, false, false)
	g.observe(same, "list_dir", "x", false, false, false) // strike 1 (note)
	for i := range repeatClear {
		sig, _, _ := callSig("read_file", fmt.Sprintf(`{"path":"f%d"}`, i), "/w")
		g.observe(sig, "read_file", "x", false, false, false)
	}
	if g.strikes != 0 {
		t.Fatalf("strikes %d after %d clean calls", g.strikes, repeatClear)
	}
	if out, _ := g.observe(same, "list_dir", "x", false, false, false); strings.Contains(out, repeatNote) {
		// list_dir is outside the window now: not a repeat at all.
		t.Errorf("an old call counted: %q", out)
	}
}

func TestRepeatGuardWhileElided(t *testing.T) {
	// Old results are being shortened: the stub asks the model to run a
	// call again, so a reread is not counted. A rewrite still is.
	g := repeatGuard{}
	read, _, _ := callSig("read_file", `{"path":"a"}`, "/w")
	for range repeatStop + 2 {
		if out, stop := g.observe(read, "read_file", "x", false, false, true); stop || strings.Contains(out, repeatNote) {
			t.Fatalf("a reread while elided counted: %q %v", out, stop)
		}
	}
	write, _, _ := callSig("write_file", `{"path":"a","content":"x"}`, "/w")
	g.observe(write, "write_file", "replaced", false, true, true)
	if out, _ := g.observe(write, "write_file", unchangedPrefix+" a", false, true, true); !strings.Contains(out, repeatNote) {
		t.Errorf("a rewrite while elided was not counted: %q", out)
	}
}

func TestCallSigNormalises(t *testing.T) {
	a, _, _ := callSig("read_file", `{"path":"x.txt"}`, "/w")
	b, _, _ := callSig("read_file", `{"path":"/w/x.txt"}`, "/w")
	c, _, _ := callSig("run_command", `{"command":"ls ","description":"one"}`, "/w")
	d, _, _ := callSig("run_command", `{"command":"ls","description":"two"}`, "/w")
	if a != b || c != d {
		t.Errorf("%q %q / %q %q", a, b, c, d)
	}
	if _, _, ok := callSig("read_file", `[1]`, "/w"); ok {
		t.Error("an array is not arguments")
	}
}

func TestWriteFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	tb := newToolbox(dir, nil, 0)
	ctx := context.Background()
	if out, bad := tb.run(ctx, "write_file", args{"path": "a.txt", "content": "x\n"}); bad || !strings.HasPrefix(out, "created") {
		t.Fatalf("%q %v", out, bad)
	}
	p := filepath.Join(dir, "a.txt")
	st, _ := os.Stat(p)
	out, bad := tb.run(ctx, "write_file", args{"path": "a.txt", "content": "x\n"})
	if bad || !strings.HasPrefix(out, unchangedPrefix) || !strings.Contains(out, "nothing was written") {
		t.Fatalf("%q %v", out, bad)
	}
	if st2, _ := os.Stat(p); !st2.ModTime().Equal(st.ModTime()) {
		t.Error("the file was written again")
	}
	if out, _ := tb.run(ctx, "write_file", args{"path": "a.txt", "content": "y\n"}); !strings.HasPrefix(out, "replaced") {
		t.Errorf("%q", out)
	}
}
