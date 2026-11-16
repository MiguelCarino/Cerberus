package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Flow tools: an outside agent (an MCP client, `cerberus mcp --client`)
// lists its owner's enabled flows, starts one, and follows the run. They
// call the agent scope's /api/agent/flows routes, which answer MCP
// clients only (a session's agent gets 403).
const (
	ToolFlows   = "cerberus_flows"
	ToolRunFlow = "cerberus_run_flow"
	ToolFlowRun = "cerberus_flow_run"

	// FlowWaitMaxS bounds one cerberus_flow_run wait.
	FlowWaitMaxS     = 300
	flowWaitDefaultS = 60
)

// FlowSummary is one flow GET /api/agent/flows lists.
type FlowSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	InputHint   string `json:"input_hint"`
	Trigger     string `json:"trigger"`
}

// FlowRunFile is a file an output step wrote (relative to the run folder).
type FlowRunFile struct {
	Path  string `json:"path"`
	Step  string `json:"step"`
	Bytes int    `json:"bytes,omitempty"`
}

// FlowRunStatus is GET /api/agent/flows/runs/{run}.
type FlowRunStatus struct {
	ID       string        `json:"id"`
	Flow     string        `json:"flow"`
	FlowName string        `json:"flow_name"`
	State    string        `json:"state"`
	Step     string        `json:"step"`
	Reason   string        `json:"reason"`
	Done     bool          `json:"done"`     // a final state
	Waiting  bool          `json:"waiting"`  // waits for a person's approval
	Output   string        `json:"output"`   // the last step output, as text
	OutStep  string        `json:"out_step"` // the step it came from
	Cut      bool          `json:"cut"`      // the output was cut
	Files    []FlowRunFile `json:"files"`
	Tokens   int           `json:"tokens"`
	CostUSD  float64       `json:"cost_usd"`
}

// FlowTools are the definitions of the flow tools.
func FlowTools() []SpawnTool {
	return []SpawnTool{
		{ToolFlows, "List the flows you can run: saved recipes of steps (models, agent sessions, a judge) " +
			"that the owner set up and enabled. Each has an id and what input it expects.", schema(map[string]any{})},
		{ToolRunFlow, "Start a flow with an input. It runs on its own on the Cerberus daemon; use cerberus_flow_run " +
			"with the run id to wait for it and read its result. At most 2 of your runs go at once.",
			schema(map[string]any{
				"flow":  prop("string", "The flow id from cerberus_flows (fl-...)."),
				"input": prop("string", "The input the flow works on (see its input hint)."),
			}, "flow")},
		{ToolFlowRun, "Read a flow run you started: its state, current step, and when it is done the last step's " +
			"output and the files it wrote. With wait_s it waits until the run ends or waits for an approval.",
			schema(map[string]any{
				"run": prop("string", "The run id from cerberus_run_flow (fr-...)."),
				"wait_s": map[string]any{"type": "integer", "minimum": 0, "maximum": FlowWaitMaxS,
					"description": "Seconds to wait for the run to end (default 60, at most 300; 0 answers at once)."},
			}, "run")},
	}
}

// IsFlowTool reports whether name is a flow tool.
func IsFlowTool(name string) bool {
	switch name {
	case ToolFlows, ToolRunFlow, ToolFlowRun:
		return true
	}
	return false
}

// callFlow runs one flow tool through the daemon.
func (c *SpawnClient) callFlow(ctx context.Context, name string, ar args) (string, error) {
	switch name {
	case ToolFlows:
		var list []FlowSummary
		if err := c.do(ctx, http.MethodGet, "/api/agent/flows", nil, &list, spawnCallTimeout); err != nil {
			return "", err
		}
		return FormatFlows(list), nil
	case ToolRunFlow:
		id := strings.TrimSpace(ar.str("flow"))
		if id == "" {
			return "", errors.New("flow is required")
		}
		var st FlowRunStatus
		p := "/api/agent/flows/" + url.PathEscape(id) + "/run"
		if err := c.do(ctx, http.MethodPost, p, map[string]string{"input": ar.str("input")}, &st, spawnCallTimeout); err != nil {
			return "", err
		}
		return fmt.Sprintf("Started run %s of %s (%s). Use cerberus_flow_run with this id to wait for it.", st.ID, st.FlowName, st.State), nil
	case ToolFlowRun:
		id := strings.TrimSpace(ar.str("run"))
		if id == "" {
			return "", errors.New("run is required")
		}
		secs := flowWaitDefaultS
		if n, ok := ar.int("wait_s"); ok {
			secs = min(max(n, 0), FlowWaitMaxS)
		}
		var st FlowRunStatus
		p := "/api/agent/flows/runs/" + url.PathEscape(id) + "?wait=" + strconv.Itoa(secs)
		if err := c.do(ctx, http.MethodGet, p, nil, &st, time.Duration(secs)*time.Second+spawnCallTimeout); err != nil {
			return "", err
		}
		return FormatFlowRun(st), nil
	}
	return "", fmt.Errorf("there is no tool %q", name)
}

// FormatFlows is cerberus_flows' text.
func FormatFlows(list []FlowSummary) string {
	if len(list) == 0 {
		return "There are no flows you can run (none is enabled for your user)."
	}
	var b strings.Builder
	b.WriteString("Flows (id · name · trigger):\n")
	for _, f := range list {
		fmt.Fprintf(&b, "- %s · %s · %s", f.ID, f.Name, f.Trigger)
		if d := strings.TrimSpace(f.Description); d != "" {
			b.WriteString("\n  " + truncRunes(d, 300))
		}
		if h := strings.TrimSpace(f.InputHint); h != "" {
			b.WriteString("\n  Input: " + truncRunes(h, 300))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// FormatFlowRun is cerberus_flow_run's text.
func FormatFlowRun(st FlowRunStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s of %s is %s", st.ID, st.FlowName, st.State)
	if st.Step != "" && !st.Done {
		fmt.Fprintf(&b, " (step %s)", st.Step)
	}
	if st.Reason != "" {
		b.WriteString(": " + truncRunes(st.Reason, 400))
	}
	b.WriteString(".")
	if st.Waiting {
		b.WriteString(" It waits for a person to approve it in Cerberus.")
	}
	if !st.Done && !st.Waiting {
		b.WriteString(" Call cerberus_flow_run again to keep waiting.")
	}
	if st.Output != "" {
		fmt.Fprintf(&b, "\nOutput of step %s", st.OutStep)
		if st.Cut {
			b.WriteString(" (cut)")
		}
		b.WriteString(":\n" + st.Output)
	}
	if len(st.Files) > 0 {
		b.WriteString("\nFiles it wrote (in the run folder):")
		for _, f := range st.Files {
			fmt.Fprintf(&b, "\n- %s (step %s)", f.Path, f.Step)
		}
	}
	return b.String()
}
