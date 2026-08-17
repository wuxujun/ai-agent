// Package multiagent implements configurable collaborative agent workflows:
//   - PlannerAgent  – decomposes the user's goal into a concrete research plan (LLM-powered)
//   - CriticAgent   – reviews plans before reviewed-workflow execution (LLM-powered)
//   - ExecutorAgent – executes approved plan steps through registered tools (tool-only)
//   - VerifierAgent – synthesises and verifies reviewed-workflow results (LLM-powered)
//   - ResearcherAgent – executes each research step using local file tools (tool-only, no LLM)
//   - WriterAgent   – synthesises all gathered evidence into a final answer (LLM-powered)
//
// The Coordinator supports both the original research workflow and a reviewed
// execution workflow:
//
//	PlannerAgent → ResearcherAgent (×N steps) → WriterAgent
//	PlannerAgent → CriticAgent → ExecutorAgent (×N steps) → VerifierAgent
package multiagent

import (
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

// AgentRole mirrors types.AgentRole for convenience inside this package.
type AgentRole = types.AgentRole

const (
	RolePlanner    AgentRole = types.AgentRolePlanner
	RoleCritic     AgentRole = types.AgentRoleCritic
	RoleExecutor   AgentRole = types.AgentRoleExecutor
	RoleVerifier   AgentRole = types.AgentRoleVerifier
	RoleResearcher AgentRole = types.AgentRoleResearcher
	RoleWriter     AgentRole = types.AgentRoleWriter
)

// ResearchStep is a single research directive produced by the PlannerAgent.
// Each step maps to exactly one tool invocation by the ResearcherAgent.
type ResearchStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// Action is the tool name to invoke; any tool registered in the tools
	// package is valid (e.g. find_files, search_text, read_file, write_file,
	// execute_code, git_diff, http_fetch, web_search).
	Action         string `json:"action"`
	SearchQuery    string `json:"search_query"`    // used by search_text / web_search
	FileGlob       string `json:"file_glob"`       // used by find_files or search_text filter
	FilePath       string `json:"file_path"`       // used by read_file / write_file / git_diff
	Content        string `json:"content"`         // used by write_file
	Command        string `json:"command"`         // used by execute_code
	Args           string `json:"args"`            // used by execute_code
	URL            string `json:"url"`             // used by http_fetch
	Prompt         string `json:"prompt"`          // used by analyze_image
	GraphURI       string `json:"graph_uri"`       // used by wiki_graph
	GraphDepth     int    `json:"graph_depth"`     // used by wiki_graph (1..2)
	GraphDirection string `json:"graph_direction"` // outgoing / incoming / both
	SuggestURI     string `json:"suggest_uri"`     // used by wiki_suggest
	SuggestLimit   int    `json:"suggest_limit"`   // used by wiki_suggest (1..10)
	// RepairedParameters is populated after plan-time argument repair. It is
	// intentionally excluded from LLM JSON and is shared by approval and execution.
	RepairedParameters map[string]any `json:"-"`
}

// ResearchPlan is the structured output of the PlannerAgent.
type ResearchPlan struct {
	ThoughtSummary string           `json:"thought_summary"`
	Steps          []ResearchStep   `json:"steps"`
	TokenUsage     types.TokenUsage `json:"token_usage,omitempty"`
}

// StepEvidence records the result of executing one ResearchStep.
type StepEvidence struct {
	StepID      string           `json:"step_id"`
	StepDesc    string           `json:"step_desc"`
	Action      string           `json:"action"`
	Observation string           `json:"observation"`
	Evidence    []types.Evidence `json:"evidence,omitempty"`
	TokenUsage  types.TokenUsage `json:"token_usage,omitempty"`
	// FollowupURIs is internal routing state emitted by a tool. It is excluded
	// from persisted/user-facing evidence and must still be validated by the
	// receiving follow-up tool before use.
	FollowupURIs []string `json:"-"`
	// Failed is set to true by ResearcherAgent when the step could not be
	// completed (tool error or policy violation). Coordinator uses this flag
	// instead of parsing Observation strings, avoiding false positives when
	// legitimate content contains words like "error" or "not found".
	Failed bool `json:"failed,omitempty"`
}

// WriterOutput is the draft answer produced by the WriterAgent. DraftConfidence
// is only a generation-time signal used by Coordinator for adaptive research;
// final confidence is owned by the answer pipeline's uncertainty stage.
type WriterOutput struct {
	FinalAnswer     string `json:"final_answer"`
	EvidenceSummary string `json:"evidence_summary"`
	// DraftConfidence is one of: "high" | "medium" | "low".
	DraftConfidence string `json:"draft_confidence"`
	// Confidence is retained as a source-compatible bridge for custom Writer
	// implementations. New implementations must set DraftConfidence instead.
	Confidence string           `json:"-"`
	TokenUsage types.TokenUsage `json:"token_usage,omitempty"`
}

func (o *WriterOutput) resolvedDraftConfidence() string {
	if o == nil {
		return "low"
	}
	value := o.DraftConfidence
	if value == "" {
		value = o.Confidence
	}
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "high", "medium", "low":
		return value
	default:
		return "low"
	}
}
