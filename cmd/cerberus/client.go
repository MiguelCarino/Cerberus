package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

const clientTimeout = 30 * time.Second

// client talks to the local daemon with the bearer token from the config.
type client struct {
	base  string // http://host:port
	token string
	http  *http.Client
}

func newClient(cfg *config.Config) *client {
	return &client{base: localURL(cfg.Server.Listen), token: cfg.Server.Token,
		http: &http.Client{Timeout: clientTimeout}}
}

// localURL turns a listen address into a URL reachable from this host
// (a wildcard bind is reached through loopback).
func localURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// do sends a JSON request and decodes a JSON reply into out (if non-nil).
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cerberus-Actor", audit.ActorCLI) // audited as "cli", not "owner:admin"
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &unreachableError{base: c.base, err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, errorText(resp.Status, raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// unreachableError means the daemon did not answer at all (as opposed to
// answering with an error), so a command may fall back to working locally.
type unreachableError struct {
	base string
	err  error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("cannot reach cerberus at %s (is `cerberus serve` running?): %v", e.base, e.err)
}

func (e *unreachableError) Unwrap() error { return e.err }

func errorText(status string, raw []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch v := e.Error.(type) {
		case string:
			return v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return m
			}
		}
	}
	if t := strings.TrimSpace(string(raw)); t != "" {
		return t
	}
	return status
}

// clientFlags parses the shared -config flag and loads the config.
func clientFlags(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (*config.Config, *flag.FlagSet, int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	cfgPath := fs.String("config", "", "config file (default ~/.config/cerberus/config.json)")
	if extra != nil {
		extra(fs)
	}
	if fs.Parse(args) != nil {
		return nil, nil, 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus:", err)
		return nil, nil, 1
	}
	return cfg, fs, 0
}

// ---------- status ----------

func statusCmd(args []string, stdout, stderr io.Writer) int {
	cfg, _, code := clientFlags("status", args, stderr, nil)
	if cfg == nil {
		return code
	}
	var st api.Status
	if err := newClient(cfg).do(context.Background(), "GET", "/api/status", nil, &st); err != nil {
		fmt.Fprintln(stderr, "cerberus:", err)
		return 1
	}
	printStatus(stdout, st)
	return 0
}

func printStatus(w io.Writer, st api.Status) {
	fmt.Fprintf(w, "Cerberus %s on %s, up %s\n", st.Version, st.Host, time.Duration(st.Uptime)*time.Second)
	for _, g := range st.GPU.GPUs {
		fmt.Fprintf(w, "GPU %d  %s: %.0f W", g.Index, g.Name, g.PowerW)
		if g.PowerLimitW > 0 {
			fmt.Fprintf(w, " of %.0f W", g.PowerLimitW)
		}
		fmt.Fprintf(w, ", VRAM %d/%d MiB, %d%%, %d °C\n", g.MemUsedMiB, g.MemTotalMiB, g.UtilPct, g.TempC)
	}
	var loaded []string
	for _, m := range st.Models {
		if m.Loaded {
			loaded = append(loaded, fmt.Sprintf("%s [%s]", m.ID, m.Backend))
		}
	}
	fmt.Fprintf(w, "Models: %d known, %d loaded", len(st.Models), len(loaded))
	if len(loaded) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(loaded, ", "))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Sessions: %s\n", stateCounts(st.Sessions))
	mem := "unknown"
	if st.Guard.MemAvailableMiB >= 0 {
		mem = fmt.Sprintf("%d MiB", st.Guard.MemAvailableMiB)
	}
	fmt.Fprintf(w, "Guard: %d of %d running, memory available %s\n", st.Guard.Running, st.Guard.MaxSessions, mem)
}

func stateCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// ---------- task ----------

func taskCmd(args []string, stdout, stderr io.Writer) int {
	var spec session.TaskSpec
	var scratch bool
	cfg, fs, code := clientFlags("task", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&spec.Repo, "repo", "", "git repo (default: the repo containing the current directory)")
		fs.StringVar(&spec.Dir, "dir", "", "folder to work in: a git repo gets an isolated worktree, any other folder is edited in place")
		fs.BoolVar(&scratch, "scratch", false, "work in a fresh empty folder (the default outside a git repo)")
		fs.StringVar(&spec.Agent, "agent", "", "agent profile (default from config)")
		fs.StringVar(&spec.Model, "model", "", "model id for agents that take one")
		fs.StringVar(&spec.EscalateTo, "escalate", "", "agent profile to hand over to when checks keep failing")
		fs.IntVar(&spec.Count, "count", 1, "independent sessions with the same task")
		fs.StringVar(&spec.Title, "title", "", "short title")
	})
	if cfg == nil {
		return code
	}
	spec.Task = strings.TrimSpace(strings.Join(fs.Args(), " "))
	if spec.Task == "" {
		fmt.Fprintln(stderr, `cerberus task: describe the task, e.g. cerberus task "fix the failing test"`)
		return 2
	}
	if err := resolveWorkspace(&spec, scratch); err != nil {
		fmt.Fprintln(stderr, "cerberus task:", err)
		return 2
	}
	c := newClient(cfg)
	var out []session.Info
	if err := c.do(context.Background(), "POST", "/api/sessions", spec, &out); err != nil {
		fmt.Fprintln(stderr, "cerberus task:", err)
		return 1
	}
	for _, in := range out {
		fmt.Fprintf(stdout, "%s  %-8s %s\n  %s/#/s/%s\n", in.ID, in.State, in.Title, c.base, url.PathEscape(in.ID))
	}
	return 0
}

// resolveWorkspace picks the session's workspace from the flags:
//
//	--scratch    a fresh empty folder
//	--dir PATH   that folder (the daemon makes a git repo a repo session)
//	--repo PATH  that git repo (its top level)
//	nothing      the git repo containing the current directory, else scratch
func resolveWorkspace(spec *session.TaskSpec, scratch bool) error {
	set := 0
	for _, on := range []bool{scratch, spec.Dir != "", spec.Repo != ""} {
		if on {
			set++
		}
	}
	if set > 1 {
		return errors.New("use only one of --scratch, --dir and --repo")
	}
	switch {
	case scratch:
		spec.Workspace = session.WorkspaceScratch
	case spec.Dir != "":
		abs, err := filepath.Abs(spec.Dir)
		if err != nil {
			return err
		}
		spec.Dir = abs
	case spec.Repo != "":
		abs, err := filepath.Abs(spec.Repo)
		if err != nil {
			return err
		}
		if top, ok := repoTop(abs); ok {
			abs = top
		}
		spec.Repo = abs // else the daemon explains what is wrong with it
		spec.Workspace = session.WorkspaceRepo
	default:
		if top, ok := repoTop("."); ok {
			spec.Repo, spec.Workspace = top, session.WorkspaceRepo
		} else {
			spec.Workspace = session.WorkspaceScratch
		}
	}
	return nil
}

// repoTop is the top level of the git repo containing dir.
func repoTop(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	return filepath.Clean(strings.TrimSpace(string(out))), true
}

// ---------- url ----------

func urlCmd(args []string, stdout, stderr io.Writer) int {
	cfg, _, code := clientFlags("url", args, stderr, nil)
	if cfg == nil {
		return code
	}
	for _, u := range pairingURLs(cfg.Server, interfaceIPs()) {
		fmt.Fprintln(stdout, u)
	}
	return 0
}

// pairingURLs lists the UI URL with ?token= for PublicURL and every listen
// address (https:// for extra addresses when TLS is configured). A wildcard
// bind expands to loopback plus each interface address.
func pairingURLs(sc config.ServerConfig, ifaceIPs []string) []string {
	q := "/?token=" + url.QueryEscape(sc.Token)
	var out []string
	seen := map[string]bool{}
	add := func(base string) {
		if u := strings.TrimRight(base, "/") + q; !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if sc.PublicURL != "" {
		add(sc.PublicURL)
	}
	// Extra addresses serve only HTTPS when a certificate is set (serve.go
	// listenAll); the primary Listen is always plain HTTP.
	tls := sc.TLSCert != "" && sc.TLSKey != ""
	for i, addr := range append([]string{sc.Listen}, sc.ExtraListen...) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		scheme := "http://"
		if i > 0 && tls {
			scheme = "https://"
		}
		if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
			add(scheme + net.JoinHostPort(host, port))
			continue
		}
		add(scheme + net.JoinHostPort("127.0.0.1", port))
		for _, ip := range ifaceIPs {
			add(scheme + net.JoinHostPort(ip, port))
		}
	}
	return out
}

// interfaceIPs are this host's non-loopback unicast addresses.
func interfaceIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if ok && n.IP.IsGlobalUnicast() { // includes private (LAN, Tailscale) ranges
			out = append(out, n.IP.String())
		}
	}
	return out
}
