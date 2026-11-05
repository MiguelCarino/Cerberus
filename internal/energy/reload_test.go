package energy

import "testing"

func TestReloadTariffAndPSU(t *testing.T) {
	c := testCfg()
	e := New(c, t.TempDir(), nil)
	before := e.Bill(Totals{Today: KWh{Wall: 1}, ProjectedMonthKWh: 30})
	n := c.Clone()
	n.Tariff.Currency = "USD"
	n.Tariff.VATPct = 0
	n.Monitor.PSUEfficiency = 0.5
	e.Reload(n)
	after := e.Bill(Totals{Today: KWh{Wall: 1}, ProjectedMonthKWh: 30})
	if after.Currency != "USD" || after.TodayCost >= before.TodayCost {
		t.Fatalf("tariff not reloaded: before %+v after %+v", before, after)
	}
	if got := e.WallJ(10); got != 20 {
		t.Fatalf("WallJ after PSU 0.5 = %v", got)
	}
	e.Reload(nil)
}
