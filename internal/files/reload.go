package files

import "carino.systems/cerberus/internal/config"

// Live reload (day phase 4). Reload hands the store a new config; every
// setting it reads goes through conf(), so size limits, the inbox folder,
// executable and extension rules, name keeping and the deny paths apply
// from the next request on. Uploads already streaming keep the limits they
// started with. cfg must not be changed afterwards: callers pass a clone.
func (s *Store) Reload(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	s.live.Store(cfg)
}

// conf returns the config from the last Reload, else the startup one.
func (c *core) conf() *config.Config {
	if l := c.live.Load(); l != nil {
		return l
	}
	return c.cfg
}
