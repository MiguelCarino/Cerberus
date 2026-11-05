package bill

import (
	"math"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func tariff() config.TariffConfig {
	c := config.Defaults()
	return c.Tariff
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestMonthlyTiers(t *testing.T) {
	tr := tariff()
	cases := []struct {
		kwh, energy float64
	}{
		{0, 0},
		{50, 50 * 1.14},
		{75, 75 * 1.14},
		{100, 75*1.14 + 25*1.385},
		{140, 75*1.14 + 65*1.385},
		{200, 75*1.14 + 65*1.385 + 60*4.054},
	}
	for _, c := range cases {
		b := Monthly(tr, c.kwh)
		if !near(b.Energy, c.energy) {
			t.Errorf("%v kWh: energy %v, want %v", c.kwh, b.Energy, c.energy)
		}
		if !near(b.VAT, b.Energy*0.16) || !near(b.Total, b.Energy*1.16) {
			t.Errorf("%v kWh: VAT/total wrong: %+v", c.kwh, b)
		}
		if b.DAC {
			t.Errorf("%v kWh flagged DAC", c.kwh)
		}
	}
}

func TestMonthlyDAC(t *testing.T) {
	tr := tariff()
	b := Monthly(tr, 300)
	if !b.DAC || !near(b.Energy, 300*6.2) || !near(b.Fixed, 130) || !near(b.Total, (300*6.2+130)*1.16) {
		t.Fatalf("DAC bill wrong: %+v", b)
	}
}

func TestMarginalExcedente(t *testing.T) {
	tr := tariff()
	tr.HouseholdKWhMonth = 200 // already in excedente: every PC kWh costs 4.054 + VAT
	m := Marginal(tr, 40)
	if !near(m.PerKWh, 4.054*1.16) || m.TierName != "excedente" || m.DACRisk != "none" {
		t.Fatalf("marginal = %+v", m)
	}
	if !near(Cost(tr, m, 2), 2*4.054*1.16) {
		t.Fatal("cost wrong")
	}
}

func TestMarginalPushesIntoDAC(t *testing.T) {
	tr := tariff()
	tr.HouseholdKWhMonth = 270 // without the 40 kWh PC: 230, under 250
	m := Marginal(tr, 40)
	if m.DACRisk != "pushes" || m.TierName != "DAC" {
		t.Fatalf("want pushes/DAC, got %+v", m)
	}
	want := Monthly(tr, 270).Total - Monthly(tr, 230).Total
	if !near(m.CostMonth, want) || m.PerKWh < 10 {
		t.Fatalf("pushing into DAC must be expensive: %+v", m)
	}
}

func TestMarginalRiskLevels(t *testing.T) {
	tr := tariff()
	for _, c := range []struct {
		h, pc float64
		want  string
	}{{100, 20, "none"}, {230, 20, "near"}, {260, 20, "pushes"}, {300, 20, "already"}} {
		tr.HouseholdKWhMonth = c.h
		if got := Marginal(tr, c.pc).DACRisk; got != c.want {
			t.Errorf("h=%v pc=%v: %s, want %s", c.h, c.pc, got, c.want)
		}
	}
}

func TestMarginalUnknownHousehold(t *testing.T) {
	tr := tariff()
	m := Marginal(tr, 60)
	if m.HouseholdSet || !near(m.HouseholdKWh, 60) || !near(m.CostMonth, Monthly(tr, 60).Total) {
		t.Fatalf("unknown household: %+v", m)
	}
	if m.DACRisk != "unknown" {
		t.Fatalf("household unset: DAC risk must be unknown, got %q", m.DACRisk)
	}
	if r := Marginal(tr, 300).DACRisk; r != "pushes" {
		t.Fatalf("PC alone over the limit: %q", r)
	}
	z := Marginal(tr, 0)
	if !near(z.PerKWh, 1.14*1.16) || z.CostMonth != 0 {
		t.Fatalf("zero pc: %+v", z)
	}
}
