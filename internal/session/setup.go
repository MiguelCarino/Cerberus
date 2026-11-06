package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/records"
)

// Setup snapshot (docs/SCOPE.md "E3 Setup snapshot"): at each run start the
// manager writes DATA/sessions/<id>/setup.json, the setup the run actually
// had, and keeps its hash on Info.SetupHash, so two runs can be compared like
// for like. Nothing is loaded to find any of it out.

const (
	setupFile = "setup.json"
	// setupHashLen is how many hex characters of the SHA-256 Info.SetupHash keeps.
	setupHashLen = 12
	// versionTimeout bounds one `agent --version` probe.
	versionTimeout = 3 * time.Second
	// versionMax is the longest version line kept, in characters.
	versionMax = 200
	// setupMaxBytes bounds a setup.json (or a profile's own MCP config) read back.
	setupMaxBytes = 1 << 20
)

// Setup is the setup one run started with. Field order is fixed, and maps
// are never used, so its JSON is canonical and the hash is stable.
type Setup struct {
	Agent      SetupAgent `json:"agent"`
	Model      SetupModel `json:"model"`
	MCPServers []SetupMCP `json:"mcp_servers"`
	Workspace  string     `json:"workspace"`
	BaseCommit string     `json:"base_commit,omitempty"`
	Cerberus   string     `json:"cerberus_version"`
}

// SetupAgent is the agent profile as resolved for the run.
type SetupAgent struct {
	Name string `json:"name"`
	// Binary is the agent's executable as found on PATH.
	Binary string `json:"binary,omitempty"`
	// Command is the profile's command line as configured (placeholders
	// such as {task} and {model} left unexpanded, so runs of different
	// tasks still compare equal), secrets masked. A resumed run records the
	// resume command and Resume.
	Command []string `json:"command"`
	Resume  bool     `json:"resume,omitempty"`
	// EnvKeys are the profile's env and secret_env names, never values.
	EnvKeys []string `json:"env_keys"`
	// Version is the first line of `BINARY --version` ("" when it failed).
	Version     string `json:"version"`
	Interactive bool   `json:"interactive,omitempty"`
	// Sandbox is "bwrap" or "none": how the run was actually confined.
	Sandbox string `json:"sandbox"`
}

// SetupModel is the model the run was given ("" fields: not known).
type SetupModel struct {
	// Requested is the session's model as asked for (it may be an alias
	// such as "default"); ID is what the engine resolved it to.
	Requested string `json:"requested,omitempty"`
	ID        string `json:"id,omitempty"`
	Engine    string `json:"engine,omitempty"`
	Path      string `json:"path,omitempty"`
	Quant     string `json:"quantization,omitempty"`
	Ctx       int    `json:"context,omitempty"`
}

// SetupMCP is one MCP server the session was given.
type SetupMCP struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

// SetupView is what GET /api/sessions/{id}/setup returns: the stored setup,
// its hash recomputed from the file, and whether that matches the hash
// recorded at run start (the session folder is writable by a sandboxed
// agent, so a changed file shows).
type SetupView struct {
	Hash     string `json:"hash"`
	Recorded string `json:"recorded_hash"`
	Verified bool   `json:"verified"`
	Setup    Setup  `json:"setup"`
}

// SetupDiff is one field that differs between two setups.
type SetupDiff struct {
	Field string `json:"field"`
	A     string `json:"a"`
	B     string `json:"b"`
}

// ErrNoSetup is returned by Setup for a session with no setup.json (it has
// not run yet, or it is from before setup snapshots).
var ErrNoSetup = errors.New("this session has no setup snapshot")

// Hash is the first 12 hex characters of the SHA-256 of the setup's
// canonical JSON.
func (s Setup) Hash() string {
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:setupHashLen]
}

// SetupField is one flattened setup field.
type SetupField struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// Fields flattens s into dotted field names and display values, in a fixed
// order (the order DiffSetups and the CLI use).
func (s Setup) Fields() []SetupField {
	join := func(v []string) string { return strings.Join(v, " ") }
	mcp := make([]string, 0, len(s.MCPServers))
	for _, m := range s.MCPServers {
		mcp = append(mcp, m.Name+"="+join(m.Command))
	}
	ctx := ""
	if s.Model.Ctx > 0 {
		ctx = fmt.Sprint(s.Model.Ctx)
	}
	return []SetupField{
		{"agent.name", s.Agent.Name},
		{"agent.binary", s.Agent.Binary},
		{"agent.command", join(s.Agent.Command)},
		{"agent.resume", fmt.Sprint(s.Agent.Resume)},
		{"agent.env_keys", strings.Join(s.Agent.EnvKeys, ",")},
		{"agent.version", s.Agent.Version},
		{"agent.interactive", fmt.Sprint(s.Agent.Interactive)},
		{"agent.sandbox", s.Agent.Sandbox},
		{"model.requested", s.Model.Requested},
		{"model.id", s.Model.ID},
		{"model.engine", s.Model.Engine},
		{"model.path", s.Model.Path},
		{"model.quantization", s.Model.Quant},
		{"model.context", ctx},
		{"mcp_servers", strings.Join(mcp, "; ")},
		{"workspace", s.Workspace},
		{"base_commit", s.BaseCommit},
		{"cerberus_version", s.Cerberus},
	}
}

// DiffSetups lists the fields that differ between a and b, in a fixed
// order (empty: the same setup).
func DiffSetups(a, b Setup) []SetupDiff {
	fa, fb := a.Fields(), b.Fields()
	var out []SetupDiff
	for i := range fa {
		if fa[i].Value != fb[i].Value {
			out = append(out, SetupDiff{Field: fa[i].Field, A: fa[i].Value, B: fb[i].Value})
		}
	}
	return out
}

// Setup returns session id's stored setup snapshot.
func (m *Manager) Setup(id string) (SetupView, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	var dir, recorded string
	if ok {
		dir, recorded = s.dir, s.info.SetupHash
	}
	m.mu.Unlock()
	if !ok {
		return SetupView{}, ErrNotFound
	}
	f, err := os.OpenFile(filepath.Join(dir, setupFile), os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, os.ErrNotExist) {
		return SetupView{}, ErrNoSetup
	}
	if err != nil {
		return SetupView{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, setupMaxBytes+1))
	if err != nil {
		return SetupView{}, err
	}
	if len(raw) > setupMaxBytes {
		return SetupView{}, fmt.Errorf("%s is too large", setupFile)
	}
	var st Setup
	if err := json.Unmarshal(raw, &st); err != nil {
		return SetupView{}, fmt.Errorf("read %s: %w", setupFile, err)
	}
	h := st.Hash()
	return SetupView{Hash: h, Recorded: recorded, Verified: h != "" && h == recorded, Setup: st}, nil
}

// setupInput is what launch knows about the run it is starting.
type setupInput struct {
	info    Info
	profile config.AgentProfile
	command []string // the profile command used (Command or ResumeCommand), unexpanded
	resume  bool
	argv    []string // the expanded command, before the sandbox and scope wrap it
	mcp     []string // the flags mcpArgs added ("" = none)
	sandbox bool
	secrets map[string]string
}

// writeSetup builds the run's setup, writes setup.json in the session
// folder and returns its hash ("" when it could not be written: the run
// starts anyway). It may run the agent with --version and ask the engine
// about the model, so it is never called with m.mu held.
func (m *Manager) writeSetup(sessDir string, in setupInput) string {
	st := m.buildSetup(in)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		err = safeWrite(filepath.Join(sessDir, setupFile), append(raw, '\n'), 0o600)
	}
	if err != nil {
		m.log.Warn("write setup snapshot", "id", in.info.ID, "err", err)
		return ""
	}
	return st.Hash()
}

func (m *Manager) buildSetup(in setupInput) Setup {
	p := in.profile
	vals := m.secretValues()
	for _, v := range in.secrets {
		vals = append(vals, v)
	}
	mask := newArgMasker(vals)

	keys := make([]string, 0, len(p.Env)+len(p.SecretEnv))
	for k := range p.Env {
		keys = append(keys, k)
	}
	for k := range p.SecretEnv {
		if _, dup := p.Env[k]; !dup {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	bin := ""
	if len(in.argv) > 0 {
		bin = in.argv[0]
		if lp, err := exec.LookPath(bin); err == nil {
			bin = lp
		}
		if abs, err := filepath.Abs(bin); err == nil {
			bin = abs
		}
	}
	sandbox := "none"
	if in.sandbox {
		sandbox = "bwrap"
	}
	st := Setup{
		Agent: SetupAgent{Name: p.Name, Binary: bin, Command: mask.args(in.command), Resume: in.resume,
			EnvKeys: keys, Version: m.agentVersion(bin), Interactive: p.Interactive, Sandbox: sandbox},
		Model:      m.setupModel(in.info, p),
		MCPServers: m.setupMCP(in, mask),
		Workspace:  in.info.Workspace,
		Cerberus:   m.version,
	}
	if in.info.Workspace == WorkspaceRepo {
		st.BaseCommit = in.info.BaseCommit
	}
	if st.Agent.EnvKeys == nil {
		st.Agent.EnvKeys = []string{}
	}
	if st.Agent.Command == nil {
		st.Agent.Command = []string{}
	}
	return st
}

// setupModel resolves the session's model through the engine's listing
// (cached; it never loads a model).
func (m *Manager) setupModel(info Info, p config.AgentProfile) SetupModel {
	sm := SetupModel{Requested: info.Model}
	if info.Model == "" && !p.NeedsModel && !usesModel(p) {
		return sm
	}
	if m.eng == nil {
		sm.ID = info.Model
		return sm
	}
	ctx, cancel := context.WithTimeout(m.ctx, resolveTimeout)
	defer cancel()
	mi, err := m.eng.Resolve(ctx, info.Model)
	if err != nil {
		sm.ID = info.Model
		return sm
	}
	sm.ID, sm.Engine, sm.Path, sm.Ctx = mi.ID, mi.Backend, mi.Path, mi.Ctx
	sm.Quant = quantOf(mi.Path, mi.Name, mi.ID)
	return sm
}

// quantRe finds a quantization tag (Q4_K_M, IQ3_XXS, Q8_0, F16, BF16...) in
// a model file or name.
var quantRe = regexp.MustCompile(`(?i)(?:^|[-_.:/ ])((?:I?Q[1-8](?:_[A-Z0-9]{1,4}){0,3})|BF16|FP16|F16|F32|FP8|MXFP4)(?:[-_.: ]|$)`)

// quantOf is the first quantization tag found in names ("" = none).
func quantOf(names ...string) string {
	for _, n := range names {
		if n == "" {
			continue
		}
		if sm := quantRe.FindStringSubmatch(filepath.Base(n)); sm != nil {
			return strings.ToUpper(sm[1])
		}
	}
	return ""
}

// setupMCP lists the MCP servers the run was given: the session's own
// `cerberus mcp` when mcpArgs added it, else the servers of a config file
// the profile's command names with --mcp-config (best effort).
func (m *Manager) setupMCP(in setupInput, mask argMasker) []SetupMCP {
	out := []SetupMCP{}
	if len(in.mcp) > 0 {
		return append(out, SetupMCP{Name: "cerberus", Command: []string{selfPath(), "mcp"}})
	}
	i := slices.Index(in.argv, "--mcp-config")
	if i < 0 || i+1 >= len(in.argv) {
		return out
	}
	path := in.argv[i+1]
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.info.Worktree, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, setupMaxBytes))
	if err != nil {
		return out
	}
	var cfg struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			URL     string   `json:"url"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return out
	}
	for name, sv := range cfg.Servers {
		cmd := append([]string{sv.Command}, sv.Args...)
		if sv.Command == "" {
			cmd = []string{sv.URL}
		}
		out = append(out, SetupMCP{Name: name, Command: mask.args(cmd)})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// ---------- agent --version, cached per binary ----------

type versionKey struct {
	path string
	mod  time.Time
	size int64
}

// versionCache keeps `--version` lines per binary path and modification
// time, so a binary is asked once until it is replaced.
type versionCache struct {
	mu sync.Mutex
	m  map[versionKey]string
}

// agentVersion is bin's version line, probed once per path and mtime. The
// running cerberus binary (the built-in agent) is never run: its version
// is known.
func (m *Manager) agentVersion(bin string) string {
	if bin == "" {
		return ""
	}
	if self, err := filepath.Abs(selfPath()); err == nil && self == bin {
		return strings.TrimSpace("cerberus " + m.version)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return ""
	}
	key := versionKey{path: bin, mod: fi.ModTime(), size: fi.Size()}
	m.versions.mu.Lock()
	v, ok := m.versions.m[key]
	m.versions.mu.Unlock()
	if ok {
		return v
	}
	probe := m.probeVersion
	if probe == nil {
		probe = defaultVersionProbe
	}
	v = clipVersion(probe(m.ctx, bin))
	m.versions.mu.Lock()
	if m.versions.m == nil {
		m.versions.m = map[versionKey]string{}
	}
	m.versions.m[key] = v
	m.versions.mu.Unlock()
	return v
}

// defaultVersionProbe runs `bin --version`, except inside a test binary,
// where agents are fake scripts that must not run an extra time (tests set
// Manager.probeVersion to exercise the real probe).
func defaultVersionProbe(ctx context.Context, bin string) string {
	if testing.Testing() {
		return ""
	}
	return execVersion(ctx, bin)
}

// execVersion runs `bin --version` in the temp folder with no input and a
// 3 s limit, and returns its output ("" on any failure).
func execVersion(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = time.Second // a grandchild holding the pipe never blocks us
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 4096}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return out.String()
}

// clipVersion is the first non-empty line of out, at most versionMax
// characters.
func clipVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.ToValidUTF8(line, ""))
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > versionMax {
			line = string([]rune(line)[:versionMax])
		}
		return line
	}
	return ""
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.n <= 0 {
		return n, nil
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	l.n -= len(p)
	_, err := l.w.Write(p)
	return n, err
}

// ---------- masking ----------

// secretFlag matches a flag or variable name that carries a secret.
var secretFlag = regexp.MustCompile(`(?i)(key|token|secret|passw(or)?d|auth|credential|bearer)`)

// secretQuery matches a URL query (or fragment) parameter whose name looks
// secret, up to its value: "?api_key=" + "sk-...".
var secretQuery = regexp.MustCompile(`(?i)([?&;#][^=&;#\s]*(?:key|token|secret|passw(?:or)?d|auth|credential|bearer|sig|signature|session)[^=&;#\s]*=)([^&;#\s]+)`)

// urlUserinfo matches the password of a URL's userinfo: "https://u:" + "pw" + "@".
var urlUserinfo = regexp.MustCompile(`(://[^/\s:@]*:)([^@/\s]+)(@)`)

// secretHeader matches a header argument whose name looks secret, such as
// "Authorization: Bearer x" or "X-Api-Key: x" (curl -H, an MCP header).
var secretHeader = regexp.MustCompile(`(?i)^(\s*[A-Za-z0-9_-]*(?:key|token|secret|passw(?:or)?d|auth|credential|bearer|cookie)[A-Za-z0-9_-]*\s*:\s*)(\S.*)$`)

// argMasker hides secrets in a command line: vault and injected values
// anywhere, API key and token shapes anywhere, secret-named URL query
// parameters and URL passwords, a secret-named header argument ("Name:
// value"), the value of a --flag=V or NAME=V whose name looks secret, and
// the argument after such a flag.
type argMasker struct{ r redactor }

// MaskTokens replaces every API key or token shape in s (provider keys,
// GitHub and Slack tokens, Cerberus session agent tokens and keys) with
// the session mask, whether or not the vault holds it. Text that leaves
// the daemon unseen by a person (an eval's judge evidence) passes through
// it after MaskSecrets.
func MaskTokens(s string) string { return records.MaskTokens(s, redactMask) }

// maskText hides what can be told from one argument alone: secret query
// parameters, URL passwords, token shapes and secret headers.
func maskText(s string) string {
	s = secretQuery.ReplaceAllString(s, "${1}"+redactMask)
	s = urlUserinfo.ReplaceAllString(s, "${1}"+redactMask+"${3}")
	s = records.MaskTokens(s, redactMask)
	if m := secretHeader.FindStringSubmatch(s); m != nil && !strings.Contains(m[1], "://") && !isPlaceholder(strings.TrimSpace(m[2])) {
		s = m[1] + redactMask
	}
	return s
}

func newArgMasker(vals []string) argMasker {
	var a argMasker
	a.r.set(vals)
	return a
}

func (a argMasker) args(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	maskNext := false
	for i, s := range in {
		if len(a.r.values) > 0 {
			s = string(a.r.mask(nil, []byte(s)))
		}
		s = maskText(s)
		switch {
		case maskNext && !strings.HasPrefix(s, "-"):
			s = redactMask
			maskNext = false
		case strings.Contains(s, "="):
			name, val, _ := strings.Cut(s, "=")
			if val != "" && !isPlaceholder(val) && secretFlag.MatchString(name) && !strings.ContainsAny(name, " /") {
				s = name + "=" + redactMask
			}
			maskNext = false
		case strings.HasPrefix(s, "-") && secretFlag.MatchString(s):
			maskNext = true
		default:
			maskNext = false
		}
		if maskNext && i+1 < len(in) && isPlaceholder(in[i+1]) {
			maskNext = false // "--api-key {api_key}"-style placeholders are not secrets
		}
		out[i] = s
	}
	return out
}

// isPlaceholder reports whether s is one profile placeholder such as {model}.
func isPlaceholder(s string) bool {
	return len(s) > 2 && s[0] == '{' && s[len(s)-1] == '}' && !strings.ContainsAny(s[1:len(s)-1], "{} ")
}
