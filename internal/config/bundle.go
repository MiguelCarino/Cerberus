package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Profile bundles (`cerberus export` / `cerberus import`): the portable part
// of a setup, so a second machine can start from the first one's agents,
// repos, providers, prices, budgets, tariff and limits. A bundle never holds
// a secret: provider keys live in the vault, not the config, and secret
// NAMES (key_secret, secret_env) are only kept with ExportOptions.SecretNames.
// Agent env entries and provider headers that look like credentials are
// always left out.

// BundleVersion is the bundle format written by Export. Import reads it and
// older versions.
const BundleVersion = 1

// BundleKind marks a Cerberus profile bundle.
const BundleKind = "cerberus-profile"

// Bundle sections, as named in ExportOptions.Sections.
const (
	SectionAgents    = "agents"
	SectionRepos     = "repos"
	SectionProviders = "providers"
	SectionPrices    = "prices"
	SectionBudgets   = "budgets"
	SectionTariff    = "tariff"
	SectionMonitor   = "monitor"
	SectionLimits    = "limits"
	SectionSandbox   = "sandbox"
	SectionPolicy    = "policy"
	SectionFiles     = "files"
	SectionModels    = "models"
)

// BundleSections lists every section, in bundle order.
func BundleSections() []string {
	return []string{SectionAgents, SectionRepos, SectionProviders, SectionPrices, SectionBudgets, SectionTariff,
		SectionMonitor, SectionLimits, SectionSandbox, SectionPolicy, SectionFiles, SectionModels}
}

// Bundle is the JSON document. Absent sections are left alone on import.
type Bundle struct {
	Version int       `json:"version"`
	Kind    string    `json:"kind"`
	Created time.Time `json:"created,omitzero"`

	Agents       []AgentProfile   `json:"agents,omitempty"`
	DefaultAgent string           `json:"default_agent,omitempty"`
	Repos        []RepoConfig     `json:"repos,omitempty"`
	RepoRoots    []string         `json:"repo_roots,omitempty"`
	Providers    []ProviderConfig `json:"providers,omitempty"`
	Prices       map[string]Price `json:"prices,omitempty"`
	Budgets      []Budget         `json:"budgets,omitempty"`
	Tariff       *TariffConfig    `json:"tariff,omitempty"`
	Monitor      *MonitorConfig   `json:"monitor,omitempty"`
	Limits       *BundleLimits    `json:"limits,omitempty"`
	Sandbox      *SandboxConfig   `json:"sandbox,omitempty"`
	Policy       *PolicyConfig    `json:"policy,omitempty"`
	Files        *FilesConfig     `json:"files,omitempty"`
	// Models lists the local models of the exporting machine, each with the
	// reference that downloads it again (see BundleModel). Importing adds the
	// registry entries (Config) and leaves the files to the person: the UI
	// offers to download each missing one or to drop it from the registry.
	Models []BundleModel `json:"models,omitempty"`

	// SecretNames lists the vault secrets the exported settings refer to
	// (with ExportOptions.SecretNames), so the new machine knows which ones
	// to add. Never their values.
	SecretNames []string `json:"secret_names,omitempty"`
	// Omitted lists paths left out because they looked like credentials.
	Omitted []string `json:"omitted,omitempty"`
}

// BundleModel is one local model in a bundle.
type BundleModel struct {
	ID      string `json:"id"`
	Backend string `json:"backend"`        // llamacpp | lmstudio | ollama
	Name    string `json:"name,omitempty"` // the backend's own name
	Ref     string `json:"ref,omitempty"`  // what downloads it: an Ollama name, an LM Studio name, a Hugging Face .gguf URL; "" = unknown
	SizeMiB int    `json:"size_mib,omitempty"`
	// Config is the models[] entry (registry entry) for this model, when the
	// exporting config has one. Machine-specific paths are kept; Import
	// rewrites a llama.cpp path that does not exist here to where its
	// download lands (ImportOptions.Place).
	Config *ModelConfig `json:"config,omitempty"`
}

// BundleLimits are the resource limits.
type BundleLimits struct {
	Guard    GuardConfig    `json:"guard"`
	Sessions SessionsConfig `json:"sessions"`
	Spawn    SpawnConfig    `json:"spawn"`
}

// ExportOptions choose what Export writes.
type ExportOptions struct {
	Sections    []string // empty = all sections
	SecretNames bool     // keep secret names (key_secret, secret_env); values are never exported
	// Inventory is the machine's local models as the engines list them (the
	// daemon passes it; the CLI has none). Models entries in the config are
	// always exported, merged into it by ID.
	Inventory []BundleModel
}

// credentialName matches env vars and headers that usually carry a secret.
var credentialName = regexp.MustCompile(`(?i)(key|token|secret|passw|auth|cookie|credential|session)`)

// Export writes the chosen sections as an indented JSON bundle.
func (c *Config) Export(opts ExportOptions) ([]byte, error) {
	n := c.Clone()
	want := map[string]bool{}
	for _, s := range opts.Sections {
		if !slices.Contains(BundleSections(), s) {
			return nil, fmt.Errorf("unknown bundle section %q (have %s)", s, strings.Join(BundleSections(), ", "))
		}
		want[s] = true
	}
	all := len(want) == 0
	has := func(s string) bool { return all || want[s] }
	b := Bundle{Version: BundleVersion, Kind: BundleKind, Created: time.Now().UTC().Truncate(time.Second)}
	names := map[string]bool{}
	secretMap := func(path string, m map[string]string) map[string]string {
		if len(m) == 0 {
			return nil
		}
		if !opts.SecretNames {
			b.Omitted = append(b.Omitted, path)
			return nil
		}
		for _, v := range m {
			names[v] = true
		}
		return m
	}
	scrub := func(path string, m map[string]string) map[string]string {
		if len(m) == 0 {
			return m
		}
		out := map[string]string{}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if credentialName.MatchString(k) {
				b.Omitted = append(b.Omitted, keyPath(path, k))
				continue
			}
			out[k] = m[k]
		}
		return out
	}
	if has(SectionAgents) {
		b.Agents = n.Agents
		for i := range b.Agents {
			a := &b.Agents[i]
			p := fmt.Sprintf("agents[%d]", i)
			a.Env = scrub(p+".env", a.Env)
			a.SecretEnv = secretMap(p+".secret_env", a.SecretEnv)
		}
		b.DefaultAgent = n.DefaultAgent
	}
	if has(SectionRepos) {
		b.Repos = n.Repos
		for i := range b.Repos {
			b.Repos[i].SecretEnv = secretMap(fmt.Sprintf("repos[%d].secret_env", i), b.Repos[i].SecretEnv)
		}
		b.RepoRoots = n.RepoRoots
	}
	if has(SectionProviders) {
		b.Providers = n.Providers
		for i := range b.Providers {
			p := &b.Providers[i]
			p.Headers = scrub(fmt.Sprintf("providers[%d].headers", i), p.Headers)
			if p.KeySecret != "" {
				if opts.SecretNames {
					names[p.KeySecret] = true
				} else {
					b.Omitted = append(b.Omitted, fmt.Sprintf("providers[%d].key_secret", i))
					p.KeySecret = ""
				}
			} else if opts.SecretNames && p.KeyEnv == "" && p.Name != "" {
				names[p.Name] = true // the default secret name
			}
		}
	}
	if has(SectionPrices) {
		b.Prices = n.Prices
	}
	if has(SectionBudgets) {
		b.Budgets = n.Budgets
	}
	if has(SectionTariff) {
		b.Tariff = &n.Tariff
	}
	if has(SectionMonitor) {
		b.Monitor = &n.Monitor
	}
	if has(SectionLimits) {
		b.Limits = &BundleLimits{Guard: n.Guard, Sessions: n.Sessions, Spawn: n.Agent.Spawn}
	}
	if has(SectionSandbox) {
		b.Sandbox = &n.Sandbox
	}
	if has(SectionPolicy) {
		b.Policy = &n.Agent.Policy
	}
	if has(SectionFiles) {
		b.Files = &n.Files
	}
	if has(SectionModels) {
		b.Models = bundleModels(opts.Inventory, n.Models)
		for i := range b.Models {
			if mc := b.Models[i].Config; mc != nil {
				before := len(mc.Args)
				mc.Args = slices.DeleteFunc(mc.Args, func(a string) bool { return credentialName.MatchString(a) && strings.Contains(a, "=") })
				if len(mc.Args) != before {
					b.Omitted = append(b.Omitted, fmt.Sprintf("models[%d].config.args", i))
				}
			}
		}
	}
	if opts.SecretNames {
		b.SecretNames = slices.Sorted(maps.Keys(names))
	}
	return json.MarshalIndent(b, "", "  ")
}

// bundleModels merges the engines' inventory with the config's models[]
// entries by ID: inventory order first, then configured models the engines
// did not list (a missing file, an engine that is off).
func bundleModels(inv []BundleModel, cfg []ModelConfig) []BundleModel {
	out := make([]BundleModel, 0, len(inv)+len(cfg))
	seen := map[string]int{}
	for _, m := range inv {
		if m.ID == "" {
			continue
		}
		if _, dup := seen[m.ID]; dup {
			continue
		}
		m.Config = nil
		seen[m.ID] = len(out)
		out = append(out, m)
	}
	for _, mc := range cfg {
		mc := mc
		mc.Args = slices.Clone(mc.Args)
		if i, ok := seen[mc.ID]; ok {
			out[i].Config = &mc
			continue
		}
		backend := mc.Backend
		if backend == "" {
			backend = "llamacpp"
		}
		seen[mc.ID] = len(out)
		out = append(out, BundleModel{ID: mc.ID, Backend: backend, Name: mc.Name, SizeMiB: mc.SizeMiB, Config: &mc})
	}
	return out
}

// ParseBundle decodes and checks a bundle.
func ParseBundle(data []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("not a profile bundle: %w", err)
	}
	if b.Kind != "" && b.Kind != BundleKind {
		return nil, fmt.Errorf("not a profile bundle (kind %q)", b.Kind)
	}
	if b.Version < 1 {
		return nil, errors.New("profile bundle has no version")
	}
	if b.Version > BundleVersion {
		return nil, fmt.Errorf("profile bundle version %d is newer than this Cerberus understands (%d); update Cerberus", b.Version, BundleVersion)
	}
	return &b, nil
}

// Import reads a bundle and returns what it would change and a function that
// makes the change (in memory; call Save after). Nothing changes until apply
// is called. Lists merge by name (agents, repos, providers; budgets by
// provider and period), the bundle winning; prices merge by model; the other
// sections replace. Settings the bundle left out (secret names, credential
// env entries and headers) keep their current values. An import that would
// bring new validation errors is refused.
func (c *Config) Import(data []byte) ([]PathChange, func() error, error) {
	return c.ImportWith(data, ImportOptions{})
}

// ImportOptions adjust an import.
type ImportOptions struct {
	// Place returns where a model's download would land on this machine
	// (a llama.cpp .gguf path), or "" when unknown. A llama.cpp registry
	// entry whose path does not exist here is pointed there, so the entry is
	// right once the file is downloaded.
	Place func(BundleModel) string
	// Exists reports whether a path exists (os.Stat by default; tests).
	Exists func(string) bool
}

// ImportWith is Import with options.
func (c *Config) ImportWith(data []byte, opts ImportOptions) ([]PathChange, func() error, error) {
	b, err := ParseBundle(data)
	if err != nil {
		return nil, nil, err
	}
	if opts.Exists == nil {
		opts.Exists = pathExists
	}
	n := c.Clone()
	n.applyBundle(b)
	n.Models = mergeBy(n.Models, importModels(b.Models, opts), func(m ModelConfig) string { return m.ID }, func(_, nw ModelConfig) ModelConfig { return nw })
	before := map[string]bool{}
	for _, p := range c.Validate() {
		before[p.Path+"\x00"+p.Message] = true
	}
	var bad []string
	for _, p := range n.Validate() {
		if p.Level == LevelError && !before[p.Path+"\x00"+p.Message] {
			bad = append(bad, p.Path+": "+p.Message)
		}
	}
	if len(bad) > 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(bad, "; "))
	}
	diff := Diff(c, n)
	apply := func() error {
		c.adopt(n)
		return nil
	}
	return diff, apply, nil
}

func (c *Config) applyBundle(b *Bundle) {
	if b.Agents != nil {
		c.Agents = mergeBy(c.Agents, b.Agents, func(a AgentProfile) string { return a.Name }, func(old, nw AgentProfile) AgentProfile {
			nw.Env = mergeMap(old.Env, nw.Env)
			if nw.SecretEnv == nil {
				nw.SecretEnv = old.SecretEnv
			}
			return nw
		})
	}
	if b.DefaultAgent != "" {
		c.DefaultAgent = b.DefaultAgent
	}
	if b.Repos != nil {
		c.Repos = mergeBy(c.Repos, b.Repos, func(r RepoConfig) string { return r.Name }, func(old, nw RepoConfig) RepoConfig {
			if nw.SecretEnv == nil {
				nw.SecretEnv = old.SecretEnv
			}
			return nw
		})
	}
	if b.RepoRoots != nil {
		c.RepoRoots = b.RepoRoots
	}
	if b.Providers != nil {
		c.Providers = mergeBy(c.Providers, b.Providers, func(p ProviderConfig) string { return p.Name }, func(old, nw ProviderConfig) ProviderConfig {
			if nw.KeySecret == "" {
				nw.KeySecret = old.KeySecret
			}
			nw.Headers = mergeMap(old.Headers, nw.Headers)
			return nw
		})
	}
	if b.Prices != nil {
		if c.Prices == nil {
			c.Prices = map[string]Price{}
		}
		maps.Copy(c.Prices, b.Prices)
	}
	if b.Budgets != nil {
		c.Budgets = mergeBy(c.Budgets, b.Budgets, func(x Budget) string { return x.Provider + "\x00" + orDefault(x.Period, "month") },
			func(_, nw Budget) Budget { return nw })
	}
	if b.Tariff != nil {
		c.Tariff = *b.Tariff
	}
	if b.Monitor != nil {
		c.Monitor = *b.Monitor
	}
	if b.Limits != nil {
		c.Guard, c.Sessions, c.Agent.Spawn = b.Limits.Guard, b.Limits.Sessions, b.Limits.Spawn
	}
	if b.Sandbox != nil {
		c.Sandbox = *b.Sandbox
	}
	if b.Policy != nil {
		c.Agent.Policy = *b.Policy
	}
	if b.Files != nil {
		c.Files = *b.Files
	}
}

// importModels returns the registry entries a bundle brings, with a
// llama.cpp path that does not exist here moved to where the download lands.
func importModels(ms []BundleModel, opts ImportOptions) []ModelConfig {
	var out []ModelConfig
	for _, m := range ms {
		if m.Config == nil || m.Config.ID == "" {
			continue
		}
		mc := *m.Config
		mc.Args = slices.Clone(mc.Args)
		if (mc.Backend == "" || mc.Backend == "llamacpp") && mc.Path != "" && !opts.Exists(mc.Path) && opts.Place != nil {
			if p := opts.Place(m); p != "" {
				mc.Path = p
			}
		}
		out = append(out, mc)
	}
	return out
}

// mergeBy replaces items of cur that share a key with one in in (through
// merge) and appends the rest of in, keeping cur's order.
func mergeBy[T any](cur, in []T, key func(T) string, merge func(old, nw T) T) []T {
	out := slices.Clone(cur)
	for _, x := range in {
		k := key(x)
		i := slices.IndexFunc(out, func(y T) bool { return key(y) == k })
		if i >= 0 {
			out[i] = merge(out[i], x)
		} else {
			out = append(out, x)
		}
	}
	return out
}

// mergeMap returns old with nw's entries over it (nil when both are empty).
func mergeMap(old, nw map[string]string) map[string]string {
	if len(old) == 0 {
		return nw
	}
	out := maps.Clone(old)
	maps.Copy(out, nw)
	return out
}

// pathExists reports whether p exists (after ~ expansion).
func pathExists(p string) bool {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + strings.TrimPrefix(p, "~")
		}
	}
	_, err := os.Stat(p)
	return err == nil
}
