package planner

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/types"
)

type PlanDecision struct {
	ThoughtSummary string         `json:"thought_summary"`
	Stop           bool           `json:"stop"`
	FinalAnswer    string         `json:"final_answer"`
	Action         string         `json:"action"`
	Parameters     map[string]any `json:"parameters"`
}

type Planner interface {
	PlanNext(ctx context.Context, task *types.Task) (*PlanDecision, error)
}

func PlannerDecisionSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"thought_summary": map[string]any{
				"type":        "string",
				"description": "A brief internal summary of why this next step was chosen, under 30 words.",
			},
			"stop": map[string]any{
				"type":        "boolean",
				"description": "Whether the agent should stop now.",
			},
			"final_answer": map[string]any{
				"type":        "string",
				"description": "If stop is true, provide a concise final answer; otherwise empty string.",
			},
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"find_files", "search_text", "read_file", "none"},
				"description": "The single next action to execute. Use none only when stop is true.",
			},
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
					"query":   map[string]any{"type": "string"},
					"glob":    map[string]any{"type": "string"},
					"path":    map[string]any{"type": "string"},
				},
				"required": []string{"pattern", "query", "glob", "path"},
			},
		},
		"required": []string{"thought_summary", "stop", "final_answer", "action", "parameters"},
	}
}
