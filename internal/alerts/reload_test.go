package alerts

import (
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/sysmon"
)

func TestReloadThresholds(t *testing.T) {
	a, sent, _, _ := setup()
	s := sysmon.Snapshot{CPU: sysmon.CPU{TempC: 70, Model: "CPU"}}
	a.Check(s)
	if len(*sent) != 0 {
		t.Fatalf("70 °C alerted under the default threshold: %+v", *sent)
	}
	cfg := config.Defaults()
	cfg.Monitor.Alerts.CPUTempC = 60
	a.Reload(cfg)
	a.Check(s)
	if len(*sent) != 1 || (*sent)[0].Title != "CPU running hot" {
		t.Fatalf("after lowering the threshold: %+v", *sent)
	}
	a.Reload(nil)
}
