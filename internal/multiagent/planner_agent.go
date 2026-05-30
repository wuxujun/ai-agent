package multiagent

import (
	"context"
	"fmt"
	"log"
)

// plannerSystemPrompt instructs the LLM to produce a structured research plan.
const plannerSystemPrompt = `You are a research planning agent. Given a user goal, decompose it into
concrete, ordered research steps. Each step specifies exactly one tool action.

Available actions:
  find_files   – discover files by glob pattern  (set file_glob, e.g. "*.yaml")
  search_text  – search for keywords/text        (set search_query; optionally file_glob)
  read_file    – read a specific file's content  (set file_path)

Rules:
1. Produce between 2 and 8 steps — prefer fewer, higher-quality steps.
2. Start broad (find_files or search_text) then narrow down (read_file).
3. Each step builds on previous findings.
4. Step IDs must be "step-1", "step-2", etc.
5. Set every unused field to an empty string "".
6. Never include steps that cannot be executed with the three actions above.`

// PlannerAgent decomposes a user goal into a structured ResearchPlan using an LLM.
type PlannerAgent struct {
	Config LLMConfig
}

// jsonSchema returns the JSON Schema used to enforce structured output.
func (p *PlannerAgent) jsonSchema() map[string]any {
	stepSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":           map[string]any{"type": "string", "description": "Unique step ID (step-1, step-2, ...)"},
			"description":  map[string]any{"type": "string", "description": "What this step investigates"},
			"action":       map[string]any{"type": "string", "enum": []string{"find_files", "search_text", "read_file"}},
			"search_query": map[string]any{"type": "string", "description": "Keyword or text to search (search_text only)"},
			"file_glob":    map[string]any{"type": "string", "description": "Glob pattern for find_files or search_text filter"},
			"file_path":    map[string]any{"type": "string", "description": "Relative file path for read_file"},
		},
		"required":             []string{"id", "description", "action", "search_query", "file_glob", "file_path"},
		"additionalProperties": false,
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"thought_summary": map[string]any{
				"type":        "string",
				"description": "One-sentence summary of the overall research strategy (max 60 words)",
			},
			"steps": map[string]any{
				"type":        "array",
				"items":       stepSchema,
				"minItems":    2,
				"maxItems":    8,
				"description": "Ordered list of research steps",
			},
		},
		"required":             []string{"thought_summary", "steps"},
		"additionalProperties": false,
	}
}

// Plan calls the LLM to produce a ResearchPlan for the given goal.
func (p *PlannerAgent) Plan(ctx context.Context, goal, workspace string) (*ResearchPlan, error) {
	log.Printf("[PlannerAgent] Decomposing goal into research plan: %q", goal)

	userPrompt := fmt.Sprintf(
		"Goal: %s\n\nWorkspace root: %s\n\n"+
			"Produce a research plan with concrete steps to achieve the goal using the available file tools.",
		goal, workspace,
	)

	var plan ResearchPlan
	if err := callLLMJSON(ctx, p.Config, plannerSystemPrompt, userPrompt, p.jsonSchema(), &plan); err != nil {
		return nil, fmt.Errorf("PlannerAgent LLM call failed: %w", err)
	}

	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("PlannerAgent returned an empty steps list")
	}

	log.Printf("[PlannerAgent] Plan created: %q — %d steps", plan.ThoughtSummary, len(plan.Steps))
	for i, s := range plan.Steps {
		log.Printf("[PlannerAgent]   step %d: action=%s  desc=%q", i+1, s.Action, s.Description)
	}
	return &plan, nil
}
