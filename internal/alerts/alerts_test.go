package alerts

import (
	"testing"
	"time"

	"carino.systems/cerberus/internal/bill"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/sysmon"
)

type fakeSrc struct {
	risk string
	cost float64
	days float64
}

func (f *fakeSrc) Totals() energy.Totals { return energy.Totals{Loaded: true, ProjectionDays: f.days} }
func (f *fakeSrc) Bill(energy.Totals) energy.BillSummary {
	return energy.BillSummary{Marginal: bill.Margin{DACRisk: f.risk}, ProjectedMonthCost: f.cost}
}

func setup() (*Alerts, *[]notify.Message, *time.Time, *fakeSrc) {
	cfg := config.Defaults()
	cfg.Monitor.Alerts.MonthBillMXN = 300
	var sent []notify.Message
	src := &fakeSrc{risk: "none", days: 7}
	a := New(cfg, src, func(m notify.Message) { sent = append(sent, m) })
	now := time.Unix(1700000000, 0)
	a.now = func() time.Time { return now }
	return a, &sent, &now, src
}

func TestDiskAndTempsRateLimited(t *testing.T) {
	a, sent, now, _ := setup()
	s := sysmon.Snapshot{
		Disks: []sysmon.Disk{{Device: "sdb1", Mounts: []string{"/mnt/x"}, SizeB: 100, UsedPct: 95, FreeB: 5 << 30},
			{Device: "nvme0n1p1", Model: "NV", SizeB: 100, UsedPct: 10, TempC: 75}},
		CPU: sysmon.CPU{TempC: 95, Model: "CPU"},
		GPU: gpu.Snapshot{GPUs: []gpu.GPU{{Index: 0, Name: "GPU", TempC: 90}}},
	}
	a.Check(s)
	if len(*sent) != 4 {
		t.Fatalf("want disk, nvme, cpu, gpu alerts; got %+v", *sent)
	}
	a.Check(s)
	if len(*sent) != 4 {
		t.Fatal("repeats must be rate limited")
	}
	*now = now.Add(7 * time.Hour)
	a.Check(s)
	if len(*sent) != 8 {
		t.Fatalf("after 6 h they repeat; got %d", len(*sent))
	}
}

func TestSustainedPower(t *testing.T) {
	a, sent, now, _ := setup()
	high := sysmon.Snapshot{Power: sysmon.Power{WallW: 700}}
	a.Check(high)
	*now = now.Add(20 * time.Minute)
	a.Check(sysmon.Snapshot{Power: sysmon.Power{WallW: 100}}) // a dip resets the streak
	*now = now.Add(time.Minute)
	a.Check(high)
	*now = now.Add(29 * time.Minute)
	a.Check(high)
	if len(*sent) != 0 {
		t.Fatalf("not sustained long enough: %+v", *sent)
	}
	*now = now.Add(2 * time.Minute)
	a.Check(high)
	if len(*sent) != 1 || (*sent)[0].Title != "High power draw" {
		t.Fatalf("sent = %+v", *sent)
	}
}

func TestBillAndDAC(t *testing.T) {
	a, sent, now, src := setup()
	src.cost = 350
	src.risk = "near"
	a.Check(sysmon.Snapshot{})
	if len(*sent) != 2 {
		t.Fatalf("want bill + DAC near; got %+v", *sent)
	}
	src.risk = "pushes"
	a.Check(sysmon.Snapshot{}) // within billEvery: not checked yet
	*now = now.Add(6 * time.Minute)
	a.Check(sysmon.Snapshot{})
	if len(*sent) != 3 || (*sent)[2].Title != "DAC tariff risk" {
		t.Fatalf("risk change must notify: %+v", *sent)
	}
}

func TestNilSender(t *testing.T) {
	a := New(config.Defaults(), nil, nil)
	a.Check(sysmon.Snapshot{CPU: sysmon.CPU{TempC: 200}})
}

// Several devices alerting in one sample each get their own title, because
// the notifier drops a repeated title within seconds.
func TestPerDeviceTitlesAreDistinct(t *testing.T) {
	a, sent, _, _ := setup()
	a.Check(sysmon.Snapshot{
		Disks: []sysmon.Disk{{Device: "nvme0n1p2", Mounts: []string{"/"}, SizeB: 100, UsedPct: 95},
			{Device: "sdb1", Mounts: []string{"/data"}, SizeB: 100, UsedPct: 96},
			{Device: "nvme1n1p1", Parent: "nvme1n1", Model: "A", SizeB: 100, TempC: 80},
			{Device: "nvme2n1p1", Parent: "nvme2n1", Model: "B", SizeB: 100, TempC: 80}},
		GPU: gpu.Snapshot{GPUs: []gpu.GPU{{Index: 0, TempC: 90}, {Index: 1, TempC: 91}}},
	})
	titles := map[string]bool{}
	for _, m := range *sent {
		if titles[m.Title] {
			t.Fatalf("duplicate title %q in %+v", m.Title, *sent)
		}
		titles[m.Title] = true
	}
	if len(*sent) != 6 || !titles["Disk almost full: /data"] || !titles["GPU 1 running hot"] {
		t.Fatalf("sent = %+v", *sent)
	}
}

// The bill and DAC alerts wait for a day of data behind the projection.
func TestBillAlertsWaitForData(t *testing.T) {
	a, sent, now, src := setup()
	src.cost, src.risk, src.days = 5000, "pushes", 5.0/(24*60)
	a.Check(sysmon.Snapshot{})
	if len(*sent) != 0 {
		t.Fatalf("five minutes of data must not alert: %+v", *sent)
	}
	src.days = 1.5
	*now = now.Add(6 * time.Minute)
	a.Check(sysmon.Snapshot{})
	if len(*sent) != 2 {
		t.Fatalf("with a day of data: %+v", *sent)
	}
}
