package planner

import (
	"context"
	"slices"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

type ActionCall struct {
	Action     string         `json:"action"`
	Parameters map[string]any `json:"parameters"`
}

type PlanDecision struct {
	ThoughtSummary string           `json:"thought_summary"`
	Stop           bool             `json:"stop"`
	FinalAnswer    string           `json:"final_answer"`
	Actions        []ActionCall     `json:"actions"`
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
// Each action is represented as its own anyOf branch. OpenAI strict mode still
// requires every property within the selected branch, but an action no longer
// has to emit unrelated parameters belonging to every other registered tool.
func PlannerDecisionSchema() map[string]any {
	registered := tools.DefaultRegistry.List() // sorted by name (deterministic)

	variants := make([]any, 0, len(registered)+1)
	for _, t := range registered {
		variants = append(variants, strictActionVariant(t.Name(), t.Parameters()))
	}
	variants = append(variants, strictActionVariant("none", map[string]any{}))

	actionCallSchema := map[string]any{"anyOf": variants}

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
			"actions": map[string]any{
				"type":        "array",
				"description": "One or more independent tool actions to execute in parallel. If no tools are needed, use a single 'none' action.",
				"items":       actionCallSchema,
				"minItems":    1,
			},
		},
		"required": []string{"thought_summary", "stop", "final_answer", "actions"},
	}
}

func strictActionVariant(action string, parameters map[string]any) map[string]any {
	required := make([]string, 0, len(parameters))
	for name := range parameters {
		required = append(required, name)
	}
	// Registry parameters are stable but maps are not; deterministic required
	// ordering keeps request bodies and tests reproducible.
	slices.Sort(required)
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{action},
				"description": "The single next action to execute. Use none only when stop is true.",
			},
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           parameters,
				"required":             required,
			},
		},
		"required": []string{"action", "parameters"},
	}
}
