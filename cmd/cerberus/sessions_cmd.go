package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"text/tabwriter"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/session"
)

// sessionsCmd is `cerberus sessions SUB`: per-session reads that do not
// fit task/search/resume: setup (docs/SCOPE.md "E3 Setup snapshot") and
// trajectory ("E4 Trajectory export (ATIF)").
func sessionsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		printHelp(stdout, "sessions")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "setup":
		return sessionsSetup(args[1:], stdout, stderr)
	case "trajectory":
		return sessionsTrajectory(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "cerberus sessions: unknown subcommand %q\n\n", args[0])
	printHelp(stderr, "sessions")
	return 2
}

// sessionsSetup prints one session's setup snapshot, or with two ids the
// fields that differ.
func sessionsSetup(args []string, stdout, stderr io.Writer) int {
	var asJSON bool
	cfg, pos, code := clientCmd("sessions setup", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print JSON")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 1 && len(pos) != 2 {
		fmt.Fprintln(stderr, "usage: cerberus sessions setup ID [OTHER_ID]")
		return 2
	}
	views := make([]session.SetupView, len(pos))
	for i, id := range pos {
		v, err := fetchSetup(cfg, id)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus sessions setup:", err)
			return 1
		}
		views[i] = v
	}
	if len(views) == 1 {
		return printSetup(stdout, pos[0], views[0], asJSON)
	}
	return printSetupDiff(stdout, pos, views, asJSON)
}

func fetchSetup(cfg *config.Config, id string) (session.SetupView, error) {
	var v session.SetupView
	err := newClient(cfg).do(context.Background(), "GET", "/api/sessions/"+url.PathEscape(id)+"/setup", nil, &v)
	return v, err
}

func printSetup(w io.Writer, id string, v session.SetupView, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return 0
	}
	fmt.Fprintf(w, "Setup of %s · %s%s\n", id, v.Hash, verifiedNote(v))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, f := range v.Setup.Fields() {
		if f.Value == "" || f.Value == "false" {
			continue // empty and false fields say nothing
		}
		fmt.Fprintf(tw, "%s\t%s\n", f.Field, f.Value)
	}
	tw.Flush()
	return 0
}

func printSetupDiff(w io.Writer, ids []string, views []session.SetupView, asJSON bool) int {
	diff := session.DiffSetups(views[0].Setup, views[1].Setup)
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"a": ids[0], "b": ids[1], "hash_a": views[0].Hash, "hash_b": views[1].Hash,
			"differ": nonNilDiff(diff)})
		return 0
	}
	fmt.Fprintf(w, "A %s · %s%s\nB %s · %s%s\n", ids[0], views[0].Hash, verifiedNote(views[0]),
		ids[1], views[1].Hash, verifiedNote(views[1]))
	if len(diff) == 0 {
		fmt.Fprintln(w, "\nThe same setup.")
		return 0
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tA\tB")
	for _, d := range diff {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", d.Field, orDash(d.A), orDash(d.B))
	}
	tw.Flush()
	return 0
}

func nonNilDiff(d []session.SetupDiff) []session.SetupDiff {
	if d == nil {
		return []session.SetupDiff{}
	}
	return d
}

// verifiedNote flags a setup.json whose hash no longer matches the one
// recorded when the run started (the file was changed afterwards).
func verifiedNote(v session.SetupView) string {
	if v.Verified {
		return ""
	}
	if v.Recorded == "" {
		return " (no hash recorded)"
	}
	return " (changed since the run started: recorded " + v.Recorded + ")"
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// sessionsTrajectory writes a session's trajectory to stdout: ATIF JSON
// built from its request records, or with --format jsonl the records' own
// lines.
func sessionsTrajectory(args []string, stdout, stderr io.Writer) int {
	format := "atif"
	cfg, pos, code := clientCmd("sessions trajectory", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&format, "format", "atif", "atif (ATIF trajectory JSON) or jsonl (the raw request records)")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus sessions trajectory ID [--format atif|jsonl] > FILE")
		return 2
	}
	if format != "atif" && format != "jsonl" {
		fmt.Fprintln(stderr, "cerberus sessions trajectory: --format must be atif or jsonl")
		return 2
	}
	path := "/api/sessions/" + url.PathEscape(pos[0]) + "/trajectory?format=" + format
	if err := newClient(cfg).download(context.Background(), path, stdout); err != nil {
		fmt.Fprintln(stderr, "cerberus sessions trajectory:", err)
		return 1
	}
	return 0
}
