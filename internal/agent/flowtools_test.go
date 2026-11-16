package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFlowToolCalls(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/api/agent/flows":
			_ = json.NewEncoder(w).Encode([]FlowSummary{{ID: "fl-a", Name: "A", InputHint: "a topic", Trigger: "Manual"}})
		case strings.HasSuffix(r.URL.Path, "/run"):
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["input"] != "cats" {
				http.Error(w, `{"error":"no input"}`, 400)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(FlowRunStatus{ID: "fr-1", FlowName: "A", State: "queued"})
		default:
			_ = json.NewEncoder(w).Encode(FlowRunStatus{ID: "fr-1", FlowName: "A", State: "passed", Done: true,
				Output: "the answer", OutStep: "out", Files: []FlowRunFile{{Path: "files/a.txt", Step: "save"}}})
		}
	}))
	defer srv.Close()
	c := NewSpawnClient(srv.URL, "mcp-tok", "")
	ctx := context.Background()
	if out, err := c.Call(ctx, ToolFlows, nil); err != nil || !strings.Contains(out, "fl-a · A · Manual") || !strings.Contains(out, "Input: a topic") {
		t.Fatalf("flows: %q %v", out, err)
	}
	if out, err := c.Call(ctx, ToolRunFlow, map[string]any{"flow": "fl-a", "input": "cats"}); err != nil || !strings.Contains(out, "Started run fr-1") {
		t.Fatalf("run: %q %v", out, err)
	}
	if _, err := c.Call(ctx, ToolRunFlow, map[string]any{}); err == nil {
		t.Fatal("run without a flow")
	}
	out, err := c.Call(ctx, ToolFlowRun, map[string]any{"run": "fr-1", "wait_s": 9999})
	if err != nil || !strings.Contains(out, "is passed") || !strings.Contains(out, "the answer") || !strings.Contains(out, "files/a.txt") {
		t.Fatalf("status: %q %v", out, err)
	}
	if last := seen[len(seen)-1]; last != "GET /api/agent/flows/runs/fr-1?wait=300 Bearer mcp-tok" {
		t.Fatalf("request %q", last)
	}
	for _, tool := range FlowTools() {
		if !IsFlowTool(tool.Name) || isSpawnTool(tool.Name) {
			t.Fatalf("%s: flow tools are not offered to the built-in agent", tool.Name)
		}
	}
}
