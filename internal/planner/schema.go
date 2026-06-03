package planner

import (
	"context"
	"sort"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

type PlanDecision struct {
	ThoughtSummary string         `json:"thought_summary"`
	Stop           bool           `json:"stop"`
	FinalAnswer    string         `json:"final_answer"`
	Action         string         `json:"action"`
	Parameters     map[string]any `json:"parameters"`
	TokenUsage     types.TokenUsage `json:"token_usage,omitempty"`
}

type Planner interface {
	PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*PlanDecision, error)
}

// PlannerDecisionSchema builds the JSON Schema constraining the planner LLM's
// output. The action enum and the parameters object are generated from the tool
// registry, so registering a new tool automatically makes it selectable by the
// planner — no manual edits to this file are required.
//
// Note: OpenAI structured-output strict mode requires every property to appear
// in "required", so the merged parameter set is listed there in full; unused
// parameters are expected to be emitted as empty strings.
func PlannerDecisionSchema() map[string]any {
	registered := tools.DefaultRegistry.List() // sorted by name (deterministic)

	actions := make([]string, 0, len(registered)+1)
	paramProps := map[string]any{}
	for _, t := range registered {
		actions = append(actions, t.Name())
		for name, spec := range t.Parameters() {
			paramProps[name] = spec
		}
	}
	// "none" is the sentinel action used when the agent stops.
	actions = append(actions, "none")

	paramKeys := make([]string, 0, len(paramProps))
	for k := range paramProps {
		paramKeys = append(paramKeys, k)
	}
	sort.Strings(paramKeys)

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
				"enum":        actions,
				"description": "The single next action to execute. Use none only when stop is true.",
			},
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           paramProps,
				"required":             paramKeys,
			},
		},
		"required": []string{"thought_summary", "stop", "final_answer", "action", "parameters"},
	}
}
