package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/users"
)

// User accounts (Phase 4a, docs/USERS.md):
//
//	cerberus user add NAME --role ROLE [--display TEXT] [--password-stdin | --generate]
//	cerberus user list [--json]
//	cerberus user passwd NAME [--password-stdin | --generate]
//	cerberus user role NAME ROLE
//	cerberus user disable NAME | enable NAME
//	cerberus user rm NAME [--to ADMIN] [--yes]
//	cerberus user budget NAME [--usd N] [--tokens N]
//	cerberus user share NAME REPO [--accept] | --unshare REPO
//
// Every command goes through the daemon when it runs (admin token, audited
// as "cli"), else it edits DATA/users.json directly and audits as "cli".

// userOpts are the flags of the user commands.
type userOpts struct {
	role, display, to, unshare string
	stdinPW, generate, yes     bool
	asJSON, accept             bool
	usd                        float64
	tokens                     int64
	usdSet, tokensSet          bool
}

func userCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpFlag(args[0]) {
		printHelp(stderr, "user")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub := args[0]
	var o userOpts
	cfg, pos, code := clientCmd("user "+sub, args[1:], stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&o.role, "role", "", "role: admin, user, viewer, monitor or chat")
		fs.StringVar(&o.display, "display", "", "display name")
		fs.StringVar(&o.to, "to", "", "rm: the admin who takes the data (default: the first other enabled admin)")
		fs.StringVar(&o.unshare, "unshare", "", "share: stop sharing this repo")
		fs.BoolVar(&o.stdinPW, "password-stdin", false, "read the password from stdin (one line)")
		fs.BoolVar(&o.generate, "generate", false, "make a 16-character password, print it once, require a change at sign-in")
		fs.BoolVar(&o.yes, "yes", false, "rm: do not ask")
		fs.BoolVar(&o.asJSON, "json", false, "list: print JSON")
		fs.BoolVar(&o.accept, "accept", false, "share: the user may accept into the repo's main")
		fs.Float64Var(&o.usd, "usd", 0, "budget: monthly US$ cap (0 = none)")
		fs.Int64Var(&o.tokens, "tokens", 0, "budget: monthly token cap (0 = none)")
	})
	if cfg == nil {
		return code
	}
	if sub == "budget" {
		for _, a := range args[1:] {
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			o.usdSet = o.usdSet || name == "usd"
			o.tokensSet = o.tokensSet || name == "tokens"
		}
	}
	u := &userCLI{cfg: cfg, o: o, stdin: stdin, stdout: stdout, stderr: stderr, c: newClient(cfg)}
	switch sub {
	case "add":
		return u.add(pos)
	case "list", "ls":
		return u.list()
	case "passwd", "password":
		return u.passwd(pos)
	case "role":
		return u.role(pos)
	case "disable", "enable":
		return u.disable(pos, sub == "disable")
	case "rm", "remove", "delete":
		return u.rm(pos)
	case "budget":
		return u.budget(pos)
	case "share":
		return u.share(pos)
	}
	fmt.Fprintf(stderr, "cerberus user: unknown command %q (add, list, passwd, role, disable, enable, rm, budget, share)\n", sub)
	return 2
}

type userCLI struct {
	cfg    *config.Config
	o      userOpts
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	c      *client
}

func (u *userCLI) fail(code int, format string, a ...any) int {
	fmt.Fprintf(u.stderr, "cerberus user: "+format+"\n", a...)
	return code
}

// errCode maps a store error to an exit code: 2 for refusals the person
// must fix (last admin, first user, bad name or password), 1 otherwise.
func errCode(err error) int {
	switch {
	case errors.Is(err, users.ErrLastAdmin), errors.Is(err, users.ErrFirstAdmin), errors.Is(err, users.ErrName),
		errors.Is(err, users.ErrNameTaken), errors.Is(err, users.ErrRole), users.IsPasswordError(err),
		errors.Is(err, users.ErrNotFound):
		return 2
	}
	return 1
}

// apiCode maps a daemon error to an exit code (409 last_admin and 400s are 2).
func apiCode(err error) int {
	msg := err.Error()
	for _, s := range []string{"last enabled admin", "first user", "user name", "password", "role", "not found", "yourself", "admin"} {
		if strings.Contains(msg, s) {
			return 2
		}
	}
	return 1
}

// store opens DATA/users.json for direct edits (the daemon is down).
func (u *userCLI) store() (*users.Store, error) {
	data, err := platform.DataDir()
	if err != nil {
		return nil, err
	}
	return users.Open(data)
}

// password gets a new password: generated, one stdin line, or typed twice.
func (u *userCLI) password(name string) (pw string, generated bool, err error) {
	switch {
	case u.o.generate && u.o.stdinPW:
		return "", false, errors.New("use --password-stdin or --generate, not both")
	case u.o.generate:
		return api.GeneratePassword(), true, nil
	case u.o.stdinPW:
		pw, err := readLine(u.stdin, users.MaxPassword)
		if err != nil {
			return "", false, fmt.Errorf("no password on stdin: %w", err)
		}
		return pw, false, users.CheckPassword(name, pw)
	case u.stdin == nil || !isTerminal(u.stdin):
		return "", false, errors.New("not a terminal: use --password-stdin or --generate")
	}
	ask := func(prompt string) (string, error) {
		fmt.Fprint(u.stderr, prompt)
		restore := noEcho(u.stdin)
		line, err := readLine(u.stdin, users.MaxPassword)
		restore()
		fmt.Fprintln(u.stderr)
		return line, err
	}
	a, err := ask("Password for " + name + ": ")
	if err != nil {
		return "", false, err
	}
	if err := users.CheckPassword(name, a); err != nil {
		return "", false, err
	}
	b, err := ask("Again: ")
	if err != nil {
		return "", false, err
	}
	if a != b {
		return "", false, errors.New("the passwords differ")
	}
	return a, false, nil
}

func (u *userCLI) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), clientTimeout)
}

// daemonUsers lists the users through the daemon; down is true when it
// does not answer (then work on the files).
func (u *userCLI) daemonUsers() (list []api.UserPublic, down bool, err error) {
	ctx, cancel := u.ctx()
	defer cancel()
	var res struct {
		Users []api.UserPublic `json:"users"`
	}
	err = u.c.do(ctx, "GET", "/api/users", nil, &res)
	if isUnreachable(err) {
		return nil, true, nil
	}
	return res.Users, false, err
}

func findPublic(list []api.UserPublic, name string) (api.UserPublic, bool) {
	name = users.NormName(name)
	for _, x := range list {
		if x.Name == name {
			return x, true
		}
	}
	return api.UserPublic{}, false
}

func (u *userCLI) add(pos []string) int {
	if len(pos) != 1 || u.o.role == "" {
		return u.fail(2, "usage: cerberus user add NAME --role ROLE [--display TEXT] [--password-stdin | --generate]")
	}
	name := users.NormName(pos[0])
	if !users.ValidName(name) {
		return u.fail(2, "%v", users.ErrName)
	}
	if !users.ValidRole(u.o.role) {
		return u.fail(2, "%v", users.ErrRole)
	}
	list, down, err := u.daemonUsers()
	if err != nil {
		return u.fail(apiCode(err), "%v", err)
	}
	var st *users.Store
	if down {
		if st, err = u.store(); err != nil {
			return u.fail(1, "%v", err)
		}
		if !st.HasUsers() && u.o.role != users.RoleAdmin {
			return u.fail(2, "the first user must be an admin: cerberus user add NAME --role admin")
		}
	} else if len(list) == 0 && u.o.role != users.RoleAdmin {
		return u.fail(2, "the first user must be an admin: cerberus user add NAME --role admin")
	}
	pw, generated, err := u.password(name)
	if err != nil {
		return u.fail(2, "%v", err)
	}
	if down {
		nu, err := st.AddContext(context.Background(), name, u.o.display, u.o.role, pw, generated)
		if err != nil {
			return u.fail(errCode(err), "%v", err)
		}
		cliAudit(u.stderr, "user.add", nu.Name+" ("+nu.Role+")", nu.ID)
	} else {
		ctx, cancel := u.ctx()
		defer cancel()
		body := map[string]string{"name": name, "display": u.o.display, "role": u.o.role, "password": pw}
		if err := u.c.do(ctx, "POST", "/api/users", body, nil); err != nil {
			return u.fail(apiCode(err), "%v", err)
		}
	}
	fmt.Fprintf(u.stdout, "Added %s (%s).\n", name, u.o.role)
	if generated {
		fmt.Fprintf(u.stdout, "Password (shown once; it must be changed at the first sign-in): %s\n", pw)
	}
	return 0
}

func (u *userCLI) list() int {
	list, down, err := u.daemonUsers()
	if err != nil {
		return u.fail(apiCode(err), "%v", err)
	}
	if down {
		st, err := u.store()
		if err != nil {
			return u.fail(1, "%v", err)
		}
		for _, x := range st.List() {
			locked, _ := st.Lockout.Locked(x.Name)
			list = append(list, api.UserPublic{ID: x.ID, Name: x.Name, Display: x.Display, Role: x.Role,
				Disabled: x.Disabled, MustChange: x.MustChange, Locked: locked, LastLogin: x.LastLogin, Budget: x.Budget,
				LegacyOwner: st.LegacyOwner() == x.ID, Repos: x.Repos, Agents: x.Agents})
		}
	}
	if u.o.asJSON {
		if list == nil {
			list = []api.UserPublic{} // "[]", never "null", so scripts can test for no accounts
		}
		enc := json.NewEncoder(u.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(list)
		return 0
	}
	if len(list) == 0 {
		fmt.Fprintln(u.stdout, "No user accounts yet. Add the first admin: cerberus user add NAME --role admin")
		return 0
	}
	tw := tabwriter.NewWriter(u.stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tSTATUS\tLAST LOGIN\tBUDGET")
	for _, x := range list {
		status := "active"
		switch {
		case x.Disabled:
			status = "disabled"
		case x.Locked:
			status = "locked"
		case x.MustChange:
			status = "must change password"
		}
		last := "never"
		if !x.LastLogin.IsZero() {
			last = x.LastLogin.Local().Format("2006-01-02 15:04")
		}
		budget := "none"
		var parts []string
		if x.Budget.MonthlyUSD > 0 {
			parts = append(parts, fmt.Sprintf("US$%.2f", x.Budget.MonthlyUSD))
		}
		if x.Budget.MonthlyTokens > 0 {
			parts = append(parts, strconv.FormatInt(x.Budget.MonthlyTokens, 10)+" tokens")
		}
		if len(parts) > 0 {
			budget = strings.Join(parts, " + ") + "/month"
		}
		name := x.Name
		if x.Display != "" {
			name += " (" + x.Display + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", clean(name), x.Role, status, last, budget)
	}
	_ = tw.Flush()
	return 0
}

// target finds NAME through the daemon (id) or in the files.
func (u *userCLI) target(name string) (id string, st *users.Store, code int) {
	list, down, err := u.daemonUsers()
	if err != nil {
		return "", nil, u.fail(apiCode(err), "%v", err)
	}
	if !down {
		x, ok := findPublic(list, name)
		if !ok {
			return "", nil, u.fail(2, "no user %q", name)
		}
		return x.ID, nil, 0
	}
	st, err = u.store()
	if err != nil {
		return "", nil, u.fail(1, "%v", err)
	}
	x, ok := st.ByName(name)
	if !ok {
		return "", nil, u.fail(2, "no user %q", name)
	}
	return x.ID, st, 0
}

// revokeLocal ends a user's sign-ins and devices when the daemon is down.
func (u *userCLI) revokeLocal(st *users.Store, id string) {
	n := st.Logins.RevokeUser(id, "")
	devs := 0
	if ds, err := devices.Open(u.cfg.Dir()); err == nil {
		gone, _ := ds.RevokeUser(id, st.Owner)
		devs = len(gone)
	}
	if n+devs > 0 {
		fmt.Fprintf(u.stdout, "Ended %d sign-in(s) and revoked %d device(s).\n", n, devs)
	}
}

func (u *userCLI) patch(id string, body map[string]any) int {
	ctx, cancel := u.ctx()
	defer cancel()
	var res struct {
		Cut int `json:"cut"`
	}
	if err := u.c.do(ctx, "PATCH", "/api/users/"+id, body, &res); err != nil {
		return u.fail(apiCode(err), "%v", err)
	}
	if res.Cut > 0 {
		fmt.Fprintf(u.stdout, "%d live request(s) ended.\n", res.Cut)
	}
	return 0
}

func (u *userCLI) passwd(pos []string) int {
	if len(pos) != 1 {
		return u.fail(2, "usage: cerberus user passwd NAME [--password-stdin | --generate]")
	}
	id, st, code := u.target(pos[0])
	if id == "" {
		return code
	}
	name := users.NormName(pos[0])
	pw, generated, err := u.password(name)
	if err != nil {
		return u.fail(2, "%v", err)
	}
	if st != nil {
		if _, err := st.SetPassword(id, pw, true); err != nil {
			return u.fail(errCode(err), "%v", err)
		}
		u.revokeLocal(st, id)
		cliAudit(u.stderr, "user.password", "reset: "+name, id)
	} else {
		ctx, cancel := u.ctx()
		defer cancel()
		if err := u.c.do(ctx, "POST", "/api/users/"+id+"/password", map[string]string{"password": pw}, nil); err != nil {
			return u.fail(apiCode(err), "%v", err)
		}
	}
	fmt.Fprintf(u.stdout, "Password of %s reset; it must be changed at the next sign-in.\n", name)
	if generated {
		fmt.Fprintf(u.stdout, "Password (shown once): %s\n", pw)
	}
	return 0
}

func (u *userCLI) role(pos []string) int {
	if len(pos) != 2 {
		return u.fail(2, "usage: cerberus user role NAME ROLE")
	}
	if !users.ValidRole(pos[1]) {
		return u.fail(2, "%v", users.ErrRole)
	}
	id, st, code := u.target(pos[0])
	if id == "" {
		return code
	}
	if st != nil {
		if _, err := st.SetRole(id, pos[1]); err != nil {
			return u.fail(errCode(err), "%v", err)
		}
		u.revokeLocal(st, id)
		cliAudit(u.stderr, "user.role", users.NormName(pos[0])+" → "+pos[1], id)
	} else if c := u.patch(id, map[string]any{"role": pos[1]}); c != 0 {
		return c
	}
	fmt.Fprintf(u.stdout, "%s is now %s.\n", users.NormName(pos[0]), pos[1])
	return 0
}

func (u *userCLI) disable(pos []string, off bool) int {
	word := map[bool]string{true: "disable", false: "enable"}[off]
	if len(pos) != 1 {
		return u.fail(2, "usage: cerberus user %s NAME", word)
	}
	id, st, code := u.target(pos[0])
	if id == "" {
		return code
	}
	if st != nil {
		if _, err := st.SetDisabled(id, off); err != nil {
			return u.fail(errCode(err), "%v", err)
		}
		if off {
			u.revokeLocal(st, id)
		}
		cliAudit(u.stderr, "user."+word, users.NormName(pos[0]), id)
	} else if c := u.patch(id, map[string]any{"disabled": off}); c != 0 {
		return c
	}
	fmt.Fprintf(u.stdout, "%s %sd.\n", users.NormName(pos[0]), word)
	return 0
}

func (u *userCLI) rm(pos []string) int {
	if len(pos) != 1 {
		return u.fail(2, "usage: cerberus user rm NAME [--to ADMIN] [--yes]")
	}
	name := users.NormName(pos[0])
	list, down, err := u.daemonUsers()
	if err != nil {
		return u.fail(apiCode(err), "%v", err)
	}
	var st *users.Store
	if down {
		if st, err = u.store(); err != nil {
			return u.fail(1, "%v", err)
		}
		for _, x := range st.List() {
			locked, _ := st.Lockout.Locked(x.Name)
			list = append(list, api.UserPublic{ID: x.ID, Name: x.Name, Role: x.Role, Disabled: x.Disabled, Locked: locked})
		}
	}
	victim, ok := findPublic(list, name)
	if !ok {
		return u.fail(2, "no user %q", name)
	}
	to := ""
	if u.o.to != "" {
		t, ok := findPublic(list, u.o.to)
		if !ok || t.Role != users.RoleAdmin || t.Disabled || t.ID == victim.ID {
			return u.fail(2, "--to must name another enabled admin")
		}
		to = t.ID
	} else {
		for _, x := range list {
			if x.ID != victim.ID && x.Role == users.RoleAdmin && !x.Disabled {
				to = x.ID
				break
			}
		}
	}
	if to == "" {
		return u.fail(2, "%v", users.ErrLastAdmin)
	}
	toName := ""
	for _, x := range list {
		if x.ID == to {
			toName = x.Name
		}
	}
	if !u.o.yes {
		if u.stdin == nil || !isTerminal(u.stdin) {
			return u.fail(2, "not a terminal: add --yes to remove %s", name)
		}
		if !confirm(u.stdin, u.stderr, fmt.Sprintf("Remove %s? Their sessions, chats and flows move to %s; nothing is deleted.", name, toName)) {
			return u.fail(1, "not removed")
		}
	}
	if st != nil {
		u.revokeLocal(st, victim.ID)
		if _, err := st.Remove(victim.ID, to); err != nil {
			return u.fail(errCode(err), "%v", err)
		}
		cliAudit(u.stderr, "user.remove", name+"; data moved to "+toName, victim.ID, to)
	} else {
		ctx, cancel := u.ctx()
		defer cancel()
		var res struct {
			Moved map[string]int `json:"moved"`
		}
		if err := u.c.do(ctx, "DELETE", "/api/users/"+victim.ID, map[string]string{"to": to}, &res); err != nil {
			return u.fail(apiCode(err), "%v", err)
		}
		if len(res.Moved) > 0 {
			fmt.Fprintf(u.stdout, "Moved to %s: %d sessions, %d chats, %d flows, %d runs, %d inbox items.\n", toName,
				res.Moved["sessions"], res.Moved["chats"], res.Moved["flows"], res.Moved["runs"], res.Moved["inbox"])
		}
	}
	fmt.Fprintf(u.stdout, "Removed %s; their data now belongs to %s.\n", name, toName)
	return 0
}

func (u *userCLI) budget(pos []string) int {
	if len(pos) != 1 || (!u.o.usdSet && !u.o.tokensSet) {
		return u.fail(2, "usage: cerberus user budget NAME [--usd N] [--tokens N]   (0 = no cap)")
	}
	if u.o.usd < 0 || u.o.tokens < 0 {
		return u.fail(2, "a budget cannot be negative")
	}
	id, st, code := u.target(pos[0])
	if id == "" {
		return code
	}
	var cur users.Budget
	if st != nil {
		x, _ := st.Get(id)
		cur = x.Budget
	} else if list, _, err := u.daemonUsers(); err == nil {
		if x, ok := findPublic(list, pos[0]); ok {
			cur = x.Budget
		}
	}
	if u.o.usdSet {
		cur.MonthlyUSD = u.o.usd
	}
	if u.o.tokensSet {
		cur.MonthlyTokens = u.o.tokens
	}
	if st != nil {
		if _, err := st.SetBudget(id, cur); err != nil {
			return u.fail(errCode(err), "%v", err)
		}
		cliAudit(u.stderr, "user.budget", fmt.Sprintf("%s: US$%.2f, %d tokens", users.NormName(pos[0]), cur.MonthlyUSD, cur.MonthlyTokens), id)
	} else if c := u.patch(id, map[string]any{"budget": cur}); c != 0 {
		return c
	}
	fmt.Fprintf(u.stdout, "Budget of %s: US$%.2f and %d tokens a month (0 = no cap).\n", users.NormName(pos[0]), cur.MonthlyUSD, cur.MonthlyTokens)
	return 0
}

func (u *userCLI) share(pos []string) int {
	if len(pos) < 1 || (u.o.unshare == "" && len(pos) != 2) {
		return u.fail(2, "usage: cerberus user share NAME REPO [--accept] | cerberus user share NAME --unshare REPO")
	}
	id, st, code := u.target(pos[0])
	if id == "" {
		return code
	}
	var repos []users.RepoShare
	if st != nil {
		x, _ := st.Get(id)
		repos = x.Repos
	} else if list, _, err := u.daemonUsers(); err == nil {
		if x, ok := findPublic(list, pos[0]); ok {
			repos = x.Repos
		}
	}
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return filepath.Clean(a)
		}
		return p
	}
	if u.o.unshare != "" {
		p := abs(u.o.unshare)
		repos = slices.DeleteFunc(repos, func(r users.RepoShare) bool { return filepath.Clean(r.Path) == p })
	} else {
		p := abs(pos[1])
		repos = slices.DeleteFunc(repos, func(r users.RepoShare) bool { return filepath.Clean(r.Path) == p })
		repos = append(repos, users.RepoShare{Path: p, Accept: u.o.accept})
	}
	if st != nil {
		if _, err := st.SetRepos(id, repos); err != nil {
			return u.fail(errCode(err), "%v", err)
		}
		cliAudit(u.stderr, "user.share", users.NormName(pos[0])+": "+strconv.Itoa(len(repos))+" repo(s)", id)
	} else if c := u.patch(id, map[string]any{"repos": repos}); c != 0 {
		return c
	}
	fmt.Fprintf(u.stdout, "%s shares %d repo(s).\n", users.NormName(pos[0]), len(repos))
	for _, r := range repos {
		mode := "diff, save, zip"
		if r.Accept {
			mode += ", accept into main"
		}
		fmt.Fprintf(u.stdout, "  %s (%s)\n", r.Path, mode)
	}
	return 0
}
