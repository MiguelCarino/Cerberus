package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/platform"
)

// finetuneCmd: cerberus finetune stats | export | train. It reads the
// example files directly; the daemon need not run.
func finetuneCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "stats":
		return finetuneStats(args, stdout, stderr)
	case "export":
		return finetuneExport(args, stdout, stderr)
	case "train":
		return finetuneTrain(args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "cerberus finetune: unknown command %q (stats, export, train)\n", sub)
	return 2
}

// finetuneStore opens the store named by the config at path.
func finetuneStore(path string) (*config.Config, *finetune.Store, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	data, err := platform.DataDir()
	if err != nil {
		return nil, nil, err
	}
	ft, err := openFinetune(cfg, data, nil) // already masked when written
	return cfg, ft, err
}

func finetuneStats(args []string, stdout, stderr io.Writer) int {
	path, _, ok := configFlags("finetune stats", args, stderr, nil)
	if !ok {
		return 2
	}
	cfg, ft, err := finetuneStore(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	st, err := ft.Stats()
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	capture := cfg.Finetune.Capture
	if capture == "" {
		capture = config.CaptureOff
	}
	fmt.Fprintf(stdout, "Collecting:   %s (finetune.capture)\n", capture)
	fmt.Fprintf(stdout, "Folder:       %s\n", st.Dir)
	fmt.Fprintf(stdout, "Examples:     %d (delegated %d, cloud %d)\n", st.Examples, st.ByKind[finetune.KindDelegated], st.ByKind[finetune.KindCloud])
	fmt.Fprintf(stdout, "Rated:        %d (good %d, corrected %d)\n", st.Rated, st.Good, st.Corrected)
	fmt.Fprintf(stdout, "Ready to use: %d rows\n", st.Usable)
	if st.Oldest != nil {
		fmt.Fprintf(stdout, "From:         %s to %s\n", st.Oldest.Local().Format("2006-01-02"), st.Newest.Local().Format("2006-01-02"))
	}
	if capture == config.CaptureOff && st.Examples == 0 {
		fmt.Fprintln(stdout, "\nNothing is collected yet. Turn it on with: cerberus config set finetune.capture delegated")
	}
	return 0
}

func finetuneExport(args []string, stdout, stderr io.Writer) int {
	var out, kind *string
	var unrated *bool
	var minChars *int
	path, _, ok := configFlags("finetune export", args, stderr, func(set *flag.FlagSet) {
		out = set.String("out", "", "write to this file (default: standard output)")
		kind = set.String("kind", "", "only delegated or cloud examples")
		unrated = set.Bool("unrated", false, "also delegated answers nobody rated")
		minChars = set.Int("min-chars", 0, "skip answers shorter than this")
	})
	if !ok {
		return 2
	}
	o := finetune.ExportOptions{Unrated: *unrated, MinChars: *minChars}
	switch *kind {
	case "":
	case finetune.KindDelegated, finetune.KindCloud:
		o.Kinds = []string{*kind}
	default:
		fmt.Fprintln(stderr, "cerberus finetune export: --kind is delegated or cloud")
		return 2
	}
	_, ft, err := finetuneStore(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	w := stdout
	var f *os.File
	if *out != "" {
		if f, err = os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			fmt.Fprintln(stderr, "cerberus finetune:", err)
			return 1
		}
		w = f
	}
	n, err := ft.Export(w, o)
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	if *out != "" {
		fmt.Fprintf(stdout, "Wrote %d rows to %s\n", n, *out)
	}
	return 0
}

// finetuneTrain exports the examples and runs finetune.train_command with
// {dataset}, {base_model} and {out} filled in. Cerberus ships no trainer:
// the command is whatever the owner set up (Unsloth, axolotl, llama.cpp's
// finetune, an MLX script, …).
func finetuneTrain(args []string, stdout, stderr io.Writer) int {
	var outDir, base *string
	var dry, unrated *bool
	path, _, ok := configFlags("finetune train", args, stderr, func(set *flag.FlagSet) {
		outDir = set.String("out", "", "folder for the trained model (default: DATA/finetune/runs/<time>)")
		base = set.String("base", "", "base model (default: finetune.base_model)")
		dry = set.Bool("dry-run", false, "print the command and export the dataset, but do not run it")
		unrated = set.Bool("unrated", false, "also use delegated answers nobody rated")
	})
	if !ok {
		return 2
	}
	cfg, ft, err := finetuneStore(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	if len(cfg.Finetune.TrainCommand) == 0 {
		fmt.Fprintln(stderr, "cerberus finetune train: no trainer is set up. Set finetune.train_command to your trainer's\n"+
			"command line, with {dataset}, {base_model} and {out} where they go, for example:\n"+
			`  cerberus config set finetune.train_command '["python3","~/train.py","--data","{dataset}","--base","{base_model}","--out","{out}"]'`)
		return 2
	}
	model := strings.TrimSpace(*base)
	if model == "" {
		model = cfg.Finetune.BaseModel
	}
	run := *outDir
	reserved := false // run is a fresh DATA/finetune/runs/<id> folder made here
	if run == "" {
		// The same exclusive folder the server's Train now makes: a run
		// started in the same second never shares (or truncates) its folder.
		_, run, err = ft.ReserveRunDir()
		reserved = err == nil
	} else {
		err = os.MkdirAll(run, 0o700)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	dataset := filepath.Join(run, "dataset.jsonl")
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if reserved {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(dataset, flags, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	n, err := ft.Export(f, finetune.ExportOptions{Unrated: *unrated})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if (err != nil || n == 0) && reserved {
		_ = os.RemoveAll(run) // leave no empty run behind
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus finetune:", err)
		return 1
	}
	if n == 0 {
		fmt.Fprintln(stderr, "cerberus finetune train: no usable examples yet (see cerberus finetune stats)")
		return 1
	}
	argv := finetune.Expand(cfg.Finetune.TrainCommand, dataset, model, run)
	for i, a := range argv {
		argv[i] = config.ExpandHome(a)
	}
	fmt.Fprintf(stdout, "Dataset: %d rows in %s\n", n, dataset)
	fmt.Fprintf(stdout, "Command: %s\n", strings.Join(argv, " "))
	if *dry {
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = run
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fmt.Fprintf(stderr, "cerberus finetune train: the trainer exited with %d\n", ee.ExitCode())
			return 1
		}
		fmt.Fprintln(stderr, "cerberus finetune train:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Done. Output in %s\n", run)
	return 0
}
