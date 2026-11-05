package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// Day phase 3: safety (sandbox, command policy, bind safety). See docs/SCOPE.md
// "Safety specification (day phase 3)".

// SandboxConfig holds the defaults for running agent sessions under bubblewrap.
type SandboxConfig struct {
	Default      string   `json:"default"`        // "none" (default) | "bwrap"
	Strict       bool     `json:"strict"`         // refuse to start a session when bwrap is wanted but missing
	Network      bool     `json:"network"`        // keep the network (default true; needed for the gateway)
	ReadOnlyHome bool     `json:"read_only_home"` // home is read-only except the workspace (default true)
	ExtraRO      []string `json:"extra_ro"`       // extra read-only binds
	ExtraRW      []string `json:"extra_rw"`       // extra read-write binds
	HideHome     []string `json:"hide_home"`      // replaced with an empty tmpfs
}

// PolicyConfig is the built-in agent's run_command guard rail (prefix patterns).
type PolicyConfig struct {
	Allow     []string `json:"allow"`      // run without asking (even in ask mode)
	Deny      []string `json:"deny"`       // never run; not askable
	AskAlways []string `json:"ask_always"` // asked even in auto mode
}

func safetyDefaults(c *Config) {
	c.Sandbox = SandboxConfig{
		Default:      "none",
		Network:      true,
		ReadOnlyHome: true,
		HideHome: []string{"~/.ssh", "~/.gnupg", "~/.config/cerberus", "~/.local/share/keyrings",
			"~/.mozilla", "~/.config/google-chrome"},
	}
	c.Agent.Policy = PolicyConfig{
		Deny: []string{"rm -rf /", "rm -rf ~", "mkfs", "dd of=/dev/", ":(){", "shutdown", "reboot",
			"systemctl --user stop cerberus", "systemctl reboot", "systemctl poweroff", "poweroff", "halt",
			"git push --force", "git push -f", "git branch",
			"git checkout -b", "git switch -c", "systemctl halt", "git worktree add -b", "git worktree add -B"},
		AskAlways: []string{"git push", "sudo", "npm publish", "pip install", "dnf", "rm -r"},
	}
}

// tokenMu guards Server.Token and Server.TokenCreated of every Config: the
// API rotates the admin token while the session manager derives agent tokens
// and other routes save the config. Writers use SetToken; readers use
// TokenSnapshot (or the token derivations, which call it). One lock for all
// configs keeps Config copyable.
var tokenMu sync.RWMutex

// TokenSnapshot returns the current admin token, safe against a concurrent
// SetToken.
func (c *Config) TokenSnapshot() string {
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	return c.Server.Token
}

// TokenCreatedAt returns Server.TokenCreated (zero when unknown, e.g. a
// config from before day phase 3), safe against a concurrent SetToken.
func (c *Config) TokenCreatedAt() time.Time {
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	return c.Server.TokenCreated
}

// SetToken replaces the admin token and stamps Server.TokenCreated. It does
// not save; call Save afterwards.
func (c *Config) SetToken(tok string) {
	tokenMu.Lock()
	c.Server.Token = tok
	c.Server.TokenCreated = time.Now().UTC().Truncate(time.Second)
	tokenMu.Unlock()
}

// RotateToken makes tok the admin token, on disk first and in memory only
// once the file is written, so a failed write leaves the live token valid.
// It re-reads config.json and changes only server.token and
// server.token_created, keeping every other key as it is on disk (edits
// the owner made while the daemon runs, and keys this build does not
// know). A missing file is written from the in-memory config.
func (c *Config) RotateToken(tok string) error {
	if c.path == "" {
		return errors.New("config has no file")
	}
	created := time.Now().UTC().Truncate(time.Second)
	raw, err := os.ReadFile(c.path)
	var out []byte
	switch {
	case errors.Is(err, fs.ErrNotExist):
		tokenMu.RLock()
		cp := *c
		tokenMu.RUnlock()
		cp.Server.Token, cp.Server.TokenCreated = tok, created
		if out, err = json.MarshalIndent(&cp, "", "  "); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		doc := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return errors.New("config " + c.path + " is not valid JSON; fix it before rotating the token: " + err.Error())
		}
		srv := map[string]json.RawMessage{}
		if b, ok := doc["server"]; ok && len(b) > 0 && string(b) != "null" {
			if err := json.Unmarshal(b, &srv); err != nil {
				return errors.New("config " + c.path + ": server is not an object: " + err.Error())
			}
		}
		tb, _ := json.Marshal(tok)
		cb, _ := json.Marshal(created)
		srv["token"], srv["token_created"] = tb, cb
		sb, err := json.Marshal(srv)
		if err != nil {
			return err
		}
		doc["server"] = sb
		if out, err = json.MarshalIndent(doc, "", "  "); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	if err := platform.WritePrivate(c.path, append(out, '\n')); err != nil {
		return err
	}
	tokenMu.Lock()
	c.Server.Token, c.Server.TokenCreated = tok, created
	tokenMu.Unlock()
	return nil
}
