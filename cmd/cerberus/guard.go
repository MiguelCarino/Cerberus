package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"text/tabwriter"
	"time"

	"carino.systems/cerberus/internal/guardbench"
	"carino.systems/cerberus/internal/platform"
)

// guardCmd is `cerberus guard`: the agent-guard red-team suite (evals phase
// E5). `guard bench` starts a run on the daemon (which drives the cases
// through the G2 guard classifier), waits for it and prints the rates;
// `guard bench compare A B` diffs two saved runs.
func guardCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		printHelp(stdout, "guard")
		return 0
	}
	if len(args) == 0 || args[0] != "bench" {
		fmt.Fprintln(stderr, "usage: cerberus guard bench [--model ID] [--threshold N] | guard bench compare A B")
		return 2
	}
	rest := args[1:]
	if len(rest) > 0 {
		switch rest[0] {
		case "compare":
			return guardBenchCompare(rest[1:], stdout, stderr)
		case "list":
			return guardBenchList(rest[1:], stdout, stderr)
		}
	}
	return guardBenchRun(rest, stdout, stderr)
}

func guardbenchDir() (string, error) {
	data, err := platform.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "guardbench"), nil
}

func guardBenchRun(args []string, stdout, stderr io.Writer) int {
	var model string
	var threshold float64
	cfg, pos, code := clientCmd("guard bench", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&model, "model", "", "guard classifier model id (default: gateway.guard.model)")
		fs.Float64Var(&threshold, "threshold", 0, "flag score 0..1 (default: gateway.guard.threshold, else 0.5)")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: cerberus guard bench [--model ID] [--threshold N]")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := newClient(cfg)
	var started struct {
		ID string `json:"id"`
	}
	in := guardbench.Options{Model: model, Threshold: threshold}
	if err := c.do(ctx, "POST", "/api/guardbench", in, &started); err != nil {
		fmt.Fprintln(stderr, "cerberus guard bench:", err)
		return 1
	}
	fmt.Fprintf(stderr, "· started %s (loading the guard model is the owner's step; this needs it loaded)\n", started.ID)
	// Poll until the run is done.
	for {
		var list guardbenchList
		if err := c.do(ctx, "GET", "/api/guardbench", nil, &list); err != nil {
			fmt.Fprintln(stderr, "cerberus guard bench:", err)
			return 1
		}
		run := list.find(started.ID)
		if run == nil {
			fmt.Fprintln(stderr, "cerberus guard bench: the run vanished")
			return 1
		}
		if run.State == "running" {
			if run.Progress != "" {
				fmt.Fprintf(stderr, "\r· %s        ", run.Progress)
			}
			select {
			case <-ctx.Done():
				fmt.Fprintln(stderr, "\n(it keeps running on the daemon; see cerberus guard bench list)")
				return 1
			case <-time.After(time.Second):
			}
			continue
		}
		fmt.Fprintln(stderr)
		printGuardRun(stdout, run)
		if run.State == "failed" {
			fmt.Fprintln(stderr, "cerberus guard bench:", run.Error)
			return 1
		}
		fmt.Fprintf(stdout, "\nSaved as %s (cerberus guard bench compare %s OTHER).\n", run.ID, run.ID)
		return 0
	}
}

type guardbenchList struct {
	Runs    []guardbench.Run `json:"runs"`
	Running *guardbench.Run  `json:"running"`
}

func (l guardbenchList) find(id string) *guardbench.Run {
	if l.Running != nil && l.Running.ID == id {
		return l.Running
	}
	for i := range l.Runs {
		if l.Runs[i].ID == id {
			return &l.Runs[i]
		}
	}
	return nil
}

func printGuardRun(w io.Writer, r *guardbench.Run) {
	fmt.Fprintf(w, "Guard red-team %s · model %s · threshold %.2f · %s\n", r.ID, orModelName(r.Model), r.Threshold, r.State)
	fmt.Fprintf(w, "Detection %s on %d attacks · false alarms %s on %d benign",
		fmtPctF(r.Detection), r.Attacks, fmtPctF(r.FalseAlarm), r.Benign)
	if r.Errors > 0 {
		fmt.Fprintf(w, " · %d case(s) errored (is the guard model loaded?)", r.Errors)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tKIND\tCASES\tFLAGGED\tRATE\tERRORS")
	for _, c := range r.Categories {
		kind, rate := "attack", "detection"
		if c.Benign {
			kind, rate = "benign", "false alarm"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s (%s)\t%d\n", c.Category, kind, c.Total, c.Flagged, fmtPctF(c.Rate), rate, c.Errors)
	}
	tw.Flush()
	if r.HasScores && len(r.Sweep) > 0 {
		fmt.Fprintln(w, "\nThreshold sweep (detection / false alarm):")
		stw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(stw, "THRESHOLD\tDETECTION\tFALSE ALARM")
		for _, p := range r.Sweep {
			fmt.Fprintf(stw, "%.1f\t%s\t%s\n", p.Threshold, fmtPctF(p.Detection), fmtPctF(p.FalseAlarm))
		}
		stw.Flush()
	} else {
		fmt.Fprintln(w, "\n(the classifier gave only pass/fail scores, so there is no threshold to sweep)")
	}
	if len(r.Misses) > 0 {
		fmt.Fprintf(w, "\nMisses (%d): %s\n", len(r.Misses), joinClip(r.Misses, 20))
	}
}

func joinClip(ss []string, n int) string {
	if len(ss) <= n {
		return join(ss)
	}
	return join(ss[:n]) + fmt.Sprintf(" … (+%d more)", len(ss)-n)
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func orModelName(m string) string {
	if m == "" {
		return "(gateway.guard.model)"
	}
	return m
}

func fmtPctF(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func guardBenchList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("guard bench list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	if _, err := parseAnywhere(fs, args); err != nil {
		return 2
	}
	dir, err := guardbenchDir()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus guard bench:", err)
		return 1
	}
	runs, err := guardbench.List(dir)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus guard bench:", err)
		return 1
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No guard red-team runs yet: cerberus guard bench")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODEL\tSTATE\tDETECTION\tFALSE ALARM")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, orModelName(r.Model), r.State, fmtPctF(r.Detection), fmtPctF(r.FalseAlarm))
	}
	tw.Flush()
	return 0
}

func guardBenchCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("guard bench compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "usage: cerberus guard bench compare A B (run ids or .json files)")
		return 2
	}
	dir, err := guardbenchDir()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus guard bench:", err)
		return 1
	}
	a, err := guardbench.Get(dir, pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus guard bench:", err)
		return 1
	}
	b, err := guardbench.Get(dir, pos[1])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus guard bench:", err)
		return 1
	}
	fmt.Fprintf(stdout, "A %s (%s, threshold %.2f)\nB %s (%s, threshold %.2f)\n\n",
		a.ID, orModelName(a.Model), a.Threshold, b.ID, orModelName(b.Model), b.Threshold)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tKIND\tA\tB\tΔ")
	for _, d := range guardbench.Compare(a, b) {
		kind := "attack"
		if d.Benign {
			kind = "benign"
		}
		av, bv := fmtPctF(d.A), fmtPctF(d.B)
		if d.OnlyB {
			av = "–"
		}
		if d.OnlyA {
			bv = "–"
		}
		delta := "–"
		if !d.OnlyA && !d.OnlyB {
			delta = fmt.Sprintf("%+.0f pts", d.Delta*100)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Category, kind, av, bv, delta)
	}
	tw.Flush()
	fmt.Fprintln(stdout, "\n"+guardbench.Summary(a, b))
	return 0
}
