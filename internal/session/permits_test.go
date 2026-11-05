//go:build !windows

package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/permits"
)

func askrules(t *testing.T, rules ...permits.Rule) string {
	t.Helper()
	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// TestAnswerSave: an "askrules" marker right after an "ask" becomes the
// wait's WaitingAlways (invalid options dropped); one with no ask before it
// is ignored; AnswerSave saves the chosen option for the owner under the
// same seq guard as AnswerApproval and types y.
func TestAnswerSave(t *testing.T) {
	good := askrules(t, permits.NewRule("Bash", "wc -l *.go"), permits.NewRule("Bash", "wc:*"))
	mixed := askrules(t, permits.NewRule("Bash", ""), permits.NewRule("Bash", "rm:*"), permits.NewRule("Bash", "make test"))
	var store *permits.Store
	e := newTestEnv(t, nil, func(e *testEnv) {
		var err error
		if store, err = permits.Open(filepath.Join(e.home, "perm")); err != nil {
			t.Fatal(err)
		}
		e.opts = append(e.opts, WithPermits(store))
		agent := e.script("agent.sh", `printf '\033]777;cerberus;ask;run_command: wc -l *.go\007'
printf '\033]777;cerberus;askrules;`+good+`\007'
printf '\033]777;cerberus;waiting;approval\007'
read -r one
echo "one:$one" >> answers.txt
printf '\033]777;cerberus;resumed\007'
printf '\033]777;cerberus;askrules;`+good+`\007'
printf '\033]777;cerberus;waiting;approval\007'
read -r two
echo "two:$two" >> answers.txt
printf '\033]777;cerberus;resumed\007'
printf '\033]777;cerberus;ask;run_command: make test\007'
printf '\033]777;cerberus;askrules;`+mixed+`\007'
printf '\033]777;cerberus;waiting;approval\007'
read -r three
echo "three:$three" >> answers.txt
printf '\033]777;cerberus;resumed\007'
exit 0
`)
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{"bash", agent, "{task_file}"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID

	info := e.waitAsk(id, 1)
	if len(info.WaitingAlways) != 2 || info.WaitingAlways[1].Content != "wc:*" || info.WaitingAlways[1].Text != "Bash(wc:*)" {
		t.Fatalf("first wait offers %+v", info.WaitingAlways)
	}
	if _, _, err := e.m.AnswerSave(id, 1, 5); !errors.Is(err, ErrBadOption) {
		t.Fatalf("option out of range: %v", err)
	}
	if _, _, err := e.m.AnswerSave(id, 0, 0); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("stale seq: %v", err)
	}
	before, sv, err := e.m.AnswerSave(id, 1, 1)
	if err != nil || sv.Content != "wc:*" || sv.Source != permits.SourceSession || before.WaitingAsk != "run_command: wc -l *.go" {
		t.Fatalf("save: %v %+v %q", err, sv, before.WaitingAsk)
	}
	if _, _, err := e.m.AnswerSave(id, 1, 0); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("second answer: %v", err)
	}
	if l := store.List(""); len(l) != 1 || l[0].ID != sv.ID {
		t.Fatalf("saved %+v", l)
	}

	// An askrules with no ask before it attaches nothing.
	info = e.waitAsk(id, 2)
	if len(info.WaitingAlways) != 0 {
		t.Fatalf("orphan askrules taken: %+v", info.WaitingAlways)
	}
	if _, _, err := e.m.AnswerSave(id, 2, 0); !errors.Is(err, ErrBadOption) {
		t.Fatalf("save with nothing offered: %v", err)
	}
	if _, err := e.m.AnswerApproval(id, 2, "n"); err != nil {
		t.Fatal(err)
	}

	info = e.waitAsk(id, 3)
	if len(info.WaitingAlways) != 1 || info.WaitingAlways[0].Content != "make test" {
		t.Fatalf("invalid options kept: %+v", info.WaitingAlways)
	}
	if _, err := e.m.AnswerApproval(id, 3, "y"); err != nil {
		t.Fatal(err)
	}
	info = e.waitState(id, StatePassed)
	if len(info.WaitingAlways) != 0 {
		t.Fatalf("WaitingAlways kept after the wait: %+v", info.WaitingAlways)
	}
	raw, _ := os.ReadFile(filepath.Join(info.Worktree, "answers.txt"))
	if got := string(raw); got != "one:y\ntwo:n\nthree:y\n" {
		t.Fatalf("answers the agent read: %q", got)
	}
}

// TestPermitsCopyRewritten: a running built-in session's copy of the saved
// rules is rewritten when the store changes, with its owner's rules only.
func TestPermitsCopyRewritten(t *testing.T) {
	var store *permits.Store
	e := newTestEnv(t, nil, func(e *testEnv) {
		var err error
		if store, err = permits.Open(filepath.Join(e.home, "perm")); err != nil {
			t.Fatal(err)
		}
		e.opts = append(e.opts, WithPermits(store))
		agent := e.script("agent.sh", "printf '\\033]777;cerberus;waiting;input needed\\007'\nread -r x\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{"bash", agent, "{task_file}"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	e.waitState(id, StateWaiting)
	// The fake agent is not the built-in one: give its run a copy as a
	// built-in launch would.
	e.m.mu.Lock()
	s := e.m.sessions[id]
	p, err := e.m.writePermits(s.dir, s.info.Owner)
	s.run.permits = p
	e.m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add("someone-else", permits.NewRule("Bash", "make lint"), permits.SourceChat); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add("", permits.NewRule("Bash", "go test:*"), permits.SourceChat); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "go test:*") || strings.Contains(string(raw), "make lint") {
		t.Fatalf("copy = %s", raw)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode %v", st.Mode().Perm())
	}
	if filepath.Dir(p) != filepath.Join(s.dir, permitsDir) {
		t.Fatalf("copy at %s", p)
	}
	if err := e.m.Input(id, []byte("\r")); err != nil {
		t.Fatal(err)
	}
	e.waitFor(id, func(i Info) bool { return final(i.State) || i.State == StatePassed })
}
