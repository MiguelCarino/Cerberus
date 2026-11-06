// Package trajectory exports a session's model calls as a trajectory
// (docs/SCOPE.md "E4 Trajectory export (ATIF)"): the Agent Trajectory
// Interchange Format, built from the gateway's request records (G3) with
// the session's setup snapshot (E3) as metadata.
//
// The format implemented is ATIF-v1.8 as published in
// harbor-framework/harbor, rfcs/0001-trajectory-format.md (Changelog v1.8)
// and its reference Pydantic models, at commit 74cc631 (2026-08-26). The
// repository the spec named, github.com/AISafety-ATIF/specification, does
// not exist; Harbor's RFC is where ATIF is maintained. testdata holds a
// JSON Schema transcribed from those models, and the tests validate every
// export against it.
package trajectory

// SchemaVersion is the ATIF version written.
const SchemaVersion = "ATIF-v1.8"

// SpecSource names the published text implemented.
const SpecSource = "harbor-framework/harbor rfcs/0001-trajectory-format.md @ 74cc631 (ATIF v1.8)"

// Trajectory is the ATIF root object.
type Trajectory struct {
	SchemaVersion string         `json:"schema_version"`
	SessionID     string         `json:"session_id,omitempty"`
	TrajectoryID  string         `json:"trajectory_id,omitempty"`
	Agent         Agent          `json:"agent"`
	Steps         []Step         `json:"steps"`
	Notes         string         `json:"notes,omitempty"`
	FinalMetrics  *FinalMetrics  `json:"final_metrics,omitempty"`
	Extra         map[string]any `json:"extra,omitempty"`
}

// Agent is ATIF's AgentSchema.
type Agent struct {
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	ModelName string         `json:"model_name,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// Step is ATIF's StepObject.
type Step struct {
	StepID           int            `json:"step_id"`
	Timestamp        string         `json:"timestamp,omitempty"`
	Source           string         `json:"source"` // system | user | agent
	ModelName        string         `json:"model_name,omitempty"`
	ReasoningEffort  string         `json:"reasoning_effort,omitempty"`
	Message          string         `json:"message"` // required, may be ""
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	Observation      *Observation   `json:"observation,omitempty"`
	Metrics          *Metrics       `json:"metrics,omitempty"`
	LLMCallCount     *int           `json:"llm_call_count,omitempty"`
	Extra            map[string]any `json:"extra,omitempty"`
}

// Step sources.
const (
	SourceSystem = "system"
	SourceUser   = "user"
	SourceAgent  = "agent"
)

// ToolCall is ATIF's ToolCallSchema.
type ToolCall struct {
	ToolCallID   string         `json:"tool_call_id"`
	FunctionName string         `json:"function_name"`
	Arguments    map[string]any `json:"arguments"` // never nil: ATIF requires an object
	Extra        map[string]any `json:"extra,omitempty"`
}

// Observation is ATIF's ObservationSchema.
type Observation struct {
	Results []ObservationResult `json:"results"`
}

// ObservationResult is ATIF's ObservationResultSchema.
type ObservationResult struct {
	SourceCallID string         `json:"source_call_id,omitempty"`
	Content      string         `json:"content,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

// Metrics is ATIF's MetricsSchema (the fields Cerberus knows).
type Metrics struct {
	PromptTokens     *int           `json:"prompt_tokens,omitempty"`
	CompletionTokens *int           `json:"completion_tokens,omitempty"`
	CachedTokens     *int           `json:"cached_tokens,omitempty"`
	Extra            map[string]any `json:"extra,omitempty"`
}

// FinalMetrics is ATIF's FinalMetricsSchema.
type FinalMetrics struct {
	TotalPromptTokens     *int           `json:"total_prompt_tokens,omitempty"`
	TotalCompletionTokens *int           `json:"total_completion_tokens,omitempty"`
	TotalCachedTokens     *int           `json:"total_cached_tokens,omitempty"`
	TotalSteps            *int           `json:"total_steps,omitempty"`
	Extra                 map[string]any `json:"extra,omitempty"`
}

func intp(n int) *int { return &n }
