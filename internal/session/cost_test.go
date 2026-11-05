//go:build !windows

package session

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func TestCostAndLivePIDs(t *testing.T) {
	e := newTestEnv(t, nil, func(e *testEnv) {
		agent := e.script("waiter", "printf 'ready\\n'\nread -r line\nexit 0\n")
		e.profiles(config.AgentProfile{Name: "waiter", Command: []string{agent}})
		e.cfg.Guard.SystemdSlice = ""
		e.opts = append(e.opts, WithCost(func(id string) (float64, float64) { return 7200, 0.5 }))
	})
	infos, err := e.m.Create(context.Background(), TaskSpec{Repo: e.repo, Task: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	id := infos[0].ID
	if infos[0].EnergyJ != 7200 || infos[0].CostMXN != 0.5 {
		t.Fatalf("created info lacks cost: %+v", infos[0])
	}
	e.waitState(id, StateRunning)
	pids := e.m.LivePIDs()
	pid, ok := pids[id]
	if !ok || pid <= 0 {
		t.Fatalf("live pids = %v", pids)
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil && !strings.Contains(string(b), "CERBERUS_SESSION="+id) {
		t.Fatal("the live pid must be the session's agent")
	}
	if in, _ := e.m.Get(id); in.EnergyJ != 7200 {
		t.Fatalf("get lacks cost: %+v", in)
	}
	if err := e.m.Input(id, []byte("go\r")); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, StatePassed)
	if _, still := e.m.LivePIDs()[id]; still {
		t.Fatal("finished sessions are not live")
	}
}
