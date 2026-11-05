package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/api/qr"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/users"
)

// Safety commands (day phase 3): audit, pair, devices, token rotate and
// doctor. Commands that change something go through the daemon when it
// runs (it audits them as actor "cli") and work on the files directly when
// it does not, recording the action in the audit log themselves.

// cliAudit appends a CLI action to DATA/audit.jsonl. A failure to write is
// reported on stderr and never fails the command.
func cliAudit(stderr io.Writer, event, detail string, targets ...string) {
	data, err := platform.DataDir()
	if err == nil {
		var l *audit.Logger
		if l, err = audit.Open(data, audit.Options{}); err == nil {
			err = l.Event(audit.ActorCLI, event, detail, targets...)
			l.Close()
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus: audit log:", err)
	}
}

func outcome(err error) string {
	if err != nil {
		return "failed: " + err.Error()
	}
	return "ok (daemon not running)"
}

// isUnreachable reports whether err means the daemon did not answer.
func isUnreachable(err error) bool {
	var ue *unreachableError
	return errors.As(err, &ue)
}

// ---------- audit ----------

func auditCmd(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("audit", flag.ContinueOnError)
	set.SetOutput(stderr)
	setUsage(set)
	since := set.String("since", "24h", "how far back: a duration (24h, 7d), a date or an RFC 3339 time; \"\" for all")
	actor := set.String("actor", "", "only this actor: cli, system, agent, owner, user, or exact (owner:phone, agent:ID)")
	user := set.String("user", "", "only this user's records (a user name; - for records with no user)")
	limit := set.Int("limit", 200, "at most this many records (newest first)")
	asJSON := set.Bool("json", false, "print JSON lines")
	if set.Parse(args) != nil {
		return 2
	}
	t, err := api.ParseSince(*since, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "cerberus audit:", err)
		return 2
	}
	data, err := platform.DataDir()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus audit:", err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(data, audit.FileName)); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stdout, "No audit records yet.")
		return 0
	}
	l, err := audit.Open(data, audit.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "cerberus audit:", err)
		return 1
	}
	defer l.Close()
	recs, err := l.QueryFilter(audit.Filter{Since: t, Actor: *actor, User: *user, Limit: *limit})
	if err != nil {
		fmt.Fprintln(stderr, "cerberus audit:", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, r := range recs {
			_ = enc.Encode(r)
		}
		return 0
	}
	if len(recs) == 0 {
		fmt.Fprintln(stdout, "No matching audit records.")
		return 0
	}
	printAudit(stdout, recs)
	return 0
}

func printAudit(w io.Writer, recs []audit.Record) {
	for _, r := range recs {
		line := []string{r.Time.Local().Format("2006-01-02 15:04:05"), r.Actor}
		if r.User != "" && r.Actor != audit.User(r.User) {
			line = append(line, "("+r.User+")")
		}
		if r.Remote != "" {
			line = append(line, r.Remote)
		}
		line = append(line, r.Route)
		if len(r.Targets) > 0 {
			line = append(line, "["+strings.Join(r.Targets, " ")+"]")
		}
		if r.Status != 0 {
			line = append(line, fmt.Sprint(r.Status))
		}
		if r.Detail != "" {
			line = append(line, "- "+r.Detail)
		}
		fmt.Fprintln(w, clean(strings.Join(line, "  ")))
	}
}

// ---------- pair and devices ----------

func pairCmd(args []string, stdout, stderr io.Writer) int {
	var name, base string
	var light, mcp bool
	cfg, _, code := clientCmd("pair", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", "", "a name for the device (the audit log shows owner:<name>)")
		fs.StringVar(&base, "url", "", "base URL for the QR code (default: public_url, else a LAN or tailnet address)")
		fs.BoolVar(&light, "light-terminal", false, "draw the QR code for a light terminal background")
		fs.BoolVar(&mcp, "mcp", false, "pair an MCP client (an outside agent such as Claude Code) instead of a device")
	})
	if cfg == nil {
		return code
	}
	if mcp {
		return pairMCPClient(cfg, name, stdout, stderr)
	}
	var res api.PairResult
	err := newClient(cfg).do(context.Background(), "POST", "/api/devices", map[string]string{"name": name}, &res)
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
		tok, d, perr := st.Pair(name)
		if perr != nil {
			fmt.Fprintln(stderr, "cerberus pair:", perr)
			return 1
		}
		cliAudit(stderr, "device.pair", "paired "+d.Name+" (daemon not running)", d.ID)
		res = api.PairResult{Device: d, Token: tok}
	}
	sc := cfg.Server
	sc.Token = res.Token
	urls := pairingURLs(sc, interfaceIPs())
	target := pickPairURL(urls, base, res.Token)
	fmt.Fprintf(stdout, "Paired %q (id %s). Open this on the device (the link is shown once):\n\n", res.Device.Name, res.Device.ID)
	if code, err := qr.Encode([]byte(target)); err == nil {
		fmt.Fprint(stdout, code.HalfBlocks(2, !light))
	} else {
		fmt.Fprintln(stdout, "(the URL is too long for a QR code)")
	}
	fmt.Fprintf(stdout, "\n  %s\n", target)
	for _, u := range urls {
		if u != target {
			fmt.Fprintf(stdout, "  %s\n", u)
		}
	}
	fmt.Fprintf(stdout, "\nRevoke it any time: cerberus devices rm %s\n", res.Device.ID)
	return 0
}

// pickPairURL is the URL for the QR code: base (with the token) when given,
// else the first that is not loopback (PublicURL comes first), else the first.
func pickPairURL(urls []string, base, token string) string {
	if base != "" {
		return strings.TrimRight(base, "/") + "/?token=" + url.QueryEscape(token)
	}
	for _, u := range urls {
		pu, err := url.Parse(u)
		if err != nil {
			continue
		}
		h := pu.Hostname()
		if ip := net.ParseIP(h); h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return u
		}
	}
	if len(urls) > 0 {
		return urls[0]
	}
	return ""
}

func devicesCmd(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("devices", args, stderr, nil)
	if cfg == nil {
		return code
	}
	c := newClient(cfg)
	switch {
	case len(pos) == 0 || pos[0] == "list" || pos[0] == "ls":
		var list api.DeviceList
		err := c.do(context.Background(), "GET", "/api/devices", nil, &list)
		if isUnreachable(err) {
			st, oerr := devices.Open(cfg.Dir())
			if oerr != nil {
				fmt.Fprintln(stderr, "cerberus devices:", oerr)
				return 1
			}
			list.Devices, err = st.List(), nil
		}
		if err != nil {
			fmt.Fprintln(stderr, "cerberus devices:", err)
			return 1
		}
		if len(list.Devices) == 0 {
			fmt.Fprintln(stdout, "No paired devices (cerberus pair --name phone).")
			return 0
		}
		for _, d := range list.Devices {
			seen := "never seen"
			if !d.LastSeen.IsZero() {
				seen = "seen " + d.LastSeen.Local().Format("2006-01-02 15:04") + " from " + d.LastAddr
			}
			kind := ""
			if d.Scope == devices.ScopeMCP {
				kind = " (MCP client)"
			}
			fmt.Fprintf(stdout, "%s  %-16s paired %s, %s%s\n", d.ID, clean(d.Name), d.Created.Local().Format("2006-01-02"), clean(seen), kind)
		}
		return 0
	case (pos[0] == "rm" || pos[0] == "revoke") && len(pos) == 2:
		err := c.do(context.Background(), "DELETE", "/api/devices/"+url.PathEscape(pos[1]), nil, nil)
		if isUnreachable(err) {
			st, oerr := devices.Open(cfg.Dir())
			if oerr != nil {
				fmt.Fprintln(stderr, "cerberus devices:", oerr)
				return 1
			}
			var d devices.Public
			if d, err = st.Revoke(pos[1]); err == nil {
				cliAudit(stderr, "device.revoke", "revoked "+d.Name+" (daemon not running)", d.ID)
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "cerberus devices:", err)
			return 1
		}
		fmt.Fprintln(stdout, "Revoked.")
		return 0
	}
	fmt.Fprintln(stderr, "usage: cerberus devices [list | rm ID]")
	return 2
}

// ---------- token ----------

func tokenCmd(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("token", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 1 || pos[0] != "rotate" {
		fmt.Fprintln(stderr, "usage: cerberus token rotate")
		return 2
	}
	var res api.RotateResult
	err := newClient(cfg).do(context.Background(), "POST", "/api/token/rotate", nil, &res)
	switch {
	case isUnreachable(err):
		old := cfg.Server.Token
		cfg.SetToken(config.NewToken())
		if err := cfg.Save(); err != nil {
			cfg.Server.Token = old
			fmt.Fprintln(stderr, "cerberus token rotate:", err)
			return 1
		}
		cliAudit(stderr, doctor.RotationEvent, "admin token rotated (daemon not running)")
	case err != nil:
		fmt.Fprintln(stderr, "cerberus token rotate:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Admin token rotated and saved to "+cfg.Path()+".")
	fmt.Fprintln(stdout, "Paired devices keep working. Browsers signed in with the old token must sign in again (cerberus url).")
	fmt.Fprintln(stdout, "Running agents keep /v1 access until they end; new sessions get new tokens.")
	return 0
}

// ---------- doctor ----------

// doctorDeps lets tests swap the environment's outside facts.
var doctorDeps = func(e doctor.Env) doctor.Env { return e }

func doctorCmd(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(stderr)
	setUsage(set)
	cfgPath := set.String("config", "", "config file (default ~/.config/cerberus/config.json)")
	asJSON := set.Bool("json", false, "print the report as JSON")
	if set.Parse(args) != nil {
		return 2
	}
	path := *cfgPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintln(stderr, "cerberus doctor:", err)
			return 1
		}
		path = p
	}
	env := doctor.Env{ConfigPath: path}
	// Never create a config here: a missing one is a finding.
	if _, err := os.Stat(path); err != nil {
		env.CfgErr = fmt.Errorf("no config at %s yet (cerberus serve creates it): %w", path, err)
	} else if env.Cfg, err = config.Load(path); err != nil {
		env.Cfg, env.CfgErr = nil, err
	}
	if data, err := platform.DataDir(); err == nil {
		env.DataDir = data
		// Read-only use: Open writes nothing when users.json is missing.
		if st, err := users.Open(data); err == nil {
			env.Users = st
		}
		// Opened lazily: the token-age check runs after the file-mode check,
		// and opening the log narrows its mode.
		env.TokenRotated = func() time.Time {
			if _, err := os.Stat(filepath.Join(data, audit.FileName)); err != nil {
				return time.Time{}
			}
			l, err := audit.Open(data, audit.Options{})
			if err != nil {
				return time.Time{}
			}
			defer l.Close()
			return doctor.LastRotation(l)
		}
	}
	rep := doctor.Run(context.Background(), doctorDeps(env))
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printDoctor(stdout, rep)
	}
	if rep.Worst() == doctor.Fail {
		return 1
	}
	return 0
}

func printDoctor(w io.Writer, rep doctor.Report) {
	fmt.Fprintln(w, "Cerberus doctor")
	for _, c := range rep.Checks {
		label := map[doctor.Status]string{doctor.OK: "ok  ", doctor.Warn: "WARN", doctor.Fail: "FAIL"}[c.Status]
		fmt.Fprintf(w, "  %s  %-26s %s\n", label, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "        %-26s fix: %s\n", "", c.Fix)
		}
	}
	fmt.Fprintf(w, "%d checks: %d ok, %d warn, %d fail\n", len(rep.Checks),
		rep.Counts[doctor.OK], rep.Counts[doctor.Warn], rep.Counts[doctor.Fail])
}
