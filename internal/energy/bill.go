package energy

import (
	"time"

	"carino.systems/cerberus/internal/bill"
)

// priceTTL caches the marginal price per kWh (Totals walks the whole history).
const priceTTL = time.Minute

// BillSummary is the money side of the Monitor (JSON keys as the API shows them).
type BillSummary struct {
	Marginal           bill.Margin `json:"marginal"`
	TodayCost          float64     `json:"today_cost"`
	MonthCost          float64     `json:"month_cost"`           // month to date
	ProjectedMonthCost float64     `json:"projected_month_cost"` // what the PC adds to this month's bill
	TariffName         string      `json:"tariff_name"`
	Currency           string      `json:"currency"`
	Verified           bool        `json:"verified"`
	DACLimitKWh        float64     `json:"dac_limit_kwh"` // the tariff's DAC monthly-average limit (0 = no DAC)
}

// Bill prices the totals with the configured tariff at the household margin.
// The PC's monthly kWh is this month's projection.
func (e *Energy) Bill(t Totals) BillSummary {
	tr := e.conf().Tariff
	m := bill.Marginal(tr, t.ProjectedMonthKWh)
	return BillSummary{Marginal: m, TodayCost: bill.Cost(tr, m, t.Today.Wall), MonthCost: bill.Cost(tr, m, t.MonthToDate.Wall),
		ProjectedMonthCost: m.CostMonth, TariffName: tr.Name, Currency: tr.Currency, Verified: tr.Verified,
		DACLimitKWh: tr.DACLimitKWhMonth}
}

// WallJ converts DC component joules to joules at the wall (PSU losses).
func (e *Energy) WallJ(j float64) float64 {
	eff := e.conf().Monitor.PSUEfficiency
	if eff <= 0 || eff > 1 {
		eff = 1
	}
	return j / eff
}

// CostOf prices wall joules at the current marginal price.
func (e *Energy) CostOf(wallJ float64, b BillSummary) float64 {
	return bill.Cost(e.conf().Tariff, b.Marginal, wallJ/3.6e6)
}

// PricePerKWh is the current marginal price per wall kWh (cached for a minute).
func (e *Energy) PricePerKWh() float64 {
	e.priceMu.Lock()
	defer e.priceMu.Unlock()
	if !e.priceAt.IsZero() && time.Since(e.priceAt) < priceTTL {
		return e.price
	}
	e.price, e.priceAt = e.Bill(e.Totals()).Marginal.PerKWh, time.Now()
	return e.price
}

// SessionCost is a session's wall energy (J) and its cost at the marginal price.
func (e *Energy) SessionCost(id string) (wallJ, cost float64) {
	cpu, gpu := e.SessionEnergy(id)
	wallJ = e.WallJ(cpu + gpu)
	return wallJ, wallJ / 3.6e6 * e.PricePerKWh()
}
