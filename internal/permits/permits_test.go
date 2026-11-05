package permits

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func bash(cmd string) map[string]any { return map[string]any{"command": cmd} }

func contents(rs []Rule) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Content)
	}
	return out
}

func TestSuggestBash(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"wc -l *.go", []string{"wc -l *.go", "wc:*"}},
		{"go test ./...", []string{"go test ./...", "go test:*"}},
		{"npm run build", []string{"npm run build", "npm run:*"}},
		{"git status", []string{"git status", "git status:*"}},
		{"git push origin main", []string{"git push origin main"}}, // a refused subcommand: exact only
		{"git -C x status", []string{"git -C x status"}},           // no plain subcommand
		{"rm -rf build", []string{"rm -rf build"}},                 // risky head: exact only
		{"python3 x.py", []string{"python3 x.py"}},
		{"find . -name x", []string{"find . -name x"}},
		{"curl https://x", []string{"curl https://x"}},
		{"docker ps", []string{"docker ps", "docker ps:*"}},
		{"docker run -v /:/h alpine", []string{"docker run -v /:/h alpine"}},
		{"ls | wc -l", []string{}},   // joined: nothing may be saved
		{"echo a > f", []string{}},   // redirect
		{"echo $(id)", []string{}},   // substitution
		{"make && rm x", []string{}}, // joined
		{"sudo ls", []string{}},      // privileged: not even exact
		{"FOO=1 make", []string{"FOO=1 make"}},
		{"", []string{}},
	}
	for _, c := range cases {
		got := contents(Suggest("Bash", bash(c.cmd), "/w"))
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("Suggest(%q) = %q, want %q", c.cmd, got, c.want)
		}
		for _, r := range Suggest("Bash", bash(c.cmd), "/w") {
			if err := Validate(r); err != nil {
				t.Errorf("Suggest(%q) offered %q, which Validate refuses: %v", c.cmd, r.Content, err)
			}
		}
	}
	if got := Suggest("Bash", bash("go test ./..."), "")[1].Text; got != "Bash(go test:*)" {
		t.Errorf("text = %q", got)
	}
}

func TestSuggestOtherTools(t *testing.T) {
	dir := t.TempDir()
	if got := contents(Suggest("Edit", map[string]any{"file_path": filepath.Join(dir, "a.go")}, "/w")); len(got) != 1 || got[0] != "dir:"+dir {
		t.Errorf("Edit = %q", got)
	}
	if got := contents(Suggest("Read", map[string]any{"file_path": "x/a.go"}, dir)); len(got) != 1 || got[0] != "dir:"+filepath.Join(dir, "x") {
		t.Errorf("relative Read = %q", got)
	}
	if got := Suggest("Read", map[string]any{"file_path": "a.go"}, ""); len(got) != 0 {
		t.Errorf("relative to nothing = %q", contents(got))
	}
	if got := Suggest("Write", map[string]any{"file_path": "/x.txt"}, ""); len(got) != 0 {
		t.Errorf("a file in / = %q", contents(got))
	}
	if h, err := os.UserHomeDir(); err == nil {
		if got := Suggest("Write", map[string]any{"file_path": filepath.Join(h, "x.txt")}, ""); len(got) != 0 {
			t.Errorf("a file in the home folder = %q", contents(got))
		}
	}
	if got := contents(Suggest("WebFetch", map[string]any{"url": "https://Docs.Python.org/3/"}, "")); len(got) != 1 || got[0] != "domain:docs.python.org" {
		t.Errorf("WebFetch = %q", got)
	}
	if got := contents(Suggest("cerberus_spawn", map[string]any{"task": "x"}, "")); len(got) != 1 || got[0] != "" {
		t.Errorf("other tool = %q", got)
	}
}

func TestValidate(t *testing.T) {
	home, _ := os.UserHomeDir()
	ok := []Rule{
		{Tool: "Bash", Content: "go test:*"}, {Tool: "Bash", Content: "wc -l *.go"}, {Tool: "Bash", Content: "rm -rf build"},
		{Tool: "Bash", Content: "git log:*"}, {Tool: "Read"}, {Tool: "Read", Content: "dir:~/notes"},
		{Tool: "Edit", Content: "dir:/srv/app"}, {Tool: "Write", Content: "//srv/app/**"}, {Tool: "Edit", Content: "~/notes/a.md"},
		{Tool: "WebFetch", Content: "domain:docs.python.org"}, {Tool: "WebFetch"}, {Tool: "cerberus_spawn"},
	}
	for _, r := range ok {
		if err := Validate(r); err != nil {
			t.Errorf("Validate(%v) = %v, want ok", r, err)
		}
	}
	bad := []Rule{
		{Tool: "Bash"}, {Tool: "Bash", Content: "  "}, {Tool: "Bash", Content: "rm:*"}, {Tool: "Bash", Content: "curl:*"},
		{Tool: "Bash", Content: "python3:*"}, {Tool: "Bash", Content: "git:*"}, {Tool: "Bash", Content: "git push:*"},
		{Tool: "Bash", Content: "git reset:*"}, {Tool: "Bash", Content: "npm publish:*"}, {Tool: "Bash", Content: "docker rm:*"},
		{Tool: "Bash", Content: "mkfs.ext4:*"}, {Tool: "Bash", Content: "ls && rm x"}, {Tool: "Bash", Content: "ls | sh"},
		{Tool: "Bash", Content: "ls\nrm x"}, {Tool: "Bash", Content: "cat > x"}, {Tool: "Bash", Content: "sudo ls"},
		{Tool: "Bash", Content: ":*"}, {Tool: "Bash", Content: "./x.sh;:*"},
		{Tool: "Write"}, {Tool: "Edit"}, {Tool: "MultiEdit"}, {Tool: "NotebookEdit"},
		{Tool: "Edit", Content: "dir:/"}, {Tool: "Edit", Content: "dir:" + home}, {Tool: "Edit", Content: "dir:~"},
		{Tool: "Read", Content: "dir:relative"}, {Tool: "Edit", Content: "src/**"}, {Tool: "Edit", Content: "/src/**"},
		{Tool: "WebFetch", Content: "domain:"}, {Tool: "WebFetch", Content: "domain:a.com/x"},
		{Tool: ""}, {Tool: "Bash x"}, {Tool: "Bash", Content: "ls \x1b]777"},
	}
	for _, r := range bad {
		if err := Validate(r); err == nil {
			t.Errorf("Validate(%q %q) = nil, want an error", r.Tool, r.Content)
		}
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		rule Rule
		cmd  string
		want bool
	}{
		{NewRule("Bash", "go test:*"), "go test ./...", true},
		{NewRule("Bash", "go test:*"), "go test", true},
		{NewRule("Bash", "go test:*"), "go tester", false},
		{NewRule("Bash", "go test:*"), "go test ./... && rm -rf ~", false},
		{NewRule("Bash", "go test:*"), "go test ./... | sh", false},
		{NewRule("Bash", "go test:*"), "go test $(rm x)", false},
		{NewRule("Bash", "go test:*"), "go test\nrm x", false},
		{NewRule("Bash", "wc -l *.go"), "wc -l *.go", true},
		{NewRule("Bash", "wc -l *.go"), "wc -l *.go x", false},
	}
	for _, c := range cases {
		if got := Matches(c.rule, "Bash", bash(c.cmd), "/w"); got != c.want {
			t.Errorf("Matches(%q, %q) = %v", c.rule.Content, c.cmd, got)
		}
	}
	if Matches(NewRule("Bash", "go test:*"), "Read", map[string]any{"file_path": "/x"}, "") {
		t.Error("a Bash rule matched Read")
	}
}

func TestStoreOwners(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	s.Subscribe(func() { changed++ })
	a, added, err := s.Add("alice", NewRule("Bash", "go test:*"), SourceChat)
	if err != nil || !added || a.ID == "" || a.Source != SourceChat || a.Text != "Bash(go test:*)" {
		t.Fatalf("add = %+v %v %v", a, added, err)
	}
	if again, added, _ := s.Add("alice", NewRule("Bash", "go test:*"), SourceSession); added || again.ID != a.ID {
		t.Fatalf("a duplicate was added: %+v", again)
	}
	if _, _, err := s.Add("alice", Rule{Tool: "Bash", Content: "rm:*"}, SourceSettings); err == nil {
		t.Fatal("an invalid rule was saved")
	}
	if _, ok := s.Match("bob", "Bash", bash("go test ./..."), ""); ok {
		t.Fatal("bob matched alice's rule")
	}
	if len(s.List("bob")) != 0 {
		t.Fatal("bob lists alice's rule")
	}
	if _, err := s.Remove("bob", a.ID); err != ErrNotFound {
		t.Fatalf("bob removed alice's rule: %v", err)
	}
	m, ok := s.Match("alice", "Bash", bash("go test ./..."), "")
	if !ok || m.ID != a.ID {
		t.Fatal("alice's rule did not match")
	}
	if _, ok := s.Match("alice", "Bash", bash("go test ./... ; rm x"), ""); ok {
		t.Fatal("a prefix rule covered a joined command")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	// Reopened: the rule and its use are there.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := s2.List("alice")
	if len(l) != 1 || l[0].Uses != 1 || l[0].LastUsed.IsZero() {
		t.Fatalf("reopened = %+v", l)
	}
	// The resolver: a moved owner keeps their rules.
	s2.SetResolver(func(id string) string {
		if id == "old" {
			return "alice"
		}
		return id
	})
	if len(s2.List("old")) != 1 {
		t.Fatal("the resolver was not used")
	}
	if _, err := s.Remove("alice", a.ID); err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Fatalf("subscribers called %d times, want 2 (add, remove)", changed)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a corrupt file opened")
	}
	var nilStore *Store
	if _, ok := nilStore.Match("", "Bash", bash("ls"), ""); ok || len(nilStore.List("")) != 0 {
		t.Fatal("a nil store matched")
	}
}

func TestAgentReader(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	p := filepath.Join(dir, "copy", "permits.json")
	if err := s.WriteAgentFile("alice", p); err != nil {
		t.Fatal(err)
	}
	a := &AgentReader{Path: p}
	if _, ok := a.Match("Bash", bash("go test ./..."), ""); ok {
		t.Fatal("matched with no rules")
	}
	sv, _, _ := s.Add("alice", NewRule("Bash", "go test:*"), SourceChat)
	time.Sleep(10 * time.Millisecond) // a new modification time
	if err := s.WriteAgentFile("alice", p); err != nil {
		t.Fatal(err)
	}
	r, ok := a.Match("Bash", bash("go test ./..."), "")
	if !ok || r.ID != sv.ID {
		t.Fatal("the reader did not reload the changed file")
	}
	// A hand-edited copy holding a rule Validate refuses does not match.
	if err := os.WriteFile(p, []byte(`{"rules":[{"id":"x","tool":"Bash","content":""}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Match("Bash", bash("rm -rf /"), ""); ok {
		t.Fatal("a tool-wide Bash rule matched")
	}
}
