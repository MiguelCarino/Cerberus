//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestAnswerApprovalFromOutside: an "ask" marker names the approval, the
// wait carries it (WaitingAsk, AskSeq), and AnswerApproval types the answer
// only for the approval still waiting: a stale seq, a second answer or a
// bad answer is refused, and a later plain wait has no WaitingAsk.
func TestAnswerApprovalFromOutside(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("agent.sh", `printf '\033]777;cerberus;ask;write_file: notes.txt\007'
printf '\033]777;cerberus;waiting;approval\007'
read -r one
echo "one:$one" >> answers.txt
printf '\033]777;cerberus;resumed\007'
printf '\033]777;cerberus;ask;run_command: make test\007'
printf '\033]777;cerberus;waiting;approval\007'
read -r two
echo "two:$two" >> answers.txt
printf '\033]777;cerberus;resumed\007'
printf '\033]777;cerberus;waiting;input needed\007'
read -r three
exit 0
`)
		e.profiles(config.AgentProfile{Name: "fake", Command: []string{"bash", agent, "{task_file}"}})
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "ask twice"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID

	info := e.waitState(id, StateWaiting)
	if info.WaitingReason != "approval" || info.WaitingAsk != "write_file: notes.txt" || info.AskSeq != 1 {
		t.Fatalf("first wait %q %q %d", info.WaitingReason, info.WaitingAsk, info.AskSeq)
	}
	if _, err := e.m.AnswerApproval(id, 1, "y\nn"); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("two lines: %v", err)
	}
	if _, err := e.m.AnswerApproval(id, 0, "y"); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("stale seq: %v", err)
	}
	before, err := e.m.AnswerApproval(id, 1, "y")
	if err != nil || before.WaitingAsk != "write_file: notes.txt" {
		t.Fatalf("answer: %v %+v", err, before.WaitingAsk)
	}
	if _, err := e.m.AnswerApproval(id, 1, "y"); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("second answer to the same approval: %v", err)
	}

	info = e.waitAsk(id, 2)
	if info.WaitingAsk != "run_command: make test" {
		t.Fatalf("second wait %q", info.WaitingAsk)
	}
	if _, err := e.m.AnswerApproval(id, 2, "not the tests, they are slow"); err != nil {
		t.Fatal(err)
	}

	// A plain wait: not an approval, nothing to answer from outside.
	info = e.waitFor(id, func(i Info) bool { return i.State == StateWaiting && i.WaitingReason == "input needed" })
	if info.WaitingAsk != "" {
		t.Fatalf("plain wait carries an ask: %q", info.WaitingAsk)
	}
	if _, err := e.m.AnswerApproval(id, 2, "y"); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("answer on a plain wait: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(info.Worktree, "answers.txt"))
	if got := string(raw); got != "one:y\ntwo:not the tests, they are slow\n" {
		t.Fatalf("answers the agent read: %q", got)
	}
	if err := e.m.Input(id, []byte("\r")); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StatePassed)
}

// waitAsk blocks until session id waits for approval number seq.
func (e *testEnv) waitAsk(id string, seq int) Info {
	return e.waitFor(id, func(i Info) bool { return i.State == StateWaiting && i.WaitingReason == "approval" && i.AskSeq == seq })
}

// waitFor blocks until ok(session id) holds, re-checking on every change.
func (e *testEnv) waitFor(id string, ok func(Info) bool) Info {
	e.t.Helper()
	events, unsub := e.m.Subscribe()
	defer func() { unsub() }()
	deadline := time.NewTimer(waitLong)
	defer deadline.Stop()
	for {
		info, _ := e.m.Get(id)
		if ok(info) {
			return info
		}
		select {
		case _, open := <-events:
			if !open {
				unsub()
				events, unsub = e.m.Subscribe()
			}
		case <-deadline.C:
			e.t.Fatalf("timed out; state %s (%q, ask #%d)", info.State, info.WaitingReason, info.AskSeq)
		}
	}
}
