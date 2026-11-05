package config

// MonitorConfig drives the system monitor, energy accounting and alerts.
type MonitorConfig struct {
	Enabled          bool        `json:"enabled"`            // default true
	ActiveIntervalMs int         `json:"active_interval_ms"` // sampling while a panel is open (default 2000)
	IdleIntervalSec  int         `json:"idle_interval_sec"`  // background sampling for energy history (default 30)
	CPUPower         string      `json:"cpu_power"`          // "auto" (RAPL if readable, else model) | "rapl" | "model"
	CPUIdleW         float64     `json:"cpu_idle_w"`         // model: package watts at idle (5900X ≈ 30)
	CPUMaxW          float64     `json:"cpu_max_w"`          // model: package watts at 100% (5900X PPT ≈ 142)
	BaselineW        float64     `json:"baseline_w"`         // board, RAM, drives, fans, USB not measured by sensors (default 45)
	PSUEfficiency    float64     `json:"psu_efficiency"`     // DC→wall (default 0.90)
	ExtraW           float64     `json:"extra_w"`            // always-on devices you want counted (monitor, router…), default 0
	HistoryMonths    int         `json:"history_months"`     // minute→hour energy history kept (default 13, DAC looks back 12)
	Alerts           AlertConfig `json:"alerts"`
	Protected        []string    `json:"protected"` // process names never killed/reniced from the UI (merged with built-ins)
}

type AlertConfig struct {
	DiskPct      int     `json:"disk_pct"`       // default 90
	CPUTempC     int     `json:"cpu_temp_c"`     // default 90
	GPUTempC     int     `json:"gpu_temp_c"`     // default 85
	NVMeTempC    int     `json:"nvme_temp_c"`    // default 70
	HighPowerW   float64 `json:"high_power_w"`   // sustained wall watts (default 600)
	HighPowerMin int     `json:"high_power_min"` // for this many minutes (default 30)
	MonthBillMXN float64 `json:"month_bill_mxn"` // projected marginal cost this month (0 = off)
	DACWarn      bool    `json:"dac_warn"`       // warn when the PC pushes the household toward DAC (default true)
}

// TariffConfig is a tiered domestic electricity tariff (CFE by default).
// Defaults are CFE Tarifa 1 (CDMX) as published for late 2026, approximate:
// check them against your own receipt and update them in config.
type TariffConfig struct {
	Name              string  `json:"name"`                // "CFE Tarifa 1"
	Currency          string  `json:"currency"`            // "MXN"
	Tiers             []Tier  `json:"tiers"`               // per month, ascending; last tier UpToKWh 0 = unlimited
	VATPct            float64 `json:"vat_pct"`             // 16
	FixedMonthly      float64 `json:"fixed_monthly"`       // fixed charges per month (0 for Tarifa 1)
	HouseholdKWhMonth float64 `json:"household_kwh_month"` // typical whole-house kWh per month INCLUDING this PC (from receipts ÷ 2 for bimonthly)
	DACLimitKWhMonth  float64 `json:"dac_limit_kwh_month"` // 12-month average that moves you to DAC (Tarifa 1: 250)
	DACPerKWh         float64 `json:"dac_per_kwh"`         // DAC energy price (≈6.2)
	DACFixedMonthly   float64 `json:"dac_fixed_monthly"`   // DAC fixed charge per month (≈130)
	Verified          bool    `json:"verified"`            // set true once you've checked the numbers; the UI says "approximate" until then
}

type Tier struct {
	Name    string  `json:"name"`      // "básico", "intermedio", "excedente"
	UpToKWh float64 `json:"up_to_kwh"` // tier width cap (cumulative upper bound per month); 0 = rest
	PerKWh  float64 `json:"per_kwh"`
}

func monitorDefaults(c *Config) {
	c.Monitor = MonitorConfig{Enabled: true, ActiveIntervalMs: 2000, IdleIntervalSec: 30, CPUPower: "auto",
		CPUIdleW: 30, CPUMaxW: 142, BaselineW: 45, PSUEfficiency: 0.90, HistoryMonths: 13,
		Alerts: AlertConfig{DiskPct: 90, CPUTempC: 90, GPUTempC: 85, NVMeTempC: 70, HighPowerW: 600, HighPowerMin: 30, DACWarn: true}}
	c.Tariff = TariffConfig{Name: "CFE Tarifa 1", Currency: "MXN", VATPct: 16,
		Tiers:            []Tier{{Name: "básico", UpToKWh: 75, PerKWh: 1.14}, {Name: "intermedio", UpToKWh: 140, PerKWh: 1.385}, {Name: "excedente", PerKWh: 4.054}},
		DACLimitKWhMonth: 250, DACPerKWh: 6.2, DACFixedMonthly: 130}
}
