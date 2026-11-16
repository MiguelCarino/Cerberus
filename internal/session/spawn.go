package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"carino.systems/cerberus/internal/config"
)

// ReasonNeedsApproval is the WaitingReason of a spawned child held in the
// queue until a person approves it (cfg.Agent.Spawn.RequireApproval).
const ReasonNeedsApproval = "needs approval"

// mcpConfigName is the generated MCP config of a session, in its folder.
const mcpConfigName = "mcp.json"

// ErrSpawnLimit is wrapped by every refusal of CanSpawn.
var ErrSpawnLimit = errors.New("spawn refused")

// ErrAgentArg is wrapped by agentArgErr's refusals (a bad request).
var ErrAgentArg = errors.New("invalid agent argument")

// spawnConfig is cfg.Agent.Spawn.
func (m *Manager) spawnConfig() config.SpawnConfig { return m.conf().Agent.Spawn }

// CanSpawn reports whether session parent may spawn one more child now:
// spawning is enabled, the parent exists and is not accepted or discarded,
// the child would be no deeper than MaxDepth, and the parent's open
// children and the open sessions of its whole tree stay within MaxChildren
// and MaxTotal. Open means not accepted or discarded: discarding a child
// frees its slot.
func (m *Manager) CanSpawn(parent string) error { return m.CanSpawnN(parent, 1) }

// CanSpawnN is CanSpawn for n children at once.
func (m *Manager) CanSpawnN(parent string, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.canSpawnLocked(parent, n)
	return err
}

func (m *Manager) canSpawnLocked(parent string, n int) (*session, error) {
	sc := m.spawnConfig()
	if !sc.Enabled {
		return nil, fmt.Errorf("%w: agents may not start sessions (agent.spawn.enabled is off)", ErrSpawnLimit)
	}
	p, ok := m.sessions[parent]
	if !ok {
		return nil, fmt.Errorf("%w: parent session %s not found", ErrSpawnLimit, parent)
	}
	if final(p.info.State) {
		return nil, fmt.Errorf("%w: parent session is %s", ErrSpawnLimit, p.info.State)
	}
	if d := p.info.Depth + 1; d > sc.MaxDepth {
		return nil, fmt.Errorf("%w: depth %d would exceed the limit of %d", ErrSpawnLimit, d, sc.MaxDepth)
	}
	children := 0
	for _, s := range m.sessions {
		if s.info.Parent == parent && !final(s.info.State) {
			children++
		}
	}
	if children+n > sc.MaxChildren {
		return nil, fmt.Errorf("%w: %d open children, the limit is %d", ErrSpawnLimit, children, sc.MaxChildren)
	}
	root := m.rootLocked(parent)
	total := 0
	for _, id := range m.descendantsLocked(root) {
		if !final(m.sessions[id].info.State) {
			total++
		}
	}
	if total+n > sc.MaxTotal {
		return nil, fmt.Errorf("%w: %d open spawned sessions in this tree, the limit is %d", ErrSpawnLimit, total, sc.MaxTotal)
	}
	return p, nil
}

// rootLocked follows Parent links up from id to the session a person
// started (a missing parent ends the walk; a cycle cannot form, but the
// walk is bounded anyway).
func (m *Manager) rootLocked(id string) string {
	for range 64 {
		s, ok := m.sessions[id]
		if !ok || s.info.Parent == "" {
			return id
		}
		if _, ok := m.sessions[s.info.Parent]; !ok {
			return id
		}
		id = s.info.Parent
	}
	return id
}

// descendantsLocked lists every session below root, breadth first by
// creation.
func (m *Manager) descendantsLocked(root string) []string {
	kids := map[string][]string{}
	for _, id := range m.order {
		if p := m.sessions[id].info.Parent; p != "" {
			kids[p] = append(kids[p], id)
		}
	}
	var out []string
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, k := range kids[id] {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
				queue = append(queue, k)
			}
		}
	}
	return out
}

// Descendants returns every session spawned below root (children,
// grandchildren…), breadth first, oldest first within a level. Root itself
// is not included.
func (m *Manager) Descendants(root string) []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.descendantsLocked(root)
	out := make([]Info, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.snapshotLocked(m.sessions[id]))
	}
	return out
}

// IsDescendant reports whether id is somewhere below root.
func (m *Manager) IsDescendant(root, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Contains(m.descendantsLocked(root), id)
}

// CanClientSpawn reports whether MCP client may start n more sessions now:
// spawning is enabled and the client's open sessions (those it started,
// not their children, which count against each session's own tree) stay
// within MaxChildren.
func (m *Manager) CanClientSpawn(client string, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canClientSpawnLocked(client, n)
}

func (m *Manager) canClientSpawnLocked(client string, n int) error {
	sc := m.spawnConfig()
	if !sc.Enabled {
		return fmt.Errorf("%w: agents may not start sessions (agent.spawn.enabled is off)", ErrSpawnLimit)
	}
	open := 0
	for _, s := range m.sessions {
		if s.info.Client == client && !final(s.info.State) {
			open++
		}
	}
	if open+n > sc.MaxChildren {
		return fmt.Errorf("%w: %d open sessions started by %s, the limit is %d", ErrSpawnLimit, open, client, sc.MaxChildren)
	}
	return nil
}

// clientTreeLocked lists the sessions client started, each followed by its
// descendants, oldest first.
func (m *Manager) clientTreeLocked(client string) []string {
	if client == "" {
		return nil
	}
	var out []string
	for _, id := range m.order {
		if s := m.sessions[id]; s != nil && s.info.Client == client {
			out = append(out, id)
			out = append(out, m.descendantsLocked(id)...)
		}
	}
	return out
}

// ClientSessions returns the sessions MCP client started and everything
// they spawned, oldest root first.
func (m *Manager) ClientSessions(client string) []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.clientTreeLocked(client)
	out := make([]Info, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.snapshotLocked(m.sessions[id]))
	}
	return out
}

// IsClientSession reports whether id was started by MCP client or spawned
// below one of its sessions.
func (m *Manager) IsClientSession(client, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return id != "" && slices.Contains(m.clientTreeLocked(client), id)
}

// agentArgErr refuses a value an agent chose that would be read as a
// command-line option: {task} and {model} expand to argv elements of
// their own, so a spawned task such as "--dangerously-skip-permissions"
// would turn into a flag of the child's agent CLI.
func agentArgErr(field, v string) error {
	if strings.HasPrefix(strings.TrimSpace(v), "-") {
		return fmt.Errorf("%w: a spawned session's %s may not start with \"-\"", ErrAgentArg, field)
	}
	return nil
}

// Approve decides on a spawned child waiting for approval: ok lets it into
// the queue, otherwise it is discarded.
func (m *Manager) Approve(id string, ok bool) error {
	m.mu.Lock()
	s, err := m.lookup(id)
	if err == nil && (!s.needsApproval || s.info.State != StateQueued) {
		err = errors.New("session is not waiting for approval")
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if ok {
		s.needsApproval = false
		s.info.WaitingReason = ""
		m.changedLocked(s)
		m.recordLocked(s, FeedState, "approved")
		m.mu.Unlock()
		m.signal()
		return nil
	}
	// needsApproval stays set until Discard has taken the session over, so
	// the scheduler never sees a rejected spawn as runnable in between.
	s.info.WaitingReason = "rejected"
	m.recordLocked(s, FeedState, "rejected")
	m.mu.Unlock()
	return m.Discard(id)
}

// sameWorkspace points a spawned child with Workspace "same" at its
// parent's workspace. A repo parent's child becomes a repo session of the
// same repo (its own worktree, resolved as usual: done is false). A folder
// or scratch parent's child edits the parent's folder in place as a folder
// session; the parent's scratch folder is Cerberus' own and so outside the
// allowed folders, which is why it is set here directly (done is true).
func (m *Manager) sameWorkspace(t *TaskSpec) (done bool, err error) {
	m.mu.Lock()
	p, ok := m.sessions[t.Parent]
	var info Info
	if ok {
		info = p.info
	}
	m.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("%w: parent session %s not found", ErrSpawnLimit, t.Parent)
	}
	switch info.Workspace {
	case WorkspaceRepo:
		t.Workspace, t.Repo, t.Dir = WorkspaceRepo, info.Repo, ""
		return false, nil
	case WorkspaceFolder:
		t.Workspace, t.Dir, t.Repo = WorkspaceFolder, info.Dir, ""
		return false, nil
	default:
		wt := info.Worktree
		if wt == "" || !within(wt, m.sessDir) {
			return false, errors.New("the parent session's workspace no longer exists")
		}
		if st, err := os.Stat(wt); err != nil || !st.IsDir() {
			return false, errors.New("the parent session's workspace no longer exists")
		}
		t.Workspace, t.Dir, t.Repo = WorkspaceFolder, wt, ""
		return true, nil
	}
}

// agentKey is what session id's agent gets as OPENAI_API_KEY: its own
// per-session agent token (cfg.SessionAgentToken), which works for /v1 and
// for the restricted /api/agent/* scope of that session only. Never the
// admin token. "cerberus" when there is no server token.
func (m *Manager) agentKey(id string) string {
	return orDefault(m.cfg.SessionAgentToken(id), "cerberus")
}

// isClaudeProfile reports whether p runs Claude Code (its binary is claude).
func isClaudeProfile(p config.AgentProfile) bool {
	bin := strings.TrimSuffix(filepath.Base(profileBinary(p)), ".exe")
	return bin == "claude"
}

// mcpStyle is how p's sessions get the cerberus_* tools: its mcp_style,
// else from the binary's name (claude, codex, opencode), else none. Aider
// has no MCP client, so it is none.
func mcpStyle(p config.AgentProfile) string {
	switch p.MCPStyle {
	case config.MCPStyleClaude, config.MCPStyleCodex, config.MCPStyleOpencode, config.MCPStyleNone:
		return p.MCPStyle
	}
	switch strings.TrimSuffix(filepath.Base(profileBinary(p)), ".exe") {
	case "claude":
		return config.MCPStyleClaude
	case "codex":
		return config.MCPStyleCodex
	case "opencode":
		return config.MCPStyleOpencode
	}
	return config.MCPStyleNone
}

// Names of the per-session MCP files and the token variable.
const (
	opencodeConfigName = "opencode.json"
	// agentTokenEnv carries the session's agent token to `cerberus mcp`
	// for agents that pass a server only the variables they are told to
	// (Codex). It is the same token as OPENAI_API_KEY, which such agents
	// keep for their own provider.
	agentTokenEnv = "CERBERUS_AGENT_TOKEN"
)

// mcpWiring is what mcpArgs adds to a session's launch: argv is the whole
// command with its flags in place, flags the flags it added (for the
// setup record), env the variables to add.
type mcpWiring struct {
	argv  []string
	flags []string
	env   []string
}

// codexIndex is the index of the first argv element that is the codex
// program (base name codex, or codex.exe; "@openai/codex" counts), -1 when
// there is none.
func codexIndex(argv []string) int {
	return slices.IndexFunc(argv, func(a string) bool {
		b := a
		if i := strings.LastIndexAny(b, `/\`); i >= 0 {
			b = b[i+1:] // both separators, whatever this OS uses
		}
		return strings.TrimSuffix(strings.ToLower(b), ".exe") == "codex"
	})
}

// mcpArgs wires `cerberus mcp` (the cerberus_* tools, with the session's
// own agent token) into the agent's command, by the profile's MCP style:
//
//   - claude: DATA/sessions/<id>/mcp.json and --mcp-config <it>, as
//     before;
//   - codex: -c mcp_servers.cerberus.{command,args,env_vars} right after
//     the codex program (not a wrapper before it), the token in
//     CERBERUS_AGENT_TOKEN (never in argv);
//   - opencode: DATA/sessions/<id>/opencode.json with an "mcp" entry whose
//     environment reads the token as {env:CERBERUS_AGENT_TOKEN} (never in
//     the file), and OPENCODE_CONFIG pointing at it.
//
// Nothing is added for other styles, when spawning is disabled, or when
// the profile already wires its own (--mcp-config, a cerberus
// mcp_servers override, OPENCODE_CONFIG in its env).
func (m *Manager) mcpArgs(p config.AgentProfile, argv []string, sessDir, id string) (mcpWiring, error) {
	out := mcpWiring{argv: argv}
	if !m.spawnConfig().Enabled || len(argv) == 0 {
		return out, nil
	}
	url := serverURL(m.conf().Server.Listen)
	switch mcpStyle(p) {
	case config.MCPStyleClaude:
		if slices.Contains(argv, "--mcp-config") {
			return out, nil
		}
		cfg := map[string]any{"mcpServers": map[string]any{"cerberus": map[string]any{
			"command": selfPath(),
			"args":    []string{"mcp"},
			"env": map[string]string{
				"CERBERUS_URL":     url,
				"OPENAI_API_KEY":   m.agentKey(id), // the session's agent token, never the admin token
				"CERBERUS_SESSION": id,
			},
		}}}
		raw, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return out, err
		}
		path := filepath.Join(sessDir, mcpConfigName)
		if err := safeWrite(path, raw, 0o600); err != nil {
			return out, err
		}
		out.flags = []string{"--mcp-config", path}
		out.argv = append(slices.Clone(argv), out.flags...)
	case config.MCPStyleCodex:
		for _, a := range argv {
			if strings.Contains(a, "mcp_servers.cerberus") {
				return out, nil
			}
		}
		// Right after the codex program itself, which is argv[0] only when
		// the profile runs codex directly: with a wrapper (npx -y
		// @openai/codex exec, bash -lc "codex ...") the flags after
		// argv[0] would be the wrapper's (npx -c runs a command, bash -c a
		// script). No codex element: nothing is added.
		at := codexIndex(argv)
		if at < 0 {
			m.log.Warn("mcp_style codex: no codex program in the command; the cerberus tools are not wired",
				"profile", p.Name, "command", argv[0])
			return out, nil
		}
		self, _ := json.Marshal(selfPath())
		out.flags = []string{
			"-c", "mcp_servers.cerberus.command=" + string(self),
			"-c", `mcp_servers.cerberus.args=["mcp"]`,
			"-c", `mcp_servers.cerberus.env_vars=["` + agentTokenEnv + `","CERBERUS_URL","CERBERUS_SESSION"]`,
		}
		out.argv = slices.Concat(argv[:at+1], out.flags, argv[at+1:])
	case config.MCPStyleOpencode:
		if _, own := p.Env["OPENCODE_CONFIG"]; own {
			return out, nil
		}
		cfg := map[string]any{"$schema": "https://opencode.ai/config.json", "mcp": map[string]any{"cerberus": map[string]any{
			"type":    "local",
			"command": []string{selfPath(), "mcp"},
			"enabled": true,
			"environment": map[string]string{
				"CERBERUS_URL":     url,
				"CERBERUS_SESSION": id,
				agentTokenEnv:      "{env:" + agentTokenEnv + "}", // read from the agent's environment
			},
		}}}
		raw, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return out, err
		}
		path := filepath.Join(sessDir, opencodeConfigName)
		if err := safeWrite(path, raw, 0o600); err != nil {
			return out, err
		}
		out.flags = []string{"OPENCODE_CONFIG=" + path}
		out.env = []string{"OPENCODE_CONFIG=" + path} // the token is in every session's env (Manager.env)
	}
	return out, nil
}
