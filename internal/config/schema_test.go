package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// schemaPaths walks Config by reflection: required are the paths that must
// have metadata (struct leaves, lists, maps); optional are scalar list and
// map items, which may.
func schemaPaths(t reflect.Type, prefix string, required, optional map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		name := jsonName(t.Field(i))
		if name == "" {
			continue
		}
		p := joinPath(prefix, name)
		ft := t.Field(i).Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != timeType {
			schemaPaths(ft, p, required, optional)
			continue
		}
		required[p] = true
		itemPaths(ft, p, required, optional)
	}
}

func itemPaths(ft reflect.Type, p string, required, optional map[string]bool) {
	var item string
	switch ft.Kind() {
	case reflect.Slice:
		item = p + "[]"
	case reflect.Map:
		item = p + ".*"
	default:
		return
	}
	et := ft.Elem()
	for et.Kind() == reflect.Pointer {
		et = et.Elem()
	}
	if et.Kind() == reflect.Struct && et != timeType {
		schemaPaths(et, item, required, optional)
		return
	}
	optional[item] = true
	itemPaths(et, item, required, optional)
}

// TestSchemaMetadataCoverage: every Config field has words, a group and a
// level; no metadata entry names a field that does not exist.
func TestSchemaMetadataCoverage(t *testing.T) {
	required, optional := map[string]bool{}, map[string]bool{}
	schemaPaths(reflect.TypeOf(Config{}), "", required, optional)
	if len(required) < 150 {
		t.Fatalf("walked only %d paths", len(required))
	}
	groups := Groups()
	for p := range required {
		m, ok := metaTable[p]
		if !ok {
			t.Errorf("config field %q has no schema metadata (add it to schema_meta.go)", p)
			continue
		}
		if m.label == "" || m.help == "" {
			t.Errorf("%q: missing label or help", p)
		}
		if !slices.Contains(groups, m.group) {
			t.Errorf("%q: unknown group %q", p, m.group)
		}
		if m.level != LevelBasic && m.level != LevelAdvanced {
			t.Errorf("%q: bad level %q", p, m.level)
		}
		if strings.Count(m.help, ". ") > 2 {
			t.Errorf("%q: help is longer than two sentences: %q", p, m.help)
		}
	}
	for p := range metaTable {
		if !required[p] && !optional[p] {
			t.Errorf("metadata %q names no config field", p)
		}
	}
}

// TestSchemaShape checks kinds, items, defaults and the small basic level.
func TestSchemaShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fs := Schema()
	by := map[string]Field{}
	basic := 0
	for _, f := range fs {
		by[f.Path] = f
		if f.Level == LevelBasic {
			basic++
		}
	}
	if basic > 20 {
		t.Errorf("%d basic settings; keep basic small", basic)
	}
	if f := by["server.listen"]; f.Default != "127.0.0.1:7340" || f.Level != LevelBasic || !f.Restart || f.Format != FormatAddress {
		t.Errorf("server.listen = %+v", f)
	}
	if f := by["server.token"]; !f.Sensitive || f.Default != nil {
		t.Errorf("server.token = %+v", f)
	}
	if f := by["engines.idle_ttl_sec"]; f.Kind != KindDuration || f.Default != 600 || f.Restart {
		t.Errorf("engines.idle_ttl_sec = %+v", f)
	}
	if f := by["agent.approve"]; f.Kind != KindEnum || !slices.Equal(f.Enum, []string{"ask", "auto"}) {
		t.Errorf("agent.approve = %+v", f)
	}
	if f := by["server.token_created"]; f.Kind != KindTime || !f.ReadOnly {
		t.Errorf("server.token_created = %+v", f)
	}
	a := by["agents"]
	if a.Kind != KindList || len(a.Items) == 0 {
		t.Fatalf("agents = %+v", a)
	}
	var cmd, sec Field
	for _, it := range a.Items {
		switch it.Path {
		case "agents[].command":
			cmd = it
		case "agents[].secret_env":
			sec = it
		}
	}
	if cmd.Kind != KindList || !cmd.Required || len(cmd.Items) != 1 || cmd.Items[0].Kind != KindString {
		t.Errorf("agents[].command = %+v", cmd)
	}
	if sec.Kind != KindMap || len(sec.Items) != 1 || sec.Items[0].Kind != KindSecretRef {
		t.Errorf("agents[].secret_env = %+v", sec)
	}
	if p := by["prices"]; p.Kind != KindMap || len(p.Items) != 3 || p.Items[0].Path != "prices.*.in" {
		t.Errorf("prices = %+v", p)
	}
	if f, ok := SchemaField("agents[3].sandbox"); !ok || f.Kind != KindEnum {
		t.Errorf("SchemaField(agents[3].sandbox) = %+v %v", f, ok)
	}
	if f, ok := SchemaField(`prices["qwen2.5"].out`); !ok || f.Path != "prices.*.out" {
		t.Errorf("SchemaField(prices) = %+v %v", f, ok)
	}
	if _, err := json.Marshal(fs); err != nil {
		t.Fatal(err)
	}
}

func hasProblem(probs []Problem, path, level, substr string) bool {
	for _, p := range probs {
		if p.Path == path && p.Level == level && strings.Contains(p.Message, substr) {
			return true
		}
	}
	return false
}

// TestValidateDefaultsClean: the defaults have no errors.
func TestValidateDefaultsClean(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, p := range Defaults().Validate() {
		if p.Level == LevelError {
			t.Errorf("default config: %+v", p)
		}
	}
}

// A built-in list entry's folder (the default subscription's data_dir)
// stays quiet when missing; a changed one still warns.
func TestValidateDefaultListItemPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Defaults()
	if len(c.Subscriptions) == 0 {
		t.Skip("no default subscriptions")
	}
	for _, p := range c.Validate() {
		if strings.HasPrefix(p.Path, "subscriptions[") {
			t.Errorf("default subscription warns: %+v", p)
		}
	}
	c.Subscriptions[0].DataDir = "/nope/projects"
	found := false
	for _, p := range c.Validate() {
		found = found || (p.Path == "subscriptions[0].data_dir" && p.Level == LevelWarn)
	}
	if !found {
		t.Error("changed missing data_dir did not warn")
	}
}

func TestValidateTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := filepath.Join(home, "repo")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mut    func(c *Config)
		path   string
		level  string
		substr string
	}{
		{"listen not host:port", func(c *Config) { c.Server.Listen = "localhost" }, "server.listen", LevelError, "host:port"},
		{"listen port range", func(c *Config) { c.Server.Listen = "127.0.0.1:70000" }, "server.listen", LevelError, "between 1 and 65535"},
		{"listen empty", func(c *Config) { c.Server.Listen = "" }, "server.listen", LevelError, "required"},
		{"extra listen item", func(c *Config) { c.Server.ExtraListen = []string{"100.1.2.3:7340", "nope"} }, "server.extra_listen[1]", LevelError, "host:port"},
		{"public url scheme", func(c *Config) { c.Server.PublicURL = "ftp://x" }, "server.public_url", LevelError, "http"},
		{"tls half", func(c *Config) { c.Server.TLSCert = existing }, "server.tls_key", LevelError, "both"},
		{"tls missing file", func(c *Config) { c.Server.TLSCert, c.Server.TLSKey = existing, "/nope/key.pem" }, "server.tls_key", LevelWarn, "does not exist"},
		{"negative duration", func(c *Config) { c.Engines.IdleTTLSec = -1 }, "engines.idle_ttl_sec", LevelError, "at least 0"},
		{"port base", func(c *Config) { c.Engines.LlamaCpp.PortBase = 80 }, "engines.llamacpp.port_base", LevelError, "at least 1024"},
		{"enum", func(c *Config) { c.Agent.Approve = "sometimes" }, "agent.approve", LevelError, "one of ask, auto"},
		{"enum alias ok", func(c *Config) { c.Sandbox.Default = "off" }, "sandbox.default", "", ""},
		{"gpu enum", func(c *Config) { c.GPU.Backend = "intel" }, "gpu.backend", LevelError, "one of"},
		{"default agent", func(c *Config) { c.DefaultAgent = "ghost" }, "default_agent", LevelError, "ghost"},
		{"custom default agent ok", func(c *Config) {
			c.Agents = []AgentProfile{{Name: "mine", Command: []string{"x"}}}
			c.DefaultAgent = "mine"
		}, "default_agent", "", ""},
		{"duplicate agent", func(c *Config) {
			c.Agents = []AgentProfile{{Name: "a", Command: []string{"x"}}, {Name: "a", Command: []string{"y"}}}
		}, "agents[1].name", LevelError, "already used"},
		{"agent command required", func(c *Config) { c.Agents = []AgentProfile{{Name: "a"}} }, "agents[0].command", LevelError, "empty"},
		{"duplicate repo", func(c *Config) {
			c.Repos = []RepoConfig{{Name: "r", Path: existing}, {Name: "r", Path: existing}}
		}, "repos[1].name", LevelError, "already used"},
		{"repo path missing", func(c *Config) { c.Repos = []RepoConfig{{Name: "r", Path: "/nope/repo"}} }, "repos[0].path", LevelWarn, "does not exist"},
		{"repo path tilde ok", func(c *Config) { c.Repos = []RepoConfig{{Name: "r", Path: "~/repo"}} }, "repos[0].path", "", ""},
		{"budget provider", func(c *Config) { c.Budgets = []Budget{{Provider: "nobody", Period: "day"}} }, "budgets[0].provider", LevelError, "nobody"},
		{"budget star ok", func(c *Config) { c.Budgets = []Budget{{Provider: "*", Period: "month", SoftUSD: 5}} }, "budgets[0].provider", "", ""},
		{"budget period", func(c *Config) { c.Budgets = []Budget{{Provider: "*", Period: "week"}} }, "budgets[0].period", LevelError, "day, month"},
		{"budget soft over hard", func(c *Config) { c.Budgets = []Budget{{Provider: "*", Period: "day", SoftUSD: 9, HardUSD: 5}} }, "budgets[0].soft_usd", LevelWarn, "above"},
		{"negative price", func(c *Config) { c.Prices = map[string]Price{"m": {In: -1}} }, "prices.m.in", LevelError, "at least 0"},
		{"dotted price key", func(c *Config) { c.Prices = map[string]Price{"q2.5": {Out: -1}} }, `prices["q2.5"].out`, LevelError, "at least 0"},
		{"price provider", func(c *Config) { c.Prices = map[string]Price{"ghost/*": {}} }, "prices.ghost/*", LevelWarn, "ghost"},
		{"tiers ascend", func(c *Config) {
			c.Tariff.Tiers = []Tier{{UpToKWh: 100}, {UpToKWh: 50}, {}}
		}, "tariff.tiers[1].up_to_kwh", LevelError, "ascend"},
		{"open tier not last", func(c *Config) { c.Tariff.Tiers = []Tier{{}, {UpToKWh: 50}} }, "tariff.tiers[0].up_to_kwh", LevelError, "last"},
		{"duplicate provider", func(c *Config) {
			c.Providers = []ProviderConfig{{Name: "p", BaseURL: "https://a/v1"}, {Name: "p", BaseURL: "https://b/v1"}}
		}, "providers[1].name", LevelError, "already used"},
		{"provider url", func(c *Config) { c.Providers = []ProviderConfig{{Name: "p", BaseURL: "api.example.com"}} }, "providers[0].base_url", LevelError, "http"},
		{"duplicate model", func(c *Config) { c.Models = []ModelConfig{{ID: "m"}, {ID: "m"}} }, "models[1].id", LevelError, "already used"},
		{"size format", func(c *Config) { c.Guard.SessionMemoryMax = "lots" }, "guard.session_memory_max", LevelError, "size"},
		{"psu efficiency", func(c *Config) { c.Monitor.PSUEfficiency = 1.5 }, "monitor.psu_efficiency", LevelError, "at most 1"},
		{"upload dir escapes", func(c *Config) { c.Files.UploadDir = "../out" }, "files.upload_dir", LevelError, "relative"},
		{"voice openai needs url", func(c *Config) { c.Voice.Backend = "openai" }, "voice.url", LevelError, "URL"},
		{"missing subscription agent", func(c *Config) { c.Subscriptions[0].Agents = []string{"ghost"} }, "subscriptions[0].agents[0]", LevelWarn, "ghost"},
		{"default model dir missing is quiet", func(c *Config) {}, "engines.llamacpp.model_dirs[0]", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.mut(c)
			probs := c.Validate()
			if tc.level == "" {
				for _, p := range probs {
					if p.Path == tc.path {
						t.Fatalf("unexpected problem %+v", p)
					}
				}
				return
			}
			if !hasProblem(probs, tc.path, tc.level, tc.substr) {
				t.Fatalf("want %s at %s containing %q; got %+v", tc.level, tc.path, tc.substr, probs)
			}
		})
	}
}

func TestValidateSecretLookup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Defaults()
	c.Providers = []ProviderConfig{
		{Name: "named", BaseURL: "https://a/v1", Enabled: true, KeySecret: "missing-key"},
		{Name: "implicit", BaseURL: "https://b/v1", Enabled: true},
		{Name: "env", BaseURL: "https://c/v1", Enabled: true, KeyEnv: "C_KEY"},
	}
	c.Agents = []AgentProfile{{Name: "x", Command: []string{"x"}, SecretEnv: map[string]string{"TOK": "have"}}}
	have := func(n string) bool { return n == "have" }
	probs := c.ValidateWith(ValidateOptions{SecretExists: have})
	if !hasProblem(probs, "providers[0].key_secret", LevelWarn, "missing-key") {
		t.Errorf("named secret not flagged: %+v", probs)
	}
	if !hasProblem(probs, "providers[1].key_secret", LevelWarn, "implicit") {
		t.Errorf("implicit secret not flagged: %+v", probs)
	}
	for _, p := range probs {
		if strings.HasPrefix(p.Path, "providers[2]") || strings.HasPrefix(p.Path, "agents[0].secret_env") {
			t.Errorf("unexpected %+v", p)
		}
	}
	if probs := c.Validate(); hasProblem(probs, "providers[0].key_secret", LevelWarn, "") {
		t.Error("secrets checked without a lookup")
	}
}

func TestGetSetRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Defaults()
	cases := []struct {
		path string
		in   any
		want any
	}{
		{"server.listen", "127.0.0.1:7381", "127.0.0.1:7381"},
		{"server.extra_listen", []any{"100.64.0.1:7340"}, []string{"100.64.0.1:7340"}},
		{"server.extra_listen", `["100.64.0.2:7340"]`, []string{"100.64.0.2:7340"}},
		{"engines.idle_ttl_sec", 900.0, 900},
		{"engines.idle_ttl_sec", "120", 120},
		{"engines.idle_ttl_sec", "10m", 600},
		{"engines.ollama.enabled", "no", false},
		{"engines.ollama.enabled", true, true},
		{"monitor.psu_efficiency", "0.92", 0.92},
		{"agent.approve", "auto", "auto"},
		{"agents[0]", map[string]any{"name": "mine", "command": []string{"mine", "{task}"}}, AgentProfile{Name: "mine", Command: []string{"mine", "{task}"}}},
		{"agents[0].command", []string{"mine", "--yes", "{task}"}, []string{"mine", "--yes", "{task}"}},
		{"agents[1]", map[string]any{"name": "second", "command": []string{"two"}}, AgentProfile{Name: "second", Command: []string{"two"}}}, // index == len appends
		{"agents[1].name", "deux", "deux"},
		{"agents[0].env.FOO", "bar", "bar"},
		{"providers[0]", map[string]any{"name": "or", "base_url": "https://openrouter.ai/api/v1"}, ProviderConfig{Name: "or", BaseURL: "https://openrouter.ai/api/v1"}},
		{"providers[0].headers.X-Title", "Cerberus", "Cerberus"},
		{"prices.gpt-4o.in", 2.5, 2.5},
		{`prices["qwen2.5-coder"].out`, 1, 1.0},
		{"prices.qwen2.5-coder.out", 3, 3.0}, // unquoted dotted key before a field name
		{"prices.or/*", map[string]any{"in": 1, "out": 2}, Price{In: 1, Out: 2}},
		{"budgets[0]", map[string]any{"provider": "*", "period": "month", "soft_usd": 10, "hard_usd": 20}, Budget{Provider: "*", Period: "month", SoftUSD: 10, HardUSD: 20}},
		{"repos[0]", map[string]any{"name": "r", "path": "/tmp"}, RepoConfig{Name: "r", Path: "/tmp"}},
		{"repos[0].preview.dir", "dist", "dist"},
		{"tariff.tiers[0].per_kwh", 1.2, 1.2},
		{"server.init_done", true, true},
	}
	for _, tc := range cases {
		if err := c.Set(tc.path, tc.in); err != nil {
			t.Fatalf("Set(%s, %v): %v", tc.path, tc.in, err)
		}
		got, err := c.Get(tc.path)
		if err != nil {
			t.Fatalf("Get(%s): %v", tc.path, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("Get(%s) = %#v, want %#v", tc.path, got, tc.want)
		}
	}
	if c.Prices["qwen2.5-coder"].Out != 3 || c.Repos[0].Preview == nil || len(c.Agents) != 2 {
		t.Fatalf("not stored: %+v %+v %d", c.Prices, c.Repos[0].Preview, len(c.Agents))
	}
	// Get returns copies.
	v, _ := c.Get("agents[0].command")
	v.([]string)[0] = "mutated"
	if c.Agents[0].Command[0] != "mine" {
		t.Fatal("Get leaked internal slice")
	}
}

func TestSetRefusals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Defaults()
	c.SetToken("secret-token")
	before := c.Clone()
	cases := []struct {
		path string
		val  any
		want error
	}{
		{"server.token", "x", ErrSensitive},
		{"server", map[string]any{"listen": "x"}, ErrSensitive},
		{"server.token_created", "2026-01-01T00:00:00Z", ErrReadOnly},
		{"server.nope", "x", ErrNoPath},
		{"agents[5].name", "x", ErrNoPath},
		{"agents[0].name", "x", ErrInvalid}, // appending an item without a command is invalid
		{"agents.name", "x", ErrNoPath},
		{"engines.idle_ttl_sec", "soon", ErrType},
		{"engines.idle_ttl_sec", 1.5, ErrType},
		{"engines.ollama.enabled", "maybe", ErrType},
		{"server.extra_listen", "a,b", ErrType},
		{"agents[0]", map[string]any{"name": "x", "bogus": 1}, ErrType},
		{"agent.approve", "sometimes", ErrInvalid},
		{"server.listen", "nope", ErrInvalid},
		{"engines.idle_ttl_sec", -5, ErrInvalid},
		{"default_agent", "ghost", ErrInvalid},
		{"tariff.tiers[1].up_to_kwh", 10, ErrInvalid},
	}
	for _, tc := range cases {
		err := c.Set(tc.path, tc.val)
		if !errors.Is(err, tc.want) {
			t.Errorf("Set(%s, %v) = %v, want %v", tc.path, tc.val, err, tc.want)
		}
	}
	if d := Diff(before, c); len(d) != 0 {
		t.Fatalf("refused sets changed the config: %+v", d)
	}
	if _, err := c.Get("server.token"); !errors.Is(err, ErrSensitive) {
		t.Errorf("Get token = %v", err)
	}
	if _, err := c.Get("server"); !errors.Is(err, ErrSensitive) {
		t.Errorf("Get server = %v", err)
	}
	// Atomic: one bad change keeps every change out.
	probs, err := c.SetAll(map[string]any{"engines.idle_ttl_sec": 60, "agent.approve": "nah"})
	if !errors.Is(err, ErrInvalid) || !hasProblem(probs, "agent.approve", LevelError, "one of") || c.Engines.IdleTTLSec != 600 {
		t.Fatalf("SetAll partial: %v %+v ttl=%d", err, probs, c.Engines.IdleTTLSec)
	}
	if _, err := c.SetAll(map[string]any{"engines.idle_ttl_sec": 60, "agent.approve": "auto"}); err != nil || c.Engines.IdleTTLSec != 60 {
		t.Fatalf("SetAll: %v", err)
	}
	if c.TokenSnapshot() != "secret-token" {
		t.Fatal("SetAll touched the token")
	}
	// A pre-existing error elsewhere does not block unrelated sets.
	c.GPU.Backend = "intel"
	if err := c.Set("engines.idle_ttl_sec", 30); err != nil {
		t.Fatalf("unrelated set blocked: %v", err)
	}
}

func TestReset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Defaults()
	if err := c.Set("engines.idle_ttl_sec", 5); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset("engines.idle_ttl_sec"); err != nil || c.Engines.IdleTTLSec != 600 {
		t.Fatalf("reset: %v %d", err, c.Engines.IdleTTLSec)
	}
	if err := c.Set("agent.policy.deny", []string{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset("agent.policy.deny"); err != nil || len(c.Agent.Policy.Deny) == 0 {
		t.Fatalf("reset list: %v", err)
	}
}

func TestCloneAndDiff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := Defaults()
	a.SetPath("/x/config.json")
	a.SetToken("t1")
	b := a.Clone()
	if b.Path() != a.Path() || b.TokenSnapshot() != "t1" {
		t.Fatal("clone lost path or token")
	}
	b.Agent.Policy.Deny[0] = "changed"
	if a.Agent.Policy.Deny[0] == "changed" {
		t.Fatal("clone shares slices")
	}
	b.Engines.IdleTTLSec = 30
	b.Server.Listen = "127.0.0.1:7397"
	b.Prices = map[string]Price{"m": {In: 1}}
	b.Budgets = []Budget{} // empty == nil: no change
	a.Budgets = nil
	b.SetToken("t2")
	got := map[string]PathChange{}
	for _, ch := range Diff(a, b) {
		got[ch.Path] = ch
	}
	if ch := got["engines.idle_ttl_sec"]; ch.Old != 600 || ch.New != 30 || ch.Restart {
		t.Errorf("ttl change %+v", ch)
	}
	if ch := got["server.listen"]; !ch.Restart {
		t.Errorf("listen change %+v", ch)
	}
	if ch, ok := got["server.token"]; !ok || !ch.Sensitive || ch.Old != nil || ch.New != nil {
		t.Errorf("token change %+v", ch)
	}
	if _, ok := got["prices"]; !ok {
		t.Error("prices change missing")
	}
	if _, ok := got["agent.policy.deny"]; !ok {
		t.Error("deny change missing")
	}
	if _, ok := got["budgets"]; ok {
		t.Error("empty vs nil reported")
	}
	if !NeedsRestart(Diff(a, b)) {
		t.Error("listen change should need a restart")
	}
	if r := a.Redacted(); r.Server.Token != "" || a.TokenSnapshot() != "t1" {
		t.Error("Redacted")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := Defaults()
	src.SetToken("admin-token-never-exported")
	src.Agents = []AgentProfile{{Name: "mine", Command: []string{"mine", "{task}"},
		Env:       map[string]string{"MODE": "fast", "MY_API_KEY": "sk-live-123"},
		SecretEnv: map[string]string{"GH_TOKEN": "github-pat"}}}
	src.DefaultAgent = "mine"
	src.Repos = []RepoConfig{{Name: "web", Path: "/srv/web", Checks: []string{"make test"}, SecretEnv: map[string]string{"NPM_TOKEN": "npm"}}}
	src.Providers = []ProviderConfig{{Name: "or", BaseURL: "https://openrouter.ai/api/v1", Enabled: true, KeySecret: "or-key",
		Headers: map[string]string{"X-Title": "Cerberus", "Authorization": "Bearer sk-or-xyz"}}}
	src.Prices = map[string]Price{"or/*": {In: 1, Out: 2}}
	src.Budgets = []Budget{{Provider: "or", Period: "month", SoftUSD: 5, HardUSD: 10}}
	src.Tariff.HouseholdKWhMonth = 210
	src.Guard.MaxSessions = 5
	src.Sandbox.Default = "bwrap"

	data, err := src.Export(ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"admin-token-never-exported", "sk-live-123", "sk-or-xyz", "github-pat", "or-key", "npm\""} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("bundle leaks %q:\n%s", leak, data)
		}
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil || b.Version != BundleVersion || b.Kind != BundleKind {
		t.Fatalf("bundle header: %v %+v", err, b)
	}
	if !slices.Contains(b.Omitted, "agents[0].env.MY_API_KEY") || !slices.Contains(b.Omitted, "providers[0].key_secret") {
		t.Errorf("omitted = %v", b.Omitted)
	}

	// With names: names only, never values.
	named, err := src.Export(ExportOptions{SecretNames: true})
	if err != nil {
		t.Fatal(err)
	}
	var nb Bundle
	_ = json.Unmarshal(named, &nb)
	if !slices.Equal(nb.SecretNames, []string{"github-pat", "npm", "or-key"}) || strings.Contains(string(named), "sk-") {
		t.Errorf("secret names %v", nb.SecretNames)
	}

	// Import into a fresh machine.
	dst := Defaults()
	dst.SetToken("other-token")
	diff, apply, err := dst.Import(data)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, ch := range diff {
		paths[ch.Path] = true
	}
	for _, p := range []string{"agents", "default_agent", "repos", "providers", "prices", "budgets", "tariff.household_kwh_month", "guard.max_sessions", "sandbox.default"} {
		if !paths[p] {
			t.Errorf("diff misses %s: %v", p, paths)
		}
	}
	if paths["server.token"] {
		t.Error("import touches the token")
	}
	if dst.DefaultAgent == "mine" {
		t.Fatal("Import changed the config before apply")
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if dst.TokenSnapshot() != "other-token" || dst.DefaultAgent != "mine" || dst.Guard.MaxSessions != 5 ||
		dst.Tariff.HouseholdKWhMonth != 210 || dst.Sandbox.Default != "bwrap" || dst.Prices["or/*"].Out != 2 {
		t.Fatalf("not applied: %+v", dst)
	}
	if _, ok := dst.Agents[0].Env["MY_API_KEY"]; ok || dst.Agents[0].Env["MODE"] != "fast" {
		t.Errorf("agent env %v", dst.Agents[0].Env)
	}
	if dst.Providers[0].KeySecret != "" || dst.Providers[0].Headers["Authorization"] != "" || dst.Providers[0].Headers["X-Title"] != "Cerberus" {
		t.Errorf("provider %+v", dst.Providers[0])
	}

	// Re-import over the source keeps its secret names and credentials.
	_, apply, err = src.Import(data)
	if err != nil {
		t.Fatal(err)
	}
	_ = apply()
	if src.Providers[0].KeySecret != "or-key" || src.Agents[0].Env["MY_API_KEY"] != "sk-live-123" ||
		src.Agents[0].SecretEnv["GH_TOKEN"] != "github-pat" || src.Providers[0].Headers["Authorization"] == "" {
		t.Errorf("re-import dropped local secrets: %+v %+v", src.Providers[0], src.Agents[0])
	}
	if len(src.Agents) != 1 || len(src.Budgets) != 1 {
		t.Errorf("merge duplicated items: %d agents %d budgets", len(src.Agents), len(src.Budgets))
	}
}

func TestImportRefusals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Defaults()
	for _, data := range []string{
		`not json`,
		`{"kind":"something-else","version":1}`,
		`{"kind":"cerberus-profile"}`,
		`{"kind":"cerberus-profile","version":99}`,
		`{"kind":"cerberus-profile","version":1,"budgets":[{"provider":"ghost","period":"day"}]}`,
	} {
		if _, _, err := c.Import([]byte(data)); err == nil {
			t.Errorf("Import(%s) accepted", data)
		}
	}
	if _, err := c.Export(ExportOptions{Sections: []string{"keys"}}); err == nil {
		t.Error("unknown section accepted")
	}
	data, err := c.Export(ExportOptions{Sections: []string{SectionTariff}})
	if err != nil || strings.Contains(string(data), `"agents"`) || !strings.Contains(string(data), `"tariff"`) {
		t.Fatalf("sections: %v %s", err, data)
	}
}

func TestInitDoneMigration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	// Fresh config: not set up yet.
	fresh, err := Load(filepath.Join(dir, "fresh.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Server.InitDone {
		t.Fatal("fresh config starts with init_done")
	}
	// Loading the file written by the first run keeps it false (the key is there).
	again, err := Load(filepath.Join(dir, "fresh.json"))
	if err != nil || again.Server.InitDone {
		t.Fatalf("reloaded fresh config: %v %v", err, again.Server.InitDone)
	}

	// An existing config from before day phase 4: already set up.
	old := filepath.Join(dir, "old.json")
	if err := os.WriteFile(old, []byte(`{"server":{"listen":"127.0.0.1:7381","token":"t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(old)
	if err != nil || !c.Server.InitDone {
		t.Fatalf("old config: %v init_done=%v", err, c.Server.InitDone)
	}
	noServer := filepath.Join(dir, "noserver.json")
	_ = os.WriteFile(noServer, []byte(`{"default_agent":"cerberus"}`), 0o600)
	if c, err := Load(noServer); err != nil || !c.Server.InitDone {
		t.Fatalf("config without server: %v", err)
	}

	// An explicit false stays false.
	explicit := filepath.Join(dir, "explicit.json")
	_ = os.WriteFile(explicit, []byte(`{"server":{"token":"t","init_done":false}}`), 0o600)
	if c, err := Load(explicit); err != nil || c.Server.InitDone {
		t.Fatalf("explicit false: %v", err)
	}
}
