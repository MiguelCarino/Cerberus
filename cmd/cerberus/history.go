package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/session"
)

// longCallTimeout is for calls that do real work before answering (prune,
// starting a fleet task).
const longCallTimeout = 10 * time.Minute

// searchCmd: cerberus search [--repo R --agent A --state S --since D --until D --limit N] QUERY…
func searchCmd(args []string, stdout, stderr io.Writer) int {
	var repo, agent, state, since, until string
	var limit int
	cfg, pos, code := clientCmd("search", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&repo, "repo", "", "only sessions on this repo or folder (path or name)")
		fs.StringVar(&agent, "agent", "", "only sessions of this agent profile")
		fs.StringVar(&state, "state", "", "only sessions in this state (passed, failed, accepted…)")
		fs.StringVar(&since, "since", "", "created at or after (YYYY-MM-DD or RFC 3339)")
		fs.StringVar(&until, "until", "", "created up to (YYYY-MM-DD includes the day, or RFC 3339)")
		fs.IntVar(&limit, "limit", 20, "most results to show")
	})
	if cfg == nil {
		return code
	}
	text := strings.TrimSpace(strings.Join(pos, " "))
	if text == "" && repo == "" && agent == "" && state == "" && since == "" && until == "" {
		fmt.Fprintln(stderr, `usage: cerberus search [--repo R] [--agent A] [--state S] [--since D] [--until D] [--limit N] "words"`)
		return 2
	}
	q := url.Values{}
	for k, v := range map[string]string{"q": text, "repo": repo, "agent": agent, "state": state, "since": since, "until": until} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	c := newClient(cfg)
	var hits []session.SearchHit
	if err := c.do(context.Background(), "GET", "/api/search?"+q.Encode(), nil, &hits); err != nil {
		fmt.Fprintln(stderr, "cerberus search:", err)
		return 1
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "No matching sessions.")
		return 0
	}
	for _, h := range hits {
		in := h.Session
		where := in.Repo
		if where == "" {
			where = in.Dir
		}
		fmt.Fprintf(stdout, "%s  %-9s %s  %s", in.ID, in.State, in.Created.Local().Format("2006-01-02"), in.Title)
		if where != "" {
			fmt.Fprintf(stdout, "  (%s)", where)
		}
		fmt.Fprintln(stdout)
		for _, sn := range h.Snippets {
			fmt.Fprintf(stdout, "    … %s …\n", oneLine(sn))
		}
	}
	return 0
}

// oneLine collapses whitespace so a snippet prints on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// resumeCmd: cerberus resume ID [text…]
func resumeCmd(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("resume", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) == 0 {
		fmt.Fprintln(stderr, `usage: cerberus resume ID ["what to do next"]`)
		return 2
	}
	id, text := pos[0], strings.TrimSpace(strings.Join(pos[1:], " "))
	c := newClient(cfg)
	if err := c.do(context.Background(), "POST", "/api/sessions/"+url.PathEscape(id)+"/resume", map[string]string{"text": text}, nil); err != nil {
		fmt.Fprintln(stderr, "cerberus resume:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s resumed\n  %s/#/s/%s\n", id, c.base, url.PathEscape(id))
	return 0
}

// pruneCmd: cerberus prune
func pruneCmd(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("prune", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: cerberus prune")
		return 2
	}
	c := newClient(cfg)
	c.http.Timeout = longCallTimeout
	var rep session.PruneReport
	if err := c.do(context.Background(), "POST", "/api/prune", nil, &rep); err != nil {
		fmt.Fprintln(stderr, "cerberus prune:", err)
		return 1
	}
	if cfg.Sessions.RetentionDays <= 0 {
		fmt.Fprintln(stdout, "Retention is off (sessions.retention_days = 0); nothing is pruned.")
	}
	fmt.Fprintf(stdout, "Removed %d finished session(s).\n", len(rep.Removed))
	for _, k := range rep.Kept {
		fmt.Fprintf(stdout, "Kept %s  %s: %s\n", k.ID, k.Title, k.Reason)
	}
	for _, e := range rep.Errors {
		fmt.Fprintln(stdout, "Error:", e)
	}
	if len(rep.Errors) > 0 {
		return 1
	}
	return 0
}
