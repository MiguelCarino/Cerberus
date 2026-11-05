package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"

	"carino.systems/cerberus/internal/session"
)

// fleetCmd: cerberus fleet --has FILE [--name GLOB] [--yes] [--agent …] "task"
//
// It lists the matching repos and asks before starting one session per repo,
// unless --yes. The task may use {repo} (path) and {name} (folder name).
func fleetCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var has, name string
	var yes bool
	var spec session.BatchSpec
	cfg, pos, code := clientCmd("fleet", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&has, "has", "", `only repos containing this file (a glob relative to the repo root, e.g. "carino-navbar.js")`)
		fs.StringVar(&name, "name", "", "only repos whose folder name matches this glob")
		fs.BoolVar(&yes, "yes", false, "start without asking")
		fs.StringVar(&spec.Agent, "agent", "", "agent profile (default from config)")
		fs.StringVar(&spec.Model, "model", "", "model id for agents that take one")
		fs.StringVar(&spec.EscalateTo, "escalate", "", "agent profile to hand over to when checks keep failing")
		fs.IntVar(&spec.MaxAttempts, "max-attempts", 0, "attempts before escalating (default from config)")
		fs.StringVar(&spec.Title, "title", "", "short title for the fleet task")
	})
	if cfg == nil {
		return code
	}
	spec.Task = strings.TrimSpace(strings.Join(pos, " "))
	if spec.Task == "" || (has == "" && name == "") {
		fmt.Fprintln(stderr, `usage: cerberus fleet --has FILE [--name GLOB] [--yes] [--agent A] [--model M] [--escalate P] [--title T] "task (may use {repo} {name})"`)
		return 2
	}
	c := newClient(cfg)
	q := url.Values{}
	if has != "" {
		q.Set("has", has)
	}
	if name != "" {
		q.Set("name", name)
	}
	var repos []session.RepoMatch
	if err := c.do(context.Background(), "GET", "/api/repos/find?"+q.Encode(), nil, &repos); err != nil {
		fmt.Fprintln(stderr, "cerberus fleet:", err)
		return 1
	}
	if len(repos) == 0 {
		fmt.Fprintln(stderr, "cerberus fleet: no repos match")
		return 1
	}
	fmt.Fprintf(stdout, "%d repo(s) match:\n", len(repos))
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	dirty := 0
	for _, r := range repos {
		note := ""
		if r.Dirty {
			note = "(dirty)"
			dirty++
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Name, r.Path, note)
	}
	tw.Flush()
	if dirty > 0 {
		fmt.Fprintf(stdout, "%d repo(s) have uncommitted changes in the main checkout: accepting into them may fail.\n", dirty)
	}
	if !yes && !confirm(stdin, stdout, fmt.Sprintf("Start %d session(s)?", len(repos))) {
		fmt.Fprintln(stderr, "cerberus fleet: cancelled (pass --yes to skip the question)")
		return 1
	}
	for _, r := range repos {
		spec.Repos = append(spec.Repos, r.Path)
	}
	c.http.Timeout = longCallTimeout
	var view struct {
		Batch    session.BatchInfo `json:"batch"`
		Sessions []session.Info    `json:"sessions"`
	}
	if err := c.do(context.Background(), "POST", "/api/batches", spec, &view); err != nil {
		fmt.Fprintln(stderr, "cerberus fleet:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Fleet task %s: %d session(s) queued\n", view.Batch.ID, len(view.Sessions))
	for _, in := range view.Sessions {
		fmt.Fprintf(stdout, "  %s  %-8s %s\n    %s/#/s/%s\n", in.ID, in.State, in.Repo, c.base, url.PathEscape(in.ID))
	}
	return 0
}
