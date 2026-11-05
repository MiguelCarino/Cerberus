package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"carino.systems/cerberus/internal/agent"
	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/config"
)

// MCP clients: an outside agent (Claude Code, say) that runs
// `cerberus mcp --client NAME` as a stdio MCP server. Its token is a device
// paired with scope "mcp" (`cerberus pair --mcp --name NAME`), which opens
// only the agent scope: delegate to local models and start and steer the
// sessions it started. The token is kept in the config folder, mode 0600,
// under mcp-clients/NAME.token, where agents' file tools never reach
// (the config folder is a protected path).

const mcpClientDir = "mcp-clients"

// clientDenyPaths are protected for an MCP client's file reads on top of
// agent.deny_paths and the config folder: the usual places of secrets in a
// home folder and a project.
var clientDenyPaths = []string{
	"**/.env", "**/.env.*", "**/*.pem", "**/*.key", "**/id_rsa*", "**/id_ed25519*", "**/id_ecdsa*",
	"**/.netrc", "**/.aws/**", "**/.kube/**", "**/.docker/config.json", "**/.config/gh/**",
	"**/.local/share/keyrings/**", "**/.password-store/**",
}

// mcpClientTokenPath is where NAME's token is kept.
func mcpClientTokenPath(cfg *config.Config, name string) string {
	return filepath.Join(cfg.Dir(), mcpClientDir, name+".token")
}

// saveMCPClientToken writes a client token (0600, in a 0700 folder).
func saveMCPClientToken(cfg *config.Config, name, token string) (string, error) {
	p := mcpClientTokenPath(cfg, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return p, nil
}

// mcpClientCaller builds the tool caller of `cerberus mcp --client NAME`:
// the daemon's local URL from the config (or url), the token from the
// client's token file (or key), and file reads on this side.
func mcpClientCaller(name, cfgPath, url, key string) (*agent.SpawnClient, error) {
	if devices.CleanName(name) != name {
		return nil, fmt.Errorf("%q is not a client name (letters, digits, '-', '_', '.')", name)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if url == "" || url == os.Getenv("CERBERUS_URL") {
		url = localURL(cfg.Server.Listen)
	}
	if key == "" || key == os.Getenv("OPENAI_API_KEY") {
		raw, err := os.ReadFile(mcpClientTokenPath(cfg, name))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no token for MCP client %q: run `cerberus pair --mcp --name %s` first", name, name)
		}
		if err != nil {
			return nil, err
		}
		key = strings.TrimSpace(string(raw))
	}
	if !strings.HasPrefix(key, devices.ClientPrefix) {
		return nil, fmt.Errorf("the token for %q is not an MCP client token", name)
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	deny := append(agent.DefaultDenyPaths(cfg), clientDenyPaths...)
	return agent.NewSpawnClient(url, key, "").LocalFiles(root, deny, cfg.Delegate.MaxFileKiB), nil
}

// pairMCPClient is `cerberus pair --mcp`: pair a client token (through the
// daemon, or the store when it is not running), keep it in the config
// folder and print how to register the MCP server.
func pairMCPClient(cfg *config.Config, name string, stdout, stderr io.Writer) int {
	if name == "" {
		name = "claude-code"
	}
	if clean := devices.CleanName(name); clean != name {
		fmt.Fprintf(stderr, "cerberus pair: %q is not a client name (letters, digits, '-', '_', '.')\n", name)
		return 2
	}
	var res api.PairResult
	err := newClient(cfg).do(context.Background(), "POST", "/api/devices",
		map[string]string{"name": name, "scope": devices.ScopeMCP}, &res)
	if err != nil && !isUnreachable(err) {
		fmt.Fprintln(stderr, "cerberus pair:", err)
		return 1
	}
	if err != nil {
		st, oerr := devices.Open(cfg.Dir())
		if oerr != nil {
			fmt.Fprintln(stderr, "cerberus pair:", oerr)
			return 1
		}
		tok, d, perr := st.PairScoped(name, devices.ScopeMCP, "")
		if perr != nil {
			fmt.Fprintln(stderr, "cerberus pair:", perr)
			return 1
		}
		cliAudit(stderr, "device.pair", "paired MCP client "+d.Name+" (daemon not running)", d.ID)
		res = api.PairResult{Device: d, Token: tok}
	}
	p, err := saveMCPClientToken(cfg, res.Device.Name, res.Token)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus pair: the client is paired but its token could not be saved:", err)
		fmt.Fprintf(stderr, "revoke it with: cerberus devices rm %s\n", res.Device.ID)
		return 1
	}
	exe := "cerberus"
	if e, err := os.Executable(); err == nil {
		exe = e
	}
	cfgArg := ""
	if cfg.Path() != "" {
		if def, err := config.DefaultPath(); err != nil || def != cfg.Path() {
			cfgArg = " --config " + cfg.Path()
		}
	}
	fmt.Fprintf(stdout, "Paired MCP client %q (id %s). Its token is in %s (mode 0600).\n", res.Device.Name, res.Device.ID, p)
	fmt.Fprintf(stdout, "It can delegate to local models and start and steer its own sessions; nothing else.\n\n")
	fmt.Fprintf(stdout, "Register it with Claude Code:\n\n  claude mcp add --scope user cerberus -- %s mcp --client %s%s\n\n", exe, res.Device.Name, cfgArg)
	fmt.Fprintf(stdout, "Revoke it any time: cerberus devices rm %s\n", res.Device.ID)
	return 0
}
