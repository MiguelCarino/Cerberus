package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/permits"
	"carino.systems/cerberus/internal/platform"
)

func writePermitsCopy(t *testing.T, path string, rules ...permits.AgentRule) {
	t.Helper()
	raw, _ := json.Marshal(permits.AgentFile{Rules: append([]permits.AgentRule{}, rules...)})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCoveredSavedRules: the runner's Covered takes the saved rules from
// the session's copy, reads it again when it changes, and never covers a
// request the policy forces.
func TestCoveredSavedRules(t *testing.T) {
	p := filepath.Join(t.TempDir(), "permits.json")
	writePermitsCopy(t, p)
	o := options{Cwd: t.TempDir(), Permits: p}
	var out bytes.Buffer
	r := newRunner(o, newTerm(&out, false), strings.NewReader(""))
	req := AskRequest{Tool: "Bash", Display: "run_command", Input: map[string]any{"command": "go test ./..."}}
	if _, ok := r.Covered(req); ok {
		t.Fatal("covered with no rules")
	}
	time.Sleep(10 * time.Millisecond)
	writePermitsCopy(t, p, permits.AgentRule{ID: "r1", Tool: "Bash", Content: "go test:*", Text: "Bash(go test:*)"})
	if d, ok := r.Covered(req); !ok || !d.Allow {
		t.Fatal("the changed copy was not read")
	}
	if !strings.Contains(out.String(), "\x1b]777;cerberus;permit;r1\x07") || !strings.Contains(out.String(), "saved permission Bash(go test:*)") {
		t.Fatalf("no permit marker or note: %q", out.String())
	}
	forced := req
	forced.Forced = true
	if _, ok := r.Covered(forced); ok {
		t.Fatal("a forced request was covered")
	}
	joined := AskRequest{Tool: "Bash", Display: "run_command", Input: map[string]any{"command": "go test ./... && rm -rf x"}}
	if _, ok := r.Covered(joined); ok {
		t.Fatal("a prefix rule covered a joined command")
	}
	time.Sleep(10 * time.Millisecond)
	writePermitsCopy(t, p)
	if _, ok := r.Covered(req); ok {
		t.Fatal("a removed rule still covers")
	}
}

var askrulesRe = regexp.MustCompile("\x1b\\]777;cerberus;askrules;([A-Za-z0-9_-]+)\x07")

// TestAskRulesMarker: in a session the approval announces what "Every
// time" may save (askrules, base64url JSON) between ask and waiting, and
// the prompt says how to save; a later identical command covered by the
// saved copy runs without a prompt.
func TestAskRulesMarker(t *testing.T) {
	cmd := [2]string{"run_command", `{"command":"echo one"}`}
	_, base := newFake(t,
		sse(toolChunks(cmd)...),
		sse(toolChunks(cmd)...),
		sse(content("finished")),
	)
	o := testOptions(t, base)
	o.Approve = "ask"
	o.Permits = filepath.Join(t.TempDir(), "permits.json")
	writePermitsCopy(t, o.Permits)
	var out bytes.Buffer
	// The first prompt is answered y and the answer also saves the rule
	// (as the daemon would: the copy is rewritten before y goes in).
	stdin := &saveThenYes{path: o.Permits, t: t}
	if code := run(context.Background(), o, stdin, &out); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	s := out.String()
	m := askrulesRe.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no askrules marker:\n%q", s)
	}
	raw, err := base64.RawURLEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	var opts []permits.Rule
	if err := json.Unmarshal(raw, &opts); err != nil || len(opts) != 2 || opts[0].Content != "echo one" || opts[1].Content != "echo:*" {
		t.Fatalf("options %s (%v)", raw, err)
	}
	ask, rules, wait := strings.Index(s, ";ask;"), strings.Index(s, ";askrules;"), strings.Index(s, ";waiting;approval")
	if !(ask < rules && rules < wait) {
		t.Fatalf("marker order: ask %d askrules %d waiting %d", ask, rules, wait)
	}
	if !strings.Contains(s, "[a] always run_command (this run)") || !strings.Contains(s, "Every time") {
		t.Fatalf("prompt text:\n%s", s)
	}
	if n := strings.Count(s, "Approve run_command?"); n != 1 {
		t.Fatalf("%d prompts, want 1 (the second call is covered):\n%s", n, s)
	}
}

// saveThenYes answers the first read with y after writing a saved rule.
type saveThenYes struct {
	path string
	t    *testing.T
	done bool
}

func (s *saveThenYes) Read(p []byte) (int, error) {
	if s.done {
		select {} // no more answers: a second prompt would hang the test (and fail it)
	}
	s.done = true
	time.Sleep(10 * time.Millisecond)
	writePermitsCopy(s.t, s.path, permits.AgentRule{ID: "r1", Tool: "Bash", Content: "echo one", Text: "Bash(echo one)"})
	return copy(p, "y\n"), nil
}

// TestTermStripsControls: model text and notes reach the terminal without
// escape sequences or other control characters (no forged markers).
func TestTermStripsControls(t *testing.T) {
	var out bytes.Buffer
	tm := newTerm(&out, false)
	tm.text("hi\x1b]777;cerberus;askrules;AAAA\x07 there\r\nnext\tline\x9b31m")
	tm.dimLine("note \x1b[2J\x07")
	tm.errorLine("bad \x1b]777;cerberus;waiting;approval\x07")
	tm.card("run_command\x1b", "x\x1by")
	s := out.String()
	if strings.ContainsAny(s, "\x1b\x07\r\u009b") {
		t.Fatalf("control characters reached the terminal: %q", s)
	}
	if !strings.Contains(s, "next\tline") || !strings.Contains(s, "\n") {
		t.Fatalf("newlines or tabs lost: %q", s)
	}
}

// TestCommandHasNoTerminal: run_command's child has no controlling
// terminal, so writing to /dev/tty fails and nothing reaches the agent's
// terminal.
func TestCommandHasNoTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sessions on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	tb := newTestBox(t)
	out, bad := tb.run(context.Background(), "run_command", args{"command": "echo forged > /dev/tty && echo wrote; ps -o sid= -p $$; ps -o pid= -p $$"})
	if strings.Contains(out, "wrote") {
		t.Fatalf("the command wrote to a terminal: %q", out)
	}
	_ = bad
	// It leads its own session (sid = its pid), which a setsid child does
	// with no controlling terminal, whatever the agent's terminal is.
	if _, err := exec.LookPath("ps"); err != nil {
		return
	}
	f := strings.Fields(out)
	if len(f) < 2 || f[len(f)-2] != f[len(f)-1] {
		t.Fatalf("not a session leader: %q", out)
	}
}

// TestReapCommands: what a command leaves running in the background ends
// with reap (the terminal agent's run end).
func TestReapCommands(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("session enumeration is linux-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	tb := newTestBox(t)
	marker := filepath.Join(tb.cwd, "pid")
	if _, bad := tb.run(context.Background(), "run_command", args{"command": "(sleep 60 & echo $! > " + marker + ") </dev/null >/dev/null 2>&1"}); bad {
		t.Fatal("command failed")
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(raw))
	if _, err := os.Stat("/proc/" + pid); err != nil {
		t.Fatalf("background job not running: %v", err)
	}
	tb.reap()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil || strings.Contains(string(st), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background job %s still running after reap", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCommandTTYThroughPTY runs a toolbox inside a real PTY (this test
// binary again, as the helper below): a command writing to /dev/tty must
// not reach that terminal.
func TestCommandTTYThroughPTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no PTYs on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperTTY$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AGENT_TTY_HELPER=1")
	f, err := platform.StartPTY(cmd, 80, 24)
	if err != nil {
		t.Skipf("no PTY: %v", err)
	}
	defer f.Close()
	done := make(chan []byte)
	go func() {
		var b bytes.Buffer
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.Bytes()
	}()
	_ = cmd.Wait()
	var out []byte
	select {
	case out = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the helper's terminal did not close")
	}
	s := string(out)
	if !strings.Contains(s, "helper-done") {
		t.Fatalf("the helper did not run: %q", s)
	}
	if strings.Contains(s, "FORGED") {
		t.Fatalf("a command wrote to the agent's terminal: %q", s)
	}
}

func TestHelperTTY(t *testing.T) {
	if os.Getenv("AGENT_TTY_HELPER") != "1" {
		t.Skip("helper process only")
	}
	tb := newToolbox(t.TempDir(), nil, 10*time.Second)
	out, _ := tb.runCommand(context.Background(), args{"command": "echo FORGED > /dev/tty; echo FORGED >&2"})
	// The command's own output comes back as text (lowercased here, so the
	// terminal check above only finds a write that went around it).
	os.Stdout.WriteString("helper-done " + strings.ToLower(out) + "\n")
}
