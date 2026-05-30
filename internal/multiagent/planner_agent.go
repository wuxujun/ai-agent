package multiagent

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

// plannerSystemPrompt instructs the LLM to produce a structured research plan.
const plannerSystemPrompt = `You are a research planning agent. Given a user goal, decompose it into
concrete, ordered research steps. Each step specifies exactly one tool action.

Available actions:
  find_files   – discover files by glob pattern  (set file_glob, e.g. "*.yaml")
  search_text  – search for keywords/text        (set search_query; optionally file_glob)
  read_file    – read a specific file's content  (set file_path)
  write_file   – write content to a file         (set file_path, content)
  execute_code – run script execution command    (set command, args; allowed commands: python3, python, go, node, bash, sh)

Rules:
1. Produce between 2 and 8 steps — prefer fewer, higher-quality steps.
2. Start broad (find_files or search_text) then narrow down or execute (read_file, write_file, execute_code).
3. Each step builds on previous findings.
4. Step IDs must be "step-1", "step-2", etc.
5. Set every unused field to an empty string "".
6. Never include steps that cannot be executed with the five actions above.`

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
			"action":       map[string]any{"type": "string", "enum": []string{"find_files", "search_text", "read_file", "write_file", "execute_code"}},
			"search_query": map[string]any{"type": "string", "description": "Keyword or text to search (search_text only)"},
			"file_glob":    map[string]any{"type": "string", "description": "Glob pattern for find_files or search_text filter"},
			"file_path":    map[string]any{"type": "string", "description": "Relative file path for read_file"},
			"content":      map[string]any{"type": "string", "description": "Content to write (write_file only)"},
			"command":      map[string]any{"type": "string", "description": "Command/Interpreter to run (execute_code only)"},
			"args":         map[string]any{"type": "string", "description": "Space-separated arguments (execute_code only)"},
		},
		"required":             []string{"id", "description", "action", "search_query", "file_glob", "file_path", "content", "command", "args"},
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

// replannerSystemPrompt instructs the LLM to produce a revised plan after failure.
const replannerSystemPrompt = `You are a research planning agent tasked with adjusting a research plan because a step has failed.

Given a user goal, the workspace, the trace of previous steps (including the failed step and its error/observation), your job is to analyze why it failed and generate a revised plan of ordered research steps to achieve the goal.

Available actions:
  find_files   – discover files by glob pattern  (set file_glob, e.g. "*.yaml")
  search_text  – search for keywords/text        (set search_query; optionally file_glob)
  read_file    – read a specific file's content  (set file_path)
  write_file   – write content to a file         (set file_path, content)
  execute_code – run script execution command    (set command, args; allowed commands: python3, python, go, node, bash, sh)

Rules:
1. Analyze the trace and explain why you think it failed in thought_summary.
2. Generate revised next steps (between 1 and 5 steps).
3. Do not repeat the exact same failed step unless you use different arguments or parameters.
4. Step IDs must be "step-1", "step-2", etc.
5. Set every unused field to an empty string "".`

// Replan calls the LLM to generate a revised ResearchPlan when a execution step fails.
func (p *PlannerAgent) Replan(ctx context.Context, goal, workspace string, traces []types.StepTrace) (*ResearchPlan, error) {
	log.Printf("[PlannerAgent] Re-planning goal: %q due to step execution failure", goal)

	userPrompt := fmt.Sprintf(
		"Goal: %s\n\nWorkspace root: %s\n\n"+
			"Execution History so far:\n%s\n\n"+
			"Please analyze the failure and generate a revised plan.",
		goal, workspace, formatTracesForReplanner(traces),
	)

	var plan ResearchPlan
	if err := callLLMJSON(ctx, p.Config, replannerSystemPrompt, userPrompt, p.jsonSchema(), &plan); err != nil {
		return nil, fmt.Errorf("PlannerAgent Replan LLM call failed: %w", err)
	}

	log.Printf("[PlannerAgent] Revised plan created: %q — %d steps", plan.ThoughtSummary, len(plan.Steps))
	for i, s := range plan.Steps {
		log.Printf("[PlannerAgent]   revised step %d: action=%s  desc=%q", i+1, s.Action, s.Description)
	}
	return &plan, nil
}

func formatTracesForReplanner(traces []types.StepTrace) string {
	var b strings.Builder
	for _, tr := range traces {
		role := tr.AgentRole
		if role == "" {
			role = "agent"
		}
		b.WriteString(fmt.Sprintf("- Step %d [%s]: Action: %s, Query: %s\n", tr.Step, role, tr.Action, tr.Query))
		b.WriteString(fmt.Sprintf("  Observation: %s\n", tr.Observation))
	}
	return b.String()
}

