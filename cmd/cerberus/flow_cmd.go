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
	"time"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/flows"
)

// flowCmd: cerberus flow list | show ID | run ID [--input TEXT] [--wait] |
// runs [ID] | cancel RUN | approve RUN [--reject]. Everything goes
// through the daemon (/api/flows).
func flowCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if sub == "" && len(args) > 0 && isHelpFlag(args[0]) {
		printHelp(stdout, "flow")
		return 0
	}
	switch sub {
	case "", "list", "ls":
		return flowList(args, stdout, stderr)
	case "show":
		return flowShow(args, stdout, stderr)
	case "run":
		return flowRun(args, stdout, stderr)
	case "runs":
		return flowRuns(args, stdout, stderr)
	case "cancel":
		return flowCancel(args, stdout, stderr)
	case "approve":
		return flowApprove(args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "cerberus flow: unknown command %q (list, show, run, runs, cancel, approve)\n", sub)
	return 2
}

// flowWhen is a time in local time, short.
func flowWhen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func flowList(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("flow list", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: cerberus flow list")
		return 2
	}
	var list []flows.Summary
	if err := newClient(cfg).do(context.Background(), "GET", "/api/flows", nil, &list); err != nil {
		fmt.Fprintln(stderr, "cerberus flow:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "No flows yet. Make one in the web UI (Flows) or start from a template there.")
		return 0
	}
	for _, s := range list {
		on := "off"
		if s.Enabled {
			on = "on"
		}
		if s.Trigger.Kind != flows.TriggerSchedule {
			on = "-"
		}
		next := "-"
		if s.Next != nil {
			next = flowWhen(*s.Next)
		}
		last := "never run"
		if s.LastRun != nil {
			last = s.LastRun.State + " " + flowWhen(s.LastRun.Created)
		}
		fmt.Fprintf(stdout, "%-24s %-3s  %-28s next %-16s  last %s\n", s.ID, on, flowClipText(s.TriggerText, 28), next, last)
		fmt.Fprintf(stdout, "    %s\n", clean(s.Name))
		if s.LastSkip != nil {
			fmt.Fprintf(stdout, "    skipped %s: %s\n", flowWhen(s.LastSkip.Time), clean(s.LastSkip.Reason))
		}
	}
	return 0
}

// flowClipText shortens s to n runes with an ellipsis, with control
// characters made harmless (clean): run text can carry a model's reply.
func flowClipText(s string, n int) string {
	s = clean(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// cleanText is clean for multi-line text: newlines and tabs stay, every
// other control character (ESC, BEL, C1, DEL) becomes "?", so model output
// cannot drive the terminal (OSC 52 clipboard writes, cursor moves that
// hide or spoof lines).
func cleanText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		parts := strings.Split(l, "\t")
		for j, p := range parts {
			parts[j] = clean(p)
		}
		lines[i] = strings.Join(parts, "\t")
	}
	return strings.Join(lines, "\n")
}

// flowStepLine is one step in a few words.
func flowStepLine(s flows.Step) string {
	var parts []string
	switch s.Kind {
	case flows.KindPrompt, flows.KindPlan:
		parts = append(parts, s.Model)
	case flows.KindFanOut:
		parts = append(parts, "from "+s.From, "as "+flowOrDash(s.As), "default "+s.DefaultModel)
	case flows.KindSession:
		parts = append(parts, s.Agent, flowOrDash(s.Workspace))
	case flows.KindJudge:
		parts = append(parts, "on "+flowOrDash(s.On), fmt.Sprintf("%d criteria", len(s.Criteria)))
		if s.Persona != "" {
			parts = append(parts, "persona "+s.Persona)
		}
	case flows.KindIf:
		parts = append(parts, s.Test+" "+s.On, "then "+flowOrDash(s.Then), "else "+flowOrDash(s.Else))
	case flows.KindOutput:
		parts = append(parts, "to "+s.To)
	}
	return clean(strings.Join(parts, " · "))
}

func flowOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func flowShow(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("flow show", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus flow show ID")
		return 2
	}
	c := newClient(cfg)
	var v api.FlowView
	if err := c.do(context.Background(), "GET", "/api/flows/"+url.PathEscape(pos[0]), nil, &v); err != nil {
		fmt.Fprintln(stderr, "cerberus flow:", err)
		return 1
	}
	f := v.Flow
	fmt.Fprintf(stdout, "%s  %s (version %d)\n", f.ID, clean(f.Name), f.Version)
	if f.Description != "" {
		fmt.Fprintf(stdout, "  %s\n", clean(f.Description))
	}
	state := "manual"
	if f.Trigger.Kind == flows.TriggerSchedule {
		state = "schedule off"
		if f.Enabled {
			state = "schedule on"
		}
	}
	fmt.Fprintf(stdout, "Trigger: %s (%s)\n", v.TriggerText, state)
	if f.InputHint != "" {
		fmt.Fprintf(stdout, "Input:   %s\n", clean(f.InputHint))
	}
	fmt.Fprintln(stdout, "Steps:")
	for i, s := range f.Steps {
		fmt.Fprintf(stdout, "  %d. %-12s %-8s %s", i+1, s.ID, s.Kind, clean(s.Label()))
		if l := flowStepLine(s); l != "" {
			fmt.Fprintf(stdout, "  (%s)", l)
		}
		fmt.Fprintln(stdout)
	}
	for _, p := range v.Problems {
		where := strings.Trim(p.Step+"."+p.Field, ".")
		if where == "" {
			where = "flow"
		}
		fmt.Fprintf(stdout, "%s: %s: %s\n", p.Severity, clean(where), clean(p.Message))
	}
	fmt.Fprintf(stdout, "  %s/#/flows/%s\n", c.base, url.PathEscape(f.ID))
	return 0
}

func flowRun(args []string, stdout, stderr io.Writer) int {
	var input string
	var wait bool
	cfg, pos, code := clientCmd("flow run", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&input, "input", "", "the run's input ({input} in the steps)")
		fs.BoolVar(&wait, "wait", false, "follow the run until it ends or waits for an approval")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, `usage: cerberus flow run ID [--input "text"] [--wait]`)
		return 2
	}
	c := newClient(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var events <-chan flowFrame
	if wait {
		// Subscribe before starting, so no change is missed.
		var err error
		if events, err = followFlows(ctx, c); err != nil {
			fmt.Fprintln(stderr, "cerberus flow:", err)
			return 1
		}
	}
	var run flows.Run
	if err := c.do(ctx, "POST", "/api/flows/"+url.PathEscape(pos[0])+"/run", map[string]string{"input": input}, &run); err != nil {
		fmt.Fprintln(stderr, "cerberus flow:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Started %s (%s)\n  %s/#/flows/runs/%s\n", run.ID, run.State, c.base, url.PathEscape(run.ID))
	if !wait {
		return 0
	}
	return followRun(ctx, c, run.ID, events, stdout, stderr)
}

// flowFrame is one relevant event-stream frame: a run event, or a resync
// (Resync set: refetch).
type flowFrame struct {
	Event  flows.Event
	Resync bool
}

// followFlows opens GET /api/events and passes on flowrun events and
// flows resyncs until ctx ends or the stream breaks (the channel closes).
func followFlows(ctx context.Context, c *client) (<-chan flowFrame, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cerberus-Actor", audit.ActorCLI)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req) // no timeout: a long-lived stream
	if err != nil {
		return nil, &unreachableError{base: c.base, err: err}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return nil, fmt.Errorf("GET /api/events: %s", errorText(resp.Status, raw))
	}
	out := make(chan flowFrame, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		readSSE(resp.Body, func(event, data string) bool {
			var fr flowFrame
			switch event {
			case flows.EventType:
				if json.Unmarshal([]byte(data), &fr.Event) != nil {
					return true
				}
			case "resync":
				if !strings.Contains(data, `"flows"`) {
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

// followRun prints a run's step changes until it ends or waits for an
// approval, then its result. It blocks on the event stream (no polling).
// Exit code: 0 passed or waiting, 1 any other end.
func followRun(ctx context.Context, c *client, id string, events <-chan flowFrame, stdout, stderr io.Writer) int {
	lastStep := ""
	check := func() (flows.Run, bool, error) {
		var d flows.RunDetail
		if err := c.do(ctx, "GET", "/api/flows/runs/"+url.PathEscape(id), nil, &d); err != nil {
			return flows.Run{}, false, err
		}
		r := d.Run
		return r, flows.Final(r.State) || r.State == flows.StateWaiting, nil
	}
	r, done, err := check()
	for err == nil && !done {
		select {
		case <-ctx.Done():
			fmt.Fprintf(stderr, "Stopped following; the run goes on: cerberus flow runs %s\n", r.Flow)
			return 1
		case fr, open := <-events:
			if !open {
				err = errors.New("the event stream ended (did the daemon stop?)")
				break
			}
			if !fr.Resync && fr.Event.Run != id {
				continue
			}
			if fr.Event.Step != "" && fr.Event.Step != lastStep {
				lastStep = fr.Event.Step
				fmt.Fprintf(stdout, "  step %s\n", lastStep)
			}
			if fr.Resync || flows.Final(fr.Event.State) || fr.Event.State == flows.StateWaiting {
				r, done, err = check()
			}
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus flow:", err)
		return 1
	}
	return printRunEnd(ctx, c, r, stdout)
}

// printRunEnd prints a finished (or waiting) run and its exit code.
func printRunEnd(ctx context.Context, c *client, r flows.Run, stdout io.Writer) int {
	fmt.Fprintf(stdout, "%s %s", r.ID, r.State)
	if r.Reason != "" {
		fmt.Fprintf(stdout, ": %s", cleanText(r.Reason))
	}
	fmt.Fprintf(stdout, "  (%s tokens, $%.4f, %.2f Wh)\n", strconv.Itoa(r.Tokens), r.CostUSD, r.EnergyWh)
	switch r.State {
	case flows.StateWaiting:
		msg := ""
		if r.Approval != nil {
			msg = cleanText(r.Approval.Message)
		}
		fmt.Fprintf(stdout, "Waiting for approval: %s\n  cerberus flow approve %s   (or --reject)\n", msg, r.ID)
		return 0
	case flows.StatePassed:
		var d flows.RunDetail
		if err := c.do(ctx, "GET", "/api/flows/runs/"+url.PathEscape(r.ID), nil, &d); err == nil && len(d.Steps) > 0 {
			last := d.Steps[len(d.Steps)-1]
			switch out := last.Output.(type) {
			case string:
				if strings.TrimSpace(out) != "" {
					fmt.Fprintln(stdout, cleanText(strings.TrimRight(out, "\n")))
				}
			case map[string]any: // an output-to-file step: {file, bytes}
				if f, ok := out["file"].(string); ok && f != "" {
					fmt.Fprintf(stdout, "Wrote %s in the run folder (%v bytes)\n", clean(f), out["bytes"])
				}
			}
		}
		return 0
	}
	return 1
}

func flowRuns(args []string, stdout, stderr io.Writer) int {
	var limit int
	cfg, pos, code := clientCmd("flow runs", args, stderr, func(fs *flag.FlagSet) {
		fs.IntVar(&limit, "limit", 20, "most runs to show")
	})
	if cfg == nil {
		return code
	}
	if len(pos) > 1 {
		fmt.Fprintln(stderr, "usage: cerberus flow runs [ID] [--limit N]")
		return 2
	}
	q := url.Values{}
	if len(pos) == 1 {
		q.Set("flow", pos[0])
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var runs []flows.Run
	if err := newClient(cfg).do(context.Background(), "GET", "/api/flows/runs?"+q.Encode(), nil, &runs); err != nil {
		fmt.Fprintln(stderr, "cerberus flow:", err)
		return 1
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No runs.")
		return 0
	}
	for _, r := range runs {
		kind := r.Trigger
		fmt.Fprintf(stdout, "%s  %-9s %-8s %s  %s", r.ID, r.State, kind, flowWhen(r.Created), clean(r.FlowName))
		if r.Reason != "" {
			fmt.Fprintf(stdout, "  (%s)", flowClipText(r.Reason, 60))
		}
		if r.Note != "" {
			fmt.Fprintf(stdout, "  [%s]", flowClipText(r.Note, 60))
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

func flowCancel(args []string, stdout, stderr io.Writer) int {
	cfg, pos, code := clientCmd("flow cancel", args, stderr, nil)
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: cerberus flow cancel RUN")
		return 2
	}
	var r flows.Run
	if err := newClient(cfg).do(context.Background(), "POST", "/api/flows/runs/"+url.PathEscape(pos[0])+"/cancel", nil, &r); err != nil {
		fmt.Fprintln(stderr, "cerberus flow:", err)
		return 1
	}
	if r.State == flows.StateRunning {
		fmt.Fprintf(stdout, "%s is stopping\n", r.ID)
	} else {
		fmt.Fprintf(stdout, "%s %s\n", r.ID, r.State)
	}
	return 0
}

func flowApprove(args []string, stdout, stderr io.Writer) int {
	var reject bool
	var note string
	cfg, pos, code := clientCmd("flow approve", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&reject, "reject", false, "reject instead: the run ends as rejected")
		fs.StringVar(&note, "note", "", "a note kept with the decision")
	})
	if cfg == nil {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, `usage: cerberus flow approve RUN [--reject] [--note "text"]`)
		return 2
	}
	var r flows.Run
	body := map[string]any{"ok": !reject, "note": note}
	if err := newClient(cfg).do(context.Background(), "POST", "/api/flows/runs/"+url.PathEscape(pos[0])+"/approve", body, &r); err != nil {
		fmt.Fprintln(stderr, "cerberus flow:", err)
		return 1
	}
	if reject {
		fmt.Fprintf(stdout, "%s rejected\n", r.ID)
	} else {
		fmt.Fprintf(stdout, "%s approved; it continues (%s)\n", r.ID, r.State)
	}
	return 0
}
