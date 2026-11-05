package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Problem levels.
const (
	LevelError = "error" // the setting cannot work as written
	LevelWarn  = "warn"  // it works, but probably not as meant
)

// Problem is one validation finding, at a concrete path ("agents[2].command").
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Level   string `json:"level"`
}

// ValidateOptions add checks that need things outside the config.
type ValidateOptions struct {
	// SecretExists reports whether the vault holds a secret by that name.
	// When nil, secret references are not checked.
	SecretExists func(name string) bool
}

// Validate checks the whole config. See ValidateWith.
func (c *Config) Validate() []Problem { return c.ValidateWith(ValidateOptions{}) }

// HasErrors reports whether any problem is an error.
func HasErrors(probs []Problem) bool {
	for _, p := range probs {
		if p.Level == LevelError {
			return true
		}
	}
	return false
}

// ValidateWith checks every field (kinds, enums, ranges, addresses, URLs,
// paths) and the rules across fields (references, duplicates, tariff order).
// Problems are sorted by path.
func (c *Config) ValidateWith(opts ValidateOptions) []Problem {
	tokenMu.RLock()
	ck := &checker{opts: opts, def: Defaults()}
	ck.tree(reflect.ValueOf(c).Elem(), "", "")
	tokenMu.RUnlock()
	ck.cross(c)
	slices.SortStableFunc(ck.probs, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	return ck.probs
}

// checkTree returns the field-level problems of the value at gen/concrete.
func checkTree(v reflect.Value, gen, concrete string, opts ValidateOptions) []Problem {
	ck := &checker{opts: opts, def: Defaults()}
	ck.tree(v, gen, concrete)
	return ck.probs
}

type checker struct {
	opts  ValidateOptions
	def   *Config
	probs []Problem
}

func (ck *checker) add(level, path, format string, args ...any) {
	ck.probs = append(ck.probs, Problem{Path: path, Message: fmt.Sprintf(format, args...), Level: level})
}

func (ck *checker) tree(v reflect.Value, gen, concrete string) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	m, _ := lookupMeta(gen)
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType {
			return
		}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			name := jsonName(t.Field(i))
			if name == "" {
				continue
			}
			ck.tree(v.Field(i), joinPath(gen, name), joinPath(concrete, name))
		}
	case reflect.Slice:
		if m.required && v.Len() == 0 {
			ck.add(LevelError, concrete, "%s must not be empty", labelOr(m, concrete))
		}
		for i := 0; i < v.Len(); i++ {
			ck.tree(v.Index(i), gen+"[]", concrete+"["+strconv.Itoa(i)+"]")
		}
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, k := range keys {
			if k.String() == "" {
				ck.add(LevelError, concrete, "%s has an empty key", labelOr(m, concrete))
				continue
			}
			ck.tree(v.MapIndex(k), gen+".*", keyPath(concrete, k.String()))
		}
	case reflect.String:
		ck.str(m, gen, concrete, v.String())
	case reflect.Float32, reflect.Float64:
		ck.num(m, concrete, v.Float(), strconv.FormatFloat(v.Float(), 'g', -1, 64))
	default:
		if isIntKind(v.Kind()) {
			var f float64
			if v.CanInt() {
				f = float64(v.Int())
			} else {
				f = float64(v.Uint())
			}
			ck.num(m, concrete, f, strconv.FormatFloat(f, 'f', -1, 64))
		}
	}
}

func labelOr(m meta, path string) string {
	if m.label != "" {
		return m.label
	}
	return path
}

// keyPath appends a map key to a concrete path, quoting it when needed.
func keyPath(base, key string) string {
	if key != "" && !strings.ContainsAny(key, `.[]"`) {
		return base + "." + key
	}
	return base + "[" + strconv.Quote(key) + "]"
}

func (ck *checker) num(m meta, path string, f float64, shown string) {
	if m.min != nil && f < *m.min {
		ck.add(LevelError, path, "%s must be at least %s (it is %s)", labelOr(m, path), fmtNum(*m.min), shown)
	}
	if m.max != nil && f > *m.max {
		ck.add(LevelError, path, "%s must be at most %s (it is %s)", labelOr(m, path), fmtNum(*m.max), shown)
	}
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

var sizeRe = regexp.MustCompile(`^(?i)(\d+(\.\d+)?[KMGT]?|\d+%|infinity)$`)

func (ck *checker) str(m meta, gen, path, s string) {
	t := strings.TrimSpace(s)
	label := labelOr(m, path)
	if t == "" {
		if m.required {
			ck.add(LevelError, path, "%s is required", label)
		}
		if len(m.enum) == 0 || slices.Contains(m.enum, "") {
			return
		}
	}
	if len(m.enum) > 0 {
		low := strings.ToLower(t)
		if !slices.Contains(m.enum, low) && !slices.Contains(m.aliases, low) {
			ck.add(LevelError, path, "%s must be one of %s (it is %q)", label, enumList(m.enum), s)
		}
		return
	}
	switch m.format {
	case FormatAddress:
		if err := checkAddress(t); err != nil {
			ck.add(LevelError, path, "%s %q: %v", label, s, err)
		}
	case FormatURL:
		if err := checkURL(t); err != nil {
			ck.add(LevelError, path, "%s %q: %v", label, s, err)
		}
	case FormatSize:
		if !sizeRe.MatchString(t) {
			ck.add(LevelError, path, "%s %q is not a size such as 6G, 512M, 50%% or infinity", label, s)
		}
	case FormatEmail:
		if !strings.Contains(t, "@") {
			ck.add(LevelWarn, path, "%s %q does not look like an email address", label, s)
		}
	}
	if m.kind == KindPath && m.mustExist && !ck.isDefault(gen, s) && !ck.isDefault(path, s) {
		if _, err := os.Stat(expandHome(t)); err != nil {
			ck.add(LevelWarn, path, "%s %q does not exist", label, s)
		}
	}
	if m.kind == KindSecretRef && ck.opts.SecretExists != nil && !ck.opts.SecretExists(t) {
		ck.add(LevelWarn, path, "secret %q is not in the vault", t)
	}
}

func enumList(e []string) string {
	var out []string
	for _, v := range e {
		if v == "" {
			out = append(out, `"" (default)`)
		} else {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
}

// isDefault reports whether s is the default value at gen (or one item of
// the default list), so default paths that are missing on this machine
// (~/models on a box without models) do not warn. Called with a concrete
// path (subscriptions[0].data_dir) it compares the default list item at
// the same index, so a built-in entry's folder stays quiet too.
func (ck *checker) isDefault(gen, s string) bool {
	if strings.Contains(strings.TrimSuffix(gen, "[]"), "[]") || strings.Contains(gen, "*") {
		return false
	}
	p := strings.TrimSuffix(gen, "[]")
	toks, err := parsePath(p)
	if err != nil {
		return false
	}
	found := false
	_ = walk(reflect.ValueOf(ck.def).Elem(), p, toks, false, func(v reflect.Value, _ string) error {
		switch v.Kind() {
		case reflect.String:
			found = v.String() == s
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				if v.Index(i).Kind() == reflect.String && v.Index(i).String() == s {
					found = true
				}
			}
		}
		return nil
	})
	return found
}

// ExpandHome turns a leading "~" into the home folder.
func ExpandHome(p string) string { return expandHome(p) }

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// privateIP is doctor.Private: loopback, RFC 1918, CGNAT (100.64/10), ULA
// (fc00::/7) or link-local, an address the public internet cannot reach.
func privateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1]&0xC0 == 64
	}
	return false
}

func checkAddress(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("not host:port (%v)", err)
	}
	if strings.ContainsAny(host, " /") {
		return fmt.Errorf("bad host %q", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not between 1 and 65535", port)
	}
	return nil
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a URL (%v)", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must start with http:// or https://")
	}
	if u.Host == "" {
		return fmt.Errorf("has no host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port %q is not between 1 and 65535", p)
		}
	}
	return nil
}

// cross holds the rules that look at more than one field.
func (ck *checker) cross(c *Config) {
	// LM Studio's --gpu takes max, off or a fraction from 0 to 1.
	if g := strings.TrimSpace(c.Engines.LMStudio.GPU); g != "" && g != "max" && g != "off" {
		if f, err := strconv.ParseFloat(g, 64); err != nil || !(f >= 0 && f <= 1) { // !(...) also refuses NaN
			ck.add(LevelError, "engines.lmstudio.gpu", "%q is not \"max\", \"off\" or a fraction from 0 to 1", g)
		}
	}

	// Work previews: a port range clear of the listen ports.
	if lo, hi, ok, err := ParsePortRange(c.Server.PreviewPorts); err != nil {
		ck.add(LevelError, "server.preview_ports", "%v", err)
	} else if ok {
		for _, a := range append([]string{c.Server.Listen}, c.Server.ExtraListen...) {
			if _, ps, err := net.SplitHostPort(strings.TrimSpace(a)); err == nil {
				if p, err := strconv.Atoi(ps); err == nil && p >= lo && p <= hi {
					ck.add(LevelError, "server.preview_ports", "the range %d-%d holds the listen port %d", lo, hi, p)
				}
			}
		}
	}

	// TLS needs both halves.
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		p := "server.tls_key"
		if c.Server.TLSCert == "" {
			p = "server.tls_cert"
		}
		ck.add(LevelError, p, "HTTPS needs both a certificate and a key")
	}

	// Bind safety, the rule serve applies at start (doctor.CheckListen): a
	// public IP needs server.allow_public, or the daemon refuses to start
	// after a restart; a wildcard bind is a warning. Host names are judged
	// when the daemon binds (it resolves them then).
	for i, a := range append([]string{c.Server.Listen}, c.Server.ExtraListen...) {
		path := "server.listen"
		if i > 0 {
			path = fmt.Sprintf("server.extra_listen[%d]", i-1)
		}
		host, _, err := net.SplitHostPort(strings.TrimSpace(a))
		if err != nil {
			continue // the field check reports it
		}
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			ck.add(LevelWarn, path, "%q listens on every interface: anyone who can reach this machine can try the token; prefer 127.0.0.1 plus a LAN or tailnet address", a)
		} else if ip != nil && !privateIP(ip) {
			if c.Server.AllowPublic {
				ck.add(LevelWarn, path, "%q is a public address (server.allow_public is on)", a)
			} else {
				ck.add(LevelError, path, "%q is a public address: Cerberus refuses to start on it; use 127.0.0.1, a LAN (192.168.x) or tailnet (100.x) address, or set server.allow_public only behind your own firewall or proxy", a)
			}
		}
	}

	// Duplicate names.
	dup := func(list string, n int, name func(i int) string, field string) {
		seen := map[string]int{}
		for i := 0; i < n; i++ {
			k := name(i)
			if k == "" {
				continue
			}
			if j, ok := seen[k]; ok {
				ck.add(LevelError, fmt.Sprintf("%s[%d].%s", list, i, field), "%q is already used by %s[%d]", k, list, j)
				continue
			}
			seen[k] = i
		}
	}
	dup("agents", len(c.Agents), func(i int) string { return c.Agents[i].Name }, "name")
	dup("repos", len(c.Repos), func(i int) string { return c.Repos[i].Name }, "name")
	dup("providers", len(c.Providers), func(i int) string { return c.Providers[i].Name }, "name")
	dup("models", len(c.Models), func(i int) string { return c.Models[i].ID }, "id")
	dup("subscriptions", len(c.Subscriptions), func(i int) string { return c.Subscriptions[i].Name }, "name")
	dup("tariff.tiers", len(c.Tariff.Tiers), func(i int) string { return c.Tariff.Tiers[i].Name }, "name")
	dup("budgets", len(c.Budgets), func(i int) string {
		b := c.Budgets[i]
		if b.Provider == "" {
			return ""
		}
		return b.Provider + " per " + orDefault(b.Period, "month")
	}, "provider")

	// References to agents.
	agents := map[string]bool{}
	for _, a := range c.AgentProfiles() {
		agents[a.Name] = true
	}
	switch {
	case strings.TrimSpace(c.DefaultAgent) == "":
		ck.add(LevelError, "default_agent", "Default agent is required")
	case !agents[c.DefaultAgent]:
		ck.add(LevelError, "default_agent", "no agent profile is named %q", c.DefaultAgent)
	}
	for i, s := range c.Subscriptions {
		for j, a := range s.Agents {
			if !agents[a] {
				ck.add(LevelWarn, fmt.Sprintf("subscriptions[%d].agents[%d]", i, j), "no agent profile is named %q", a)
			}
		}
	}

	// References to providers.
	providers := map[string]bool{}
	for _, p := range c.Providers {
		providers[p.Name] = true
	}
	for i, b := range c.Budgets {
		if b.Provider != "" && b.Provider != "*" && !providers[b.Provider] {
			ck.add(LevelError, fmt.Sprintf("budgets[%d].provider", i), "no provider is named %q (use * for all)", b.Provider)
		}
		if b.HardUSD > 0 && b.SoftUSD > b.HardUSD {
			ck.add(LevelWarn, fmt.Sprintf("budgets[%d].soft_usd", i), "the soft limit (%s) is above the hard limit (%s)", fmtNum(b.SoftUSD), fmtNum(b.HardUSD))
		}
	}
	for k := range c.Prices {
		if prov, ok := strings.CutSuffix(k, "/*"); ok && !providers[prov] {
			ck.add(LevelWarn, keyPath("prices", k), "no provider is named %q", prov)
		}
	}
	if ck.opts.SecretExists != nil {
		for i, p := range c.Providers {
			if !p.Enabled || p.KeyEnv != "" || p.KeySecret != "" || p.Name == "" {
				continue // a named key_secret is checked as a secret-ref field
			}
			if !ck.opts.SecretExists(p.Name) {
				ck.add(LevelWarn, fmt.Sprintf("providers[%d].key_secret", i), "no key: secret %q is not in the vault and key_env is empty", p.Name)
			}
		}
	}

	// Tariff tiers ascend; only the last may be open-ended.
	tiers := c.Tariff.Tiers
	prev := 0.0
	for i, t := range tiers {
		p := fmt.Sprintf("tariff.tiers[%d].up_to_kwh", i)
		last := i == len(tiers)-1
		switch {
		case t.UpToKWh == 0 && !last:
			ck.add(LevelError, p, "only the last tier can be open-ended (0)")
		case t.UpToKWh != 0 && t.UpToKWh <= prev:
			ck.add(LevelError, p, "tiers must ascend: %s kWh is not above the previous tier's %s kWh", fmtNum(t.UpToKWh), fmtNum(prev))
		case t.UpToKWh != 0 && last:
			ck.add(LevelWarn, p, "the last tier ends at %s kWh, so use above it has no price; set it to 0 for the rest", fmtNum(t.UpToKWh))
		}
		if t.UpToKWh > prev {
			prev = t.UpToKWh
		}
	}

	// Power model.
	if c.Monitor.CPUMaxW > 0 && c.Monitor.CPUMaxW < c.Monitor.CPUIdleW {
		ck.add(LevelWarn, "monitor.cpu_max_w", "CPU full-load watts (%s) are below idle watts (%s)", fmtNum(c.Monitor.CPUMaxW), fmtNum(c.Monitor.CPUIdleW))
	}

	// Upload folder stays inside the workspace.
	if d := strings.TrimSpace(c.Files.UploadDir); d != "" {
		clean := filepath.ToSlash(filepath.Clean(d))
		if filepath.IsAbs(d) || strings.HasPrefix(d, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
			ck.add(LevelError, "files.upload_dir", "upload folder %q must be a relative path inside the workspace", d)
		}
	}

	// Quick answers go through the gateway: a claude CLI model cannot answer them.
	if q := strings.TrimSpace(c.Chat.QuickModel); strings.HasPrefix(strings.ToLower(q), "claude:") {
		ck.add(LevelError, "chat.quick_model", "quick answers go through the gateway; %q is a claude CLI model", q)
	}

	// Voice backends that need a setting.
	switch strings.ToLower(c.Voice.Backend) {
	case "openai":
		if c.Voice.URL == "" {
			ck.add(LevelError, "voice.url", "the openai voice backend needs a URL")
		}
	case "command":
		if len(c.Voice.Command) == 0 {
			ck.add(LevelError, "voice.command", "the command voice backend needs a command")
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
