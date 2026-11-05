// Package alerts watches sysmon samples and energy totals and notifies when
// something needs attention: a disk filling up, a hot CPU, GPU or NVMe drive,
// sustained high power, a projected bill over budget, or the PC pushing the
// household toward CFE's DAC tariff. Each condition notifies at most once
// every six hours.
package alerts

import (
	"fmt"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/sysmon"
)

// repeatAfter is the per-condition rate limit.
const repeatAfter = 6 * time.Hour

// billEvery spaces the (heavier) totals and bill checks.
const billEvery = 5 * time.Minute

// minProjectionDays: the bill and DAC alerts wait until the month projection
// rests on a day of data, so minutes of a busy GPU never read as a month.
const minProjectionDays = 1.0

// Source gives the energy figures.
type Source interface {
	Totals() energy.Totals
	Bill(energy.Totals) energy.BillSummary
}

// Alerts checks each sample.
type Alerts struct {
	cfg  config.AlertConfig
	cur  string
	src  Source
	send func(notify.Message)
	now  func() time.Time

	mu        sync.Mutex
	last      map[string]time.Time
	highSince time.Time
	billAt    time.Time
	dacRisk   string
}

// New returns an alerter; send may be nil (alerts off).
func New(cfg *config.Config, src Source, send func(notify.Message)) *Alerts {
	return &Alerts{cfg: cfg.Monitor.Alerts, cur: cfg.Tariff.Currency, src: src, send: send, now: time.Now, last: map[string]time.Time{}}
}

// Check runs every rule against one sample (a sysmon hook).
func (a *Alerts) Check(s sysmon.Snapshot) {
	if a.send == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.cfg // a.mu held: Reload swaps it under the same lock
	now := a.now()
	for _, d := range s.Disks {
		if c.DiskPct > 0 && d.SizeB > 0 && d.UsedPct >= float64(c.DiskPct) {
			// Titles name the device: the notifier rate-limits by title, so
			// two disks alerting in one sample would otherwise lose one.
			a.fire("disk:"+d.Device, "Disk almost full: "+mountOf(d), fmt.Sprintf("%s (%s) is %.0f%% full, %s free.", mountOf(d), d.Device, d.UsedPct, bytesH(d.FreeB)), "normal")
		}
		if c.NVMeTempC > 0 && !d.Rotational && d.TempC >= float64(c.NVMeTempC) {
			a.fire("nvme:"+d.Model+"|"+d.Parent, "Drive running hot: "+driveName(d), fmt.Sprintf("%s is at %.0f °C.", d.Model, d.TempC), "normal")
		}
	}
	if c.CPUTempC > 0 && s.CPU.TempC >= float64(c.CPUTempC) {
		a.fire("cputemp", "CPU running hot", fmt.Sprintf("%s is at %.0f °C.", s.CPU.Model, s.CPU.TempC), "critical")
	}
	for _, g := range s.GPU.GPUs {
		if c.GPUTempC > 0 && g.TempC >= c.GPUTempC {
			a.fire(fmt.Sprintf("gputemp:%d", g.Index), fmt.Sprintf("GPU %d running hot", g.Index), fmt.Sprintf("%s is at %d °C.", g.Name, g.TempC), "critical")
		}
	}
	if c.HighPowerW > 0 && s.Power.WallW >= c.HighPowerW {
		if a.highSince.IsZero() {
			a.highSince = now
		}
		if mins := c.HighPowerMin; now.Sub(a.highSince) >= time.Duration(max(mins, 1))*time.Minute {
			a.fire("power", "High power draw", fmt.Sprintf("About %.0f W at the wall for %d minutes.", s.Power.WallW, max(mins, 1)), "normal")
		}
	} else {
		a.highSince = time.Time{}
	}
	if a.src != nil && now.Sub(a.billAt) >= billEvery {
		a.billAt = now
		t := a.src.Totals()
		if !t.Loaded || t.ProjectionDays < minProjectionDays {
			return
		}
		b := a.src.Bill(t)
		if c.MonthBillMXN > 0 && b.ProjectedMonthCost >= c.MonthBillMXN {
			a.fire("bill", "Electricity budget", fmt.Sprintf("This PC is projected to add %.0f %s to this month's bill (limit %.0f).", b.ProjectedMonthCost, a.cur, c.MonthBillMXN), "normal")
		}
		risk := b.Marginal.DACRisk
		if c.DACWarn && risk != a.dacRisk && (risk == "near" || risk == "pushes") {
			msg := "The household is near the DAC limit; this PC's use counts toward it."
			if risk == "pushes" {
				msg = "This PC's use pushes the household over the DAC limit: every kWh would be billed at the DAC price."
			}
			a.fire("dac:"+risk, "DAC tariff risk", msg, "critical")
		}
		a.dacRisk = risk
	}
}

func (a *Alerts) fire(key, title, body, urgency string) {
	now := a.now()
	if t, ok := a.last[key]; ok && now.Sub(t) < repeatAfter {
		return
	}
	a.last[key] = now
	a.send(notify.Message{Title: title, Body: body, Urgency: urgency, Kind: "alert", Link: "#/monitor"})
}

func driveName(d sysmon.Disk) string {
	if d.Model != "" {
		return d.Model
	}
	if d.Parent != "" {
		return d.Parent
	}
	return d.Device
}

func mountOf(d sysmon.Disk) string {
	if len(d.Mounts) > 0 {
		return d.Mounts[0]
	}
	return d.Device
}

func bytesH(b uint64) string {
	const g = 1 << 30
	if b >= 1<<40 {
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/g)
}
