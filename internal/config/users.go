package config

// UsersConfig tunes sign-ins of user accounts (Phase 4a). The accounts
// themselves never live in config.json: they are in DATA/users.json.
type UsersConfig struct {
	SessionIdleHours int `json:"session_idle_hours"` // default 336 (14 days): a login unused this long ends
	SessionMaxDays   int `json:"session_max_days"`   // default 30: a login ends this long after sign-in
	LockoutFailures  int `json:"lockout_failures"`   // default 5 failures per name ...
	LockoutMinutes   int `json:"lockout_minutes"`    // default 15 ... within this window locks the name for this long
}

func usersDefaults(c *Config) {
	c.Users = UsersConfig{SessionIdleHours: 336, SessionMaxDays: 30, LockoutFailures: 5, LockoutMinutes: 15}
}
