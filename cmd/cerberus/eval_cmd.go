package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/evals"
	"carino.systems/cerberus/internal/session"
)

// evalCmd: cerberus eval list | show ID | validate FILE | import FILE |
// export ID | run ID [--wait] [--setup LABEL] | runs [ID] | report RUN |
// compare RUN_A RUN_B | cancel RUN | rejudge RUN. Everything goes through
// the daemon (/api/evals), except that validate falls back to the
// structure checks when the daemon is not running.
func evalCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if sub == "" && len(args) > 0 && isHelpFlag(args[0]) {
		printHelp(stdout, "eval")
		return 0
	}
	switch sub {
	case "", "list", "ls":
		return evalList(args, stdout, stderr)
	case "show":
		return evalShow(args, stdout, stderr)
	case "validate":
		return evalValidate(args, stdout, stderr)
	case "import":
		return evalImport(args, stdout, stderr)
	case "export":
		return evalExport(args, stdout, stderr)
	case "run":
		return evalRun(args, stdout, stderr)
	case "runs":
		return evalRuns(args, stdout, stderr)
	case "report":
		return evalReport(args, stdout, stderr)
	case "compare":
		return evalCompare(args, stdout, stderr)
	case "cancel", "rejudge":
		return evalRunAction(sub, args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "cerberus eval: unknown command %q (list, show, validate, import, export, run, runs, report, compare, cancel, rejudge)\n", sub)
	return 2
}

func evalList(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("eval list", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: cerberus eval list")
		return 2
	}
	var list []api.EvalSummary
	if err := newClient(cfg).do(context.Background(), "GET", "/api/evals", nil, &list); err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "No evals yet: cerberus eval import FILE (a JSON definition), or Usage → Evals in the web UI.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCRITERIA\tSETUPS\tLAST RUN\tNAME")
	for _, e := range list {
		last := "never run"
		if e.LastRun != nil {
			last = fmt.Sprintf("%s %s (%d/%d judged)", e.LastRun.State, flowWhen(e.LastRun.Created), e.LastRun.Judged, e.LastRun.Sessions)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n", e.ID, len(e.Criteria), len(e.Setups), last, flowClipText(e.Name, 50))
	}
	tw.Flush()
	return 0
}

func printEvalProblems(w io.Writer, ps []evals.Problem) {
	for _, p := range ps {
		where := p.Field
		if where == "" {
			where = "eval"
		}
		fmt.Fprintf(w, "%s: %s: %s\n", p.Severity, clean(where), clean(p.Message))
	}
}

func evalShow(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("eval show", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus eval show ID")
		return 2
	}
	c := newClient(cfg)
	var v api.EvalView
	if err := c.do(context.Background(), "GET", "/api/evals/"+url.PathEscape(pos[0]), nil, &v); err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	e := v.Eval
	fmt.Fprintf(stdout, "%s  %s (version %d)\n", e.ID, clean(e.Name), e.Version)
	ws := e.Workspace
	if e.Workspace == evals.WorkspaceRepo {
		ws += " " + clean(e.Repo)
		if e.Base != "" {
			ws += " @ " + e.Base
		}
	}
	fmt.Fprintf(stdout, "Workspace: %s   Repeat: %d   Judge: %s\n", ws, e.Repeat, orStr(v.JudgeModel, "(none: set evals.judge_model)"))
	fmt.Fprintf(stdout, "Task: %s\n", flowClipText(e.Task, 200))
	for _, g := range v.Gates {
		fmt.Fprintf(stdout, "Gate %s (%d iteration(s)):\n", g.ID, g.MaxIterations)
		for _, cr := range e.Criteria {
			if evals.GateOf(e, cr) != g.ID {
				continue
			}
			deps := ""
			if len(cr.DependsOn) > 0 {
				deps = "  needs " + strings.Join(cr.DependsOn, ", ")
			}
			fmt.Fprintf(stdout, "  %-20s %s%s\n", cr.ID, flowClipText(cr.Prompt, 80), deps)
		}
	}
	fmt.Fprintln(stdout, "Setups:")
	for _, s := range e.Setups {
		fmt.Fprintf(stdout, "  %-20s %s %s\n", clean(s.Label), clean(s.Agent), clean(s.Model))
	}
	printEvalProblems(stdout, v.Problems)
	fmt.Fprintf(stdout, "  %s/#/usage (Evals)\n", c.base)
	return 0
}

// readEvalFile reads a definition from a file ("-" = stdin).
func readEvalFile(path string) (evals.Eval, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return evals.Eval{}, err
	}
	if len(raw) > 1<<20 {
		return evals.Eval{}, errors.New("the file is over 1 MiB")
	}
	var e evals.Eval
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return evals.Eval{}, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

func evalValidate(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("eval validate", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus eval validate FILE")
		return 2
	}
	e, err := readEvalFile(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	var out struct {
		Problems []evals.Problem `json:"problems"`
		Order    [][]string      `json:"order"`
	}
	err = newClient(cfg).do(context.Background(), "POST", "/api/evals/validate", e, &out)
	var ue *unreachableError
	if errors.As(err, &ue) {
		fmt.Fprintln(stderr, "The daemon is not running: structure only (agents and models are not checked).")
		out.Problems = evals.Validate(e, nil)
		err = nil
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	printEvalProblems(stdout, out.Problems)
	if evals.HasErrors(out.Problems) {
		return 1
	}
	fmt.Fprintln(stdout, "Valid.")
	return 0
}

func evalImport(args []string, stdout, stderr io.Writer) int {
	var replace bool
	cfg, pos, code := clientCmd("eval import", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&replace, "replace", false, "replace the eval with the same id when there is one")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus eval import [--replace] FILE")
		return 2
	}
	e, err := readEvalFile(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	c := newClient(cfg)
	ctx := context.Background()
	var v api.EvalView
	if e.ID != "" && evals.ValidID(e.ID) {
		var cur api.EvalView
		if c.do(ctx, "GET", "/api/evals/"+url.PathEscape(e.ID), nil, &cur) == nil {
			if !replace {
				fmt.Fprintf(stderr, "cerberus eval: %s exists (version %d): pass --replace to replace it\n", e.ID, cur.Eval.Version)
				return 1
			}
			e.Version = cur.Eval.Version
			if err := c.do(ctx, "PUT", "/api/evals/"+url.PathEscape(e.ID), e, &v); err != nil {
				fmt.Fprintln(stderr, "cerberus eval:", err)
				return 1
			}
			fmt.Fprintf(stdout, "Replaced %s (version %d)\n", v.Eval.ID, v.Eval.Version)
			printEvalProblems(stdout, v.Problems)
			return 0
		}
	}
	if err := c.do(ctx, "POST", "/api/evals", e, &v); err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Imported %s (version %d)\n", v.Eval.ID, v.Eval.Version)
	printEvalProblems(stdout, v.Problems)
	return 0
}

func evalExport(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("eval export", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus eval export ID > file.json")
		return 2
	}
	var v api.EvalView
	if err := newClient(cfg).do(context.Background(), "GET", "/api/evals/"+url.PathEscape(pos[0]), nil, &v); err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	e := v.Eval
	// A definition, portable: no version, time or owner.
	e.Version, e.Updated, e.Owner, e.OwnerName = 0, time.Time{}, "", ""
	b, _ := json.MarshalIndent(exportShape(e), "", "  ")
	fmt.Fprintln(stdout, string(b))
	return 0
}

// exportShape drops the stored-only fields from an export.
func exportShape(e evals.Eval) map[string]any {
	raw, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "version")
	delete(m, "updated")
	return m
}

func evalRun(args []string, stdout, stderr io.Writer) int {
	var setup string
	var wait bool
	cfg, pos, code := clientCmd("eval run", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&setup, "setup", "", "run only the setup with this label")
		fs.BoolVar(&wait, "wait", false, "follow the run until every session is judged, then print the report")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus eval run ID [--setup LABEL] [--wait]")
		return 2
	}
	c := newClient(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var events <-chan evalFrame
	if wait {
		var err error
		if events, err = followEvals(ctx, c); err != nil {
			fmt.Fprintln(stderr, "cerberus eval:", err)
			return 1
		}
	}
	var run evals.Run
	if err := c.do(ctx, "POST", "/api/evals/"+url.PathEscape(pos[0])+"/run", map[string]string{"setup": setup}, &run); err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Started %s: %d session(s), judge %s\n  %s/#/evals/runs/%s\n", run.ID, len(run.Slots), run.JudgeModel,
		c.base, url.PathEscape(run.ID))
	if !wait {
		return 0
	}
	d, err := followEvalRun(ctx, c, run.ID, events, stdout)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(stderr, "Stopped following; the run goes on: cerberus eval report %s\n", run.ID)
		} else {
			fmt.Fprintln(stderr, "cerberus eval:", err)
		}
		return 1
	}
	printEvalReport(stdout, d)
	if d.Run.State != evals.RunDone {
		return 1
	}
	return 0
}

// evalFrame is one relevant event-stream frame: a run event, or a resync.
type evalFrame struct {
	Event  evals.Event
	Resync bool
}

// followEvals opens GET /api/events and passes on evalrun events and
// evals resyncs until ctx ends or the stream breaks.
func followEvals(ctx context.Context, c *client) (<-chan evalFrame, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cerberus-Actor", audit.ActorCLI)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, &unreachableError{base: c.base, err: err}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return nil, fmt.Errorf("GET /api/events: %s", errorText(resp.Status, raw))
	}
	out := make(chan evalFrame, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		readSSE(resp.Body, func(event, data string) bool {
			var fr evalFrame
			switch event {
			case evals.EventType:
				if json.Unmarshal([]byte(data), &fr.Event) != nil {
					return true
				}
			case "resync":
				if !strings.Contains(data, `"evals"`) {
					return true
				}
				fr.Resync = true
			default:
				return true
			}
			select {
			case out <- fr:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return out, nil
}

// followEvalRun prints session changes until the run ends.
func followEvalRun(ctx context.Context, c *client, id string, events <-chan evalFrame, stdout io.Writer) (evals.RunDetail, error) {
	seen := map[string]string{}
	get := func() (evals.RunDetail, error) {
		var d evals.RunDetail
		err := c.do(ctx, "GET", "/api/evals/runs/"+url.PathEscape(id), nil, &d)
		return d, err
	}
	d, err := get()
	for err == nil && !evals.Final(d.Run.State) {
		for _, s := range d.Run.Slots {
			key := s.Setup + "#" + strconv.Itoa(s.Repeat)
			if seen[key] != s.State {
				seen[key] = s.State
				fmt.Fprintf(stdout, "  %-24s %-8s %s\n", flowClipText(key, 24), s.State, s.Session)
			}
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case fr, open := <-events:
			if !open {
				return d, errors.New("the event stream ended (did the daemon stop?)")
			}
			if !fr.Resync && fr.Event.Run != id {
				continue
			}
			d, err = get()
		}
	}
	return d, err
}

func stateMark(s string) string {
	switch s {
	case evals.StatePass:
		return "✓"
	case evals.StateFail:
		return "✗"
	case evals.StateSkipped:
		return "–"
	case evals.StateError:
		return "!"
	}
	return "·"
}

// printEvalReport prints a run: the ranking, then per session the
// criteria with their marks, and the rationale of each failure.
func printEvalReport(w io.Writer, d evals.RunDetail) {
	r := d.Run
	fmt.Fprintf(w, "%s  %s (v%d) · %s · judge %s\n", r.ID, clean(r.EvalName), r.EvalVersion, r.State, r.JudgeModel)
	if r.Reason != "" {
		fmt.Fprintf(w, "  %s\n", cleanText(r.Reason))
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RANK\tSETUP\tGATES\tCRITERIA\tTIME\tTOKENS\tJUDGE TOKENS")
	for _, k := range d.Summary {
		if k.Reports == 0 {
			fmt.Fprintf(tw, "-\t%s\t-\t-\t-\t-\t-\n", clean(k.Setup))
			continue
		}
		fmt.Fprintf(tw, "%d\t%s\t%d/%d\t%d/%d\t%s\t%d\t%d\n", k.Rank, clean(k.Setup), k.GatesPassed, k.GatesTotal,
			k.CriteriaPassed, k.CriteriaTotal, fmtMsF(float64(k.WallMs)), k.Session.Tokens, k.Judge.Tokens)
	}
	tw.Flush()
	for _, rep := range d.Reports {
		fmt.Fprintf(w, "\n%s #%d · %s · setup %s · %s\n", clean(rep.Setup), rep.Repeat, rep.Session, orStr(rep.SetupHash, "-"), rep.SessionState)
		if rep.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", cleanText(rep.Error))
		}
		fmt.Fprintf(w, "  trajectory: cerberus sessions trajectory %s > %s.atif.json\n", rep.Session, rep.Session)
		for _, x := range rep.Results {
			fmt.Fprintf(w, "  %s %-20s %s", stateMark(x.State), x.Criterion, x.Gate)
			if x.Iteration > 1 {
				fmt.Fprintf(w, " (iteration %d)", x.Iteration)
			}
			fmt.Fprintln(w)
			if x.State != evals.StatePass && x.Rationale != "" {
				fmt.Fprintf(w, "      %s\n", flowClipText(x.Rationale, 200))
			}
		}
	}
	for _, s := range r.Slots {
		if s.Error != "" {
			fmt.Fprintf(w, "\n%s #%d: %s\n", clean(s.Setup), s.Repeat, cleanText(s.Error))
		}
	}
}

func evalRuns(args []string, stdout, stderr io.Writer) int {
	var limit int
	cfg, pos, code := clientCmd("eval runs", args, stderr, func(fs *flag.FlagSet) {
		fs.IntVar(&limit, "limit", 20, "most runs to show")
	})
	if cfg == nil {
		return code
	}
	if len(pos) > 1 {
		fmt.Fprintln(stderr, "usage: cerberus eval runs [ID] [--limit N]")
		return 2
	}
	q := url.Values{}
	if len(pos) == 1 {
		q.Set("eval", pos[0])
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var runs []evals.Run
	if err := newClient(cfg).do(context.Background(), "GET", "/api/evals/runs?"+q.Encode(), nil, &runs); err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No runs.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATE\tSTARTED\tSESSIONS\tJUDGE\tEVAL")
	for _, r := range runs {
		judged := 0
		for _, s := range r.Slots {
			if s.State == evals.SlotJudged {
				judged++
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", r.ID, r.State, flowWhen(r.Created), judged, len(r.Slots), clean(r.JudgeModel),
			flowClipText(r.EvalName, 40))
	}
	tw.Flush()
	return 0
}

func fetchEvalRun(cfg *client, id string) (evals.RunDetail, error) {
	var d evals.RunDetail
	err := cfg.do(context.Background(), "GET", "/api/evals/runs/"+url.PathEscape(id), nil, &d)
	return d, err
}

func evalReport(args []string, stdout, stderr io.Writer) int {
	var asJSON bool
	cfg, pos, code := clientCmd("eval report", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print the run, reports and summary as JSON")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus eval report [--json] RUN")
		return 2
	}
	d, err := fetchEvalRun(newClient(cfg), pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(d)
		return 0
	}
	printEvalReport(stdout, d)
	return 0
}

func evalCompare(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("eval compare", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "usage: cerberus eval compare RUN_A RUN_B")
		return 2
	}
	c := newClient(cfg)
	a, err := fetchEvalRun(c, pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	b, err := fetchEvalRun(c, pos[1])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	fmt.Fprintf(stdout, "A %s  %s v%d · judge %s\nB %s  %s v%d · judge %s\n", a.Run.ID, clean(a.Run.EvalName), a.Run.EvalVersion,
		a.Run.JudgeModel, b.Run.ID, clean(b.Run.EvalName), b.Run.EvalVersion, b.Run.JudgeModel)
	for _, sc := range evals.Compare(a.Reports, b.Reports) {
		fmt.Fprintf(stdout, "\n%s: gates %s → %s\n", clean(sc.Setup), orStr(sc.GatesA, "-"), orStr(sc.GatesB, "-"))
		for _, f := range sc.Flips {
			fmt.Fprintf(stdout, "  %-20s %s → %s\n", f.Criterion, f.A, f.B)
		}
		if sc.SessionA == "" || sc.SessionB == "" {
			continue
		}
		if sc.SetupHashA != "" && sc.SetupHashA == sc.SetupHashB {
			fmt.Fprintf(stdout, "  the same setup (%s)\n", sc.SetupHashA)
			continue
		}
		var va, vb session.SetupView
		ea := c.do(context.Background(), "GET", "/api/sessions/"+url.PathEscape(sc.SessionA)+"/setup", nil, &va)
		eb := c.do(context.Background(), "GET", "/api/sessions/"+url.PathEscape(sc.SessionB)+"/setup", nil, &vb)
		if ea != nil || eb != nil {
			fmt.Fprintln(stdout, "  setup: not available for both sessions")
			continue
		}
		diff := session.DiffSetups(va.Setup, vb.Setup)
		if len(diff) == 0 {
			fmt.Fprintln(stdout, "  the same setup")
			continue
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  FIELD\tA\tB")
		for _, d := range diff {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.Field, orDash(clean(d.A)), orDash(clean(d.B)))
		}
		tw.Flush()
	}
	return 0
}

func evalRunAction(verb string, args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("eval "+verb, args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "usage: cerberus eval %s RUN\n", verb)
		return 2
	}
	var r evals.Run
	if err := newClient(cfg).do(context.Background(), "POST", "/api/evals/runs/"+url.PathEscape(pos[0])+"/"+verb, nil, &r); err != nil {
		fmt.Fprintln(stderr, "cerberus eval:", err)
		return 1
	}
	if verb == "cancel" {
		fmt.Fprintf(stdout, "%s is stopping\n", r.ID)
	} else {
		fmt.Fprintf(stdout, "%s is being judged again with %s\n", r.ID, r.JudgeModel)
	}
	return 0
}
