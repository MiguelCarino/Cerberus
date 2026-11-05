package session

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const markerStream = "a\x1b[31mred\x1b[0m" +
	"\x1b]777;cerberus;waiting;approve run_command?\x07" +
	"b\x1b]777;notify;other\x07" +
	"\x1b]777;cerberus;resumed\x1b\\" +
	"c\x1b]777;cerberus;done\x07"

const markerStripped = "a\x1b[31mred\x1b[0m" + "b\x1b]777;notify;other\x07" + "c"

var markerWant = []marker{{"waiting", "approve run_command?"}, {"resumed", ""}, {"done", ""}}

// feedAll runs chunks through one parser and collects output and markers.
func feedAll(chunks [][]byte) (string, []marker) {
	var p markerParser
	var out bytes.Buffer
	var marks []marker
	for _, c := range chunks {
		o, mk := p.feed(c)
		out.Write(o)
		marks = append(marks, mk...)
	}
	out.Write(p.flush())
	return out.String(), marks
}

func TestMarkerSplitAtEveryByte(t *testing.T) {
	in := []byte(markerStream)
	for i := 0; i <= len(in); i++ {
		for j := i; j <= len(in); j += 7 {
			out, marks := feedAll([][]byte{in[:i], in[i:j], in[j:]})
			if out != markerStripped {
				t.Fatalf("split %d/%d: output %q, want %q", i, j, out, markerStripped)
			}
			if !reflect.DeepEqual(marks, markerWant) {
				t.Fatalf("split %d/%d: markers %v, want %v", i, j, marks, markerWant)
			}
		}
	}
}

func TestMarkerByteByByte(t *testing.T) {
	var chunks [][]byte
	for _, b := range []byte(markerStream) {
		chunks = append(chunks, []byte{b})
	}
	out, marks := feedAll(chunks)
	if out != markerStripped || !reflect.DeepEqual(marks, markerWant) {
		t.Fatalf("got %q %v", out, marks)
	}
}

func TestMarkerEdgeCases(t *testing.T) {
	long := "\x1b]777;cerberus;waiting;" + strings.Repeat("x", 2*maxMarkerLen) + "\x07"
	tests := []struct {
		name, in, want string
		marks          int
	}{
		{"plain", "hello world", "hello world", 0},
		{"lone esc at end", "abc\x1b", "abc\x1b", 0},
		{"unterminated at eof", "x\x1b]777;cerb", "x\x1b]777;cerb", 0},
		{"too long passes through", long, long, 0},
		{"two in a row", "\x1b]777;cerberus;resumed\x07\x1b]777;cerberus;done\x07!", "!", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, marks := feedAll([][]byte{[]byte(tt.in)})
			if out != tt.want || len(marks) != tt.marks {
				t.Fatalf("got %q (%d markers), want %q (%d)", out, len(marks), tt.want, tt.marks)
			}
		})
	}
}

func TestOutputCapAndTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scrollback.log")
	o := newOutput(path, 4096, 1024)
	var all bytes.Buffer
	for i := range 200 {
		line := []byte(strings.Repeat(string(rune('a'+i%26)), 63) + "\n")
		all.Write(line)
		o.Write(line)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 4096 || st.Size() < 1024 {
		t.Fatalf("scrollback size %d outside (1024, 4096]", st.Size())
	}
	file, _ := os.ReadFile(path)
	if !bytes.HasSuffix(all.Bytes(), file) {
		t.Fatal("scrollback is not the tail of the output")
	}
	backlog, _, detach := o.attach()
	detach()
	if want := all.Bytes()[all.Len()-1024:]; !bytes.Equal(backlog, want) {
		t.Fatalf("backlog len %d, want the last 1024 bytes", len(backlog))
	}
	// After release the backlog comes from disk.
	o.release()
	backlog, _, detach = o.attach()
	detach()
	if !bytes.Equal(backlog, all.Bytes()[all.Len()-1024:]) {
		t.Fatal("backlog from disk differs")
	}
}

func TestOutputSlowSubscriberDropped(t *testing.T) {
	o := newOutput(filepath.Join(t.TempDir(), "sb.log"), 1<<20, 1024)
	_, slow, detach := o.attach()
	defer detach()
	for range subBuffer + 10 {
		o.Write([]byte("x")) // must never block
	}
	n := 0
	for range slow { // closed once its buffer filled
		n++
	}
	if n != subBuffer {
		t.Fatalf("slow subscriber got %d chunks before close, want %d", n, subBuffer)
	}
	if len(o.subs) != 0 {
		t.Fatal("dropped subscriber still registered")
	}
}

func TestPlaceholders(t *testing.T) {
	p := placeholders{Task: "fix {model} bug", TaskFile: "/d/task.md", Model: "qwen", BaseURL: "http://127.0.0.1:7340/v1",
		Cwd: "/w", Session: "s1", Self: "/bin/cerberus"}
	got := p.expand([]string{"{self}", "agent", "--model", "{model}", "--task", "{task}", "--file={task_file}", "{cwd}/{session}", "{base_url}"})
	want := []string{"/bin/cerberus", "agent", "--model", "qwen", "--task", "fix {model} bug", "--file=/d/task.md", "/w/s1", "http://127.0.0.1:7340/v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7340":  "http://127.0.0.1:7340/v1",
		"0.0.0.0:7340":    "http://127.0.0.1:7340/v1",
		":8080":           "http://127.0.0.1:8080/v1",
		"[::]:7340":       "http://127.0.0.1:7340/v1",
		"100.64.0.1:7340": "http://100.64.0.1:7340/v1",
		"[::1]:7340":      "http://[::1]:7340/v1",
	} {
		if got := baseURL(in); got != want {
			t.Errorf("baseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuardDecide(t *testing.T) {
	base := guardState{Running: 0, MaxSessions: 3, MemAvailMiB: 16000, MinFreeMiB: 4096, FreeVRAMMiB: -1}
	tests := []struct {
		name   string
		mod    func(*guardState)
		ok     bool
		reason string
	}{
		{"free", func(*guardState) {}, true, ""},
		{"max sessions", func(g *guardState) { g.Running = 3 }, false, "3 of 3 sessions"},
		{"no limit", func(g *guardState) { g.Running = 9; g.MaxSessions = 0 }, true, ""},
		{"low memory", func(g *guardState) { g.MemAvailMiB = 1000 }, false, "low memory"},
		{"memory unknown", func(g *guardState) { g.MemAvailMiB = -1 }, true, ""},
		{"vram short", func(g *guardState) { g.LocalModel = true; g.FreeVRAMMiB = 2000; g.NeedVRAMMiB = 8000 }, false, "VRAM"},
		{"vram short but loaded", func(g *guardState) {
			g.LocalModel, g.ModelLoaded, g.FreeVRAMMiB, g.NeedVRAMMiB = true, true, 2000, 8000
		}, true, ""},
		{"vram unknown", func(g *guardState) { g.LocalModel = true; g.NeedVRAMMiB = 8000 }, true, ""},
		{"cloud model", func(g *guardState) { g.FreeVRAMMiB = 10; g.NeedVRAMMiB = 8000 }, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := base
			tt.mod(&g)
			ok, reason := g.decide()
			if ok != tt.ok || !strings.Contains(reason, tt.reason) {
				t.Fatalf("decide() = %v %q, want %v containing %q", ok, reason, tt.ok, tt.reason)
			}
		})
	}
}

func TestTailBufferClips(t *testing.T) {
	tb := tailBuffer{max: 10}
	tb.Write([]byte("0123456789abcdefghijklmnopqrstuvwxyz"))
	if got := tb.String(); !strings.HasSuffix(got, "qrstuvwxyz") || !strings.HasPrefix(got, "…") {
		t.Fatalf("got %q", got)
	}
}

func TestTitleFrom(t *testing.T) {
	if got := titleFrom("  Fix the login bug  \nmore detail"); got != "Fix the login bug" {
		t.Fatalf("got %q", got)
	}
	if got := []rune(titleFrom(strings.Repeat("é", 100))); len(got) != 72 {
		t.Fatalf("len %d", len(got))
	}
}

func TestRunGitRefusesBranchCommands(t *testing.T) {
	for _, args := range [][]string{{"branch", "x"}, {"-c", "user.name=a", "checkout", "-b", "x"}, {"switch", "-c", "x"}} {
		if _, err := runGit(t.Context(), t.TempDir(), nil, args...); err == nil || !strings.Contains(err.Error(), "never creates branches") {
			t.Errorf("runGit %v: %v", args, err)
		}
	}
}
