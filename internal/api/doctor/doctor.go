// Package doctor is Cerberus' safety and health checklist, shared by
// `cerberus doctor [--json]` and GET /api/doctor. Each check answers ok,
// warn or fail with a one-line explanation and, unless ok, a fix.
//
// Every outside fact (PATH lookups, command output, disk space, network
// reachability, the bwrap probe, file metadata, environment variables, the
// clock) comes through Env, so tests run each check against fixture states
// without touching the real machine. Defaults fills the real
// implementations.
//
// The package also holds the bind-safety rules the daemon applies at start
// (CheckListen): a listen address on a public IP is refused unless
// Server.AllowPublic, and a wildcard bind is allowed with a loud warning.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/sandbox"
	"carino.systems/cerberus/internal/users"
)

// Status of a check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check is one line of the checklist.
type Check struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the whole checklist.
type Report struct {
	Time   time.Time      `json:"time"`
	Checks []Check        `json:"checks"`
	Counts map[Status]int `json:"counts"`
}

// Worst is the most severe status in the report.
func (r Report) Worst() Status {
	switch {
	case r.Counts[Fail] > 0:
		return Fail
	case r.Counts[Warn] > 0:
		return Warn
	}
	return OK
}

// Env is everything the checks look at.
type Env struct {
	Cfg        *config.Config // nil when it failed to load (CfgErr)
	CfgErr     error
	ConfigPath string // config.json
	ConfigDir  string // holds config.json, devices.json and the vault files
	DataDir    string // holds audit.jsonl and session data
	UserConfig string // os.UserConfigDir (systemd user units live under it)
	Home       string
	GOOS       string

	LookPath  func(name string) (string, error)
	Output    func(ctx context.Context, name string, args ...string) (string, error)
	DiskFree  func(path string) (uint64, error)
	Reachable func(ctx context.Context, url string) error
	Bwrap     func(ctx context.Context) (string, error)
	Stat      func(path string) (fs.FileInfo, error)
	Readable  func(path string) bool
	Getenv    func(string) string
	Resolve   func(host string) ([]net.IP, error)
	Now       func() time.Time
	// TokenRotated is when the admin token was last rotated (zero: unknown).
	TokenRotated func() time.Time
	// Users is the account store (Phase 4a; nil: no accounts known).
	Users *users.Store
	// LingerDir is where systemd keeps linger flags (default
	// /var/lib/systemd/linger).
	LingerDir string
	// Bindable reports whether a host:port can be bound now (preview
	// ports; default: listen and close at once).
	Bindable func(addr string) bool
}

// Defaults fills every nil field of e with the real implementation.
func Defaults(e Env) Env {
	if e.GOOS == "" {
		e.GOOS = runtime.GOOS
	}
	if e.Home == "" {
		e.Home, _ = os.UserHomeDir()
	}
	if e.UserConfig == "" {
		e.UserConfig, _ = os.UserConfigDir()
	}
	if e.ConfigPath == "" && e.Cfg != nil {
		e.ConfigPath = e.Cfg.Path()
	}
	if e.ConfigDir == "" && e.ConfigPath != "" {
		e.ConfigDir = filepath.Dir(e.ConfigPath)
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.Output == nil {
		e.Output = func(ctx context.Context, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return string(out), err
		}
	}
	if e.DiskFree == nil {
		e.DiskFree = diskFree
	}
	if e.Reachable == nil {
		e.Reachable = reachable
	}
	if e.Bwrap == nil {
		e.Bwrap = sandbox.Available
	}
	if e.Stat == nil {
		e.Stat = os.Stat
	}
	if e.Readable == nil {
		e.Readable = func(p string) bool {
			f, err := os.Open(p)
			if err != nil {
				return false
			}
			defer f.Close()
			_, err = f.Read(make([]byte, 1))
			return err == nil
		}
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Resolve == nil {
		e.Resolve = func(host string) ([]net.IP, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			ips := make([]net.IP, len(addrs))
			for i, a := range addrs {
				ips[i] = a.IP
			}
			return ips, err
		}
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Bindable == nil {
		e.Bindable = func(addr string) bool {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return false
			}
			ln.Close()
			return true
		}
	}
	if e.TokenRotated == nil {
		e.TokenRotated = func() time.Time { return time.Time{} }
	}
	return e
}

func reachable(ctx context.Context, u string) error {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// Run runs every check. e should have been through Defaults.
func Run(ctx context.Context, e Env) Report {
	e = Defaults(e)
	var checks []Check
	checks = append(checks, configCheck(e))
	if e.Cfg != nil {
		checks = append(checks, modesCheck(e), listenCheck(e), previewPortsCheck(e), tlsCheck(e), tokenAgeCheck(e),
			localNoAuthCheck(e), sandboxCheck(ctx, e), raplCheck(e), enginesCheck(ctx, e),
			agentsCheck(e), systemdCheck(e), diskCheck(e), retentionCheck(e), gitCheck(ctx, e),
			voiceCheck(e), serviceCheck(e), usersCheck(e), usersTLSCheck(e), lingerCheck(e), usersSandboxCheck(ctx, e))
	}
	r := Report{Time: e.Now().UTC(), Checks: checks, Counts: map[Status]int{OK: 0, Warn: 0, Fail: 0}}
	for _, c := range checks {
		r.Counts[c.Status]++
	}
	return r
}

func ok(id, name, detail string) Check { return Check{ID: id, Name: name, Status: OK, Detail: detail} }
func warn(id, name, detail, fix string) Check {
	return Check{ID: id, Name: name, Status: Warn, Detail: detail, Fix: fix}
}
func fail(id, name, detail, fix string) Check {
	return Check{ID: id, Name: name, Status: Fail, Detail: detail, Fix: fix}
}

// ---------- config ----------

func configCheck(e Env) Check {
	const id, name = "config", "Config"
	if e.Cfg == nil {
		msg := "the config could not be loaded"
		if e.CfgErr != nil {
			msg = e.CfgErr.Error()
		}
		return fail(id, name, msg, "fix the JSON in "+orDash(e.ConfigPath)+" (or move it away to start from defaults)")
	}
	c := e.Cfg
	var probs []string
	for _, a := range append([]string{c.Server.Listen}, c.Server.ExtraListen...) {
		if _, _, err := net.SplitHostPort(a); err != nil {
			probs = append(probs, fmt.Sprintf("listen %q is not host:port", a))
		}
	}
	if c.TokenSnapshot() == "" {
		probs = append(probs, "server.token is empty")
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		probs = append(probs, "set both server.tls_cert and server.tls_key, or neither")
	}
	if c.Server.PublicURL != "" {
		if u, err := url.Parse(c.Server.PublicURL); err != nil || u.Host == "" {
			probs = append(probs, "server.public_url is not a URL")
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Sandbox.Default)) {
	case "", "none", "off", "false", "no", "bwrap":
	default:
		probs = append(probs, fmt.Sprintf("sandbox.default %q is not none or bwrap (it is treated as bwrap)", c.Sandbox.Default))
	}
	switch c.Agent.Approve {
	case "", "ask", "auto":
	default:
		probs = append(probs, fmt.Sprintf("agent.approve %q is not ask or auto", c.Agent.Approve))
	}
	if c.DefaultAgent != "" {
		if _, found := c.AgentProfile(c.DefaultAgent); !found {
			probs = append(probs, fmt.Sprintf("default_agent %q has no profile", c.DefaultAgent))
		}
	}
	if len(probs) > 0 {
		return fail(id, name, strings.Join(probs, "; "), "edit "+orDash(e.ConfigPath))
	}
	return ok(id, name, "valid ("+orDash(e.ConfigPath)+")")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---------- file modes ----------

// modesCheck wants every file holding a credential or the audit trail to
// be readable by the owner only.
func modesCheck(e Env) Check {
	const id, name = "modes", "File permissions"
	if e.GOOS == "windows" {
		return ok(id, name, "not checked on Windows (ACLs)")
	}
	var files []string
	if e.ConfigPath != "" {
		files = append(files, e.ConfigPath)
	}
	if e.ConfigDir != "" {
		for _, f := range []string{"devices.json", "vault.json", "vault.key", "vault-index.json", "secrets.json"} {
			files = append(files, filepath.Join(e.ConfigDir, f))
		}
	}
	if e.DataDir != "" {
		files = append(files, filepath.Join(e.DataDir, "audit.jsonl"), filepath.Join(e.DataDir, users.FileName),
			filepath.Join(e.DataDir, users.LoginsFile))
	}
	var loose []string
	checked := 0
	for _, f := range files {
		st, err := e.Stat(f)
		if err != nil {
			continue
		}
		checked++
		if st.Mode().Perm()&0o077 != 0 {
			loose = append(loose, fmt.Sprintf("%s (%04o)", f, st.Mode().Perm()))
		}
	}
	if len(loose) > 0 {
		return fail(id, name, "readable by others: "+strings.Join(loose, ", "),
			"chmod 600 "+strings.Join(pathsOf(loose), " "))
	}
	return ok(id, name, fmt.Sprintf("%d sensitive files, all 0600", checked))
}

func pathsOf(loose []string) []string {
	out := make([]string, len(loose))
	for i, l := range loose {
		out[i], _, _ = strings.Cut(l, " (")
	}
	return out
}

// ---------- listen addresses ----------

// Private reports whether ip is loopback, RFC 1918, CGNAT (100.64/10), ULA
// (fc00::/7) or link-local: an address the public internet cannot reach
// directly.
func Private(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1]&0xC0 == 64
	}
	return false
}

// Wildcard reports whether a listen host binds every interface.
func Wildcard(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// CheckListen applies bind safety to listen addresses (host:port). A host
// name is resolved and judged by its addresses ("localhost" needs no
// lookup); a name that does not resolve is a warning, and the caller must
// check the address it finally binds (BoundSafe). A public address is an error unless allowPublic; a wildcard bind
// and an allowed public address are warnings.
func CheckListen(addrs []string, allowPublic bool, resolve func(string) ([]net.IP, error)) (warnings []string, err error) {
	var public []string
	for _, a := range addrs {
		if a == "" {
			continue
		}
		host, _, e := net.SplitHostPort(a)
		if e != nil {
			return warnings, fmt.Errorf("listen %q: %w", a, e)
		}
		if Wildcard(host) {
			warnings = append(warnings, fmt.Sprintf("%s listens on EVERY interface: anyone who can reach this machine can try the token; prefer 127.0.0.1 plus a LAN or tailnet address", a))
			continue
		}
		var ips []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else if strings.EqualFold(host, "localhost") {
			continue
		} else if resolve != nil {
			var rerr error
			ips, rerr = resolve(host)
			if rerr != nil || len(ips) == 0 {
				// Not bindable now, but DNS may come up later and the
				// daemon retries extra addresses: the server checks the
				// address it actually bound (serve.go), so say so here.
				warnings = append(warnings, fmt.Sprintf("%s: cannot resolve %q yet; the address it binds later is checked again and refused if public", a, host))
			}
		}
		for _, ip := range ips {
			if !Private(ip) {
				public = append(public, fmt.Sprintf("%s (%s)", a, ip))
				break
			}
		}
	}
	if len(public) > 0 {
		if !allowPublic {
			return warnings, fmt.Errorf("refusing to listen on a public address: %s; Cerberus can run commands on this machine, keep it on loopback, a LAN or a tailnet (set server.allow_public only behind your own firewall or proxy)", strings.Join(public, ", "))
		}
		warnings = append(warnings, "listening on a PUBLIC address (server.allow_public): "+strings.Join(public, ", "))
	}
	return warnings, nil
}

// BoundSafe applies bind safety to an address actually bound (a
// listener's Addr): a public IP is an error unless allowPublic. A wildcard
// bind passes (CheckListen warns about it up front). It closes the gap of
// a host name that did not resolve, or resolved differently, when
// CheckListen ran.
func BoundSafe(addr net.Addr, allowPublic bool) error {
	if allowPublic || addr == nil {
		return nil
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP == nil || tcp.IP.IsUnspecified() || Private(tcp.IP) {
		return nil
	}
	return fmt.Errorf("refusing to serve on public address %s; set server.allow_public only behind your own firewall or proxy", addr)
}

func listenAddrs(c *config.Config) []string {
	return append([]string{c.Server.Listen}, c.Server.ExtraListen...)
}

func listenCheck(e Env) Check {
	const id, name = "listen", "Listen addresses"
	addrs := listenAddrs(e.Cfg)
	warns, err := CheckListen(addrs, e.Cfg.Server.AllowPublic, e.Resolve)
	if err != nil {
		return fail(id, name, err.Error(), "use 127.0.0.1, a LAN (192.168.x) or tailnet (100.x) address in server.listen / extra_listen")
	}
	if len(warns) > 0 {
		if inContainer(e) && onlyWildcardWarnings(addrs) {
			return ok(id, name, "inside a container: "+strings.Join(nonEmpty(addrs), ", ")+" is the container's own network; who reaches it depends on the published port (keep it on 127.0.0.1 or a private address)")
		}
		return warn(id, name, strings.Join(warns, "; "), "bind specific private addresses instead of 0.0.0.0 / ::")
	}
	return ok(id, name, "private only: "+strings.Join(nonEmpty(addrs), ", "))
}

// previewPortsCheck: server.preview_ports parses, stays clear of the
// listen ports, binds only where the app may (private addresses unless
// allow_public) and has free ports. Ports held by this daemon's own open
// previews count as busy here.
func previewPortsCheck(e Env) Check {
	const id, name = "preview_ports", "Preview ports"
	lo, hi, on, err := config.ParsePortRange(e.Cfg.Server.PreviewPorts)
	if err != nil {
		return fail(id, name, "server.preview_ports: "+err.Error(), "set a range such as 7350-7369, or empty it")
	}
	if !on {
		return ok(id, name, "off: static previews stay under /preview/<id>/ and live dev servers are not offered (set server.preview_ports, e.g. 7350-7369)")
	}
	addrs := listenAddrs(e.Cfg)
	var hosts []string
	seen := map[string]bool{}
	for _, a := range addrs {
		h, p, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		if n, _ := strconv.Atoi(p); n >= lo && n <= hi {
			return fail(id, name, fmt.Sprintf("the range %d-%d holds the listen port %d", lo, hi, n), "move server.preview_ports clear of the listen ports")
		}
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	probe := make([]string, 0, len(hosts))
	for _, h := range hosts {
		probe = append(probe, net.JoinHostPort(h, strconv.Itoa(lo)))
	}
	warns, err := CheckListen(probe, e.Cfg.Server.AllowPublic, e.Resolve)
	if err != nil {
		return fail(id, name, "preview ports would bind "+err.Error(), "keep server.listen / extra_listen on private addresses")
	}
	free := 0
	for port := lo; port <= hi; port++ {
		all := true
		for _, h := range hosts {
			if !e.Bindable(net.JoinHostPort(h, strconv.Itoa(port))) {
				all = false
				break
			}
		}
		if all {
			free++
		}
	}
	total := hi - lo + 1
	detail := fmt.Sprintf("%d-%d on %s: %d of %d free now", lo, hi, strings.Join(hosts, ", "), free, total)
	if free == 0 {
		return warn(id, name, detail+" (every port is taken, by open previews or by other programs)", "pick another server.preview_ports range")
	}
	if len(warns) > 0 && !(inContainer(e) && onlyWildcardWarnings(addrs)) {
		return warn(id, name, detail+"; "+strings.Join(warns, "; "), "bind specific private addresses instead of 0.0.0.0 / ::")
	}
	return ok(id, name, detail+"; each open preview gets its own origin")
}

// containerEnv is set by the container image (Containerfile). Inside a
// container the daemon cannot see the published port, the service manager
// or the memory limits, so a few checks speak about the container instead.
const containerEnv = "CERBERUS_CONTAINER"

func inContainer(e Env) bool {
	return e.Getenv != nil && e.Getenv(containerEnv) != ""
}

const containerHTTPSFix = "publish the port on 127.0.0.1 and put HTTPS in front (tailscale serve, Caddy or another reverse proxy) for other machines"

// onlyWildcardWarnings: every listen address is a wildcard (0.0.0.0 / ::)
// or empty, the normal shape inside a container.
func onlyWildcardWarnings(addrs []string) bool {
	for _, a := range addrs {
		if a == "" {
			continue
		}
		host, _, err := net.SplitHostPort(a)
		if err != nil || !Wildcard(host) {
			return false
		}
	}
	return true
}

func nonEmpty(s []string) []string {
	var out []string
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func tlsCheck(e Env) Check {
	const id, name = "tls", "TLS"
	c := e.Cfg
	var open []string
	for i, a := range listenAddrs(c) {
		host, _, err := net.SplitHostPort(a)
		if err != nil || loopbackHost(host) {
			continue
		}
		// Only extra addresses serve HTTPS (the primary Listen is plain HTTP).
		if i == 0 || c.Server.TLSCert == "" || c.Server.TLSKey == "" {
			open = append(open, a)
		}
	}
	if len(open) == 0 {
		if c.Server.TLSCert != "" {
			return ok(id, name, "extra addresses serve HTTPS")
		}
		return ok(id, name, "loopback only: TLS not needed")
	}
	for _, f := range []string{c.Server.TLSCert, c.Server.TLSKey} {
		if f == "" {
			continue
		}
		if _, err := e.Stat(f); err != nil {
			return fail(id, name, "TLS file missing: "+f, "run `tailscale cert` again or fix server.tls_cert / tls_key")
		}
	}
	if inContainer(e) {
		return warn(id, name, "inside a container: plain HTTP on "+strings.Join(open, ", ")+"; fine while the published port stays on this machine or a tailnet", containerHTTPSFix)
	}
	return warn(id, name, "plain HTTP on a non-loopback address: "+strings.Join(open, ", ")+" (fine inside a tailnet, which encrypts; not on a shared LAN)",
		"set server.tls_cert and tls_key (e.g. from `tailscale cert`) and move the address to extra_listen")
}

const tokenMaxAge = 180 * 24 * time.Hour

func tokenAgeCheck(e Env) Check {
	const id, name = "token_age", "Admin token age"
	// Server.TokenCreated is exact when present (first run and every
	// rotation stamp it); only configs from before it fall back to the
	// newest audited rotation, which scans the audit log, so it is not
	// read when it would not be used.
	var t time.Time
	verb := "created"
	if e.Cfg != nil {
		t = e.Cfg.TokenCreatedAt()
	}
	if t.IsZero() {
		t, verb = e.TokenRotated(), "last rotated"
	}
	if t.IsZero() {
		return ok(id, name, "no rotation recorded yet; rotate after sharing it anywhere you do not trust")
	}
	age := e.Now().Sub(t)
	days := int(age.Hours() / 24)
	if age > tokenMaxAge {
		return warn(id, name, fmt.Sprintf("%s %d days ago", verb, days), "cerberus token rotate (paired devices keep working)")
	}
	return ok(id, name, fmt.Sprintf("%s %d days ago", verb, days))
}

func localNoAuthCheck(e Env) Check {
	const id, name = "local_no_auth", "Local /v1 without a key"
	if !e.Cfg.Server.LocalNoAuth {
		return ok(id, name, "off: every /v1 request needs a key")
	}
	if e.Users != nil && e.Users.HasUsers() {
		for _, u := range e.Users.List() {
			if u.Role == users.RoleUser && !u.Disabled {
				return fail(id, name, "on with role-user accounts: keyless local /v1 acts as the admin (no budget, no attribution); "+
					"Cerberus pauses it while their sandboxed work runs, but anything else they start on this machine bypasses budgets",
					"set server.local_no_auth to false and give people personal API keys")
			}
		}
		return warn(id, name, "on with user accounts: any local program acts as the admin on /v1 (no budget, no attribution)",
			"set server.local_no_auth to false and give people personal API keys")
	}
	return warn(id, name, "on: any program of any user on this machine can use /v1 (models and API spend) without a key; web pages cannot",
		"set server.local_no_auth to false if other people log in to this machine")
}

// ---------- sandbox, power, engines, agents ----------

func sandboxCheck(ctx context.Context, e Env) Check {
	const id, name = "sandbox", "Session sandbox (bwrap)"
	want := false
	for _, a := range e.Cfg.AgentProfiles() {
		want = want || sandbox.Resolve(e.Cfg.Sandbox, a, "") == sandbox.ModeBwrap
	}
	for _, r := range e.Cfg.Repos {
		want = want || sandbox.Normalize(r.Sandbox) == sandbox.ModeBwrap
	}
	bin, err := e.Bwrap(ctx)
	switch {
	case err == nil && want:
		return ok(id, name, "available ("+bin+") and in use")
	case err == nil:
		return warn(id, name, "available ("+bin+") but no session uses it: agents can read everything you can",
			`set sandbox.default to "bwrap"`)
	case want && e.Cfg.Sandbox.Strict:
		return fail(id, name, err.Error()+"; sandboxed sessions will refuse to start (sandbox.strict)", "install bubblewrap")
	case want:
		return fail(id, name, err.Error()+"; sandboxed sessions fall back to none", "install bubblewrap, or set sandbox.strict to refuse instead")
	}
	return warn(id, name, err.Error()+"; sessions run unsandboxed", "install bubblewrap and set sandbox.default to \"bwrap\"")
}

func raplCheck(e Env) Check {
	const id, name = "rapl", "CPU power (RAPL)"
	if e.GOOS != "linux" {
		return ok(id, name, "not available on "+e.GOOS+"; CPU watts are estimated")
	}
	p := "/sys/class/powercap/intel-rapl:0/energy_uj"
	if _, err := e.Stat(p); err != nil {
		return ok(id, name, "no RAPL on this CPU; CPU watts are estimated")
	}
	if e.Readable(p) {
		return ok(id, name, "readable")
	}
	return warn(id, name, p+" is root-only; CPU watts are estimated", "cerberus rapl-setup")
}

func enginesCheck(ctx context.Context, e Env) Check {
	const id, name = "engines", "Model engines"
	en := e.Cfg.Engines
	var have, notes []string
	if en.LlamaCpp.Enabled {
		if _, err := e.LookPath(orDefault(en.LlamaCpp.Binary, "llama-server")); err == nil {
			have = append(have, "llama.cpp")
		} else {
			notes = append(notes, "llama-server not on PATH")
		}
	}
	if en.LMStudio.Enabled {
		_, err := e.LookPath(orDefault(en.LMStudio.Binary, "lms"))
		if err != nil {
			_, err = e.Stat(filepath.Join(e.Home, ".lmstudio", "bin", "lms"))
		}
		switch {
		case err == nil:
			have = append(have, "LM Studio")
		case en.LMStudio.BaseURL != "" && e.Reachable(ctx, strings.TrimRight(en.LMStudio.BaseURL, "/")+"/models") == nil:
			have = append(have, "LM Studio (server)")
		default:
			notes = append(notes, "LM Studio not installed")
		}
	}
	if en.Ollama.Enabled && en.Ollama.URL != "" {
		if err := e.Reachable(ctx, strings.TrimRight(en.Ollama.URL, "/")+"/api/version"); err == nil {
			have = append(have, "Ollama")
		} else {
			notes = append(notes, "Ollama not reachable")
		}
	}
	providers := 0
	for _, p := range e.Cfg.Providers {
		if p.Enabled {
			providers++
		}
	}
	if len(have) == 0 {
		detail := "no local engine found"
		if len(notes) > 0 {
			detail += " (" + strings.Join(notes, ", ") + ")"
		} else {
			detail += " (all local engines are turned off)"
		}
		if providers > 0 {
			detail += fmt.Sprintf("; %d API provider(s) configured", providers)
		}
		return warn(id, name, detail, "install llama.cpp (llama-server), LM Studio or Ollama, or add a provider")
	}
	detail := strings.Join(have, ", ")
	if providers > 0 {
		detail += fmt.Sprintf(" + %d API provider(s)", providers)
	}
	return ok(id, name, detail)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func agentsCheck(e Env) Check {
	const id, name = "agents", "Agents installed"
	var have, missing []string
	defOK := true
	for _, a := range e.Cfg.AgentProfiles() {
		if len(a.Command) == 0 {
			continue
		}
		bin := a.Command[0]
		found := bin == "{self}"
		if !found {
			_, err := e.LookPath(bin)
			found = err == nil
		}
		if found {
			have = append(have, a.Name)
		} else {
			missing = append(missing, a.Name)
			if a.Name == orDefault(e.Cfg.DefaultAgent, "cerberus") {
				defOK = false
			}
		}
	}
	detail := fmt.Sprintf("%d of %d available", len(have), len(have)+len(missing))
	if len(missing) > 0 {
		detail += " (missing: " + strings.Join(missing, ", ") + ")"
	}
	if !defOK {
		return fail(id, name, "the default agent is not installed; "+detail, "install it or change default_agent")
	}
	return ok(id, name, detail)
}

func systemdCheck(e Env) Check {
	const id, name = "systemd", "systemd-run and user bus"
	if e.GOOS != "linux" {
		return ok(id, name, "not used on "+e.GOOS)
	}
	if e.Cfg != nil && e.Cfg.Guard.SystemdSlice == "" && inContainer(e) {
		return ok(id, name, "inside a container: the container's own limits apply (podman --memory, or MemoryMax in the Quadlet unit)")
	}
	if e.Cfg != nil && e.Cfg.Guard.SystemdSlice == "" {
		return warn(id, name, "guard.systemd_slice is empty: sessions run without a memory limit or slice",
			`set guard.systemd_slice (default "agents.slice")`)
	}
	if _, err := e.LookPath("systemd-run"); err != nil {
		return warn(id, name, "systemd-run not found: sessions run without a memory limit or slice", "install systemd (or accept unlimited sessions)")
	}
	bus := e.Getenv("DBUS_SESSION_BUS_ADDRESS") != ""
	if rt := e.Getenv("XDG_RUNTIME_DIR"); !bus && rt != "" {
		_, err := e.Stat(filepath.Join(rt, "bus"))
		bus = err == nil
	}
	if !bus {
		return warn(id, name, "no user D-Bus: systemd-run --user scopes cannot start, so sessions run without limits",
			"run Cerberus as a systemd user service (cerberus install-service) or enable lingering")
	}
	return ok(id, name, "sessions get a memory-limited scope")
}

// ---------- disk, retention, git, voice, service ----------

const (
	diskWarn = 5 << 30
	diskFail = 1 << 30
)

func diskCheck(e Env) Check {
	const id, name = "disk", "Disk space for data"
	if e.DataDir == "" {
		return warn(id, name, "data folder unknown", "")
	}
	dir := e.DataDir
	if _, err := e.Stat(dir); err != nil {
		dir = filepath.Dir(dir)
	}
	free, err := e.DiskFree(dir)
	if err != nil {
		return warn(id, name, "could not measure free space: "+err.Error(), "")
	}
	gib := float64(free) / (1 << 30)
	switch {
	case free < diskFail:
		return fail(id, name, fmt.Sprintf("%.1f GiB free under %s", gib, e.DataDir), "free space or run cerberus prune")
	case free < diskWarn:
		return warn(id, name, fmt.Sprintf("%.1f GiB free under %s", gib, e.DataDir), "free space or lower sessions.retention_days")
	}
	return ok(id, name, fmt.Sprintf("%.0f GiB free", gib))
}

func retentionCheck(e Env) Check {
	const id, name = "retention", "Session retention"
	d := e.Cfg.Sessions.RetentionDays
	if d <= 0 {
		return warn(id, name, "finished sessions are kept forever (retention_days 0)", "set sessions.retention_days (e.g. 30)")
	}
	return ok(id, name, fmt.Sprintf("finished sessions pruned after %d days", d))
}

var gitVersionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

// minGit is the oldest git with `git worktree remove` (2.17).
var minGit = [2]int{2, 17}

func gitCheck(ctx context.Context, e Env) Check {
	const id, name = "git", "git"
	if _, err := e.LookPath("git"); err != nil {
		return fail(id, name, "git is not installed: repo sessions cannot work", "install git")
	}
	out, err := e.Output(ctx, "git", "--version")
	if err != nil {
		return warn(id, name, "git --version failed: "+err.Error(), "")
	}
	m := gitVersionRE.FindStringSubmatch(out)
	if m == nil {
		return warn(id, name, "unrecognised version: "+strings.TrimSpace(out), "")
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "git version"))
	if maj < minGit[0] || (maj == minGit[0] && mnr < minGit[1]) {
		return fail(id, name, "git "+v+" lacks worktree support Cerberus needs (2.17+)", "upgrade git")
	}
	return ok(id, name, "git "+v+" (worktrees supported)")
}

func voiceCheck(e Env) Check {
	const id, name = "voice", "Voice (ffmpeg)"
	v := e.Cfg.Voice
	if !v.Enabled || v.Backend == "off" {
		return ok(id, name, "voice input is off")
	}
	ff := orDefault(v.FFmpeg, "ffmpeg")
	if _, err := e.LookPath(ff); err != nil {
		return warn(id, name, ff+" not found: browser recordings cannot be converted for transcription", "install ffmpeg")
	}
	return ok(id, name, "ffmpeg found")
}

func serviceCheck(e Env) Check {
	const id, name = "service", "Service unit"
	if e.GOOS != "linux" {
		return ok(id, name, "no service integration on "+e.GOOS+" yet")
	}
	if inContainer(e) {
		return ok(id, name, "inside a container: the container runtime keeps it running (Quadlet unit or a restart policy)")
	}
	if e.UserConfig == "" {
		return warn(id, name, "user config folder unknown", "")
	}
	unit := filepath.Join(e.UserConfig, "systemd", "user", "cerberus.service")
	if _, err := e.Stat(unit); errors.Is(err, fs.ErrNotExist) || err != nil {
		return warn(id, name, "no systemd user unit: Cerberus stops when you log out", "cerberus install-service")
	}
	return ok(id, name, unit)
}

// RotationEvent is the audit route of a token rotation made by the CLI
// without the daemon; through the API it is RotationRoute.
const (
	RotationEvent = "token.rotate"
	RotationRoute = "POST /api/token/rotate"
)

// LastRotation is the time of the newest successful token rotation in the
// audit log (zero if none is recorded among the newest audit.MaxLimit).
func LastRotation(l *audit.Logger) time.Time {
	if l == nil {
		return time.Time{}
	}
	recs, _ := l.Query(time.Time{}, "", audit.MaxLimit)
	for _, r := range recs {
		if r.Route == RotationEvent || (r.Route == RotationRoute && r.Status == http.StatusOK) {
			return r.Time
		}
	}
	return time.Time{}
}

// ---------- users and roles (Phase 4a) ----------

func usersCheck(e Env) Check {
	const id, name = "users", "User accounts"
	if e.Users == nil || !e.Users.HasUsers() {
		return warn(id, name, "no admin account yet: only the admin token signs in",
			"Run: cerberus user add NAME --role admin")
	}
	per := map[string]int{}
	admins := 0
	for _, u := range e.Users.List() {
		per[u.Role]++
		if u.Role == users.RoleAdmin && !u.Disabled {
			admins++
		}
	}
	if admins == 0 {
		return warn(id, name, "no enabled admin account", "cerberus user enable NAME (or add an admin)")
	}
	var parts []string
	for _, r := range users.Roles {
		if per[r] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", per[r], r))
		}
	}
	return ok(id, name, strings.Join(parts, ", "))
}

func usersTLSCheck(e Env) Check {
	const id, name = "users_tls", "Passwords over HTTPS"
	if e.Users == nil || !e.Users.HasUsers() {
		return ok(id, name, "no accounts")
	}
	c := e.Cfg
	var open []string
	for i, a := range listenAddrs(c) {
		host, _, err := net.SplitHostPort(a)
		if err != nil || loopbackHost(host) {
			continue
		}
		if i == 0 || c.Server.TLSCert == "" || c.Server.TLSKey == "" {
			open = append(open, a)
		}
	}
	if len(open) > 0 && inContainer(e) {
		return warn(id, name, "accounts exist and the container serves plain HTTP on "+strings.Join(open, ", ")+": passwords are safe only while the published port stays on this machine", containerHTTPSFix)
	}
	if len(open) > 0 {
		return fail(id, name, "accounts exist and "+strings.Join(open, ", ")+" serves plain HTTP: passwords would cross the network in clear",
			"set server.tls_cert and tls_key (tailscale cert) and listen there through extra_listen, or use Tailscale serve")
	}
	return ok(id, name, "sign-ins stay on loopback or HTTPS")
}

func lingerCheck(e Env) Check {
	const id, name = "linger", "Runs after logout"
	if e.GOOS != "linux" {
		return ok(id, name, "not checked on "+e.GOOS)
	}
	if inContainer(e) {
		return ok(id, name, "inside a container: for a rootless Quadlet unit, run loginctl enable-linger on the host")
	}
	if e.UserConfig == "" {
		return ok(id, name, "no service unit")
	}
	if _, err := e.Stat(filepath.Join(e.UserConfig, "systemd", "user", "cerberus.service")); err != nil {
		return ok(id, name, "no service unit installed")
	}
	user := e.Getenv("USER")
	if user == "" {
		return warn(id, name, "cannot tell the user name ($USER)", "loginctl enable-linger $USER")
	}
	dir := e.LingerDir
	if dir == "" {
		dir = "/var/lib/systemd/linger"
	}
	if _, err := e.Stat(filepath.Join(dir, user)); err != nil {
		return warn(id, name, "lingering is off: the service stops when "+user+" logs out", "loginctl enable-linger $USER")
	}
	return ok(id, name, "lingering is on for "+user)
}

func usersSandboxCheck(ctx context.Context, e Env) Check {
	const id, name = "users_sandbox", "Sandbox for other users"
	if e.Users == nil {
		return ok(id, name, "no accounts")
	}
	n := 0
	for _, u := range e.Users.List() {
		if u.Role == users.RoleUser && !u.Disabled {
			n++
		}
	}
	if n == 0 {
		return ok(id, name, "no accounts with role user")
	}
	if _, err := e.Bwrap(ctx); err != nil {
		return fail(id, name, fmt.Sprintf("%d account(s) with role user cannot start sessions: %v", n, err), "install bubblewrap (bwrap)")
	}
	return ok(id, name, "bwrap confines sessions of other users")
}
