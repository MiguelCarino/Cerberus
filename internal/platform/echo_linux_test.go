//go:build linux

package platform

import (
	"bufio"
	"os/exec"
	"strings"
	"testing"
)

func TestPTYEcho(t *testing.T) {
	for _, c := range []struct {
		script string
		want   bool
	}{
		{"echo ready; read -r x", true},
		{"stty -echo; echo ready; read -r x", false},
	} {
		cmd := exec.Command("sh", "-c", c.script)
		f, err := StartPTY(cmd, 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(f)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(line, "ready") {
				break
			}
		}
		on, known := PTYEcho(f)
		if !known || on != c.want {
			t.Errorf("%q: echo %v known %v, want %v", c.script, on, known, c.want)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		f.Close()
	}
	if _, known := PTYEcho(nil); known {
		t.Fatal("nil file reported known")
	}
}
