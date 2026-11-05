// Package bill prices electricity with a tiered domestic tariff (CFE by
// default): what a month of kWh costs, and what this PC adds to the
// household bill at the margin. Pure functions; money is rounded only for
// display, never here.
package bill

import (
	"math"

	"carino.systems/cerberus/internal/config"
)

// TierLine is one tier's share of a monthly bill.
type TierLine struct {
	Name   string
	KWh    float64
	PerKWh float64
	Amount float64 // before VAT
}

// Breakdown is one month's bill for a given consumption.
type Breakdown struct {
	KWh, Energy, Fixed, VAT, Total float64
	Tiers                          []TierLine
	DAC                            bool // priced at the high-consumption tariff
}

// Margin is what this PC adds to the household's monthly bill.
type Margin struct {
	PCKWhMonth   float64
	HouseholdKWh float64 // the household figure used (the PC's own kWh when unknown)
	CostMonth    float64 // bill(household) − bill(household − pc), VAT included
	PerKWh       float64 // CostMonth ÷ PCKWhMonth, or the price at the margin when the PC used nothing
	TierName     string  // the tier the household's last kWh falls in ("DAC" when priced at DAC)
	DACRisk      string  // "none" | "near" | "pushes" | "already" | "unknown" (household not set)
	DACAvgKWh    float64 // the monthly average compared with the DAC limit
	HouseholdSet bool    // false: HouseholdKWhMonth was 0, so the PC alone stood in for the house
}

// Monthly is the bill for kwh in one month: tiers (or DAC), fixed charges, VAT.
func Monthly(t config.TariffConfig, kwh float64) Breakdown {
	kwh = math.Max(kwh, 0)
	b := Breakdown{KWh: kwh}
	if t.DACLimitKWhMonth > 0 && t.DACPerKWh > 0 && kwh > t.DACLimitKWhMonth {
		b.DAC = true
		b.Energy = kwh * t.DACPerKWh
		b.Fixed = t.DACFixedMonthly
		b.Tiers = []TierLine{{Name: "DAC", KWh: kwh, PerKWh: t.DACPerKWh, Amount: b.Energy}}
	} else {
		left, floor := kwh, 0.0
		for i, tier := range t.Tiers {
			width := left
			if tier.UpToKWh > 0 && i < len(t.Tiers)-1 {
				width = math.Min(left, math.Max(tier.UpToKWh-floor, 0))
				floor = math.Max(floor, tier.UpToKWh)
			}
			if width <= 0 && left > 0 && i < len(t.Tiers)-1 {
				continue
			}
			amt := width * tier.PerKWh
			b.Tiers = append(b.Tiers, TierLine{Name: tier.Name, KWh: width, PerKWh: tier.PerKWh, Amount: amt})
			b.Energy += amt
			left -= width
			if left <= 0 {
				break
			}
		}
		b.Fixed = t.FixedMonthly
	}
	b.VAT = (b.Energy + b.Fixed) * t.VATPct / 100
	b.Total = b.Energy + b.Fixed + b.VAT
	return b
}

// Marginal prices this PC's monthly kWh at the household's margin: the
// difference between the household bill with and without the PC.
func Marginal(t config.TariffConfig, pcKWhMonth float64) Margin {
	pc := math.Max(pcKWhMonth, 0)
	h := t.HouseholdKWhMonth
	m := Margin{PCKWhMonth: pc, HouseholdSet: h > 0}
	if h <= 0 {
		h = pc
	}
	if h < pc {
		h = pc // the house uses at least what the PC uses
	}
	m.HouseholdKWh, m.DACAvgKWh = h, h
	with, without := Monthly(t, h), Monthly(t, h-pc)
	m.CostMonth = with.Total - without.Total
	if pc > 0 {
		m.PerKWh = m.CostMonth / pc
	} else {
		m.PerKWh = priceAt(t, h)
	}
	m.TierName = tierAt(t, h)
	m.DACRisk = dacRisk(t, h, h-pc)
	if !m.HouseholdSet && t.DACLimitKWhMonth > 0 {
		// With H unknown only the PC is known: over the limit on its own,
		// the household certainly is too; otherwise the risk is unknown
		// (never a reassuring "none").
		m.DACRisk = "unknown"
		if pc > t.DACLimitKWhMonth {
			m.DACRisk = "pushes"
		}
	}
	return m
}

// Cost is kwh priced at the marginal price per kWh.
func Cost(t config.TariffConfig, m Margin, kwh float64) float64 {
	return math.Max(kwh, 0) * m.PerKWh
}

// priceAt is the VAT-included price of the next kWh at monthly consumption h.
func priceAt(t config.TariffConfig, h float64) float64 {
	return (Monthly(t, h+1).Energy - Monthly(t, h).Energy) * (1 + t.VATPct/100)
}

// tierAt names the tier holding the household's last kWh.
func tierAt(t config.TariffConfig, h float64) string {
	b := Monthly(t, h)
	if b.DAC {
		return "DAC"
	}
	for i := len(b.Tiers) - 1; i >= 0; i-- {
		if b.Tiers[i].KWh > 0 {
			return b.Tiers[i].Name
		}
	}
	if len(t.Tiers) > 0 {
		return t.Tiers[0].Name
	}
	return ""
}

func dacRisk(t config.TariffConfig, with, without float64) string {
	limit := t.DACLimitKWhMonth
	switch {
	case limit <= 0:
		return "none"
	case without > limit:
		return "already"
	case with > limit:
		return "pushes"
	case with >= 0.9*limit:
		return "near"
	}
	return "none"
}
