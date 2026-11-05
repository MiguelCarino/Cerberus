package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"carino.systems/cerberus/internal/bench"
	"carino.systems/cerberus/internal/platform"
)

// benchCmd is `cerberus bench` (gateway phase G6): run the stable bench
// through the gateway, list runs, or compare two.
func benchCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "compare":
			return benchCompare(args[1:], stdout, stderr)
		case "list":
			return benchList(args[1:], stdout, stderr)
		}
	}
	var o bench.Options
	var kinds string
	var force, asJSON bool
	cfg, pos, code := clientCmd("bench", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&o.Model, "model", "", "model id (default: engines.default_model)")
		fs.IntVar(&o.Repeat, "repeat", 3, "runs of each prompt")
		fs.StringVar(&kinds, "kinds", "", "comma-separated kinds ("+strings.Join(bench.Kinds, ",")+"; default all)")
		fs.BoolVar(&o.Prefill, "prefill", false, "also time 8k, 32k and ctx/2 prompts")
		fs.StringVar(&o.Label, "label", "", "a note saved with the run (e.g. \"power cap 300 W\")")
		fs.BoolVar(&o.Thinking, "thinking", false, "leave the model's thinking on")
		fs.BoolVar(&force, "force", false, "run even while agent sessions are running")
		fs.BoolVar(&asJSON, "json", false, "print the run as JSON")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: cerberus bench [flags] | bench list | bench compare A B")
		return 2
	}
	if kinds != "" {
		for _, k := range strings.Split(kinds, ",") {
			if k = strings.TrimSpace(k); k != "" {
				o.Kinds = append(o.Kinds, k)
			}
		}
	}
	if o.Model == "" {
		o.Model = cfg.Engines.DefaultModel
	}
	if o.Model == "" {
		fmt.Fprintln(stderr, "cerberus bench: no model: pass --model or set engines.default_model")
		return 2
	}
	dir, err := benchDir()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := bench.Client{Base: localURL(cfg.Server.Listen), Token: cfg.Server.Token, HTTP: &http.Client{Timeout: 30 * time.Minute}}
	if !force {
		busy, err := c.Busy(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus bench: is the daemon running?", err)
			return 1
		}
		if busy {
			fmt.Fprintln(stderr, "cerberus bench:", bench.ErrBusy)
			return 1
		}
	}
	r := bench.NewRun(cfg, o, version)
	set, err := bench.Load()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	err = bench.Execute(ctx, c, set, r.Options, r, func(s string) { fmt.Fprintln(stderr, "·", s) })
	r.Done = time.Now()
	r.State = "done"
	if err != nil {
		r.State, r.Error = "failed", err.Error()
	}
	if serr := bench.Save(dir, r); serr != nil {
		fmt.Fprintln(stderr, "cerberus bench: saving:", serr)
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
	} else {
		printRun(stdout, r)
		fmt.Fprintf(stdout, "\nSaved as %s (cerberus bench compare %s OTHER).\n", r.ID, r.ID)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	return 0
}

func benchDir() (string, error) {
	data, err := platform.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "bench"), nil
}

func printRun(w io.Writer, r *bench.Run) {
	fmt.Fprintf(w, "Bench %s · %s · %s\n", r.ID, r.Options.Model, r.State)
	if r.Options.Label != "" {
		fmt.Fprintln(w, "Label:", r.Options.Label)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tTTFT\tDECODE\tPREFILL\tTOK/J\tERRORS")
	for _, k := range r.Kinds {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", k.Kind, fmtMsF(k.Median.TTFTMs), fmtRate(k.Median.DecodeTPS),
			fmtRate(k.Median.PrefillTPS), fmtF(k.Median.TokPerJ), k.Errors)
	}
	tw.Flush()
	if r.Error != "" {
		fmt.Fprintln(w, "Error:", r.Error)
	}
}

func fmtMsF(ms float64) string {
	if ms <= 0 {
		return "–"
	}
	if ms >= 1000 {
		return fmt.Sprintf("%.2f s", ms/1000)
	}
	return fmt.Sprintf("%.0f ms", ms)
}

func fmtRate(v float64) string {
	if v <= 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f tok/s", v)
}

func fmtF(v float64) string {
	if v <= 0 {
		return "–"
	}
	return fmt.Sprintf("%.2f", v)
}

func fmtPct(p float64) string {
	if p == 0 {
		return "–"
	}
	return fmt.Sprintf("%+.0f%%", p)
}

func benchList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	if _, err := parseAnywhere(fs, args); err != nil {
		return 2
	}
	dir, err := benchDir()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	runs, err := bench.List(dir)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No bench runs yet: cerberus bench --model ID")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODEL\tSTATE\tLABEL")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Options.Model, r.State, r.Options.Label)
	}
	tw.Flush()
	return 0
}

func benchCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "usage: cerberus bench compare A B (run ids or .json files)")
		return 2
	}
	dir, err := benchDir()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	a, err := bench.Get(dir, pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	b, err := bench.Get(dir, pos[1])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus bench:", err)
		return 1
	}
	rows := bench.Compare(a, b)
	fmt.Fprintf(stdout, "A %s (%s)\nB %s (%s)\n\n", a.ID, orStr(a.Options.Label, a.Options.Model), b.ID, orStr(b.Options.Label, b.Options.Model))
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tDECODE A\tDECODE B\tΔ\tTTFT A\tTTFT B\tΔ\tTOK/J Δ")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Kind, fmtRate(r.A.DecodeTPS), fmtRate(r.B.DecodeTPS), fmtPct(r.DecodePct),
			fmtMsF(r.A.TTFTMs), fmtMsF(r.B.TTFTMs), fmtPct(r.TTFTPct), fmtPct(r.TokJPct))
	}
	tw.Flush()
	fmt.Fprintln(stdout, "\n"+bench.Summary(a, b, rows))
	return 0
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
