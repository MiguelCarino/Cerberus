package config

import "testing"

func TestChatDefaultsAndValidation(t *testing.T) {
	c := Defaults()
	want := ChatConfig{Enabled: true, DefaultMode: "ask", MaxSteps: 12, ResultClip: 12000, CommandTimeoutS: 120, AtriumImport: true}
	if c.Chat != want {
		t.Fatalf("chat defaults = %+v", c.Chat)
	}
	for _, p := range c.Validate() {
		if len(p.Path) >= 5 && p.Path[:5] == "chat." {
			t.Errorf("defaults: %+v", p)
		}
	}
	cases := []struct {
		name, path, level, substr string
		mut                       func(*Config)
	}{
		{"mode", "chat.default_mode", LevelError, "", func(c *Config) { c.Chat.DefaultMode = "yolo" }},
		{"steps", "chat.max_steps", LevelError, "", func(c *Config) { c.Chat.MaxSteps = 0 }},
		{"clip", "chat.result_clip", LevelError, "", func(c *Config) { c.Chat.ResultClip = 10 }},
		{"timeout", "chat.command_timeout_s", LevelError, "", func(c *Config) { c.Chat.CommandTimeoutS = 601 }},
		{"quick claude", "chat.quick_model", LevelError, "gateway", func(c *Config) { c.Chat.QuickModel = "claude:opus" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.mut(c)
			if !hasProblem(c.Validate(), tc.path, tc.level, tc.substr) {
				t.Fatalf("no %s problem at %s: %+v", tc.level, tc.path, c.Validate())
			}
		})
	}
	for _, m := range ChatModes {
		c := Defaults()
		c.Chat.DefaultMode = m
		if hasProblem(c.Validate(), "chat.default_mode", LevelError, "") {
			t.Errorf("mode %q refused", m)
		}
	}
}
