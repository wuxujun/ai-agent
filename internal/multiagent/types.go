// Package multiagent implements a three-role collaborative agent system:
//   - PlannerAgent  – decomposes the user's goal into a concrete research plan (LLM-powered)
//   - ResearcherAgent – executes each research step using local file tools (tool-only, no LLM)
//   - WriterAgent   – synthesises all gathered evidence into a final answer (LLM-powered)
//
// The Coordinator orchestrates the three agents in sequence:
//
//	PlannerAgent → ResearcherAgent (×N steps) → WriterAgent
package multiagent

import "github.com/wuxujun/ai-agent/internal/types"

// AgentRole mirrors types.AgentRole for convenience inside this package.
type AgentRole = types.AgentRole

const (
	RolePlanner    AgentRole = types.AgentRolePlanner
	RoleResearcher AgentRole = types.AgentRoleResearcher
	RoleWriter     AgentRole = types.AgentRoleWriter
)

// ResearchStep is a single research directive produced by the PlannerAgent.
// Each step maps to exactly one tool invocation by the ResearcherAgent.
type ResearchStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// Action is one of: "find_files" | "search_text" | "read_file" | "write_file" | "execute_code"
	Action      string `json:"action"`
	SearchQuery string `json:"search_query"` // used by search_text
	FileGlob    string `json:"file_glob"`    // used by find_files or search_text filter
	FilePath    string `json:"file_path"`    // used by read_file
	Content     string `json:"content"`      // used by write_file
	Command     string `json:"command"`      // used by execute_code
	Args        string `json:"args"`         // used by execute_code
}

// ResearchPlan is the structured output of the PlannerAgent.
type ResearchPlan struct {
	ThoughtSummary string         `json:"thought_summary"`
	Steps          []ResearchStep `json:"steps"`
}

// StepEvidence records the result of executing one ResearchStep.
type StepEvidence struct {
	StepID      string           `json:"step_id"`
	StepDesc    string           `json:"step_desc"`
	Action      string           `json:"action"`
	Observation string           `json:"observation"`
	Evidence    []types.Evidence `json:"evidence,omitempty"`
}

// WriterOutput is the final synthesised answer produced by the WriterAgent.
type WriterOutput struct {
	FinalAnswer     string `json:"final_answer"`
	EvidenceSummary string `json:"evidence_summary"`
	// Confidence is one of: "high" | "medium" | "low"
	Confidence string `json:"confidence"`
}
