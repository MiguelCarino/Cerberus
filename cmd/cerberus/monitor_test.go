package main

import (
	"bytes"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/bill"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/sysmon"
)

func sampleView() api.MonitorView {
	return api.MonitorView{
		Snapshot: sysmon.Snapshot{CPU: sysmon.CPU{Pct: 12.5, Model: "Ryzen", TempC: 50},
			Mem:   sysmon.Mem{TotalMiB: 32000, AvailMiB: 16000},
			GPU:   gpu.Snapshot{GPUs: []gpu.GPU{{Name: "RTX", PowerW: 60, MemTotalMiB: 24000, MemUsedMiB: 2000, TempC: 40}}},
			Disks: []sysmon.Disk{{Mounts: []string{"/"}, SizeB: 1 << 40, FreeB: 1 << 39, UsedPct: 50}}},
		Power:  sysmon.Power{WallW: 180, CPUW: 40, GPUW: 60, RestW: 45, Estimated: true, CPUSource: "model"},
		Totals: energy.Totals{Today: energy.KWh{Wall: 1.5}, MonthToDate: energy.KWh{Wall: 12}, ProjectedMonthKWh: 40, Loaded: true},
		Bill: &energy.BillSummary{Marginal: bill.Margin{PerKWh: 4.7, TierName: "excedente", DACRisk: "near", DACAvgKWh: 230},
			TodayCost: 7.05, MonthCost: 56.4, ProjectedMonthCost: 188, TariffName: "CFE Tarifa 1", Currency: "MXN"},
	}
}

func TestRenderTop(t *testing.T) {
	out := renderTop(sampleView(), api.ProcessView{Processes: []sysmon.Proc{{PID: 9, User: "me", Name: "llama-server", Group: "lmstudio", CPUPct: 80, RSSMiB: 2048, GPUMiB: 18000, Cerberus: true}}},
		[]session.Info{{State: session.StateRunning, Title: "fix tests", Agent: "cerberus", EnergyJ: 3.6e6, CostMXN: 4.7}})
	for _, want := range []string{"CPU   12.5%", "180 W at the wall", "estimated", "projected +188 MXN", "*    9", "llama-server", "SESSIONS", "1.0000 kWh ≈ 4.70"} {
		if !strings.Contains(out, want) {
			t.Errorf("top output lacks %q:\n%s", want, out)
		}
	}
}

func TestPrintEnergy(t *testing.T) {
	var b bytes.Buffer
	printEnergy(&b, sampleView())
	out := b.String()
	for _, want := range []string{"180 W at the wall", "rapl-setup", "month to date", "projected this month: 40.0 kWh", "4.700 (excedente tier)", "near the limit", "household kWh unknown", "approximate"} {
		if !strings.Contains(out, want) {
			t.Errorf("energy output lacks %q:\n%s", want, out)
		}
	}
}

func TestRaplSetupRuns(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"rapl-setup"}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "RAPL") && !strings.Contains(out.String(), "energy counter") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRenderTopStripsControlCharacters(t *testing.T) {
	v := sampleView()
	v.Snapshot.Disks[0].Mounts = []string{"/run/media/\x1b[2Jusb"}
	out := renderTop(v, api.ProcessView{Processes: []sysmon.Proc{{PID: 7, User: "me", Name: "\x1b]52;c;cm0K\a", Group: "g\x9b31m", CPUPct: 99}}},
		[]session.Info{{State: session.StateRunning, Title: "t\x1b[H", Agent: "a"}})
	if strings.IndexByte(out, 0x1b) >= 0 || strings.IndexByte(out, 0x07) >= 0 || strings.IndexByte(out, 0x9b) >= 0 {
		t.Fatalf("control characters reached the terminal: %q", out)
	}
	if !strings.Contains(out, "?]52;c;cm0K?") {
		t.Fatalf("name not shown sanitised: %q", out)
	}
}

func TestTopRejectsNonPositiveInterval(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	for _, every := range []string{"0", "-1s"} {
		var out, errb bytes.Buffer
		code := topCmd([]string{"--config", dir + "/none.json", "--every", every}, strings.NewReader(""), &out, &errb)
		if code != 2 || !strings.Contains(errb.String(), "--every") {
			t.Fatalf("--every %s: exit %d, %q", every, code, errb.String())
		}
	}
}
