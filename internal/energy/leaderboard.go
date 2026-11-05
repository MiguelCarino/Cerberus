package energy

import "carino.systems/cerberus/internal/usage"

// Leaderboard ranks usage over a period (usage.Leaderboard) with this
// accountant's energy: sessions without a WallJ get their attributed
// energy, request energy goes through the PSU efficiency, and energy is
// priced at the current marginal tariff price in the tariff's currency.
func (e *Energy) Leaderboard(u *usage.Store, sessions []usage.SessionInfo, period, by, metric string, limit int, keep func(string) bool) (usage.Board, error) {
	ss := make([]usage.SessionInfo, len(sessions))
	copy(ss, sessions)
	for i := range ss {
		if ss[i].WallJ == 0 {
			cpu, gpu := e.SessionEnergy(ss[i].ID)
			ss[i].WallJ = e.WallJ(cpu + gpu)
		}
	}
	return usage.Leaderboard(usage.Sources{Usage: u, Sessions: ss, WallJ: e.WallJ, PricePerKWh: e.PricePerKWh(),
		Currency: e.conf().Tariff.Currency, Now: e.now(), Limit: limit, Keep: keep}, period, by, metric)
}
